package app

import (
	"context"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fridge/internal/store"
)

var nodeNameRe = regexp.MustCompile(`^fridge-postgres-([0-9a-f]{16})$`)

func lastExec(t *testing.T, rt *fakeRuntime) execCall {
	t.Helper()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.execs) == 0 {
		t.Fatal("no exec")
	}
	return rt.execs[len(rt.execs)-1]
}

func appPassword(t *testing.T, a *App, d store.Database) string {
	t.Helper()
	cs, err := a.ConnString(d)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(cs)
	pw, _ := u.User.Password()
	return pw
}

func TestTwoUsersPerNode(t *testing.T) {
	e := newNameEnv(t)
	seen := map[string]bool{}
	pws := map[string]bool{}
	for _, n := range []string{"shop", "blog"} {
		d, err := e.create(e.alice, e.dom(e.alice, n), n, "", "")
		if err != nil {
			t.Fatal(err)
		}
		run := e.rt.runs[len(e.rt.runs)-1]
		m := nodeNameRe.FindStringSubmatch(run.Name)
		if m == nil || run.Volume != run.Name || seen[m[1]] {
			t.Fatalf("run name %q volume %q (seen %v)", run.Name, run.Volume, seen)
		}
		seen[m[1]] = true
		key := m[1]
		if d.NodeKey != key || d.AdminUser != "su_"+key || d.Volume != run.Name || d.AdminPasswordEnc == "" {
			t.Fatalf("db = %+v", d)
		}
		if envOf(e.rt, "POSTGRES_USER") != "su_"+key {
			t.Fatalf("env = %v", run.Env)
		}
		adminPW := envOf(e.rt, "POSTGRES_PASSWORD")
		if adminPW == "" || pws[adminPW] {
			t.Fatalf("admin password %q reused", adminPW)
		}
		pws[adminPW] = true
		appPW := appPassword(t, e.a, d)
		x := lastExec(t, e.rt)
		if got := strings.Join(x.Cmd, " "); !strings.HasPrefix(got, "psql -U su_"+key+" -d alice_"+n) || strings.Contains(got, appPW) {
			t.Fatalf("exec cmd = %q", got)
		}
		for _, want := range []string{`CREATE ROLE "alice_` + n + `" LOGIN`, "NOSUPERUSER", `OWNER TO "alice_` + n + `"`, "'" + appPW + "'"} {
			if !strings.Contains(x.Stdin, want) {
				t.Errorf("stdin %q missing %q", x.Stdin, want)
			}
		}
		if appPW == adminPW {
			t.Fatal("app and admin passwords equal")
		}

		dest, err := e.a.CreateDestination(context.Background(), store.Destination{Name: "s3-" + n, Endpoint: "http://localhost:9000", Bucket: "b", AccessKey: "ak"}, "sk")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.a.StartBackup(context.Background(), d.ID, dest.ID, nil, "manual", nil); err != nil {
			t.Fatal(err)
		}
		e.a.Wait()
		if got := strings.Join(lastExec(t, e.rt).Cmd, " "); got != "pg_dump -U su_"+key+" -d alice_"+n+" -Fc" {
			t.Fatalf("dump = %q", got)
		}
	}
}

func TestCustomCredentialsTwoUser(t *testing.T) {
	e := newNameEnv(t)
	pw := "S3cure-pass.~x"
	d, err := e.create(e.alice, e.aliceDom, "shop", "shop_rw", pw)
	if err != nil {
		t.Fatal(err)
	}
	if u := envOf(e.rt, "POSTGRES_USER"); u != "su_"+d.NodeKey || u == "alice_shop_rw" {
		t.Fatalf("POSTGRES_USER = %q", u)
	}
	x := lastExec(t, e.rt)
	if !strings.Contains(x.Stdin, `CREATE ROLE "alice_shop_rw" LOGIN PASSWORD '`+pw+`'`) || strings.Contains(strings.Join(x.Cmd, " "), pw) {
		t.Fatalf("exec %+v", x)
	}
}

func TestProvisionAppUserFailureRollsBack(t *testing.T) {
	e := newNameEnv(t)
	e.rt.execFn = func(cmd []string, stdin io.Reader, stdout, stderr io.Writer) int { return 3 }
	if _, err := e.create(e.alice, e.aliceDom, "shop", "", ""); err == nil {
		t.Fatal("want error")
	}
	dbs, _ := e.a.Store.ListDatabases(context.Background())
	if len(dbs) != 1 || dbs[0].Status != "error" || len(e.rt.removes) != 1 {
		t.Fatalf("dbs %+v removes %+v", dbs, e.rt.removes)
	}
}

