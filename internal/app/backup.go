package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"fridge/internal/engine"
	"fridge/internal/store"
)

// BackupKey is <prefix>/<engine>-<version>/<db>/<db>-<YYYYMMDD-HHMMSS><ext> (UTC).
func BackupKey(prefix, engineName, version, db string, t time.Time, ext string) string {
	k := fmt.Sprintf("%s-%s/%s/%s-%s%s", engineName, version, db, db, t.UTC().Format("20060102-150405"), ext)
	if p := strings.Trim(prefix, "/"); p != "" {
		return p + "/" + k
	}
	return k
}

func (a *App) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// StartBackup records a running backup and streams the dump to S3 in the background.
// store.ErrConflict is returned as-is when this database already has a backup running.
func (a *App) StartBackup(ctx context.Context, dbID, destID int64, scheduleID *int64, trigger string, startedBy *int64) (store.Backup, error) {
	db, err := a.Store.GetDatabase(ctx, dbID)
	if err != nil {
		return store.Backup{}, err
	}
	if db.Status != "running" {
		return store.Backup{}, invalid("database %s is %s, not running", db.Name, db.Status)
	}
	prefix := "fridge"
	switch {
	case scheduleID != nil:
		sc, err := a.Store.GetSchedule(ctx, *scheduleID)
		if err != nil {
			return store.Backup{}, err
		}
		destID, prefix = sc.DestinationID, sc.Prefix
	case destID > 0:
	default:
		scs, err := a.Store.ListSchedules(ctx, dbID)
		if err != nil {
			return store.Backup{}, err
		}
		if len(scs) > 0 {
			destID, prefix = scs[0].DestinationID, scs[0].Prefix
		} else {
			dest, ok, err := a.ownerDestination(ctx, db)
			if err != nil {
				return store.Backup{}, err
			}
			if !ok {
				return store.Backup{}, invalid("no usable destination")
			}
			destID = dest.ID
		}
	}
	objs, _, err := a.objects(ctx, destID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Backup{}, invalid("destination %d does not exist", destID)
		}
		return store.Backup{}, err
	}
	eng, _, err := engine.Get(db.Engine)
	if err != nil {
		return store.Backup{}, err
	}
	b := store.Backup{
		DatabaseID: dbID, DestinationID: destID, ScheduleID: scheduleID, Trigger: trigger, StartedByID: startedBy,
		Engine: db.Engine, EngineVersion: db.Version,
		S3Key: BackupKey(prefix, db.Engine, db.Version, db.Name, a.clock(), eng.DumpExt()),
	}
	if err := a.Store.StartBackup(ctx, &b); err != nil {
		return store.Backup{}, err
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.runBackup(objs, db, b)
	}()
	return b, nil
}

func (a *App) runBackup(objs ObjectStore, db store.Database, b store.Backup) {
	ctx := context.Background() // outlives the request; bookkeeping uses it so a timeout cannot block recording the result
	timeout := a.backupTimeout
	if timeout == 0 {
		timeout = backupTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	eng, _, _ := engine.Get(db.Engine)
	cmd := eng.DumpCmd(nodeParams(db))

	pr, pw := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() { // stderr is only read after <-done
		code, err := a.RT.Exec(runCtx, db.ContainerID, cmd, nil, pw, &stderr)
		switch {
		case err != nil:
			err = fmt.Errorf("dump failed: %w", err)
		case code != 0:
			err = fmt.Errorf("dump exited %d", code)
		}
		pw.CloseWithError(err) // nil -> plain EOF
		done <- err
	}()

	size, putErr := objs.Put(runCtx, b.S3Key, pr, map[string]string{
		"fridge-engine": db.Engine, "fridge-version": db.Version, "fridge-db": db.Name,
	})
	if putErr != nil {
		pr.CloseWithError(putErr) // unblock the dump writer
	} else {
		pr.Close()
	}
	dumpErr := <-done

	if err := errors.Join(dumpErr, putErr); err != nil {
		a.failBackup(ctx, objs, db, b, stderr.String(), err)
		return
	}
	if err := a.Store.FinishBackup(ctx, b.ID, "ok", size, tail(stderr.String()), ""); err != nil {
		a.alertf(ctx, "backup %s: recording result failed: %v", db.Name, err)
		return
	}
	if b.ScheduleID != nil {
		a.retain(ctx, objs, *b.ScheduleID)
	}
}

func (a *App) failBackup(ctx context.Context, objs ObjectStore, db store.Database, b store.Backup, stderr string, err error) {
	if ferr := a.Store.FinishBackup(ctx, b.ID, "failed", 0, tail(stderr), strings.ToValidUTF8(err.Error(), "?")); ferr != nil {
		a.alertf(ctx, "backup %s: recording failure failed: %v (original: %v)", db.Name, ferr, err)
	}
	if derr := objs.Delete(ctx, b.S3Key); derr != nil {
		log.Printf("backup %d: cleanup %s: %v", b.ID, b.S3Key, derr)
	}
	if a.Alert != nil {
		a.Alert(ctx, fmt.Sprintf("backup failed: %s: %v", db.Name, err))
	}
}

// retain deletes ok backups of the schedule beyond KeepLatest (newest first). Manual backups have no schedule.
func (a *App) retain(ctx context.Context, objs ObjectStore, scheduleID int64) {
	sc, err := a.Store.GetSchedule(ctx, scheduleID)
	if err != nil {
		log.Printf("retention: schedule %d: %v", scheduleID, err)
		return
	}
	oks, err := a.Store.OKBackupsForSchedule(ctx, scheduleID)
	if err != nil {
		log.Printf("retention: %v", err)
		return
	}
	for i := sc.KeepLatest; i < len(oks); i++ {
		if err := objs.Delete(ctx, oks[i].S3Key); err != nil {
			log.Printf("retention: delete %s: %v", oks[i].S3Key, err)
			continue // keep the row so the object is retried next time
		}
		if err := a.Store.DeleteBackup(ctx, oks[i].ID); err != nil {
			log.Printf("retention: delete backup %d: %v", oks[i].ID, err)
		}
	}
}

// BackupDownloadURL returns a 15-minute presigned URL for an ok backup.
func (a *App) BackupDownloadURL(ctx context.Context, backupID int64) (string, error) {
	b, err := a.Store.GetBackup(ctx, backupID)
	if err != nil {
		return "", err
	}
	if b.Status != "ok" {
		return "", invalid("backup %d is %s", b.ID, b.Status)
	}
	objs, _, err := a.objects(ctx, b.DestinationID)
	if err != nil {
		return "", err
	}
	return objs.PresignGet(ctx, b.S3Key, 15*time.Minute)
}

// ponytail: fixed 6h ceiling per backup; make configurable if databases get huge.
const backupTimeout = 6 * time.Hour

// tail keeps the last 8 KiB as valid UTF-8 (Postgres TEXT rejects invalid bytes).
func tail(s string) string {
	if len(s) > 8192 {
		s = s[len(s)-8192:]
	}
	return strings.ToValidUTF8(s, "?")
}

func (a *App) alertf(ctx context.Context, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Print(msg)
	if a.Alert != nil {
		a.Alert(ctx, msg)
	}
}
