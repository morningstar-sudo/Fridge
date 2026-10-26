package store_test

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"fridge/internal/store"
	"fridge/internal/store/storetest"
)

func newUser(name, role string) *store.User {
	return &store.User{Username: name, Role: role, Salt: bytes.Repeat([]byte{1}, 16), Iterations: 600000,
		StoredKey: bytes.Repeat([]byte{2}, 32), ServerKey: bytes.Repeat([]byte{3}, 32)}
}

func mustUser(t *testing.T, s *store.Store, name, role string) *store.User {
	t.Helper()
	u := newUser(name, role)
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestMetaPostgres18(t *testing.T) {
	s := storetest.New(t)
	var v string
	if err := s.Pool().QueryRow(ctx, "SHOW server_version").Scan(&v); err != nil || !strings.HasPrefix(v, "18") {
		t.Fatalf("server_version = %q, %v", v, err)
	}
}

func TestMigration4Applied(t *testing.T) {
	s := storetest.New(t)
	var n int
	if err := s.Pool().QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil || n != 7 {
		t.Fatalf("schema_migrations = %d, %v", n, err)
	}
	for _, c := range [][2]string{{"databases", "owner_id"}, {"destinations", "shared"}, {"backups", "started_by_id"}} {
		var ok bool
		if err := s.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name=$1 AND column_name=$2)`, c[0], c[1]).Scan(&ok); err != nil || !ok {
			t.Fatalf("column %s.%s missing (%v)", c[0], c[1], err)
		}
	}
}

func TestCreateFirstAdminOnce(t *testing.T) {
	s := storetest.New(t)
	db, dest := seed(t, s, "a")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, name := range []string{"alice", "bob"} {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = s.CreateFirstAdmin(ctx, newUser(name, "admin")) }()
	}
	wg.Wait()
	nilN, conflictN := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			nilN++
		case errors.Is(e, store.ErrConflict):
			conflictN++
		}
	}
	if nilN != 1 || conflictN != 1 {
		t.Fatalf("errs = %v", errs)
	}
	if n, _ := s.CountUsers(ctx); n != 1 {
		t.Fatalf("users = %d", n)
	}
	var dOwner, deOwner, admin int64
	if err := s.Pool().QueryRow(ctx, `SELECT owner_id FROM databases WHERE id=$1`, db.ID).Scan(&dOwner); err != nil {
		t.Fatal(err)
	}
	if err := s.Pool().QueryRow(ctx, `SELECT owner_id FROM destinations WHERE id=$1`, dest.ID).Scan(&deOwner); err != nil {
		t.Fatal(err)
	}
	s.Pool().QueryRow(ctx, `SELECT id FROM users`).Scan(&admin)
	if dOwner != admin || deOwner != admin {
		t.Fatalf("owners %d %d admin %d", dOwner, deOwner, admin)
	}
}

func TestSetupBackfillsDomains(t *testing.T) {
	s := storetest.New(t)
	for _, d := range []store.Database{
		{Name: "a", Domain: "old.com", Status: "error"},
		{Name: "b", Domain: "old.com", Status: "running"},
		{Name: "c", Domain: "other.com", Status: "running"},
	} {
		d.Engine, d.Version, d.TLSMode = "postgres", "17", "internal"
		if err := s.CreateDatabase(ctx, &d); err != nil {
			t.Fatal(err)
		}
	}
	admin := newUser("alice", "admin")
	if err := s.CreateFirstAdmin(ctx, admin); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Pool().Query(ctx, `SELECT name, owner_id FROM domains ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		var o int64
		if err := rows.Scan(&n, &o); err != nil || o != admin.ID {
			t.Fatalf("%s owner %d err %v", n, o, err)
		}
		names = append(names, n)
	}
	rows.Close()
	if strings.Join(names, ",") != "old.com,other.com" {
		t.Fatalf("domains = %v", names)
	}
	var bad int
	s.Pool().QueryRow(ctx, `SELECT count(*) FROM databases d LEFT JOIN domains m ON m.id=d.domain_id WHERE m.name IS DISTINCT FROM d.domain`).Scan(&bad)
	if bad != 0 {
		t.Fatalf("%d databases with wrong domain_id", bad)
	}
	if _, err := s.Pool().Exec(ctx, `INSERT INTO domains(owner_id, name) VALUES (NULL, 'x.com')`); err == nil {
		t.Fatal("NULL domain owner accepted")
	}
}

