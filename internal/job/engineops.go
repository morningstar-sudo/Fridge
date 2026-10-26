package job

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fridge/internal/engine"
)

// execScript runs an admin script (create a user, change a password) on a node, as its superuser, for the engines that
// have no Go driver in Fridge: it waits until the node accepts an authenticated connection, then feeds Script to the
// engine's admin shell once. Waiting is separate from running so that a statement the server refuses is not retried.
func execScript(ctx context.Context, s Spec, stdout, stderr io.Writer) error {
	eng, _, err := engine.Get(s.Engine)
	if err != nil {
		return err
	}
	if s.Script == "" {
		return errors.New("exec: no script")
	}
	deadline := time.Now().Add(readyWait)
	for {
		ping := command(ctx, s, eng.PingCmd(s.Params), io.Discard)
		perr := ping.Run()
		if perr == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("the node did not accept an admin connection: %w", perr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	c := command(ctx, s, eng.AdminCmd(s.Params), stderr)
	var out bytes.Buffer // some tools (redis-cli) exit 0 on a refused command and say so only in their output
	c.Stdout = &out
	c.Stdin = strings.NewReader(s.Script)
	if err := c.Run(); err != nil {
		return fmt.Errorf("admin script failed: %w", err)
	}
	result(stdout, map[string]any{"output": truncate(out.String(), 4096)}) // what the shell printed: the answer of a query (nothing for a provisioning script)
	if eng.AdminFailed(out.String()) {
		return errors.New("admin script failed: the engine refused a command")
	}
	return nil
}

// readyWait is how long exec waits for a node that has just been scheduled (the first start initialises the data directory).
var readyWait = 2 * time.Minute

// preload puts a backup file where the engine reads its data at start (Redis: dump.rdb in the data volume), for the
// engines that cannot load a file into a running node. The job runs on the node's host with the volume mounted at /data,
// before the database service exists; the file is handed to the user the database runs as.
func preload(ctx context.Context, s Spec, stderr io.Writer) error {
	eng, _, err := engine.Get(s.Engine)
	if err != nil {
		return err
	}
	rel, uid := eng.Preload(s.Key)
	if rel == "" {
		return fmt.Errorf("%s does not take a preloaded file", s.Engine)
	}
	objs, err := open(s)
	if err != nil {
		return err
	}
	rc, err := objs.Get(ctx, s.Key)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer rc.Close()
	dst := filepath.Join(preloadRoot, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, rc)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	if uid > 0 {
		if err := chown(dst, uid, uid); err != nil {
			return fmt.Errorf("chown: %w", err)
		}
	}
	fmt.Fprintf(stderr, "preloaded %d bytes into %s\n", n, rel)
	return nil
}

// chown is os.Chown; tests replace it (they do not run as root).
var chown = os.Chown

// preloadRoot is where the node's data volume is mounted in a preload job.
var preloadRoot = "/data"

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// pingOnce succeeds when the node accepts an authenticated admin connection right now (no waiting): how the tests, and
// anything else, learn whether a certificate and a password are accepted.
func pingOnce(ctx context.Context, s Spec, stderr io.Writer) error {
	eng, _, err := engine.Get(s.Engine)
	if err != nil {
		return err
	}
	return command(ctx, s, eng.PingCmd(s.Params), stderr).Run()
}
