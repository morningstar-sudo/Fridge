package store_test

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"fridge/internal/store"
	"fridge/internal/store/storetest"
)

func seed(t *testing.T, s *store.Store, name string) (db store.Database, dest store.Destination) {
	t.Helper()
	d := &store.Database{Name: name, Engine: "postgres", Version: "17", Domain: name + ".com", TLSMode: "internal", Status: "running"}
	if err := s.CreateDatabase(ctx, d); err != nil {
		t.Fatal(err)
	}
	de := &store.Destination{Name: "dest-" + name, Provider: "minio"}
	if err := s.CreateDestination(ctx, de); err != nil {
		t.Fatal(err)
	}
	return *d, *de
}

var keySeq atomic.Int64

func newBackup(db store.Database, dest store.Destination, sched *int64) *store.Backup {
	return &store.Backup{DatabaseID: db.ID, DestinationID: dest.ID, ScheduleID: sched, Trigger: "manual", Engine: "postgres", EngineVersion: "17",
		S3Key: fmt.Sprintf("k%d", keySeq.Add(1))}
}

func TestOneRunningBackupPerDatabase(t *testing.T) {
	s := storetest.New(t)
	db, dest := seed(t, s, "a")
	db2, _ := seed(t, s, "b")
	b1 := newBackup(db, dest, nil)
	if err := s.StartBackup(ctx, b1); err != nil {
		t.Fatal(err)
	}
	if b1.ID == 0 || b1.Status != "running" || b1.StartedAt.IsZero() {
		t.Fatalf("not populated: %+v", b1)
	}
	if err := s.StartBackup(ctx, newBackup(db, dest, nil)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
	if err := s.StartBackup(ctx, newBackup(db2, dest, nil)); err != nil {
		t.Fatalf("other db: %v", err)
	}
	if err := s.FinishBackup(ctx, b1.ID, "ok", 42, "log", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.StartBackup(ctx, newBackup(db, dest, nil)); err != nil {
		t.Fatalf("after finish: %v", err)
	}
	got, err := s.GetBackup(ctx, b1.ID)
	if err != nil || got.Status != "ok" || got.SizeBytes != 42 || got.FinishedAt == nil || got.Log != "log" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestOKBackupsForScheduleOrder(t *testing.T) {
	s := storetest.New(t)
	db, dest := seed(t, s, "a")
	sc := &store.Schedule{DatabaseID: db.ID, DestinationID: dest.ID, Cron: "0 2 * * *", Prefix: "fridge", KeepLatest: 7, Enabled: true}
	if err := s.CreateSchedule(ctx, sc); err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{"ok", "ok", "failed", "ok"} {
		b := newBackup(db, dest, &sc.ID)
		if err := s.StartBackup(ctx, b); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishBackup(ctx, b.ID, st, 1, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	m := newBackup(db, dest, nil)
	if err := s.StartBackup(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishBackup(ctx, m.ID, "ok", 1, "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.OKBackupsForSchedule(ctx, sc.ID)
	if err != nil || len(got) != 3 {
		t.Fatalf("len=%d err=%v", len(got), err)
	}
	if !(got[0].ID > got[1].ID && got[1].ID > got[2].ID) {
		t.Fatalf("order: %d %d %d", got[0].ID, got[1].ID, got[2].ID)
	}
}

func TestFailRunning(t *testing.T) {
	s := storetest.New(t)
	db, dest := seed(t, s, "a")
	db2, _ := seed(t, s, "b")
	run := newBackup(db, dest, nil)
	ok := newBackup(db2, dest, nil)
	for _, b := range []*store.Backup{run, ok} {
		if err := s.StartBackup(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FinishBackup(ctx, ok.ID, "ok", 1, "", ""); err != nil {
		t.Fatal(err)
	}
	r := &store.Restore{DestinationID: dest.ID, S3Key: "k", SourceEngine: "postgres", SourceVersion: "17", Mode: "new"}
	if err := s.StartRestore(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRestoreTarget(ctx, r.ID, db.ID); err != nil {
		t.Fatal(err)
	}
	n, err := s.FailRunning(ctx, "interrupted by restart")
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	gb, _ := s.GetBackup(ctx, run.ID)
	gr, err := s.GetRestore(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gb.Status != "failed" || gb.Error != "interrupted by restart" || gb.FinishedAt == nil {
		t.Fatalf("backup %+v", gb)
	}
	if gr.Status != "failed" || gr.Error != "interrupted by restart" || gr.FinishedAt == nil || gr.TargetDatabaseID == nil || *gr.TargetDatabaseID != db.ID || gr.SourceEngine != "postgres" || gr.SourceVersion != "17" {
		t.Fatalf("restore %+v", gr)
	}
	if g, _ := s.GetBackup(ctx, ok.ID); g.Status != "ok" || g.Error != "" {
		t.Fatalf("ok backup changed: %+v", g)
	}
}

func TestListEnabledSchedules(t *testing.T) {
	s := storetest.New(t)
	db, dest := seed(t, s, "a")
	a := &store.Schedule{DatabaseID: db.ID, DestinationID: dest.ID, Cron: "0 2 * * *", Prefix: "fridge", KeepLatest: 7, Enabled: true}
	b := &store.Schedule{DatabaseID: db.ID, DestinationID: dest.ID, Cron: "0 3 * * *", Prefix: "fridge", KeepLatest: 7, Enabled: false}
	for _, x := range []*store.Schedule{a, b} {
		if err := s.CreateSchedule(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListEnabledSchedules(ctx)
	if err != nil || len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("got %+v, %v", got, err)
	}
	if all, err := s.ListSchedules(ctx, db.ID); err != nil || len(all) != 2 {
		t.Fatalf("all=%d err=%v", len(all), err)
	}
	b.Enabled = true
	b.KeepLatest = 3
	if err := s.UpdateSchedule(ctx, *b); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.GetSchedule(ctx, b.ID); !g.Enabled || g.KeepLatest != 3 {
		t.Fatalf("update: %+v", g)
	}
	if err := s.DeleteSchedule(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSchedule(ctx, b.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestDeleteSchedulesForDatabase(t *testing.T) {
	s := storetest.New(t)
	db, dest := seed(t, s, "a")
	db2, _ := seed(t, s, "b")
	for _, id := range []int64{db.ID, db.ID, db2.ID} {
		if err := s.CreateSchedule(ctx, &store.Schedule{DatabaseID: id, DestinationID: dest.ID, Cron: "0 2 * * *", Prefix: "fridge", KeepLatest: 7, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteSchedulesForDatabase(ctx, db.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSchedulesForDatabase(ctx, db.ID); err != nil { // none left: still fine
		t.Fatal(err)
	}
	if got, _ := s.ListSchedules(ctx, db.ID); len(got) != 0 {
		t.Fatalf("left: %+v", got)
	}
	if got, _ := s.ListSchedules(ctx, db2.ID); len(got) != 1 {
		t.Fatalf("other db: %+v", got)
	}
}

func TestOrphanedRestoreTargets(t *testing.T) {
	s := storetest.New(t)
	live, dest := seed(t, s, "a")   // target of a failed restore, still running -> orphan
	gone, _ := seed(t, s, "b")      // target of a failed restore, already torn down
	ok, _ := seed(t, s, "c")        // target of a successful restore
	overwrite, _ := seed(t, s, "d") // target of a failed overwrite restore: never touched
	if err := s.SetDatabaseState(ctx, gone.ID, "error", ""); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		db           store.Database
		mode, status string
	}{{live, "new", "failed"}, {gone, "new", "failed"}, {ok, "new", "ok"}, {overwrite, "overwrite", "failed"}} {
		r := &store.Restore{DestinationID: dest.ID, S3Key: "k", SourceEngine: "postgres", Mode: c.mode}
		if err := s.StartRestore(ctx, r); err != nil {
			t.Fatal(err)
		}
		if err := s.SetRestoreTarget(ctx, r.ID, c.db.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishRestore(ctx, r.ID, c.status, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.OrphanedRestoreTargets(ctx)
	if err != nil || len(got) != 1 || got[0].ID != live.ID || got[0].Name != "a" {
		t.Fatalf("got %+v, %v", got, err)
	}
}
