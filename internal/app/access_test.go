package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"fridge/internal/auth"
	"fridge/internal/store"
)

type actors struct {
	alice, bob, root            int64
	pAlice, pBob, pRoot, pCarol auth.Principal
}

func mkActors(t *testing.T, a *App) actors {
	t.Helper()
	x := actors{alice: mkUser(t, a, "alice", "user"), bob: mkUser(t, a, "bob", "user"), root: mkUser(t, a, "root1", "admin")}
	carol := mkUser(t, a, "carol", "user")
	x.pAlice = auth.Principal{UserID: x.alice, Username: "alice", Role: "user"}
	x.pBob = auth.Principal{UserID: x.bob, Username: "bob", Role: "user"}
	x.pRoot = auth.Principal{UserID: x.root, Username: "root1", Role: "admin"}
	x.pCarol = auth.Principal{UserID: carol, Username: "carol", Role: "user"}
	return x
}

func mkDest(t *testing.T, a *App, name string, owner int64, shared bool) store.Destination {
	t.Helper()
	d, err := a.CreateDestination(context.Background(), store.Destination{Name: name, Endpoint: "http://localhost:9000", Bucket: "b", AccessKey: "ak", OwnerID: &owner, Shared: shared}, "sk")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mkDB(t *testing.T, a *App, name string, owner int64) store.Database {
	t.Helper()
	d := store.Database{Name: name, Engine: "postgres", Version: "17", Domain: name + ".example.com", TLSMode: "internal",
		User: name + "_app", Volume: "fridge-postgres-" + name, Status: "running", ContainerID: "ctr-" + name, OwnerID: &owner}
	if err := a.Store.CreateDatabase(context.Background(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestOwnershipOnCreate(t *testing.T) {
	a, _, objs := newTestApp(t)
	x := mkActors(t, a)
	ctx := context.Background()
	d, err := createDB(a, ctx, CreateRequest{Engine: "postgres", Name: "abc", Domain: "abc.com", OwnerID: x.alice})
	if err != nil || d.OwnerID == nil || *d.OwnerID != x.alice {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := createDB(a, ctx, CreateRequest{Engine: "postgres", Name: "abd", Domain: "abd.com"}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	dest := mkDest(t, a, "s", x.root, true)
	objs.objs["k.dump"] = fakeObj{[]byte("x"), map[string]string{"fridge-engine": "postgres", "fridge-version": "17"}}
	nr := &CreateRequest{Name: "abc2", Domain: "abc2.com"}
	if _, err := startRestore(a, ctx, RestoreRequest{DestinationID: dest.ID, Key: "k.dump", New: nr}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	r, err := startRestore(a, ctx, RestoreRequest{DestinationID: dest.ID, Key: "k.dump", New: nr, OwnerID: x.bob})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	got, _ := a.Store.GetRestore(ctx, r.ID)
	if got.Status != "ok" || got.StartedByID == nil || *got.StartedByID != x.bob || got.TargetDatabaseID == nil {
		t.Fatalf("%+v", got)
	}
	n, _ := a.Store.GetDatabase(ctx, *got.TargetDatabaseID)
	if n.OwnerID == nil || *n.OwnerID != x.bob {
		t.Fatalf("%+v", n)
	}
}

func TestAccessHelpers(t *testing.T) {
	a, _, _ := newTestApp(t)
	x := mkActors(t, a)
	ctx := context.Background()
	d := mkDB(t, a, "abc", x.alice)
	if err := a.Store.AddMember(ctx, d.ID, x.bob, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name          string
		p             auth.Principal
		access, owner bool
	}{{"admin", x.pRoot, true, true}, {"owner", x.pAlice, true, true}, {"member", x.pBob, true, false}, {"other", x.pCarol, false, false}} {
		got, err := a.CanAccessDB(ctx, c.p, d)
		if err != nil || got != c.access || IsDBOwner(c.p, d) != c.owner {
			t.Errorf("%s: access=%v owner=%v %v", c.name, got, IsDBOwner(c.p, d), err)
		}
	}
	priv := mkDest(t, a, "priv", x.alice, false)
	shared := mkDest(t, a, "sh", x.alice, true)
	gone := mkDest(t, a, "gone", x.alice, true)
	if err := a.DeleteDestination(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	gone, _ = a.Store.GetDestination(ctx, gone.ID)
	type row struct {
		p           auth.Principal
		d           store.Destination
		manage, use bool
	}
	for i, c := range []row{
		{x.pAlice, priv, true, true}, {x.pBob, priv, false, false}, {x.pBob, shared, false, true},
		{x.pRoot, priv, true, true}, {x.pAlice, gone, true, false}, {x.pBob, gone, false, false},
	} {
		if CanManageDestination(c.p, c.d) != c.manage || CanUseDestination(c.p, c.d) != c.use {
			t.Errorf("row %d", i)
		}
	}
	vis, _ := a.VisibleDestinations(ctx, x.pBob)
	if len(vis) != 1 || vis[0].ID != shared.ID {
		t.Fatalf("%+v", vis)
	}
	if v, _ := a.VisibleDatabases(ctx, x.pCarol); len(v) != 0 {
		t.Fatal(v)
	}
	if v, _ := a.VisibleDatabases(ctx, x.pRoot); len(v) != 1 {
		t.Fatal(v)
	}
}

func TestMembersApp(t *testing.T) {
	a, _, _ := newTestApp(t)
	x := mkActors(t, a)
	ctx := context.Background()
	d := mkDB(t, a, "abc", x.alice)
	var msgs []string
	for _, n := range []string{"nobody", "alice"} {
		err := a.AddMember(ctx, d, n, x.alice)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "không thêm được") {
			t.Fatal(n, err)
		}
		msgs = append(msgs, err.Error())
	}
	if err := a.AddMember(ctx, d, "bob", x.alice); err != nil {
		t.Fatal(err)
	}
	err := a.AddMember(ctx, d, "bob", x.alice)
	if !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if msgs = append(msgs, err.Error()); msgs[0] != msgs[1] || msgs[1] != msgs[2] {
		t.Fatal(msgs)
	}
	if v, _ := a.VisibleDatabases(ctx, x.pBob); len(v) != 1 {
		t.Fatal(v)
	}
	if m, _ := a.ListMembers(ctx, d); len(m) != 1 || m[0].Username != "bob" {
		t.Fatal(m)
	}
	if err := a.RemoveMember(ctx, d, x.bob); err != nil {
		t.Fatal(err)
	}
	if v, _ := a.VisibleDatabases(ctx, x.pBob); len(v) != 0 {
		t.Fatal(v)
	}
}

// restorableFixture: shared dS, alice_one (alice) and bob_two (bob) backed up on it plus a foreign file.
func restorableFixture(t *testing.T) (*App, actors, store.Destination, *fakeObjects) {
	a, _, objs := newTestApp(t)
	x := mkActors(t, a)
	ctx := context.Background()
	dS := mkDest(t, a, "ds", x.root, true)
	for _, c := range []struct {
		name  string
		owner int64
	}{{"alice_one", x.alice}, {"bob_two", x.bob}} {
		d := mkDB(t, a, c.name, c.owner)
		b := store.Backup{DatabaseID: d.ID, DestinationID: dS.ID, Trigger: "manual", Engine: "postgres", EngineVersion: "17", S3Key: "fridge/" + c.name + ".dump"}
		if err := a.Store.StartBackup(ctx, &b); err != nil {
			t.Fatal(err)
		}
		if err := a.Store.FinishBackup(ctx, b.ID, "ok", 3, "", ""); err != nil {
			t.Fatal(err)
		}
		objs.objs[b.S3Key] = fakeObj{[]byte("abc"), nil}
	}
	objs.objs["upload/x.sql.gz"] = fakeObj{[]byte("abc"), nil}
	return a, x, dS, objs
}

func TestListRestorableShared(t *testing.T) {
	a, x, dS, _ := restorableFixture(t)
	ctx := context.Background()
	got, _, err := a.ListRestorable(ctx, x.pAlice, dS.ID, "", "")
	if err != nil || len(got) != 1 || got[0].Key != "fridge/alice_one.dump" || got[0].DatabaseName != "alice_one" {
		t.Fatalf("%+v %v", got, err)
	}
	if got, _, _ := a.ListRestorable(ctx, x.pAlice, dS.ID, "nope", ""); len(got) != 0 {
		t.Fatal(got)
	}
	all, _, err := a.ListRestorable(ctx, x.pRoot, dS.ID, "", "")
	if err != nil || len(all) != 3 {
		t.Fatalf("%+v %v", all, err)
	}
	for _, r := range all {
		if (r.Key == "upload/x.sql.gz") != (r.DatabaseName == "") {
			t.Fatalf("%+v", r)
		}
	}
}

func TestListRestorablePrivateHidden(t *testing.T) {
	a, _, _ := newTestApp(t)
	x := mkActors(t, a)
	d := mkDest(t, a, "p", x.bob, false)
	if _, _, err := a.ListRestorable(context.Background(), x.pAlice, d.ID, "", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestCanRestoreKey(t *testing.T) {
	a, x, dS, _ := restorableFixture(t)
	ctx := context.Background()
	for key, want := range map[string]bool{"fridge/bob_two.dump": false, "fridge/alice_one.dump": true, "upload/x.sql.gz": false} {
		if got, err := a.CanRestoreKey(ctx, x.pAlice, dS.ID, key); err != nil || got != want {
			t.Errorf("%s: %v %v", key, got, err)
		}
	}
	mine := mkDest(t, a, "mine", x.bob, false)
	if ok, err := a.CanRestoreKey(ctx, x.pBob, mine.ID, "foreign.sql"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	// only ok backups: running and failed ones of alice's own database are refused
	d, _ := a.Store.GetDatabaseByName(ctx, "alice_one")
	for _, st := range []string{"running", "failed"} {
		b := store.Backup{DatabaseID: d.ID, DestinationID: dS.ID, Trigger: "manual", Engine: "postgres", EngineVersion: "17", S3Key: "fridge/" + st + ".dump"}
		if err := a.Store.StartBackup(ctx, &b); err != nil {
			t.Fatal(err)
		}
		if st == "failed" {
			if err := a.Store.FinishBackup(ctx, b.ID, "failed", 0, "", "boom"); err != nil {
				t.Fatal(err)
			}
		}
		if ok, err := a.CanRestoreKey(ctx, x.pAlice, dS.ID, b.S3Key); err != nil || ok {
			t.Errorf("%s: %v %v", st, ok, err)
		}
		if st == "running" {
			a.Store.FinishBackup(ctx, b.ID, "failed", 0, "", "x") // one running backup per DB
		}
	}
}

func TestDefaultScheduleUsesOwnersDestination(t *testing.T) {
	a, _, _ := newTestApp(t)
	x := mkActors(t, a)
	ctx := context.Background()
	mkDest(t, a, "rootpriv", x.root, false)
	d1, err := createDB(a, ctx, CreateRequest{Engine: "postgres", Name: "one", Domain: "one.com", OwnerID: x.alice})
	if err != nil {
		t.Fatal(err)
	}
	if scs, _ := a.Store.ListSchedules(ctx, d1.ID); len(scs) != 0 {
		t.Fatal(scs)
	}
	dS := mkDest(t, a, "ds", x.root, true)
	d2, err := createDB(a, ctx, CreateRequest{Engine: "postgres", Name: "two", Domain: "two.com", OwnerID: x.alice})
	if err != nil {
		t.Fatal(err)
	}
	if scs, _ := a.Store.ListSchedules(ctx, d2.ID); len(scs) != 1 || scs[0].DestinationID != dS.ID {
		t.Fatal(scs)
	}
}

func TestBackupNowFallbackOwnerDestination(t *testing.T) {
	a, _, _ := newTestApp(t)
	x := mkActors(t, a)
	ctx := context.Background()
	d := mkDB(t, a, "abc", x.alice)
	mkDest(t, a, "rootpriv", x.root, false)
	if _, err := a.StartBackup(ctx, d.ID, 0, nil, "manual", &x.alice); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if bs, _ := a.Store.ListBackups(ctx, d.ID); len(bs) != 0 {
		t.Fatal(bs)
	}
	dS := mkDest(t, a, "ds", x.root, true)
	b, err := a.StartBackup(ctx, d.ID, 0, nil, "manual", &x.alice)
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if b.DestinationID != dS.ID || b.StartedByID == nil || *b.StartedByID != x.alice {
		t.Fatalf("%+v", b)
	}
	got, _ := a.Store.GetBackup(ctx, b.ID)
	if got.StartedByID == nil || *got.StartedByID != x.alice {
		t.Fatalf("%+v", got)
	}
}

func TestDeletedDestinationBlocked(t *testing.T) {
	a, _, objs := newTestApp(t)
	x := mkActors(t, a)
	ctx := context.Background()
	d := mkDB(t, a, "abc", x.alice)
	dest := mkDest(t, a, "ds", x.alice, false)
	b, err := a.StartBackup(ctx, d.ID, dest.ID, nil, "manual", &x.alice)
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if err := a.DeleteDestination(ctx, dest.ID); err != nil {
		t.Fatal(err)
	}
	objs.objs[b.S3Key] = fakeObj{[]byte("x"), nil}
	_, _, e1 := a.OpenBackup(ctx, b.ID)
	_, e2 := startRestore(a, ctx, RestoreRequest{DestinationID: dest.ID, Key: b.S3Key, OwnerID: x.alice, New: &CreateRequest{Name: "abc2", Domain: "abc2.com"}})
	e3 := a.TestDestination(ctx, dest.ID)
	e4 := a.UpdateDestination(ctx, store.Destination{ID: dest.ID, Name: "n", Endpoint: "http://h:1", Bucket: "b"}, "")
	_, e5 := a.CreateSchedule(ctx, store.Schedule{DatabaseID: d.ID, DestinationID: dest.ID, Cron: "0 2 * * *", KeepLatest: 1})
	for i, e := range []error{e1, e2, e3, e4, e5} {
		if !errors.Is(e, ErrInvalid) || !strings.Contains(e.Error(), "destination deleted") {
			t.Errorf("e%d: %v", i+1, e)
		}
	}
	if bs, _ := a.Store.ListBackups(ctx, d.ID); len(bs) != 1 {
		t.Fatal(bs)
	}
}
