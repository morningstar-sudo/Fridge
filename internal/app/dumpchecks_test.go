package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"fridge/internal/job"
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

func TestDumpCheckRejectsBeforeStarting(t *testing.T) {
	a, rt, objs := newTestApp(t)
	ctx := context.Background()
	dest := restoreFixture(t, a, objs, "up/ok.sql.gz", gz("CREATE TABLE t(id int);"), nil)
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
	if len(rt.jobs) != 0 {
		t.Fatalf("%d job(s) started for requests that were refused", len(rt.jobs))
	}
}

// The check runs as a job that can reach S3 and nothing else: no database network, no sidecar.
func TestDumpCheckRunsAsAJobWithoutDatabaseAccess(t *testing.T) {
	a, rt, objs := newTestApp(t)
	ctx := context.Background()
	dest := restoreFixture(t, a, objs, "up/a.dump", dumpFile(t, "pg14.dump"), nil)
	rt.jobFn = func(spec job.Spec, networks []string) (string, string, int) {
		return result(map[string]any{"format": "custom", "bytes": 2877, "source_version": "14.24", "pg_dump_version": "14.24", "toc_entries": 2}),
			"pg_restore: creating TABLE \"public.note\"\n", 0
	}
	c, err := a.StartDumpCheck(ctx, dest.ID, "up/a.dump", own(t, a))
	if err != nil {
		t.Fatal(err)
	}
	got := waitCheck(t, a, c.ID)
	if got.Status != "ok" || got.Result["toc_entries"] != float64(2) || got.Result["bytes"] != float64(2877) || got.Result["source_version"] != "14.24" || !strings.Contains(got.Log, "creating TABLE") || got.FinishedAt == nil {
		t.Fatalf("%+v", got)
	}
	j, ok := rt.lastJob("check")
	if !ok || strings.Join(j.Networks, ",") != "fridge_egress" || j.Spec.Key != "up/a.dump" || j.Spec.S3.Bucket != "b" || j.NodeID != "" {
		t.Fatalf("job %+v", j)
	}
	if v := j.Spec.Params.Version; v != "18" { // the newest catalog version: its pg_restore reads every older archive
		t.Fatalf("pg_restore version %q", v)
	}
	if len(rt.runs) != 0 {
		t.Fatalf("a check created %d node(s)", len(rt.runs))
	}
}

func TestDumpCheckFailureCarriesTheJobsReason(t *testing.T) {
	a, rt, objs := newTestApp(t)
	ctx := context.Background()
	dest := restoreFixture(t, a, objs, "up/cut.dump", dumpFile(t, "pg14.dump"), nil)
	rt.jobFn = func(spec job.Spec, networks []string) (string, string, int) {
		return result(map[string]any{"format": "custom", "bytes": 100}),
			"pg_restore: error: could not read from input file: end of file\njob failed: pg_restore: exit status 1\n", 1
	}
	c, _ := a.StartDumpCheck(ctx, dest.ID, "up/cut.dump", own(t, a))
	got := waitCheck(t, a, c.ID)
	if got.Status != "failed" || got.Error != "pg_restore: exit status 1" || !strings.Contains(got.Log, "end of file") || got.Result["bytes"] != float64(100) {
		t.Fatalf("%+v", got)
	}

	// a job that dies without saying why: the error is the exit status
	rt.jobFn = func(spec job.Spec, networks []string) (string, string, int) { return "", "", 137 }
	c, _ = a.StartDumpCheck(ctx, dest.ID, "up/cut.dump", own(t, a))
	if got = waitCheck(t, a, c.ID); got.Status != "failed" || !strings.Contains(got.Error, "137") {
		t.Fatalf("%+v", got)
	}
}
