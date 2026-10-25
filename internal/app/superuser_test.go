package app

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fridge/internal/store"
	"fridge/internal/swarm"
)

var nodeNameRe = regexp.MustCompile(`^fridge-postgres-([0-9a-f]{16})$`)

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
		if m == nil || run.Spec.VolumeName != run.Name || seen[m[1]] {
			t.Fatalf("run name %q volume %q (seen %v)", run.Name, run.Spec.VolumeName, seen)
		}
		seen[m[1]] = true
		key := m[1]
		if d.NodeKey != key || d.AdminUser != "su_"+key || d.Volume != run.Name || d.AdminPasswordEnc == "" || run.Spec.Key != key || d.HostID != "host1" || run.Spec.HostID != "host1" {
			t.Fatalf("db = %+v run = %+v", d, run.Spec)
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
		x := adminOf(e.a).last()
		// the superuser connects through the node's edge sidecar, not to the database service
		if u, _ := url.Parse(x.DSN); u.User.Username() != "su_"+key || u.Host != swarm.EdgeService(key)+":5432" || u.Path != "/alice_"+n {
			t.Fatalf("admin dsn = %q", x.DSN)
		}
		if pw, _ := mustURL(t, x.DSN).User.Password(); pw != adminPW {
			t.Fatalf("admin connected with %q, want the node's superuser password", pw)
		}
		for _, want := range []string{`CREATE ROLE "alice_` + n + `" LOGIN`, "NOSUPERUSER", `OWNER TO "alice_` + n + `"`, "'" + appPW + "'"} {
			if !strings.Contains(x.SQL, want) {
				t.Errorf("sql %q missing %q", x.SQL, want)
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
		j, ok := e.rt.lastJob("dump")
		if !ok || j.Spec.Params.AdminUser != "su_"+key || j.Spec.Params.Name != "alice_"+n || j.Spec.Params.Host != swarm.DBService(key) || j.Spec.Params.AdminPassword != adminPW {
			t.Fatalf("dump job = %+v", j)
		}
		if strings.Join(j.Networks, ",") != swarm.Network(key)+",fridge_egress" {
			t.Fatalf("dump job networks = %v", j.Networks)
		}
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
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
	x := adminOf(e.a).last()
	if !strings.Contains(x.SQL, `CREATE ROLE "alice_shop_rw" LOGIN PASSWORD '`+pw+`'`) {
		t.Fatalf("sql %q", x.SQL)
	}
}

func TestProvisionAppUserFailureRollsBack(t *testing.T) {
	e := newNameEnv(t)
	adminOf(e.a).err = errors.New("boom")
	_, err := e.create(e.alice, e.aliceDom, "shop", "", "")
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "CREATE ROLE") {
		t.Fatalf("the error carries the SQL (and so a password): %v", err)
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
	x := adminOf(e.a).last()
	if pw, _ := mustURL(t, x.DSN).User.Password(); pw != before { // connects with the old password, sets the new
		t.Fatalf("rotation connected with %q, want the old password", pw)
	}
	if after == before || x.SQL != `ALTER ROLE "su_`+d.NodeKey+`" PASSWORD '`+after+`';` || strings.Contains(x.DSN, after) {
		t.Fatalf("before %q after %q exec %+v", before, after, x)
	}

	adminOf(e.a).err = errors.New("refused")
	if err := e.a.RotateSuperuser(ctx, d); err == nil {
		t.Fatal("want error on failed exec")
	}
	d2, _ := e.a.Store.GetDatabase(ctx, d.ID)
	if d2.AdminPasswordEnc != d.AdminPasswordEnc {
		t.Fatal("password changed despite failed exec")
	}
	adminOf(e.a).err = nil

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
	adminOf(e.a).fn = func(dsn, sql string) error {
		if inflight.Add(1) > 1 {
			overlap.Store(1)
		}
		time.Sleep(20 * time.Millisecond)
		inflight.Add(-1)
		return nil
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
	if x := adminOf(e.a).last(); x.SQL != `ALTER ROLE "su_`+d.NodeKey+`" PASSWORD '`+stored+`';` {
		t.Fatalf("node got %q, stored %q", x.SQL, stored)
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

// The restore job carries the new node's superuser and the app user to hand the objects to; the exact
// commands and the SET ROLE prefix are the job's own business (internal/job tests).
func TestRestoreJobTargetsTheNewNode(t *testing.T) {
	a, rt, objs := newTestApp(t)
	ctx := context.Background()
	dumpKey, sqlKey := "fridge/postgres-17/abc/abc-1.dump", "upload/x.sql.gz"
	dest := restoreFixture(t, a, objs, dumpKey, dumpFile(t, "pg17.dump"), nil)
	objs.objs[sqlKey] = fakeObj{gz("CREATE TABLE t(id int);"), nil}

	for _, c := range []struct{ key, eng, name string }{{dumpKey, "", "r1"}, {sqlKey, "postgres", "r2"}} {
		if _, err := startRestore(a, ctx, RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: c.key, Engine: c.eng, New: &CreateRequest{Name: c.name, Domain: c.name + ".com"}}); err != nil {
			t.Fatal(err)
		}
		a.Wait()
		j, ok := rt.lastJob("restore")
		node := rt.runs[len(rt.runs)-1].Spec.Key
		if !ok || j.Spec.Key != c.key || j.Spec.Engine != "postgres" || j.Spec.Params.User != "root1_"+c.name || j.Spec.Params.AdminUser != "su_"+node ||
			j.Spec.Params.Host != swarm.DBService(node) || j.Spec.S3.Bucket != "b" || j.Spec.S3.SecretKey != "sk" {
			t.Fatalf("restore job = %+v", j)
		}
		if strings.Join(j.Networks, ",") != swarm.Network(node)+",fridge_egress" {
			t.Fatalf("restore job networks = %v", j.Networks)
		}
	}
}
