package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/minio/minio-go/v7"

	"fridge/internal/container"
	"fridge/internal/dumpcheck"
	"fridge/internal/engine"
	"fridge/internal/store"
)

// StartDumpCheck begins the deep check ("Kiểm tra kỹ") of a backup file in the background.
// The caller has already decided that the user may touch key.
func (a *App) StartDumpCheck(ctx context.Context, destID int64, key string, startedBy int64) (store.DumpCheck, error) {
	if !strings.HasSuffix(key, ".dump") && !strings.HasSuffix(key, ".sql") && !strings.HasSuffix(key, ".sql.gz") {
		return store.DumpCheck{}, invalid("unsupported file type (want .dump, .sql or .sql.gz)")
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
		a.runDumpCheck(objs, c)
	}()
	return c, nil
}

func (a *App) GetDumpCheck(ctx context.Context, id int64) (store.DumpCheck, error) {
	return a.Store.GetDumpCheck(ctx, id)
}

// countReader counts the bytes that pass through it.
type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (a *App) runDumpCheck(objs ObjectStore, c store.DumpCheck) {
	ctx := context.Background() // outlives the request
	timeout := a.backupTimeout
	if timeout == 0 {
		timeout = backupTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, log, err := a.checkFile(runCtx, objs, c)
	status, errText := "ok", ""
	if err != nil {
		status, errText = "failed", strings.ToValidUTF8(err.Error(), "?")
	}
	if ferr := a.Store.FinishDumpCheck(ctx, c.ID, status, result, tail(log), errText); ferr != nil {
		a.alertf(ctx, "dump check %d: recording result failed: %v", c.ID, ferr)
	}
}

// checkFile reads the whole object. A .dump goes through the real pg_restore in a throwaway container with
// no network; plain SQL is read in Go, so no container is needed for it.
func (a *App) checkFile(ctx context.Context, objs ObjectStore, c store.DumpCheck) (map[string]any, string, error) {
	head, err := objs.ReadHead(ctx, c.S3Key, dumpcheck.HeadSize)
	if err != nil {
		return nil, "", fmt.Errorf("read head: %w", err)
	}
	hdr, err := dumpcheck.Inspect(c.S3Key, head)
	if err != nil {
		return nil, "", err
	}
	rc, err := objs.Get(ctx, c.S3Key)
	if err != nil {
		return nil, "", fmt.Errorf("download: %w", err)
	}
	defer rc.Close()
	in := &countReader{r: rc}
	result := map[string]any{"format": hdr.Format}

	if hdr.Format == "sql" {
		var r io.Reader = in
		if hdr.Gzip {
			zr, err := gzip.NewReader(in)
			if err != nil {
				return nil, "", fmt.Errorf("not a gzip file: %w", err)
			}
			defer zr.Close()
			r = zr
		}
		var tc dumpcheck.TextChecker
		_, err := io.Copy(&tc, r)
		if err == nil {
			err = tc.Close()
		}
		result["bytes"] = in.n
		if hdr.Gzip {
			result["sql_bytes"] = tc.N
		}
		return result, "", err
	}

	result["source_version"], result["pg_dump_version"] = hdr.ServerVersion, hdr.PgDumpVersion
	_, info, err := engine.Get("postgres")
	if err != nil {
		return nil, "", err
	}
	// The newest catalog version: its pg_restore reads every archive version a supported pg_dump writes.
	spec := container.RunSpec{Name: "fridge-check-" + strconv.FormatInt(c.ID, 10), Image: "postgres:" + info.Versions[len(info.Versions)-1],
		Cmd: []string{"sleep", strconv.Itoa(int(backupTimeout.Seconds()) + 600)}, Network: container.NoNetwork, Ephemeral: true}
	id, err := a.RT.Run(ctx, spec)
	defer func() {
		if id == "" {
			id = spec.Name // Run can fail after creating the container
		}
		if rerr := a.RT.Remove(context.Background(), id, ""); rerr != nil {
			a.alertf(context.Background(), "dump check %d: removing %s: %v", c.ID, spec.Name, rerr)
		}
	}()
	if err != nil {
		return nil, "", fmt.Errorf("start checker: %w", err)
	}
	var out tailCounter
	code, err := a.RT.Exec(ctx, id, []string{"pg_restore", "-v", "-f", "/dev/null"}, in, io.Discard, &out)
	result["bytes"] = in.n
	result["toc_entries"] = out.items
	switch {
	case err != nil:
		return result, out.String(), fmt.Errorf("pg_restore: %w", err)
	case code != 0:
		return result, out.String(), fmt.Errorf("pg_restore exited %d", code)
	}
	return result, out.String(), nil
}

// tailCounter keeps the last 8 KiB of what it is fed and counts pg_restore's "processing item" lines.
type tailCounter struct {
	items int
	line  []byte
	buf   []byte
}

func (t *tailCounter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 16384 {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-8192:]...)
	}
	for _, b := range p {
		if b != '\n' {
			if len(t.line) < 512 {
				t.line = append(t.line, b)
			}
			continue
		}
		if isTOCLine(t.line) {
			t.items++
		}
		t.line = t.line[:0]
	}
	return len(p), nil
}

func (t *tailCounter) String() string { return string(t.buf) }

// isTOCLine: pg_restore -v names every archive entry it works through, one line each
// ("processing item" when restoring into a database, "creating ..." / "processing data ..." when writing a script).
// ponytail: best effort — the count is for display only.
func isTOCLine(l []byte) bool {
	for _, p := range []string{"pg_restore: creating ", "pg_restore: processing data ", "pg_restore: executing ", "pg_restore: processing item "} {
		if bytes.HasPrefix(l, []byte(p)) {
			return true
		}
	}
	return false
}