func TestUserCRUD(t *testing.T) {
	s := storetest.New(t)
	a := mustUser(t, s, "alice", "admin")
	mustUser(t, s, "bob", "admin")
	if err := s.CreateUser(ctx, newUser("alice", "user")); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup: %v", err)
	}
	got, err := s.GetUserByName(ctx, "alice")
	if err != nil || got.ID != a.ID || got.Iterations != 600000 || len(got.Salt) != 16 {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.GetUser(ctx, 9999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := s.SetUserDisabled(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountActiveAdmins(ctx); n != 1 {
		t.Fatalf("active admins = %d", n)
	}
	if err := s.SetUserRole(ctx, a.ID, "user"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUserCredentials(ctx, a.ID, bytes.Repeat([]byte{9}, 16), 700000, bytes.Repeat([]byte{8}, 32), bytes.Repeat([]byte{7}, 32)); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetUser(ctx, a.ID)
	if got.Role != "user" || !got.Disabled || got.Iterations != 700000 || got.Salt[0] != 9 {
		t.Fatalf("%+v", got)
	}
	if us, err := s.ListUsers(ctx); err != nil || len(us) != 2 {
		t.Fatalf("%v %v", us, err)
	}
}

func TestUsernameCheck(t *testing.T) {
	s := storetest.New(t)
	for _, n := range []string{"alice_x", "Al", "ab", "a-b", "1abc", "abcdefghijklmnopq"} {
		if err := s.CreateUser(ctx, newUser(n, "user")); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%q: %v", n, err)
		}
	}
	for _, n := range []string{"alice", "bob2"} {
		if err := s.CreateUser(ctx, newUser(n, "user")); err != nil {
			t.Errorf("%q: %v", n, err)
		}
	}
	inv := &store.Invite{TokenHash: []byte("h"), Username: "bob_1", Role: "user", Purpose: "new_user", ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.CreateInvite(ctx, inv); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("invite: %v", err)
	}
}

func TestLastAdminConcurrent(t *testing.T) {
	s := storetest.New(t)
	a := mustUser(t, s, "alice", "admin")
	b := mustUser(t, s, "bob", "admin")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, id := range []int64{a.ID, b.ID} {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = s.SetUserRole(ctx, id, "user") }()
	}
	wg.Wait()
	nilN, lastN := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			nilN++
		case errors.Is(e, store.ErrLastAdmin):
			lastN++
		}
	}
	if nilN != 1 || lastN != 1 {
		t.Fatalf("errs = %v", errs)
	}
	if n, _ := s.CountActiveAdmins(ctx); n != 1 {
		t.Fatalf("admins = %d", n)
	}
	var last int64
	s.Pool().QueryRow(ctx, `SELECT id FROM users WHERE role='admin'`).Scan(&last)
	if err := s.SetUserDisabled(ctx, last, true); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("disable last: %v", err)
	}
}

