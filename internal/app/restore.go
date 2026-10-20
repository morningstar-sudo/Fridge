package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/minio/minio-go/v7"

	"fridge/internal/engine"
	"fridge/internal/store"
)

var segRe = regexp.MustCompile(`^(postgres|mysql|mariadb|mongo|redis)-(\d+(?:\.\d+)*)$`)

// DetectSource guesses the engine/version of a backup file: object metadata, then a
// "<engine>-<version>" path segment, then the file extension (version unknown).
func DetectSource(meta map[string]string, key string) (eng, version string) {
	if e := meta["fridge-engine"]; e != "" {
		return e, meta["fridge-version"]
	}
	for _, seg := range strings.Split(key, "/") {
		if m := segRe.FindStringSubmatch(seg); m != nil {
			return m[1], m[2]
		}
	}
	switch {
	case strings.HasSuffix(key, ".dump"):
		return "postgres", ""
	case strings.HasSuffix(key, ".rdb"):
		return "redis", ""
	case strings.HasSuffix(key, ".archive.gz"):
		return "mongo", ""
	}
	return "", ""
}

// CheckCompat rejects restores into a different engine or an older server version.
func CheckCompat(srcEngine, srcVer, dstEngine, dstVer string) (string, error) {
	if srcEngine != dstEngine {
		if (srcEngine == "mysql" && dstEngine == "mariadb") || (srcEngine == "mariadb" && dstEngine == "mysql") {
			return "cross MySQL/MariaDB restore: verify compatibility", nil
		}
		return "", invalid("cannot restore %s backup into %s", srcEngine, dstEngine)
	}
	if srcVer != "" && engine.CompareVersions(dstVer, srcVer) < 0 {
		return "", invalid("target version %s is older than source %s", dstVer, srcVer)
	}
	return "", nil
}

type RestoreRequest struct {
	DestinationID    int64
	Key, Engine      string
	New              *CreateRequest
	TargetDatabaseID int64
	OwnerID          int64 // required, > 0: owns the new node, recorded as started_by
}

// StartRestore validates, records a running restore and restores into a brand-new node in the background.
func (a *App) StartRestore(ctx context.Context, req RestoreRequest) (store.Restore, error) {
	if req.TargetDatabaseID != 0 {
		return store.Restore{}, invalid("overwrite lands in phase 3")
	}
	if req.New == nil {
		return store.Restore{}, invalid("new database is required")
	}
	if req.OwnerID <= 0 {
		return store.Restore{}, invalid("owner is required")
	}
	nr := *req.New
	nr.OwnerID = req.OwnerID
	nr.Domain = strings.ToLower(strings.TrimSpace(nr.Domain))
	if !validDomain(nr.Domain) {
		return store.Restore{}, invalid("domain must be a hostname")
	}
	name, _, _, err := a.resolveNames(ctx, nr)
	if err != nil {
		return store.Restore{}, err
	}
	nr.Name = name // full name; CreateDatabase re-applies the (idempotent) prefix rule
	objs, _, err := a.objects(ctx, req.DestinationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.Restore{}, invalid("destination %d does not exist", req.DestinationID)
		}
		return store.Restore{}, err
	}
	meta, err := objs.Meta(ctx, req.Key)
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return store.Restore{}, fmt.Errorf("%w: object %q", store.ErrNotFound, req.Key)
		}
		return store.Restore{}, err
	}
	srcEng, srcVer := DetectSource(meta, req.Key)
	switch {
	case srcEng == "" && req.Engine == "":
		return store.Restore{}, invalid("cannot detect engine; pass engine")
	case srcEng != "" && req.Engine != "" && srcEng != req.Engine:
		return store.Restore{}, invalid("file looks like %s but engine %s was requested", srcEng, req.Engine)
	case srcEng == "":
		srcEng = req.Engine
	}
	eng, _, err := engine.Get(srcEng)
	if err != nil {
		return store.Restore{}, invalid("%v", err)
	}
	nr.Engine = srcEng
	if nr.Version == "" && srcVer != "" {
		if v, err := engine.ResolveVersion(srcEng, srcVer); err == nil {
			nr.Version = v
		}
	}
	if nr.Version, err = engine.ResolveVersion(srcEng, nr.Version); err != nil {
		return store.Restore{}, invalid("%v", err)
	}
	warning, err := CheckCompat(srcEng, srcVer, srcEng, nr.Version)
	if err != nil {
		return store.Restore{}, err
	}
	if _, _, _, err := eng.RestoreCmd(engine.Params{}, req.Key); err != nil {
		return store.Restore{}, invalid("%v", err)
	}
	if _, err := a.Store.GetDatabaseByName(ctx, nr.Name); err == nil {
		return store.Restore{}, fmt.Errorf("%w: database %q exists", store.ErrConflict, nr.Name)
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Restore{}, err
	}

	// ponytail: linear scan; add a store query if database counts get large.
	dbs, err := a.Store.ListDatabases(ctx)
	if err != nil {
		return store.Restore{}, err
	}
	for _, d := range dbs {
		if d.Status != "error" && d.Status != "deleted" && d.Engine == srcEng && d.Domain == nr.Domain {
			return store.Restore{}, fmt.Errorf("%w: domain %q in use", store.ErrConflict, nr.Domain)
		}
	}

	r := store.Restore{DestinationID: req.DestinationID, S3Key: req.Key, SourceEngine: srcEng, SourceVersion: srcVer, Mode: "new", StartedByID: &req.OwnerID}
	if warning != "" {
		r.Log = warning + "\n"
	}
	if err := a.Store.StartRestore(ctx, &r); err != nil {
		return store.Restore{}, err
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.runRestore(objs, eng, r, nr, r.Log)
	}()
	return r, nil
}

