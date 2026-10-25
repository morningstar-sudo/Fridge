package store_test

import (
	"testing"
	"time"

	"fridge/internal/store"
	"fridge/internal/store/storetest"
)

func TestAuditAppendOnly(t *testing.T) {
	s := storetest.New(t)
	if err := s.InsertAudit(ctx, store.AuditEntry{Actor: "alice", Action: "db.create", Success: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool().Exec(ctx, `UPDATE audit_log SET actor='mallory'`); err == nil {
		t.Fatal("UPDATE allowed")
	}
	if _, err := s.Pool().Exec(ctx, `DELETE FROM audit_log`); err == nil {
		t.Fatal("DELETE of a fresh row allowed")
	}
	if _, err := s.Pool().Exec(ctx, `INSERT INTO audit_log(at, actor, action, success) VALUES(now() - interval '366 days','old','auth.login',true)`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PurgeAudit(ctx, time.Now().AddDate(0, 0, -365)); err != nil || n != 1 {
		t.Fatalf("purged %d, %v", n, err)
	}
	if es, _ := s.ListAudit(ctx, store.AuditFilter{}); len(es) != 1 {
		t.Fatalf("%d entries left", len(es))
	}
}

func TestAuditFiltersAndVisibility(t *testing.T) {
	s := storetest.New(t)
	alice, bob, carol := mustUser(t, s, "alice", "user"), mustUser(t, s, "bob", "user"), mustUser(t, s, "carol", "user")
	db, _ := seed(t, s, "app")
	if _, err := s.Pool().Exec(ctx, `UPDATE databases SET owner_id=$1 WHERE id=$2`, alice.ID, db.ID); err != nil {
		t.Fatal(err)
	}
	add := func(actor *store.User, action string, dbID *int64, detail map[string]any) {
		t.Helper()
		if err := s.InsertAudit(ctx, store.AuditEntry{ActorID: &actor.ID, Actor: actor.Username, Action: action, DBID: dbID, Success: true, Detail: detail}); err != nil {
			t.Fatal(err)
		}
	}
	add(alice, "db.create", &db.ID, map[string]any{"engine": "postgres"})
	add(bob, "backup.start", &db.ID, nil)
	add(bob, "auth.login", nil, nil)
	add(carol, "auth.login", nil, nil)

	names := func(f store.AuditFilter) []string {
		t.Helper()
		es, err := s.ListAudit(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range es {
			out = append(out, e.Actor+":"+e.Action)
		}
		return out
	}
	eq := func(got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("got %v want %v", got, want)
			}
		}
	}
	eq(names(store.AuditFilter{}), "carol:auth.login", "bob:auth.login", "bob:backup.start", "alice:db.create") // newest first
	eq(names(store.AuditFilter{Actor: "bob"}), "bob:auth.login", "bob:backup.start")
	eq(names(store.AuditFilter{Action: "auth."}), "carol:auth.login", "bob:auth.login")
	eq(names(store.AuditFilter{DBID: &db.ID}), "bob:backup.start", "alice:db.create")
	eq(names(store.AuditFilter{ActorID: &carol.ID}), "carol:auth.login")
	eq(names(store.AuditFilter{VisibleTo: &alice.ID}), "bob:backup.start", "alice:db.create") // her own + actions on her database
	eq(names(store.AuditFilter{VisibleTo: &bob.ID}), "bob:auth.login", "bob:backup.start")
	eq(names(store.AuditFilter{VisibleTo: &carol.ID}), "carol:auth.login")

	es, _ := s.ListAudit(ctx, store.AuditFilter{Limit: 2})
	if len(es) != 2 {
		t.Fatal(len(es))
	}
	eq(names(store.AuditFilter{BeforeID: es[1].ID}), "bob:backup.start", "alice:db.create")
	now := time.Now()
	eq(names(store.AuditFilter{From: &now}))
	if es, _ := s.ListAudit(ctx, store.AuditFilter{Action: "db."}); es[0].Detail["engine"] != "postgres" {
		t.Fatalf("detail %v", es[0].Detail)
	}
}

func TestRunningJobsAndOnlineSessions(t *testing.T) {
	s := storetest.New(t)
	alice := mustUser(t, s, "alice", "user")
	db, dest := seed(t, s, "app")
	b1, b2 := newBackup(db, dest, nil), newBackup(db, dest, nil)
	b1.StartedByID = &alice.ID
	if err := s.StartBackup(ctx, b1); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishBackup(ctx, b1.ID, "ok", 1, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.StartBackup(ctx, b2); err != nil { // no starter = scheduler
		t.Fatal(err)
	}
	if err := s.StartRestore(ctx, &store.Restore{DestinationID: dest.ID, S3Key: "k", Mode: "new", StartedByID: &alice.ID}); err != nil {
		t.Fatal(err)
	}
	js, err := s.RunningJobs(ctx)
	if err != nil || len(js) != 2 {
		t.Fatalf("%v %v", js, err)
	}
	if js[0].Kind != "backup" || js[0].DBName != "app" || js[0].StartedBy != "system" || js[1].Kind != "restore" || js[1].StartedBy != "alice" {
		t.Fatalf("%+v", js)
	}

	for i, ls := range []time.Duration{time.Minute, time.Hour} {
		x := &store.Session{IDHash: []byte{byte(i + 1)}, UserID: alice.ID, CreatedAt: time.Now().Add(-2 * time.Hour), LastSeenAt: time.Now().Add(-ls), ExpiresAt: time.Now().Add(time.Hour), IP: "1.2.3.4"}
		if err := s.CreateSession(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	os, err := s.OnlineSessions(ctx, time.Now().Add(-5*time.Minute))
	if err != nil || len(os) != 1 || os[0].Username != "alice" {
		t.Fatalf("%+v %v", os, err)
	}
}
