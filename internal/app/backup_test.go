package app

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fridge/internal/audit"
	"fridge/internal/job"
	"fridge/internal/store"
)

func TestBackupKey(t *testing.T) {
	ts := time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC)
	if got, want := BackupKey("fridge/", "postgres", "17", "abc", ts, ".dump"), "fridge/postgres-17/abc/abc-20261020-020000.dump"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got, want := BackupKey("", "postgres", "17", "abc", ts, ".dump"), "postgres-17/abc/abc-20261020-020000.dump"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// fixture: a running database "abc" and one destination.
func fixture(t *testing.T, a *App) (store.Database, store.Destination) {
	t.Helper()
	ctx := context.Background()
	adminEnc, err := a.Box.Encrypt("fixture-admin-pw")
	if err != nil {
		t.Fatal(err)
	}
	d := store.Database{Name: "abc", Engine: "postgres", Version: "17", Domain: "abc.example.com", TLSMode: "internal",
		User: "abc_app", Volume: "fridge-postgres-0123456789abcdef", Status: "running", ContainerID: "fridge-db-0123456789abcdef",
		NodeKey: "0123456789abcdef", AdminUser: "su_0123456789abcdef", AdminPasswordEnc: adminEnc, HostID: "host1"}
	if err := a.Store.CreateDatabase(ctx, &d); err != nil {
		t.Fatal(err)
	}
	if err := a.issueNodeTLS(ctx, d); err != nil {
		t.Fatal(err)
	}
	dest, err := a.CreateDestination(ctx, store.Destination{Name: "s3", Endpoint: "http://localhost:9000", Bucket: "b", AccessKey: "ak"}, "sk")
	if err != nil {
		t.Fatal(err)
	}
	// distinct second per backup so keys never collide
	var n atomic.Int64
	a.now = func() time.Time {
		return time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC).Add(time.Duration(n.Add(1)) * time.Second)
	}
	return d, dest
}

