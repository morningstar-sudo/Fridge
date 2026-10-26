package store_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"fridge/internal/store"
	"fridge/internal/store/storetest"
)

func mkDB(t *testing.T, s *store.Store, name string, owner *store.User, status string) store.Database {
	t.Helper()
	d := &store.Database{Name: name, Engine: "postgres", Version: "17", Domain: name + ".lan", Status: status, OwnerID: &owner.ID}
	if err := s.CreateDatabase(ctx, d); err != nil {
		t.Fatal(err)
	}
	return *d
}

func mkDest(t *testing.T, s *store.Store, name string, owner *store.User, shared bool) store.Destination {
	t.Helper()
	d := &store.Destination{Name: name, Provider: "minio", AccessKey: "ak", SecretKeyEnc: "sk", OwnerID: &owner.ID, Shared: shared}
	if err := s.CreateDestination(ctx, d); err != nil {
		t.Fatal(err)
	}
	return *d
}

func names(ds []store.Database) (out []string) {
	for _, d := range ds {
		out = append(out, d.Name)
	}
	return
}

func TestVisibleDatabases(t *testing.T) {
	s := storetest.New(t)
	alice, bob := mustUser(t, s, "alice", "user"), mustUser(t, s, "bob", "user")
	mkDB(t, s, "alice_one", alice, "running")
	b := mkDB(t, s, "bob_two", bob, "running")
	mkDB(t, s, "bob_gone", bob, "deleted")
	if err := s.AddMember(ctx, b.ID, alice.ID, &bob.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListDatabasesVisible(ctx, alice.ID)
	if err != nil || fmt.Sprint(names(got)) != "[alice_one bob_two]" {
		t.Fatalf("alice sees %v, %v", names(got), err)
	}
	if got[0].OwnerName != "alice" || got[1].OwnerName != "bob" {
		t.Fatalf("owner names %q %q", got[0].OwnerName, got[1].OwnerName)
	}
	got, _ = s.ListDatabasesVisible(ctx, bob.ID)
	if fmt.Sprint(names(got)) != "[bob_two]" {
		t.Fatalf("bob sees %v", names(got))
	}
}

func TestMembers(t *testing.T) {
	s := storetest.New(t)
	alice, bob := mustUser(t, s, "alice", "user"), mustUser(t, s, "bob", "user")
	d := mkDB(t, s, "alice_one", alice, "running")
	if err := s.AddMember(ctx, d.ID, bob.ID, &alice.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, d.ID, bob.ID, nil); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup = %v", err)
	}
	if ok, _ := s.IsMember(ctx, d.ID, bob.ID); !ok {
		t.Fatal("not member")
	}
	ms, err := s.ListMembers(ctx, d.ID)
	if err != nil || len(ms) != 1 || ms[0].Username != "bob" {
		t.Fatalf("members %v %v", ms, err)
	}
	if err := s.RemoveMember(ctx, d.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveMember(ctx, d.ID, bob.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("remove = %v", err)
	}
}

func TestVisibleDestinations(t *testing.T) {
	s := storetest.New(t)
	alice, root := mustUser(t, s, "alice", "user"), mustUser(t, s, "root1", "admin")
	mkDest(t, s, "dA", alice, false)
	mkDest(t, s, "dS", root, true)
	mkDest(t, s, "dP", root, false)
	got, err := s.ListDestinationsVisible(ctx, alice.ID)
	if err != nil || len(got) != 2 || got[0].Name != "dA" || got[1].Name != "dS" || got[1].OwnerName != "root1" || !got[1].Shared {
		t.Fatalf("visible %+v %v", got, err)
	}
}

func TestDestinationSoftDelete(t *testing.T) {
	s := storetest.New(t)
	alice, bob := mustUser(t, s, "alice", "user"), mustUser(t, s, "bob", "user")
	db := mkDB(t, s, "alice_one", alice, "running")
	d := mkDest(t, s, "dA", alice, true)
	sc := &store.Schedule{DatabaseID: db.ID, DestinationID: d.ID, Cron: "* * * * *", KeepLatest: 1, Enabled: true}
	if err := s.CreateSchedule(ctx, sc); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDestination(ctx, d.ID); !errors.Is(err, store.ErrInUse) {
		t.Fatalf("in use = %v", err)
	}
	b := newBackup(db, d, nil)
	if err := s.StartBackup(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishBackup(ctx, b.ID, "ok", 1, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSchedule(ctx, sc.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDestination(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDestination(ctx, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("again = %v", err)
	}
	if l, _ := s.ListDestinations(ctx); len(l) != 0 {
		t.Fatal("still listed")
	}
	if l, _ := s.ListDestinationsVisible(ctx, alice.ID); len(l) != 0 {
		t.Fatal("still visible")
	}
	g, err := s.GetDestination(ctx, d.ID)
	if err != nil || g.DeletedAt == nil || g.SecretKeyEnc != "" || g.AccessKey != "" || g.Shared {
		t.Fatalf("get %+v %v", g, err)
	}
	if bs, err := s.ListBackups(ctx, db.ID); err != nil || len(bs) != 1 {
		t.Fatalf("backups %v %v", bs, err)
	}
	mkDest(t, s, "dA", alice, false)
	mkDest(t, s, "dA", bob, false)
	dup := &store.Destination{Name: "dA", Provider: "minio", OwnerID: &alice.ID}
	if err := s.CreateDestination(ctx, dup); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup = %v", err)
	}
}

func TestBackupsOnDestination(t *testing.T) {
	s := storetest.New(t)
	alice, bob, root := mustUser(t, s, "alice", "user"), mustUser(t, s, "bob", "user"), mustUser(t, s, "root1", "admin")
	dS := mkDest(t, s, "dS", root, true)
	var keys []string
	for _, d := range []store.Database{mkDB(t, s, "alice_one", alice, "running"), mkDB(t, s, "bob_two", bob, "running")} {
		b := newBackup(d, dS, nil)
		if err := s.StartBackup(ctx, b); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishBackup(ctx, b.ID, "ok", 1, "", ""); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, b.S3Key)
	}
	if l, err := s.ListOKBackupsOnDestination(ctx, dS.ID, &alice.ID); err != nil || len(l) != 1 || l[0].S3Key != keys[0] {
		t.Fatalf("alice %v %v", l, err)
	}
	if l, _ := s.ListOKBackupsOnDestination(ctx, dS.ID, nil); len(l) != 2 || l[0].S3Key != keys[1] {
		t.Fatalf("all %v", l)
	}
	if b, err := s.GetBackupByKey(ctx, dS.ID, keys[1]); err != nil || b.S3Key != keys[1] {
		t.Fatalf("bykey %v %v", b, err)
	}
	if _, err := s.GetBackupByKey(ctx, dS.ID, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown key = %v", err)
	}
}

func TestBackupStartedBy(t *testing.T) {
	s := storetest.New(t)
	alice := mustUser(t, s, "alice", "user")
	db, dest := seed(t, s, "a")
	b := newBackup(db, dest, nil)
	b.StartedByID = &alice.ID
	if err := s.StartBackup(ctx, b); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.GetBackup(ctx, b.ID); g.StartedByID == nil || *g.StartedByID != alice.ID {
		t.Fatalf("started_by %v", g.StartedByID)
	}
	s.FinishBackup(ctx, b.ID, "ok", 1, "", "")
	b2 := newBackup(db, dest, nil)
	if err := s.StartBackup(ctx, b2); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.GetBackup(ctx, b2.ID); g.StartedByID != nil {
		t.Fatal("want nil")
	}
	r := &store.Restore{DestinationID: dest.ID, S3Key: "x", SourceEngine: "postgres", Mode: "new", StartedByID: &alice.ID}
	if err := s.StartRestore(ctx, r); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.GetRestore(ctx, r.ID); g.StartedByID == nil || *g.StartedByID != alice.ID {
		t.Fatal("restore started_by")
	}
}

func TestDumpAndImageChecks(t *testing.T) {
	s := storetest.New(t)
	_, dest := seed(t, s, "a")
	c := &store.DumpCheck{DestinationID: dest.ID, S3Key: "k"}
	if err := s.StartDumpCheck(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDumpCheck(ctx, c.ID, "ok", map[string]any{"tables": 3.0, "engine": "postgres"}, "log", ""); err != nil {
		t.Fatal(err)
	}
	g, err := s.GetDumpCheck(ctx, c.ID)
	if err != nil || g.Status != "ok" || g.Result["tables"] != 3.0 || g.Result["engine"] != "postgres" || g.FinishedAt == nil || g.Log != "log" {
		t.Fatalf("dump check %+v %v", g, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, v := range []string{"1", "2"} {
		if err := s.UpsertImageCheck(ctx, store.ImageCheck{ImageRef: "postgres:17", LocalDigest: "sha" + v, CheckedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	l, err := s.ListImageChecks(ctx)
	if err != nil || len(l) != 1 || l[0].LocalDigest != "sha2" {
		t.Fatalf("image checks %v %v", l, err)
	}
}

func TestDatabaseAdminUser(t *testing.T) {
	s := storetest.New(t)
	alice := mustUser(t, s, "alice", "user")
	mk := func(name, admin, key string) error {
		return s.CreateDatabase(ctx, &store.Database{Name: name, Engine: "postgres", Version: "17", Domain: name + ".lan",
			OwnerID: &alice.ID, AdminUser: admin, NodeKey: key})
	}
	if err := mk("alice_a", "su-bad!", ""); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("admin_user = %v", err)
	}
	if err := mk("alice_a", "", "xyz"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("node_key = %v", err)
	}
	if err := mk("alice_a", "su_0123456789abcdef", "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if err := mk("alice_b", "", "0123456789abcdef"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup key = %v", err)
	}
	g, _ := s.GetDatabaseByName(ctx, "alice_a")
	if err := s.SetDatabaseAdminPassword(ctx, g.ID, "enc"); err != nil {
		t.Fatal(err)
	}
	if g, _ = s.GetDatabase(ctx, g.ID); g.AdminPasswordEnc != "enc" || g.AdminUser != "su_0123456789abcdef" {
		t.Fatalf("%+v", g)
	}
}

func TestDomains(t *testing.T) {
	s := storetest.New(t)
	alice, bob := mustUser(t, s, "alice", "user"), mustUser(t, s, "bob", "user")
	d := &store.Domain{OwnerID: alice.ID, Name: "alice.lan"}
	if err := s.CreateDomain(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateDomain(ctx, &store.Domain{OwnerID: bob.ID, Name: "alice.lan"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup = %v", err)
	}
	if err := s.CreateDomain(ctx, &store.Domain{OwnerID: bob.ID, Name: "bob.lan"}); err != nil {
		t.Fatal(err)
	}
	run := mkDB(t, s, "alice_run", alice, "running")
	bad := mkDB(t, s, "alice_bad", alice, "error")
	for _, db := range []store.Database{run, bad} {
		if _, err := s.Pool().Exec(ctx, `UPDATE databases SET domain_id=$2 WHERE id=$1`, db.ID, d.ID); err != nil {
			t.Fatal(err)
		}
	}
	l, err := s.ListDomains(ctx, &alice.ID)
	if err != nil || len(l) != 1 || l[0].InUse != 1 || l[0].OwnerName != "alice" {
		t.Fatalf("list %+v %v", l, err)
	}
	if all, _ := s.ListDomains(ctx, nil); len(all) != 2 {
		t.Fatal("nil owner should list all")
	}
	if g, err := s.GetDomainByName(ctx, "alice.lan"); err != nil || g.ID != d.ID {
		t.Fatalf("by name %v %v", g, err)
	}
	if err := s.DeleteDomain(ctx, d.ID); !errors.Is(err, store.ErrInUse) {
		t.Fatalf("delete = %v", err)
	}
	if err := s.SetDatabaseState(ctx, run.ID, "deleted", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDomain(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.GetDatabase(ctx, bad.ID); g.DomainID != nil {
		t.Fatal("domain_id should be NULL")
	}
	if _, err := s.GetDomain(ctx, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestDBNameGlobalUnique(t *testing.T) {
	s := storetest.New(t)
	alice, bob := mustUser(t, s, "alice", "user"), mustUser(t, s, "bob", "user")
	mkDB(t, s, "alice_shop", alice, "running")
	d := &store.Database{Name: "alice_shop", Engine: "postgres", Version: "17", Domain: "x.lan", OwnerID: &alice.ID}
	if err := s.CreateDatabase(ctx, d); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup = %v", err)
	}
	mkDB(t, s, "bob_shop", bob, "running")
}
