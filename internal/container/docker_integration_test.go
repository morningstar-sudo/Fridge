//go:build integration

package container

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestRunExecRemove(t *testing.T) {
	ctx := context.Background()
	c, err := New("fridge-test-net")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureNetwork(ctx); err != nil { // idempotent
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.cli.NetworkRemove(ctx, "fridge-test-net") })

	spec := RunSpec{
		Name: "fridge-it-pg", Image: "postgres:17-alpine",
		Env:         []string{"POSTGRES_PASSWORD=x"},
		Volume:      "fridge-it-pg",
		VolumePath:  "/var/lib/postgresql/data",
		Healthcheck: []string{"CMD-SHELL", "pg_isready -U postgres"},
		Labels:      map[string]string{"fridge.test": "1"},
	}
	_ = c.Remove(ctx, spec.Name, spec.Volume) // leftovers from a failed run
	id, err := c.Run(ctx, spec)
	t.Cleanup(func() { _ = c.Remove(ctx, spec.Name, spec.Volume) })
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WaitHealthy(ctx, id, 60*time.Second); err != nil {
		t.Fatal(err)
	}

	run := func(stdin string, cmd ...string) (int, string) {
		t.Helper()
		var out, errb bytes.Buffer
		var code int
		var err error
		if stdin != "" {
			code, err = c.Exec(ctx, id, cmd, strings.NewReader(stdin), &out, &errb)
		} else {
			code, err = c.Exec(ctx, id, cmd, nil, &out, &errb)
		}
		if err != nil {
			t.Fatal(err)
		}
		return code, strings.TrimSpace(out.String())
	}
	if code, out := run("", "psql", "-U", "postgres", "-tAc", "select 41+1"); code != 0 || out != "42" {
		t.Fatalf("got %d %q", code, out)
	}
	if code, out := run("select 1;", "psql", "-U", "postgres", "-tA"); code != 0 || out != "1" {
		t.Fatalf("stdin: got %d %q", code, out)
	}
	if code, _ := run("", "sh", "-c", "exit 3"); code != 3 {
		t.Fatalf("exit code %d", code)
	}

	// stdin that fails mid-stream must surface as an error, not a clean exit
	bad := io.MultiReader(strings.NewReader("select 1;"), iotest.ErrReader(errors.New("boom")))
	if code, err := c.Exec(ctx, id, []string{"psql", "-U", "postgres", "-tA"}, bad, io.Discard, io.Discard); err == nil {
		t.Fatalf("expected stdin error, got code %d", code)
	}

	// a stalled exec must honour ctx (the backup timeout depends on it)
	tctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	start := time.Now()
	if code, err := c.Exec(tctx, id, []string{"sleep", "300"}, nil, io.Discard, io.Discard); err == nil {
		t.Fatalf("stalled exec: want error, got code %d", code)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("stalled exec returned after %s", d)
	}

	if err := c.Remove(ctx, id, spec.Volume); err != nil {
		t.Fatal(err)
	}
	if _, err := c.cli.ContainerInspect(ctx, id); err == nil {
		t.Fatal("container still exists")
	}
	if err := c.Remove(ctx, id, spec.Volume); err != nil {
		t.Fatalf("second remove: %v", err)
	}
}
