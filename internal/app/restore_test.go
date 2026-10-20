package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"fridge/internal/store"
)

func TestDetectSource(t *testing.T) {
	cases := []struct {
		meta     map[string]string
		key      string
		eng, ver string
	}{
		{map[string]string{"fridge-engine": "postgres", "fridge-version": "16"}, "x.dump", "postgres", "16"},
		{nil, "fridge/postgres-17/abc/abc-1.dump", "postgres", "17"},
		{nil, "manual/x.dump", "postgres", ""},
		{nil, "manual/x.sql.gz", "", ""},
		{nil, "m/x.rdb", "redis", ""},
		{nil, "m/x.archive.gz", "mongo", ""},
	}
	for _, c := range cases {
		if e, v := DetectSource(c.meta, c.key); e != c.eng || v != c.ver {
			t.Errorf("%s: got %q,%q want %q,%q", c.key, e, v, c.eng, c.ver)
		}
	}
}

func TestCheckCompat(t *testing.T) {
	if _, err := CheckCompat("postgres", "17", "postgres", "14"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "older") {
		t.Fatalf("downgrade: %v", err)
	}
	for _, c := range [][2]string{{"14", "17"}, {"", "13"}} {
		if _, err := CheckCompat("postgres", c[0], "postgres", c[1]); err != nil {
			t.Fatalf("%v: %v", c, err)
		}
	}
	if w, err := CheckCompat("mysql", "8.4", "mariadb", "11.4"); err != nil || w == "" {
		t.Fatalf("mysql/mariadb: %q %v", w, err)
	}
	if _, err := CheckCompat("postgres", "", "redis", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cross: %v", err)
	}
}

func restoreFixture(t *testing.T, a *App, objs *fakeObjects, key string, data []byte, meta map[string]string) store.Destination {
	t.Helper()
	dest, err := a.CreateDestination(context.Background(), store.Destination{Name: "s3", Endpoint: "http://localhost:9000", Bucket: "b", AccessKey: "ak"}, "sk")
	if err != nil {
		t.Fatal(err)
	}
	objs.objs[key] = fakeObj{data, meta}
	return dest
}

func gz(s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func TestRestoreHappyPath(t *testing.T) {
	a, rt, objs := newTestApp(t)
	key := "fridge/postgres-14/abc/abc-1.dump"
	dest := restoreFixture(t, a, objs, key, []byte("DUMP"), map[string]string{"fridge-engine": "postgres", "fridge-version": "14"})
	var cmd0, in string
	rt.execFn = func(cmd []string, stdin io.Reader, stdout, stderr io.Writer) int {
		b, _ := io.ReadAll(stdin)
		cmd0, in = cmd[0], string(b)
		return 0
	}
	r, err := a.StartRestore(context.Background(), RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, New: &CreateRequest{OwnerID: own(t, a), Name: "abc2", Domain: "abc2.com", Version: "17"}})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	got, _ := a.Store.GetRestore(context.Background(), r.ID)
	if got.Status != "ok" || got.TargetDatabaseID == nil {
		t.Fatalf("restore %+v", got)
	}
	db, _ := a.Store.GetDatabase(context.Background(), *got.TargetDatabaseID)
	if db.Name != "abc2" || db.Status != "running" || db.Version != "17" {
		t.Fatalf("db %+v", db)
	}
	if cmd0 != "pg_restore" || in != "DUMP" {
		t.Fatalf("exec %q %q", cmd0, in)
	}
}

func TestRestoreBlocksDowngrade(t *testing.T) {
	a, rt, objs := newTestApp(t)
	key := "fridge/postgres-17/abc/abc-1.dump"
	dest := restoreFixture(t, a, objs, key, []byte("D"), nil)
	_, err := a.StartRestore(context.Background(), RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, New: &CreateRequest{OwnerID: own(t, a), Name: "abc2", Domain: "abc2.com", Version: "14"}})
	if !errors.Is(err, ErrInvalid) || len(rt.runs) != 0 {
		t.Fatalf("err=%v runs=%d", err, len(rt.runs))
	}
}

func TestRestorePlainSQLGzNeedsEngine(t *testing.T) {
	a, rt, objs := newTestApp(t)
	key := "upload/x.sql.gz"
	dest := restoreFixture(t, a, objs, key, gz("select 1;"), nil)
	nr := &CreateRequest{OwnerID: own(t, a), Name: "abc2", Domain: "abc2.com"}
	if _, err := a.StartRestore(context.Background(), RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, New: nr}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	var cmd0, in string
	rt.execFn = func(cmd []string, stdin io.Reader, stdout, stderr io.Writer) int {
		b, _ := io.ReadAll(stdin)
		cmd0, in = cmd[0], string(b)
		return 0
	}
	r, err := a.StartRestore(context.Background(), RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, Engine: "postgres", New: nr})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if got, _ := a.Store.GetRestore(context.Background(), r.ID); got.Status != "ok" {
		t.Fatalf("restore %+v", got)
	}
	if cmd0 != "psql" || in != "select 1;" {
		t.Fatalf("exec %q %q", cmd0, in)
	}
}