func (a *App) runRestore(objs ObjectStore, eng engine.Engine, r store.Restore, nr CreateRequest, prefixLog string) {
	ctx := context.Background() // outlives the request
	timeout := a.backupTimeout
	if timeout == 0 {
		timeout = backupTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stderr bytes.Buffer
	var node *store.Database
	err := func() error {
		d, err := a.CreateDatabase(runCtx, nr)
		if err != nil {
			return fmt.Errorf("create node: %w", err)
		}
		node = &d
		if err := a.Store.SetRestoreTarget(ctx, r.ID, d.ID); err != nil {
			return err
		}
		cmd, gunzip, setRole, err := eng.RestoreCmd(nodeParams(d), r.S3Key)
		if err != nil {
			return err
		}
		rc, err := objs.Get(runCtx, r.S3Key)
		if err != nil {
			return fmt.Errorf("download: %w", err)
		}
		defer rc.Close()
		var in io.Reader = rc
		if gunzip {
			zr, err := gzip.NewReader(rc)
			if err != nil {
				return fmt.Errorf("not a gzip file: %w", err)
			}
			defer zr.Close()
			in = zr
		}
		if setRole { // objects from a plain SQL file must belong to the app user, not the superuser
			in = io.MultiReader(strings.NewReader("SET ROLE "+engine.QuoteIdent(d.User)+";\n"), in)
		}
		code, err := a.RT.Exec(runCtx, d.ContainerID, cmd, in, io.Discard, &stderr)
		switch {
		case err != nil:
			return fmt.Errorf("restore failed: %w", err)
		case code != 0:
			return fmt.Errorf("restore exited %d", code)
		}
		return nil
	}()

	logText := tail(prefixLog + stderr.String())
	if err == nil {
		if ferr := a.Store.FinishRestore(ctx, r.ID, "ok", logText, ""); ferr != nil {
			a.alertf(ctx, "restore %d: recording result failed: %v", r.ID, ferr)
		}
		return
	}
	if node != nil {
		a.removeNode(ctx, *node)
	}
	if ferr := a.Store.FinishRestore(ctx, r.ID, "failed", logText, strings.ToValidUTF8(err.Error(), "?")); ferr != nil {
		a.alertf(ctx, "restore %d: recording failure failed: %v (original: %v)", r.ID, ferr, err)
	}
	if a.Alert != nil {
		a.Alert(ctx, fmt.Sprintf("restore failed: %s: %v", r.S3Key, err))
	}
}
