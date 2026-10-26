package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"

	"fridge/internal/engine"
	"fridge/internal/job"
	"fridge/internal/store"
)

// StartDumpCheck begins the deep check ("Kiểm tra kỹ") of a backup file in the background.
// The caller has already decided that the user may touch key.
func (a *App) StartDumpCheck(ctx context.Context, destID int64, key string, startedBy int64) (store.DumpCheck, error) {
	if !restorableKey(key) {
		return store.DumpCheck{}, invalid("unsupported file type (want %s)", strings.Join(backupExts, ", "))
	}
	objs, _, err := a.objects(ctx, destID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.DumpCheck{}, invalid("destination %d does not exist", destID)
		}
		return store.DumpCheck{}, err
	}
	if _, err := objs.Meta(ctx, key); err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return store.DumpCheck{}, fmt.Errorf("%w: object %q", store.ErrNotFound, key)
		}
		return store.DumpCheck{}, err
	}
	c := store.DumpCheck{DestinationID: destID, S3Key: key, StartedByID: &startedBy}
	if err := a.Store.StartDumpCheck(ctx, &c); err != nil {
		return store.DumpCheck{}, err
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.runDumpCheck(c)
	}()
	return c, nil
}

func (a *App) GetDumpCheck(ctx context.Context, id int64) (store.DumpCheck, error) {
	return a.Store.GetDumpCheck(ctx, id)
}

func (a *App) runDumpCheck(c store.DumpCheck) {
	ctx := context.Background() // outlives the request
	timeout := a.backupTimeout
	if timeout == 0 {
		timeout = backupTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, log, err := a.checkFile(runCtx, c, timeout)
	status, errText := "ok", ""
	if err != nil {
		status, errText = "failed", strings.ToValidUTF8(reason(log, err), "?")
	}
	if ferr := a.Store.FinishDumpCheck(ctx, c.ID, status, result, tail(log), errText); ferr != nil {
		a.alertf(ctx, "dump check %d: recording result failed: %v", c.ID, ferr)
	}
}

// checkFile runs the check as a job: it needs S3 (the egress network) and no database network at all. A custom
// archive goes through the real pg_restore of the archive's own version, writing to /dev/null.
func (a *App) checkFile(ctx context.Context, c store.DumpCheck, timeout time.Duration) (map[string]any, string, error) {
	s3, guard, err := a.s3Job(ctx, c.DestinationID)
	if err != nil {
		return nil, "", err
	}
	eng, _ := DetectSource(nil, c.S3Key) // from the key: "<engine>-<version>" path segment or file extension
	if eng == "" {
		eng = "postgres" // plain .sql and everything undecided are looked at as before: a pg_dump file
	}
	_, info, err := engine.Get(eng)
	if err != nil {
		return nil, "", err
	}
	// the newest client reads every older archive; the job needs only S3, never the database network
	spec := job.Spec{Op: "check", Engine: eng, Params: engine.Params{Version: info.Versions[len(info.Versions)-1]}, S3: s3, Guard: guard, Key: c.S3Key}
	return a.runJob(ctx, "check", spec, []string{a.EgressNetwork}, timeout)
}

// reason is why a failed job failed: its own "job failed: …" line, else the error that ended the run.
func reason(stderr string, err error) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if r, ok := strings.CutPrefix(lines[i], "job failed: "); ok {
			return r
		}
	}
	return err.Error()
}

// backupExts are the file types Fridge can check and restore: Postgres, MySQL/MariaDB, MongoDB and Redis dumps.
var backupExts = []string{".dump", ".sql", ".sql.gz", ".archive.gz", ".rdb"}

func restorableKey(key string) bool {
	for _, e := range backupExts {
		if strings.HasSuffix(key, e) {
			return true
		}
	}
	return false
}
