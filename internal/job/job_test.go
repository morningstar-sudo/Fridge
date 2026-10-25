package job

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"fridge/internal/engine"
)

// memBucket is an S3 bucket in memory.
type memBucket struct {
	mu   sync.Mutex
	objs map[string][]byte
	meta map[string]map[string]string
}

func (m *memBucket) Put(ctx context.Context, key string, r io.Reader, meta map[string]string) (int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key], m.meta[key] = b, meta
	return int64(len(b)), nil
}

func (m *memBucket) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memBucket) ReadHead(ctx context.Context, key string, n int) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objs[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b[:min(n, len(b))], nil
}

// tools puts fake client tools first on PATH. Each script appends "name args" to $LOG, stores its stdin in
// $LOG.stdin-<name>, then does what the test script body says. Version "99" has no /usr/lib/postgresql dir,
// so the job falls back to PATH.
func tools(t *testing.T, bodies map[string]string) (logFile string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts stand in for the PostgreSQL client tools")
	}
	dir := t.TempDir()
	logFile = filepath.Join(dir, "log")
	for name, body := range bodies {
		script := "#!/bin/sh\necho \"" + name + " $* PW=$PGPASSWORD\" >> \"" + logFile + "\"\n" + body + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logFile
}

func bucket(t *testing.T) *memBucket {
	t.Helper()
	b := &memBucket{objs: map[string][]byte{}, meta: map[string]map[string]string{}}
	old := open
	open = func(Spec) (Objects, error) { return b, nil }
	t.Cleanup(func() { open = old })
	return b
}

func params() engine.Params {
	return engine.Params{Name: "alice_shop", User: "alice_shop", AdminUser: "su_0123456789abcdef", AdminPassword: "sekret", Version: "99", Host: "fridge-db-0123456789abcdef", Port: 5432}
}

func run(t *testing.T, s Spec) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Run(context.Background(), s, &out, &errb)
	return code, out.String(), errb.String()
}

func resultOf(t *testing.T, stdout string) map[string]any {
	t.Helper()
	for _, l := range strings.Split(stdout, "\n") {
		if rest, ok := strings.CutPrefix(l, ResultPrefix); ok {
			var m map[string]any
			if err := json.Unmarshal([]byte(rest), &m); err != nil {
				t.Fatal(err)
			}
			return m
		}
	}
	t.Fatalf("no %q line in %q", ResultPrefix, stdout)
	return nil
}

func TestDumpUploadsWhatPgDumpWrites(t *testing.T) {
	log := tools(t, map[string]string{"pg_dump": `printf 'DUMPBYTES'`})
	b := bucket(t)
	meta := map[string]string{"fridge-engine": "postgres", "fridge-db": "alice_shop"}
	code, out, errs := run(t, Spec{Op: "dump", Engine: "postgres", Params: params(), Key: "fridge/x.dump", Meta: meta})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if string(b.objs["fridge/x.dump"]) != "DUMPBYTES" || b.meta["fridge/x.dump"]["fridge-db"] != "alice_shop" {
		t.Fatalf("object %q meta %v", b.objs["fridge/x.dump"], b.meta)
	}
	if r := resultOf(t, out); r["size"] != float64(9) {
		t.Fatalf("result %v", r)
	}
	got, _ := os.ReadFile(log)
	want := "pg_dump -U su_0123456789abcdef -d alice_shop -Fc -h fridge-db-0123456789abcdef -p 5432 PW=sekret"
	if strings.TrimSpace(string(got)) != want {
		t.Fatalf("pg_dump ran as %q\nwant %q", got, want)
	}
	if strings.Contains(errs+out, "sekret") {
		t.Fatal("the password reached the job's output")
	}
}

func TestDumpFailureReportsAndSaysWhy(t *testing.T) {
	tools(t, map[string]string{"pg_dump": `printf PART; echo 'pg_dump: error: boom' >&2; exit 3`})
	bucket(t)
	code, out, errs := run(t, Spec{Op: "dump", Engine: "postgres", Params: params(), Key: "k.dump"})
	if code != 1 || !strings.Contains(errs, "boom") || !strings.Contains(errs, "job failed: dump failed") || strings.Contains(out, ResultPrefix) {
		t.Fatalf("exit %d out %q err %q", code, out, errs)
	}
}

func TestRestoreFeedsTheToolAndHandsObjectsToTheAppUser(t *testing.T) {
	log := tools(t, map[string]string{"pg_restore": `cat > "$0.stdin"`, "psql": `cat > "$0.stdin"`})
	b := bucket(t)
	b.objs["a/x.dump"] = []byte("ARCHIVE")
	b.objs["u/x.sql.gz"] = gzipped("CREATE TABLE t(id int);")
	binDir := filepath.Dir(log)

	if code, _, errs := run(t, Spec{Op: "restore", Engine: "postgres", Params: params(), Key: "a/x.dump"}); code != 0 {
		t.Fatalf("dump restore: %d %s", code, errs)
	}
	if in, _ := os.ReadFile(filepath.Join(binDir, "pg_restore.stdin")); string(in) != "ARCHIVE" {
		t.Fatalf("pg_restore stdin %q", in)
	}
	if code, _, errs := run(t, Spec{Op: "restore", Engine: "postgres", Params: params(), Key: "u/x.sql.gz"}); code != 0 {
		t.Fatalf("sql restore: %d %s", code, errs)
	}
	if in, _ := os.ReadFile(filepath.Join(binDir, "psql.stdin")); string(in) != "SET ROLE \"alice_shop\";\nCREATE TABLE t(id int);" {
		t.Fatalf("psql stdin %q", in)
	}
	lines, _ := os.ReadFile(log)
	for _, want := range []string{"--role=alice_shop", "-h fridge-db-0123456789abcdef -p 5432", "PW=sekret", "psql -U su_0123456789abcdef -d alice_shop -v ON_ERROR_STOP=1 -q -h fridge-db-0123456789abcdef"} {
		if !strings.Contains(string(lines), want) {
			t.Errorf("tool log %q lacks %q", lines, want)
		}
	}
}