func TestBackupHappyPath(t *testing.T) {
	a, rt, objs := newTestApp(t)
	d, dest := fixture(t, a)
	rt.dumps("DUMPDATA", "", 0)
	b, err := a.StartBackup(context.Background(), d.ID, dest.ID, nil, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	got, _ := a.Store.GetBackup(context.Background(), b.ID)
	if got.Status != "ok" || got.SizeBytes != 8 {
		t.Fatalf("backup = %+v", got)
	}
	o, ok := objs.objs[b.S3Key]
	if !ok || string(o.data) != "DUMPDATA" {
		t.Fatalf("objects = %+v", objs.objs)
	}
	want := map[string]string{"fridge-engine": "postgres", "fridge-version": "17", "fridge-db": "abc"}
	for k, v := range want {
		if o.meta[k] != v {
			t.Fatalf("meta = %v", o.meta)
		}
	}
	if j, ok := rt.lastJob("dump"); !ok || j.Spec.Key != b.S3Key || j.Spec.Engine != "postgres" || j.Spec.Params.Name != "abc" || j.Spec.S3.Bucket != "b" || j.Spec.S3.SecretKey != "sk" {
		t.Fatalf("dump job = %+v", j)
	}
}

func TestBackupDumpFailsMidStream(t *testing.T) {
	a, rt, objs := newTestApp(t)
	d, dest := fixture(t, a)
	rt.dumps("PART", "boom", 1)
	b, err := a.StartBackup(context.Background(), d.ID, dest.ID, nil, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	got, _ := a.Store.GetBackup(context.Background(), b.ID)
	if got.Status != "failed" || !strings.Contains(got.Log, "boom") {
		t.Fatalf("backup = %+v", got)
	}
	if objs.has(b.S3Key) {
		t.Fatal("partial object must not remain")
	}
	if len(rt.alerts) != 1 || !strings.Contains(rt.alerts[0], "abc") {
		t.Fatalf("alerts = %v", rt.alerts)
	}
}

func TestBackupUploadFails(t *testing.T) {
	a, rt, _ := newTestApp(t)
	d, dest := fixture(t, a)
	rt.dumps("", "job failed: s3 down", 1) // the job does the upload, so an S3 failure is the job's failure
	b, err := a.StartBackup(context.Background(), d.ID, dest.ID, nil, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	got, _ := a.Store.GetBackup(context.Background(), b.ID)
	if got.Status != "failed" || len(rt.alerts) != 1 {
		t.Fatalf("backup = %+v alerts = %v", got, rt.alerts)
	}
}

func TestBackupConcurrent(t *testing.T) {
	a, rt, _ := newTestApp(t)
	d, dest := fixture(t, a)
	gate := make(chan struct{})
	rt.jobFn = func(spec job.Spec, _ []string) (string, string, int) {
		<-gate
		rt.objs.put(spec.Key, []byte("X"), spec.Meta)
		return result(map[string]any{"size": 1}), "", 0
	}
	ctx := context.Background()
	if _, err := a.StartBackup(ctx, d.ID, dest.ID, nil, "schedule", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.StartBackup(ctx, d.ID, dest.ID, nil, "manual", nil); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("err = %v", err)
	}
	close(gate)
	a.Wait()
}

func TestBackupNoDestination(t *testing.T) {
	a, _, _ := newTestApp(t)
	d := store.Database{Name: "abc", Engine: "postgres", Version: "17", Domain: "a.b.c", TLSMode: "internal", Volume: "v", Status: "running"}
	if err := a.Store.CreateDatabase(context.Background(), &d); err != nil {
		t.Fatal(err)
	}
	if _, err := a.StartBackup(context.Background(), d.ID, 0, nil, "manual", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

func TestRetention(t *testing.T) {
	a, _, objs := newTestApp(t)
	d, dest := fixture(t, a)
	ctx := context.Background()
	sc, err := a.CreateSchedule(ctx, store.Schedule{DatabaseID: d.ID, DestinationID: dest.ID, Cron: "0 2 * * *", Prefix: "fridge", KeepLatest: 2, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	run := func(schedID *int64, trigger string) {
		b, err := a.StartBackup(ctx, d.ID, dest.ID, schedID, trigger, nil)
		if err != nil {
			t.Fatal(err)
		}
		a.Wait()
		keys = append(keys, b.S3Key)
	}
	run(&sc.ID, "schedule")
	run(nil, "manual")
	run(&sc.ID, "schedule")
	run(&sc.ID, "schedule")
	// keys[0] is the oldest schedule backup: gone. Others stay.
	if objs.has(keys[0]) || !objs.has(keys[1]) || !objs.has(keys[2]) || !objs.has(keys[3]) {
		t.Fatalf("objects = %v", objs.objs)
	}
	bs, _ := a.Store.ListBackups(ctx, d.ID)
	if len(bs) != 3 {
		t.Fatalf("backups = %+v", bs)
	}
}

func TestCreateScheduleValidation(t *testing.T) {
	a, _, _ := newTestApp(t)
	d, dest := fixture(t, a)
	ctx := context.Background()
	for _, sc := range []store.Schedule{
		{DatabaseID: d.ID, DestinationID: dest.ID, Cron: "bad", KeepLatest: 1},
		{DatabaseID: d.ID, DestinationID: dest.ID, Cron: "0 2 * * *", KeepLatest: 0},
		{DatabaseID: d.ID, DestinationID: 9999, Cron: "0 2 * * *", KeepLatest: 1},
	} {
		if _, err := a.CreateSchedule(ctx, sc); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v: err = %v", sc, err)
		}
	}
}

func TestDeleteDestinationInUse(t *testing.T) {
	a, _, _ := newTestApp(t)
	d, dest := fixture(t, a)
	ctx := context.Background()
	if _, err := a.CreateSchedule(ctx, store.Schedule{DatabaseID: d.ID, DestinationID: dest.ID, Cron: "0 2 * * *", KeepLatest: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteDestination(ctx, dest.ID); !errors.Is(err, store.ErrInUse) {
		t.Fatalf("err = %v", err)
	}
}

func TestBackupTimeout(t *testing.T) {
	a, rt, objs := newTestApp(t)
	d, dest := fixture(t, a)
	a.backupTimeout = 50 * time.Millisecond
	rt.hang = true
	b, err := a.StartBackup(context.Background(), d.ID, dest.ID, nil, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	got, _ := a.Store.GetBackup(context.Background(), b.ID)
	if got.Status != "failed" || objs.has(b.S3Key) || len(rt.alerts) != 1 {
		t.Fatalf("backup = %+v alerts = %v", got, rt.alerts)
	}
}

func TestBackupInvalidUTF8Log(t *testing.T) {
	a, rt, _ := newTestApp(t)
	d, dest := fixture(t, a)
	rt.dumps("X", "bad \xff\xfe bytes", 0)
	b, _ := a.StartBackup(context.Background(), d.ID, dest.ID, nil, "manual", nil)
	a.Wait()
	got, _ := a.Store.GetBackup(context.Background(), b.ID)
	if got.Status != "ok" || len(rt.alerts) != 0 {
		t.Fatalf("backup = %+v alerts = %v", got, rt.alerts)
	}
}

func TestBackupFinishFailureAlerts(t *testing.T) {
	a, rt, _ := newTestApp(t)
	d, dest := fixture(t, a)
	rt.jobFn = func(spec job.Spec, _ []string) (string, string, int) {
		rt.objs.put(spec.Key, []byte("X"), spec.Meta)
		a.Store.Close() // FinishBackup will now fail
		return result(map[string]any{"size": 1}), "", 0
	}
	if _, err := a.StartBackup(context.Background(), d.ID, dest.ID, nil, "manual", nil); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.alerts) != 1 || !strings.Contains(rt.alerts[0], "abc") {
		t.Fatalf("alerts = %v", rt.alerts)
	}
}

func TestBackupSameSecondKeyConflict(t *testing.T) {
	a, _, objs := newTestApp(t)
	d, dest := fixture(t, a)
	fixed := time.Date(2026, 10, 20, 2, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return fixed }
	ctx := context.Background()
	b, err := a.StartBackup(ctx, d.ID, dest.ID, nil, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if _, err := a.StartBackup(ctx, d.ID, dest.ID, nil, "manual", nil); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("err = %v", err)
	}
	a.Wait()
	got, _ := a.Store.GetBackup(ctx, b.ID)
	if got.Status != "ok" || !objs.has(b.S3Key) {
		t.Fatalf("first backup damaged: %+v", got)
	}
}

func TestSystemAudit(t *testing.T) {
	a, rt, _ := newTestApp(t)
	a.Audit = &audit.Logger{Store: a.Store}
	d, dest := fixture(t, a)
	ctx := context.Background()
	sc, err := a.CreateSchedule(ctx, store.Schedule{DatabaseID: d.ID, DestinationID: dest.ID, Cron: "0 2 * * *", Prefix: "fridge", KeepLatest: 1, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	run := func(sched *int64, trigger string) {
		t.Helper()
		if _, err := a.StartBackup(ctx, d.ID, dest.ID, sched, trigger, nil); err != nil {
			t.Fatal(err)
		}
		a.Wait()
	}
	run(nil, "manual") // not a system action
	run(&sc.ID, "schedule")
	a.now = func() time.Time { return time.Now().Add(time.Hour) } // another key: the first one is the same second
	run(&sc.ID, "schedule")                                       // keeps 1: the first is deleted
	rt.dumps("", "", 1)
	a.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	run(&sc.ID, "schedule") // fails

	es, err := a.Store.ListAudit(ctx, store.AuditFilter{Action: "system."})
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for _, e := range es {
		if e.Actor != "system" || e.ActorID != nil || e.DBID == nil || *e.DBID != d.ID {
			t.Fatalf("%+v", e)
		}
		count[e.Action+map[bool]string{true: "/ok", false: "/failed"}[e.Success]]++
	}
	want := map[string]int{"system.backup/ok": 2, "system.backup/failed": 1, "system.retention_delete/ok": 1}
	for k, v := range want {
		if count[k] != v {
			t.Fatalf("counts %v, want %v", count, want)
		}
	}
	if len(count) != len(want) {
		t.Fatalf("counts %v", count)
	}
}