func TestRotateSuperuser(t *testing.T) {
	e := newNameEnv(t)
	ctx := context.Background()
	d, err := e.create(e.alice, e.aliceDom, "shop", "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, before, err := e.a.RevealSuperuser(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.a.RotateSuperuser(ctx, d); err != nil {
		t.Fatal(err)
	}
	d, _ = e.a.Store.GetDatabase(ctx, d.ID)
	_, after, _ := e.a.RevealSuperuser(ctx, d)
	x := lastExec(t, e.rt)
	if after == before || x.Stdin != `ALTER ROLE "su_`+d.NodeKey+`" PASSWORD '`+after+`';` || strings.Contains(strings.Join(x.Cmd, " "), after) {
		t.Fatalf("before %q after %q exec %+v", before, after, x)
	}

	e.rt.execFn = func(cmd []string, stdin io.Reader, stdout, stderr io.Writer) int { return 1 }
	if err := e.a.RotateSuperuser(ctx, d); err == nil {
		t.Fatal("want error on failed exec")
	}
	d2, _ := e.a.Store.GetDatabase(ctx, d.ID)
	if d2.AdminPasswordEnc != d.AdminPasswordEnc {
		t.Fatal("password changed despite failed exec")
	}
	e.rt.execFn = nil

	stopped := d
	stopped.Status = "stopped"
	if err := e.a.RotateSuperuser(ctx, stopped); !errors.Is(err, ErrInvalid) {
		t.Fatalf("stopped: %v", err)
	}
	legacy := store.Database{Name: "old", Engine: "postgres", Version: "17", Domain: "old.lan", TLSMode: "internal",
		User: "old_app", Volume: "fridge-postgres-old", Status: "running", ContainerID: "c"}
	if err := e.a.RotateSuperuser(ctx, legacy); !errors.Is(err, ErrInvalid) {
		t.Fatalf("legacy: %v", err)
	}
}

func TestRotateSuperuserSerialized(t *testing.T) {
	e := newNameEnv(t)
	ctx := context.Background()
	d, err := e.create(e.alice, e.aliceDom, "shop", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var inflight, overlap atomic.Int32
	e.rt.execFn = func([]string, io.Reader, io.Writer, io.Writer) int {
		if inflight.Add(1) > 1 {
			overlap.Store(1)
		}
		time.Sleep(20 * time.Millisecond)
		inflight.Add(-1)
		return 0
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Go(func() { errs[i] = e.a.RotateSuperuser(ctx, d) })
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || overlap.Load() != 0 {
		t.Fatalf("errs %v overlap %d", errs, overlap.Load())
	}
	d, _ = e.a.Store.GetDatabase(ctx, d.ID)
	_, stored, _ := e.a.RevealSuperuser(ctx, d)
	if x := lastExec(t, e.rt); x.Stdin != `ALTER ROLE "su_`+d.NodeKey+`" PASSWORD '`+stored+`';` {
		t.Fatalf("node got %q, stored %q", x.Stdin, stored)
	}
}

func TestRevealSuperuser(t *testing.T) {
	e := newNameEnv(t)
	ctx := context.Background()
	d, err := e.create(e.alice, e.aliceDom, "shop", "", "")
	if err != nil {
		t.Fatal(err)
	}
	u, pw, err := e.a.RevealSuperuser(ctx, d)
	if err != nil || u != "su_"+d.NodeKey || pw != envOf(e.rt, "POSTGRES_PASSWORD") {
		t.Fatalf("reveal = %q %q %v", u, pw, err)
	}
	enc, _ := e.a.Box.Encrypt("legacy-pw")
	legacy := store.Database{User: "old_app", PasswordEnc: enc}
	if u, pw, err := e.a.RevealSuperuser(ctx, legacy); err != nil || u != "old_app" || pw != "legacy-pw" {
		t.Fatalf("legacy reveal = %q %q %v", u, pw, err)
	}
}

func TestRestoreUsesAppRole(t *testing.T) {
	a, rt, objs := newTestApp(t)
	ctx := context.Background()
	dumpKey, sqlKey := "fridge/postgres-17/abc/abc-1.dump", "upload/x.sql.gz"
	dest := restoreFixture(t, a, objs, dumpKey, []byte("DUMP"), nil)
	objs.objs[sqlKey] = fakeObj{gz("select 1;"), nil}

	if _, err := startRestore(a, ctx, RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: dumpKey, New: &CreateRequest{Name: "r1", Domain: "r1.com"}}); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	x := lastExec(t, rt)
	if got := strings.Join(x.Cmd, " "); !strings.Contains(got, "--role=root1_r1") || !strings.Contains(got, "-U su_") || x.Stdin != "DUMP" {
		t.Fatalf("dump restore exec %+v", x)
	}

	if _, err := startRestore(a, ctx, RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: sqlKey, Engine: "postgres", New: &CreateRequest{Name: "r2", Domain: "r2.com"}}); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	x = lastExec(t, rt)
	if x.Cmd[0] != "psql" || x.Stdin != "SET ROLE \"root1_r2\";\nselect 1;" {
		t.Fatalf("sql restore exec %+v", x)
	}
}

func TestLegacyDBUsesSingleUser(t *testing.T) {
	a, rt, _ := newTestApp(t)
	d, dest := fixture(t, a) // legacy row: AdminUser "", NodeKey ""
	if _, err := a.StartBackup(context.Background(), d.ID, dest.ID, nil, "manual", nil); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if got := strings.Join(lastExec(t, rt).Cmd, " "); got != "pg_dump -U abc_app -d abc -Fc" {
		t.Fatalf("dump = %q", got)
	}
	a.removeNode(context.Background(), d)
	if len(rt.removes) != 1 || rt.removes[0] != (removeCall{"ctr-abc", "fridge-postgres-abc"}) {
		t.Fatalf("removes = %+v", rt.removes)
	}
}