func TestRestoreFailuresFail(t *testing.T) {
	tools(t, map[string]string{"pg_restore": `cat >/dev/null; echo 'pg_restore: error: nope' >&2; exit 1`})
	b := bucket(t)
	b.objs["a/x.dump"] = []byte("ARCHIVE")
	b.objs["u/bad.sql.gz"] = []byte("not gzip")
	if code, _, errs := run(t, Spec{Op: "restore", Engine: "postgres", Params: params(), Key: "a/x.dump"}); code != 1 || !strings.Contains(errs, "restore failed") {
		t.Fatalf("exit %d %q", code, errs)
	}
	if code, _, errs := run(t, Spec{Op: "restore", Engine: "postgres", Params: params(), Key: "u/bad.sql.gz"}); code != 1 || !strings.Contains(errs, "not a gzip file") {
		t.Fatalf("exit %d %q", code, errs)
	}
	if code, _, errs := run(t, Spec{Op: "restore", Engine: "postgres", Params: params(), Key: "missing.dump"}); code != 1 || !strings.Contains(errs, "download") {
		t.Fatalf("exit %d %q", code, errs)
	}
}

func gzipped(s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func testdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "dumpcheck", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCheckSQLInGo(t *testing.T) {
	b := bucket(t)
	full := gzipped("CREATE TABLE t(id int);\n" + strings.Repeat("INSERT INTO t VALUES (1);\n", 5000))
	b.objs["ok.sql.gz"] = full
	b.objs["cut.sql.gz"] = full[:len(full)/2]
	b.objs["nul.sql"] = []byte("CREATE TABLE t(id int);\n\x00")
	b.objs["png.sql"] = []byte("hello, this is a letter\n")

	code, out, errs := run(t, Spec{Op: "check", Engine: "postgres", Params: params(), Key: "ok.sql.gz"})
	if r := resultOf(t, out); code != 0 || r["format"] != "sql" || r["bytes"] != float64(len(full)) || r["sql_bytes"] == nil {
		t.Fatalf("intact: %d %v %q", code, r, errs)
	}
	for key, why := range map[string]string{"cut.sql.gz": "unexpected EOF", "nul.sql": "NUL", "png.sql": "not a pg_dump SQL"} {
		code, out, errs := run(t, Spec{Op: "check", Engine: "postgres", Params: params(), Key: key})
		if code != 1 || !strings.Contains(errs, "job failed: ") || !strings.Contains(errs, why) {
			t.Errorf("%s: exit %d %q", key, code, errs)
		}
		if r := resultOf(t, out); key == "cut.sql.gz" && r["bytes"] == nil { // the other two never get past the header check
			t.Errorf("%s: no byte count in %v", key, r)
		}
	}
}

func TestCheckCustomArchiveThroughPgRestore(t *testing.T) {
	// the stand-in drains stdin like pg_restore and logs entries the way -v does
	tools(t, map[string]string{"pg_restore": `cat >/dev/null; echo 'pg_restore: creating TABLE "public.note"' >&2; echo 'pg_restore: processing data for table "public.note"' >&2`})
	b := bucket(t)
	data := testdata(t, "pg14.dump")
	b.objs["up/a.dump"] = data
	b.objs["up/png.dump"] = testdata(t, "not-a-dump.dump")

	code, out, errs := run(t, Spec{Op: "check", Engine: "postgres", Params: params(), Key: "up/a.dump"})
	r := resultOf(t, out)
	if code != 0 || r["format"] != "custom" || r["bytes"] != float64(len(data)) || r["toc_entries"] != float64(2) || r["source_version"] == "" || r["pg_dump_version"] == "" {
		t.Fatalf("%d %v %q", code, r, errs)
	}
	if code, out, errs := run(t, Spec{Op: "check", Engine: "postgres", Params: params(), Key: "up/png.dump"}); code != 1 || !strings.Contains(errs, "PGDMP") || resultOf(t, out)["format"] != nil {
		t.Fatalf("png: %d %q %q", code, out, errs)
	}
}

func TestCheckPgRestoreRefusingTheFile(t *testing.T) {
	tools(t, map[string]string{"pg_restore": `cat >/dev/null; echo 'pg_restore: error: could not read from input file: end of file' >&2; exit 1`})
	b := bucket(t)
	b.objs["up/cut.dump"] = testdata(t, "pg14.dump")
	code, out, errs := run(t, Spec{Op: "check", Engine: "postgres", Params: params(), Key: "up/cut.dump"})
	if code != 1 || !strings.Contains(errs, "end of file") || !strings.Contains(errs, "job failed: pg_restore:") || resultOf(t, out)["bytes"] == nil {
		t.Fatalf("%d %q %q", code, out, errs)
	}
}

func TestUnknownJob(t *testing.T) {
	if code, _, errs := run(t, Spec{Op: "frobnicate"}); code != 1 || !strings.Contains(errs, "unknown job") {
		t.Fatalf("%d %q", code, errs)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv(EnvSpec, "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("empty env accepted")
	}
	raw, _ := json.Marshal(Spec{Op: "dump", Key: "k", S3: S3{Bucket: "b", SecretKey: "s"}})
	t.Setenv(EnvSpec, string(raw))
	s, err := FromEnv()
	if err != nil || s.Op != "dump" || s.S3.SecretKey != "s" {
		t.Fatalf("%+v %v", s, err)
	}
}
