package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"fridge/internal/container"
	"fridge/internal/store"
)

func waitCheck(t *testing.T, a *App, id int64) store.DumpCheck {
	t.Helper()
	a.Wait()
	c, err := a.GetDumpCheck(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDumpCheckSQLGzTruncated(t *testing.T) {
	a, rt, objs := newTestApp(t)
	ctx := context.Background()
	full := gz("CREATE TABLE t(id int);\n" + strings.Repeat("INSERT INTO t VALUES (1);\n", 5000))
	dest := restoreFixture(t, a, objs, "up/ok.sql.gz", full, nil)
	objs.objs["up/cut.sql.gz"] = fakeObj{full[:len(full)/2], nil}
	objs.objs["up/nul.sql"] = fakeObj{[]byte("CREATE TABLE t(id int);\n\x00"), nil}

	c, err := a.StartDumpCheck(ctx, dest.ID, "up/ok.sql.gz", own(t, a))
	if err != nil {
		t.Fatal(err)
	}
	got := waitCheck(t, a, c.ID)
	if got.Status != "ok" || got.Result["format"] != "sql" || got.Result["bytes"] != float64(len(full)) {
		t.Fatalf("intact: %+v", got)
	}
	c, _ = a.StartDumpCheck(ctx, dest.ID, "up/cut.sql.gz", own(t, a))
	if got = waitCheck(t, a, c.ID); got.Status != "failed" || !strings.Contains(got.Error, "unexpected EOF") || got.FinishedAt == nil {
		t.Fatalf("truncated: %+v", got)
	}
	c, _ = a.StartDumpCheck(ctx, dest.ID, "up/nul.sql", own(t, a))
	if got = waitCheck(t, a, c.ID); got.Status != "failed" || !strings.Contains(got.Error, "NUL") {
		t.Fatalf("NUL: %+v", got)
	}
	if len(rt.runs) != 0 {
		t.Fatalf("plain SQL started %d container(s)", len(rt.runs))
	}
	for _, bad := range []struct {
		key  string
		want error
	}{{"up/nope.sql", store.ErrNotFound}, {"up/x.zip", ErrInvalid}} {
		if _, err := a.StartDumpCheck(ctx, dest.ID, bad.key, own(t, a)); !errors.Is(err, bad.want) {
			t.Errorf("%s: %v", bad.key, err)
		}
	}
	if _, err := a.StartDumpCheck(ctx, 999, "up/ok.sql.gz", own(t, a)); !errors.Is(err, ErrInvalid) {
		t.Errorf("no destination: %v", err)
	}
}

func TestDumpCheckDumpUsesTempContainer(t *testing.T) {
	a, rt, objs := newTestApp(t)
	ctx := context.Background()
	data := dumpFile(t, "pg14.dump")
	dest := restoreFixture(t, a, objs, "up/a.dump", data, nil)
	objs.objs["up/bad.dump"] = fakeObj{data, nil}

	var stdin string
	var cmd []string
	rt.execFn = func(c []string, in io.Reader, stdout, stderr io.Writer) int {
		b, _ := io.ReadAll(in)
		stdin, cmd = string(b), c
		stderr.Write([]byte("pg_restore: connecting to database for restore\npg_restore: processing item 1 ENCODING\npg_restore: processing item 2 STDSTRINGS\n"))
		return 0
	}
	c, err := a.StartDumpCheck(ctx, dest.ID, "up/a.dump", own(t, a))
	if err != nil {
		t.Fatal(err)
	}
	got := waitCheck(t, a, c.ID)
	if got.Status != "ok" || got.Result["toc_entries"] != float64(2) || got.Result["bytes"] != float64(len(data)) ||
		got.Result["source_version"] == "" || got.Result["pg_dump_version"] == "" {
		t.Fatalf("%+v", got)
	}
	if stdin != string(data) || strings.Join(cmd, " ") != "pg_restore -v -f /dev/null" {
		t.Fatalf("exec %q stdin ok=%v", cmd, stdin == string(data))
	}
	if len(rt.runs) != 1 {
		t.Fatalf("runs: %+v", rt.runs)
	}
	r := rt.runs[0]
	if r.Network != container.NoNetwork || !r.Ephemeral || r.Cmd[0] != "sleep" || r.Name != "fridge-check-1" || !strings.HasPrefix(r.Image, "postgres:") || r.Volume != "" {
		t.Fatalf("run spec %+v", r)
	}
	if len(rt.removes) != 1 || rt.removes[0].ID != "ctr-fridge-check-1" {
		t.Fatalf("removes %+v", rt.removes)
	}

	// pg_restore refuses the file: the check fails with its message, and the container still goes
	rt.execFn = func(c []string, in io.Reader, stdout, stderr io.Writer) int {
		io.Copy(io.Discard, in)
		stderr.Write([]byte("pg_restore: error: could not read from input file: end of file"))
		return 1
	}
	c, _ = a.StartDumpCheck(ctx, dest.ID, "up/bad.dump", own(t, a))
	got = waitCheck(t, a, c.ID)
	if got.Status != "failed" || !strings.Contains(got.Log, "end of file") || !strings.Contains(got.Error, "exited 1") {
		t.Fatalf("%+v", got)
	}
	if len(rt.removes) != 2 {
		t.Fatalf("removes %+v", rt.removes)
	}
	// a file with a bad header never reaches a container
	objs.objs["up/png.dump"] = fakeObj{dumpFile(t, "not-a-dump.dump"), nil}
	c, _ = a.StartDumpCheck(ctx, dest.ID, "up/png.dump", own(t, a))
	if got = waitCheck(t, a, c.ID); got.Status != "failed" || !strings.Contains(got.Error, "PGDMP") || len(rt.runs) != 2 {
		t.Fatalf("%+v runs=%d", got, len(rt.runs))
	}
}