func TestInviteSingleUse(t *testing.T) {
	s := storetest.New(t)
	inv := &store.Invite{TokenHash: []byte("hash1"), Username: "carol", Role: "user", Purpose: "new_user", ExpiresAt: time.Now().Add(48 * time.Hour)}
	if err := s.CreateInvite(ctx, inv); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetInviteByHash(ctx, []byte("hash1"))
	if err != nil || got.ID != inv.ID || got.UsedAt != nil {
		t.Fatalf("%+v %v", got, err)
	}
	if err := s.MarkInviteUsed(ctx, inv.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkInviteUsed(ctx, inv.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second: %v", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := storetest.New(t)
	u := mustUser(t, s, "alice", "admin")
	now := time.Now()
	mk := func(h string, seen, exp time.Time) {
		x := &store.Session{IDHash: []byte(h), UserID: u.ID, CreatedAt: now.Add(-48 * time.Hour), LastSeenAt: seen, ExpiresAt: exp, IP: "1.2.3.4", UserAgent: "ua"}
		if err := s.CreateSession(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	mk("h1", now, now.Add(time.Hour))
	mk("h2", now, now.Add(time.Hour))
	mk("idle", now.Add(-13*time.Hour), now.Add(time.Hour))
	mk("old", now, now.Add(-time.Minute))
	got, err := s.GetSession(ctx, []byte("h1"))
	if err != nil || got.UserID != u.ID || got.IP != "1.2.3.4" {
		t.Fatalf("%+v %v", got, err)
	}
	at := now.Add(time.Minute).Truncate(time.Microsecond)
	if err := s.TouchSession(ctx, []byte("h1"), at); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetSession(ctx, []byte("h1")); !got.LastSeenAt.Equal(at) {
		t.Fatalf("touch: %v", got.LastSeenAt)
	}
	if n, err := s.DeleteExpiredSessions(ctx, now, 12*time.Hour); err != nil || n != 2 {
		t.Fatalf("expired n=%d err=%v", n, err)
	}
	if l, _ := s.ListSessions(ctx, nil); len(l) != 2 {
		t.Fatalf("list = %d", len(l))
	}
	if err := s.DeleteUserSessions(ctx, u.ID, []byte("h1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, []byte("h2")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("h2: %v", err)
	}
	if _, err := s.GetSession(ctx, []byte("h1")); err != nil {
		t.Fatalf("h1: %v", err)
	}
	if err := s.DeleteSession(ctx, []byte("h1")); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.ListSessions(ctx, &u.ID); len(l) != 0 {
		t.Fatalf("list = %d", len(l))
	}
}

func TestFailRunningIncludesDumpChecks(t *testing.T) {
	s := storetest.New(t)
	_, dest := seed(t, s, "a")
	var id int64
	if err := s.Pool().QueryRow(ctx, `INSERT INTO dump_checks(destination_id, s3_key, status) VALUES($1,'k','running') RETURNING id`, dest.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if n, err := s.FailRunning(ctx, "boom"); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	var st, e string
	var fin *time.Time
	s.Pool().QueryRow(ctx, `SELECT status, error, finished_at FROM dump_checks WHERE id=$1`, id).Scan(&st, &e, &fin)
	if st != "failed" || e != "boom" || fin == nil {
		t.Fatalf("%s %s %v", st, e, fin)
	}
}

func TestMigration4Tombstones(t *testing.T) {
	url := storetest.NewEmpty(t)
	s, err := store.OpenRaw(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.MigrateTo(ctx, 3); err != nil {
		t.Fatal(err)
	}
	p := s.Pool()
	if _, err := p.Exec(ctx, `INSERT INTO databases(name, engine, version, domain) VALUES('a','postgres','17','a.com')`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO backups(database_id, destination_id, schedule_id, trigger, engine, engine_version, s3_key)
		SELECT id, 99, 77, 'manual', 'postgres', '17', 'k' FROM databases`); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateTo(ctx, 4); err != nil {
		t.Fatal(err)
	}
	var del bool
	var prov string
	if err := p.QueryRow(ctx, `SELECT deleted_at IS NOT NULL, provider FROM destinations WHERE id=99`).Scan(&del, &prov); err != nil || !del || prov != "deleted" {
		t.Fatalf("tombstone: %v %q %v", del, prov, err)
	}
	var sched *int64
	if err := p.QueryRow(ctx, `SELECT schedule_id FROM backups`).Scan(&sched); err != nil || sched != nil {
		t.Fatalf("schedule_id %v %v", sched, err)
	}
	var fk int
	p.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE contype='f' AND conrelid='backups'::regclass AND confrelid='destinations'::regclass`).Scan(&fk)
	if fk != 1 {
		t.Fatalf("fk count %d", fk)
	}
}

func TestIntegrityConstraints(t *testing.T) {
	s := storetest.New(t)
	db, dest := seed(t, s, "a")
	u := mustUser(t, s, "alice", "admin")
	cases := []struct {
		name string
		sql  string
		args []any
		want error
	}{
		{"backup weird status", `INSERT INTO backups(database_id, destination_id, trigger, engine, engine_version, status, s3_key, finished_at) VALUES($1,$2,'manual','postgres','17','weird','x1', now())`, []any{db.ID, dest.ID}, store.ErrInvalid},
		{"backup ok without key", `INSERT INTO backups(database_id, destination_id, trigger, engine, engine_version, status, s3_key, finished_at) VALUES($1,$2,'manual','postgres','17','ok','', now())`, []any{db.ID, dest.ID}, store.ErrInvalid},
		{"schedule keep 0", `INSERT INTO backup_schedules(database_id, destination_id, cron, keep_latest) VALUES($1,$2,'0 2 * * *',0)`, []any{db.ID, dest.ID}, store.ErrInvalid},
		{"user short salt", `INSERT INTO users(username, role, scram_salt, scram_iterations, stored_key, server_key) VALUES('carol','user',decode('00112233445566778899aabbccddee','hex'),600000,$1,$1)`, []any{bytes.Repeat([]byte{1}, 32)}, store.ErrInvalid},
		{"reset invite without user", `INSERT INTO invites(token_hash, username, role, purpose, expires_at) VALUES('\x01','carol','user','reset_password', now()+interval '1 day')`, nil, store.ErrInvalid},
		{"owner as member", `INSERT INTO database_members(database_id, user_id) VALUES($1,$2)`, []any{db.ID, u.ID}, store.ErrInvalid},
		{"backup bad destination", `INSERT INTO backups(database_id, destination_id, trigger, engine, engine_version, s3_key) VALUES($1,424242,'manual','postgres','17','x2')`, []any{db.ID}, store.ErrInUse},
	}
	if _, err := s.Pool().Exec(ctx, `UPDATE databases SET owner_id=$1 WHERE id=$2`, u.ID, db.ID); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		_, err := s.Pool().Exec(ctx, c.sql, c.args...)
		if err == nil || !errors.Is(store.MapErr(err), c.want) {
			t.Errorf("%s: err=%v mapped=%v", c.name, err, store.MapErr(err))
		}
	}
}

func TestOwnerNotNullAfterSetup(t *testing.T) {
	s := storetest.New(t)
	if err := s.CreateFirstAdmin(ctx, newUser("alice", "admin")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool().Exec(ctx, `INSERT INTO databases(name, engine, version, domain) VALUES('x','postgres','17','x.com')`); err == nil {
		t.Fatal("NULL databases.owner_id accepted after setup")
	}
}