func TestRestoreGarbageRollsBack(t *testing.T) {
	a, rt, objs := newTestApp(t)
	key := "upload/x.dump"
	dest := restoreFixture(t, a, objs, key, []byte("garbage"), nil)
	rt.execFn = func(cmd []string, stdin io.Reader, stdout, stderr io.Writer) int {
		stderr.Write([]byte("invalid input"))
		return 1
	}
	r, err := a.StartRestore(context.Background(), RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, New: &CreateRequest{OwnerID: own(t, a), Name: "abc2", Domain: "abc2.com"}})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	got, _ := a.Store.GetRestore(context.Background(), r.ID)
	if got.Status != "failed" || !strings.Contains(got.Log, "invalid input") || got.TargetDatabaseID == nil {
		t.Fatalf("restore %+v", got)
	}
	db, _ := a.Store.GetDatabase(context.Background(), *got.TargetDatabaseID)
	if db.Status != "error" || len(rt.removes) != 1 || rt.removes[0].ID != "ctr-"+db.Volume {
		t.Fatalf("db %+v removes %+v", db, rt.removes)
	}
	if s, _ := a.Store.ListSchedules(context.Background(), db.ID); len(s) != 0 {
		t.Fatalf("rolled-back node kept schedules: %+v", s)
	}
}

func TestRestoreBadGzipRollsBack(t *testing.T) {
	a, rt, objs := newTestApp(t)
	key := "upload/x.sql.gz"
	dest := restoreFixture(t, a, objs, key, []byte("not gzip"), nil)
	r, err := a.StartRestore(context.Background(), RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, Engine: "postgres", New: &CreateRequest{OwnerID: own(t, a), Name: "abc2", Domain: "abc2.com"}})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if got, _ := a.Store.GetRestore(context.Background(), r.ID); got.Status != "failed" || len(rt.removes) != 1 {
		t.Fatalf("restore %+v removes %+v", got, rt.removes)
	}
}

func TestRestoreOverwriteRejected(t *testing.T) {
	a, _, _ := newTestApp(t)
	_, err := a.StartRestore(context.Background(), RestoreRequest{TargetDatabaseID: 1, New: &CreateRequest{}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v", err)
	}
}

func TestRestoreFallbackVersion(t *testing.T) {
	a, _, objs := newTestApp(t)
	key := "fridge/postgres-14/abc/abc-1.dump"
	dest := restoreFixture(t, a, objs, key, []byte("D"), nil)
	r, err := a.StartRestore(context.Background(), RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, New: &CreateRequest{OwnerID: own(t, a), Name: "abc2", Domain: "abc2.com"}})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	got, _ := a.Store.GetRestore(context.Background(), r.ID)
	db, _ := a.Store.GetDatabase(context.Background(), *got.TargetDatabaseID)
	if db.Version != "14" {
		t.Fatalf("version %q", db.Version)
	}
}

func TestRestoreSyncRejections(t *testing.T) {
	a, rt, objs := newTestApp(t)
	key := "fridge/postgres-14/abc/abc-1.dump"
	dest := restoreFixture(t, a, objs, key, []byte("D"), nil)
	ctx := context.Background()
	d := store.Database{Name: "taken", Engine: "postgres", Version: "17", Domain: "taken.com", TLSMode: "internal",
		User: "taken_app", Volume: "fridge-postgres-taken", Status: "running", ContainerID: "c"}
	if err := a.Store.CreateDatabase(ctx, &d); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		req  RestoreRequest
		want error
	}{
		{"name used", RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, New: &CreateRequest{OwnerID: own(t, a), Name: "taken", Domain: "other.com"}}, store.ErrConflict},
		{"domain used", RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, New: &CreateRequest{OwnerID: own(t, a), Name: "fresh", Domain: " TAKEN.com "}}, store.ErrConflict},
		{"missing key", RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: "nope.dump", New: &CreateRequest{OwnerID: own(t, a), Name: "fresh", Domain: "fresh.com"}}, store.ErrNotFound},
		{"no destination", RestoreRequest{OwnerID: own(t, a), DestinationID: 999, Key: key, New: &CreateRequest{OwnerID: own(t, a), Name: "fresh", Domain: "fresh.com"}}, ErrInvalid},
		{"engine mismatch", RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, Engine: "redis", New: &CreateRequest{OwnerID: own(t, a), Name: "fresh", Domain: "fresh.com"}}, ErrInvalid},
	}
	for _, c := range cases {
		if _, err := a.StartRestore(ctx, c.req); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v want %v", c.name, err, c.want)
		}
	}
	a.Wait()
	if len(rt.runs) != 0 {
		t.Fatalf("runs: %d", len(rt.runs))
	}
	if _, err := a.Store.GetRestore(ctx, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("restore row created: %v", err)
	}
}
