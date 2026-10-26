package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"strings"
	"testing"

	"fridge/internal/job"
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
	data := dumpFile(t, "pg14.dump")
	dest := restoreFixture(t, a, objs, key, data, map[string]string{"fridge-engine": "postgres", "fridge-version": "14"})
	r, err := startRestore(a, context.Background(), RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, New: &CreateRequest{OwnerID: own(t, a), Name: "abc2", Domain: "abc2.com", Version: "17"}})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	got, _ := a.Store.GetRestore(context.Background(), r.ID)
	if got.Status != "ok" || got.TargetDatabaseID == nil {
		t.Fatalf("restore %+v", got)
	}
	db, _ := a.Store.GetDatabase(context.Background(), *got.TargetDatabaseID)
	if db.Name != "root1_abc2" || db.Status != "running" || db.Version != "17" {
		t.Fatalf("db %+v", db)
	}
	if j, ok := rt.lastJob("restore"); !ok || j.Spec.Key != key || j.Spec.Params.User != "root1_abc2" || j.Spec.Params.Version != "17" {
		t.Fatalf("restore job = %+v", j)
	}
}

func TestRestoreBlocksDowngrade(t *testing.T) {
	a, rt, objs := newTestApp(t)
	key := "fridge/postgres-17/abc/abc-1.dump"
	dest := restoreFixture(t, a, objs, key, dumpFile(t, "pg17.dump"), nil)
	_, err := startRestore(a, context.Background(), RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, New: &CreateRequest{OwnerID: own(t, a), Name: "abc2", Domain: "abc2.com", Version: "14"}})
	if !errors.Is(err, ErrInvalid) || len(rt.runs) != 0 {
		t.Fatalf("err=%v runs=%d", err, len(rt.runs))
	}
}

func TestRestorePlainSQLGzNeedsEngine(t *testing.T) {
	a, rt, objs := newTestApp(t)
	key := "upload/x.sql.gz"
	dest := restoreFixture(t, a, objs, key, gz("CREATE TABLE t(id int);"), nil)
	nr := &CreateRequest{OwnerID: own(t, a), Name: "abc2", Domain: "abc2.com"}
	if _, err := startRestore(a, context.Background(), RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, New: nr}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	r, err := startRestore(a, context.Background(), RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, Engine: "postgres", New: nr})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if got, _ := a.Store.GetRestore(context.Background(), r.ID); got.Status != "ok" {
		t.Fatalf("restore %+v", got)
	}
	if j, ok := rt.lastJob("restore"); !ok || j.Spec.Key != key || j.Spec.Engine != "postgres" || j.Spec.Params.User != "root1_abc2" {
		t.Fatalf("restore job = %+v", j)
	}
}

func TestRestoreGarbageRollsBack(t *testing.T) {
	a, rt, objs := newTestApp(t)
	key := "upload/x.dump"
	dest := restoreFixture(t, a, objs, key, dumpFile(t, "pg17.dump"), nil)
	rt.jobFn = func(spec job.Spec, _ []string) (string, string, int) {
		return "", "pg_restore: error: invalid input\njob failed: restore failed: exit status 1", 1
	}
	r, err := startRestore(a, context.Background(), RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, New: &CreateRequest{OwnerID: own(t, a), Name: "abc2", Domain: "abc2.com"}})
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

func TestRestoreOverwriteRejected(t *testing.T) {
	a, _, _ := newTestApp(t)
	_, err := startRestore(a, context.Background(), RestoreRequest{TargetDatabaseID: 1, New: &CreateRequest{}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v", err)
	}
}

func TestRestoreFallbackVersion(t *testing.T) {
	a, _, objs := newTestApp(t)
	key := "fridge/postgres-14/abc/abc-1.dump"
	dest := restoreFixture(t, a, objs, key, dumpFile(t, "pg14.dump"), nil)
	r, err := startRestore(a, context.Background(), RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, New: &CreateRequest{OwnerID: own(t, a), Name: "abc2", Domain: "abc2.com"}})
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
	dest := restoreFixture(t, a, objs, key, dumpFile(t, "pg14.dump"), nil)
	ctx := context.Background()
	d := store.Database{Name: "root1_taken", Engine: "postgres", Version: "17", Domain: "taken.com",
		User: "root1_taken", Volume: "fridge-postgres-taken", Status: "running", ContainerID: "c"}
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
		if _, err := startRestore(a, ctx, c.req); !errors.Is(err, c.want) {
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

func TestRestoreHeaderChecks(t *testing.T) {
	a, rt, objs := newTestApp(t)
	ctx := context.Background()
	dest, err := a.CreateDestination(ctx, store.Destination{Name: "s3", Endpoint: "http://localhost:9000", Bucket: "b", AccessKey: "ak"}, "sk")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"not-a-dump.dump", "empty.dump", "pg17.dump", "pg13-by-pgdump17.dump", "pg14.dump"} {
		objs.objs["up/"+f] = fakeObj{dumpFile(t, f), nil}
	}
	try := func(file, version string) error {
		_, err := startRestore(a, ctx, RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: "up/" + file,
			New: &CreateRequest{OwnerID: own(t, a), Name: "chk" + strings.TrimSuffix(file, ".dump"), Domain: strings.ReplaceAll(file, ".", "-") + ".com", Version: version}})
		return err
	}
	for file, version := range map[string]string{"not-a-dump.dump": "17", "empty.dump": "17", "pg17.dump": "16", "pg13-by-pgdump17.dump": "16"} {
		if err := try(file, version); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s into %s: %v", file, version, err)
		}
	}
	if len(rt.runs) != 0 {
		t.Fatalf("a rejected file started %d node(s)", len(rt.runs))
	}
	if _, err := a.Store.GetRestore(ctx, 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a rejected file left a restore row: %v", err)
	}
	// the messages say what is wrong
	err = try("pg13-by-pgdump17.dump", "16")
	if err == nil || !strings.Contains(err.Error(), "pg_dump 17") {
		t.Fatalf("pg_dump message: %v", err)
	}
	if err = try("pg17.dump", "16"); err == nil || !strings.Contains(err.Error(), "older than source") {
		t.Fatalf("downgrade message: %v", err)
	}
	if err := try("pg14.dump", "17"); err != nil {
		t.Fatalf("pg14 dump into 17: %v", err)
	}
	a.Wait()
	// no version asked for: the file's server version picks the node
	if err := try("pg13-by-pgdump17.dump", ""); !errors.Is(err, ErrInvalid) && err != nil {
		t.Fatal(err)
	}
}
