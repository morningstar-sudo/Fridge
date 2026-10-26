package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"fridge/internal/job"
	"fridge/internal/store"
	"fridge/internal/swarm"
)

// Every engine but Postgres is administered by a job: the node's superuser runs the script in a job on the database
// network, with the node's client certificate, and never through pgx.
func TestEveryEngineIsProvisionedAndRoutedItsOwnWay(t *testing.T) {
	cases := []struct {
		eng, version, entry, scriptHas string
		sidecar, traefik               bool
	}{
		{"redis", "", "redis", "ACL SETUSER", true, true},
		{"mongo", "", "mongo", "createUser", true, true},
		{"mysql", "", "", "CREATE USER", false, false},
		{"mariadb", "", "", "CREATE USER", false, false},
	}
	for _, c := range cases {
		t.Run(c.eng, func(t *testing.T) {
			a, rt, _ := newTestApp(t)
			ctx := context.Background()
			d, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: c.eng, Name: "shop", Domain: "shop.example.com"})
			if err != nil {
				t.Fatal(err)
			}
			if d.Status != "running" || len(rt.runs) != 1 {
				t.Fatalf("%+v %d runs", d, len(rt.runs))
			}
			n := rt.runs[0].Spec
			if (n.Upstream == "none") == c.sidecar || len(n.Command) != 3 || n.Image == "" {
				t.Fatalf("node spec %+v", n)
			}
			if c.sidecar && len(n.EdgeEnv) != 3 {
				t.Fatalf("the sidecar of a %s node gets the client certificate: %v", c.eng, n.EdgeEnv)
			}
			if got := adminOf(a).last(); got.SQL != "" {
				t.Fatalf("a %s node was administered through pgx: %+v", c.eng, got)
			}
			x, ok := rt.lastJob("exec")
			if !ok {
				t.Fatal("no admin job ran")
			}
			p := x.Spec.Params
			if x.Spec.Engine != c.eng || !strings.Contains(x.Spec.Script, c.scriptHas) || !strings.Contains(x.Spec.Script, d.User) ||
				strings.Join(x.Networks, ",") != "fridge_db" || p.Host != swarm.DBService(d.NodeKey) || p.AdminUser != d.AdminUser ||
				p.TLS == nil || p.TLS.ClientKey == "" || p.AdminPassword == "" {
				t.Fatalf("admin job %+v", x)
			}
			cfg, err := a.ProxyConfig(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if c.traefik {
				if len(cfg.DBs) != 1 || cfg.DBs[0].EntryPoint != c.entry || cfg.DBs[0].ALPN != "" || cfg.DBs[0].Domain != "shop.example.com" {
					t.Fatalf("traefik routes: %+v", cfg.DBs)
				}
			} else if len(cfg.DBs) != 0 {
				t.Fatalf("%s is routed by the MySQL router, not Traefik: %+v", c.eng, cfg.DBs)
			}
			doc, err := a.MySQLRouterConfig(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if c.traefik {
				if len(doc.Users) != 0 {
					t.Fatalf("the MySQL router got a %s user: %v", c.eng, doc.Users)
				}
				return
			}
			for _, u := range []string{d.User, d.AdminUser} {
				node, ok := doc.Users[u]
				if !ok || node.Addr != swarm.DBService(d.NodeKey)+":3306" || node.Key == "" || node.Cert == "" || node.ServerName != swarm.DBService(d.NodeKey) {
					t.Fatalf("router user %s: %+v", u, node)
				}
			}
			if doc.Frontend.Cert == "" || doc.Frontend.Key == "" {
				t.Fatal("no frontend certificate")
			}
			// the instance CA vouches for the frontend certificate, so that is the CA a MySQL client downloads
			if ca, _ := a.DatabaseCA(ctx, d); ca != string(a.CA.CertPEM()) {
				t.Fatal("a MySQL/MariaDB database must hand out the instance CA")
			}
		})
	}
}

// The router chooses the database by the user name, so no two live databases may share one (the store enforces it for every engine).
func TestMySQLUsersAreGlobalBecauseTheRouterChoosesByThem(t *testing.T) {
	a, _, _ := newTestApp(t)
	ctx := context.Background()
	alice, bob := mkUser(t, a, "alice", "user"), mkUser(t, a, "bob", "user")
	regDomain(a, alice, "a.com")
	regDomain(a, alice, "b.com")
	regDomain(a, bob, "c.com")
	if _, err := createDB(a, ctx, CreateRequest{OwnerID: alice, Engine: "mysql", Name: "one", Domain: "a.com", DBUser: "app"}); err != nil {
		t.Fatal(err)
	}
	// the same owner, the same user name, another MySQL-routed engine: the router could not tell them apart
	if _, err := createDB(a, ctx, CreateRequest{OwnerID: alice, Engine: "mariadb", Name: "two", Domain: "b.com", DBUser: "app"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a user shared by two MySQL-routed databases: %v", err)
	}
	// owners cannot clash: user names carry the owner
	if _, err := createDB(a, ctx, CreateRequest{OwnerID: bob, Engine: "mysql", Name: "one", Domain: "c.com", DBUser: "app"}); err != nil {
		t.Fatalf("another owner's user: %v", err)
	}
}

func TestRestoreIntoARedisNodeLoadsTheFileBeforeTheNodeStarts(t *testing.T) {
	a, rt, objs := newTestApp(t)
	ctx := context.Background()
	key := "fridge/redis-7.4/root1_abc/root1_abc-1.rdb"
	dest := restoreFixture(t, a, objs, key, append([]byte("REDIS0011"), make([]byte, 40)...), map[string]string{"fridge-engine": "redis", "fridge-version": "7.4"})
	r, err := startRestore(a, ctx, RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, New: &CreateRequest{OwnerID: own(t, a), Name: "copy", Domain: "copy.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	got, _ := a.Store.GetRestore(ctx, r.ID)
	if got.Status != "ok" || got.TargetDatabaseID == nil {
		t.Fatalf("restore %+v", got)
	}
	d, _ := a.Store.GetDatabase(ctx, *got.TargetDatabaseID)
	pre, ok := rt.lastJob("preload")
	if !ok || pre.Spec.Key != key || pre.NodeID != d.HostID || len(pre.Volumes) != 1 || pre.Volumes[0] != d.Volume+":/data" || pre.User != "0" || strings.Join(pre.Networks, ",") != "fridge_egress" {
		t.Fatalf("preload job %+v", pre)
	}
	if _, ran := rt.lastJob("restore"); ran {
		t.Fatal("a restore job ran against a Redis node: the file was already in its volume")
	}
	// the volume was filled before the service existed, so Redis loads it as it starts
	var order []string
	for _, j := range rt.jobs {
		order = append(order, j.Spec.Op)
	}
	if len(order) < 2 || order[0] != "preload" || order[1] != "exec" {
		t.Fatalf("job order %v: the preload comes first, the admin script after the node is up", order)
	}
	if d.Engine != "redis" || d.Version != "7.4" {
		t.Fatalf("restored node %+v", d)
	}
}

func TestRestoreIntoMongoMapsTheSourceDatabase(t *testing.T) {
	a, rt, objs := newTestApp(t)
	ctx := context.Background()
	key := "fridge/mongo-7.0/bob_old/bob_old-1.archive.gz"
	archive := gz("\x6d\xe2\x99\x81" + strings.Repeat("x", 100))
	dest := restoreFixture(t, a, objs, key, archive, map[string]string{"fridge-engine": "mongo", "fridge-version": "7.0", "fridge-db": "bob_old"})
	r, err := startRestore(a, ctx, RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, New: &CreateRequest{OwnerID: own(t, a), Name: "new", Domain: "new.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if got, _ := a.Store.GetRestore(ctx, r.ID); got.Status != "ok" {
		t.Fatalf("restore %+v", got)
	}
	j, ok := rt.lastJob("restore")
	if !ok || j.Spec.Engine != "mongo" || j.Spec.Params.SourceDB != "bob_old" || j.Spec.Params.Name != "root1_new" || strings.Join(j.Networks, ",") != "fridge_db,fridge_egress" {
		t.Fatalf("restore job %+v", j)
	}
}

func TestRestoreIntoMySQLPicksTheCatalogVersionOfTheDump(t *testing.T) {
	a, rt, objs := newTestApp(t)
	ctx := context.Background()
	key := "upload/dump.sql.gz"
	dump := "-- MariaDB dump 10.19  Distrib 10.11.9-MariaDB, for debian-linux-gnu (x86_64)\n-- Server version\t10.11.9-MariaDB\nCREATE TABLE t (a int);\n-- Dump completed on 2026-10-26\n"
	dest := restoreFixture(t, a, objs, key, gz(dump), map[string]string{"fridge-engine": "mariadb"})
	if _, err := startRestore(a, ctx, RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: key, New: &CreateRequest{OwnerID: own(t, a), Name: "m", Domain: "m.example.com"}}); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	dbs, _ := a.Store.ListDatabases(ctx)
	if len(dbs) != 1 || dbs[0].Engine != "mariadb" || dbs[0].Version != "10.11" {
		t.Fatalf("dbs %+v", dbs)
	}
	if j, ok := rt.lastJob("restore"); !ok || j.Spec.Params.Version != "10.11" || j.Spec.Key != key {
		t.Fatalf("restore job %+v", j)
	}
	// a file of another engine, or one that only claims to be a dump, is refused before any node is built
	n := len(rt.runs)
	objs.put("upload/pg.sql.gz", gz("-- PostgreSQL database dump\nSET x = 1;\n"), map[string]string{"fridge-engine": "mysql"})
	if _, err := startRestore(a, ctx, RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: "upload/pg.sql.gz", New: &CreateRequest{OwnerID: own(t, a), Name: "n", Domain: "n.example.com"}}); !errors.Is(err, ErrInvalid) || len(rt.runs) != n {
		t.Fatalf("a pg_dump file restored into MySQL: %v", err)
	}
}

func TestBackupOfEachEngine(t *testing.T) {
	for _, c := range []struct{ eng, ext, seg string }{
		{"redis", ".rdb", "redis-7.4"}, {"mongo", ".archive.gz", "mongo-7.0"}, {"mysql", ".sql.gz", "mysql-8.4"}, {"mariadb", ".sql.gz", "mariadb-11.4"},
	} {
		t.Run(c.eng, func(t *testing.T) {
			a, rt, _ := newTestApp(t)
			ctx := context.Background()
			rt.dumps("DUMP", "", 0)
			d, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: c.eng, Name: "shop", Domain: "shop.example.com"})
			if err != nil {
				t.Fatal(err)
			}
			dest, err := a.CreateDestination(ctx, store.Destination{Name: "s3", Endpoint: "http://localhost:9000", Bucket: "b", AccessKey: "ak"}, "sk")
			if err != nil {
				t.Fatal(err)
			}
			b, err := a.StartBackup(ctx, d.ID, dest.ID, nil, "manual", nil)
			if err != nil {
				t.Fatal(err)
			}
			a.Wait()
			if !strings.Contains(b.S3Key, "/"+c.seg+"/") || !strings.HasSuffix(b.S3Key, c.ext) {
				t.Fatalf("key %q", b.S3Key)
			}
			j, ok := rt.lastJob("dump")
			if !ok || j.Spec.Engine != c.eng || j.Spec.Params.Name != d.Name || j.Spec.Params.Host != swarm.DBService(d.NodeKey) || j.Spec.Params.TLS == nil || j.Spec.Meta["fridge-db"] != d.Name {
				t.Fatalf("dump job %+v", j)
			}
		})
	}
}

func TestDeepCheckPicksTheEngineFromTheKey(t *testing.T) {
	for _, c := range []struct{ key, eng string }{
		{"fridge/redis-7.4/a/b.rdb", "redis"}, {"fridge/mongo-7.0/a/b.archive.gz", "mongo"}, {"fridge/mysql-8.4/a/b.sql.gz", "mysql"},
		{"fridge/mariadb-11.4/a/b.sql.gz", "mariadb"}, {"fridge/postgres-17/a/b.dump", "postgres"}, {"up/x.sql", "postgres"},
	} {
		a, rt, objs := newTestApp(t)
		dest := restoreFixture(t, a, objs, c.key, []byte("data"), nil)
		rt.jobFn = func(spec job.Spec, networks []string) (string, string, int) {
			return result(map[string]any{"format": "x"}), "", 0
		}
		ck, err := a.StartDumpCheck(context.Background(), dest.ID, c.key, own(t, a))
		if err != nil {
			t.Fatalf("%s: %v", c.key, err)
		}
		waitCheck(t, a, ck.ID)
		if j, ok := rt.lastJob("check"); !ok || j.Spec.Engine != c.eng || strings.Join(j.Networks, ",") != "fridge_egress" {
			t.Errorf("%s: check job %+v", c.key, j)
		}
	}
}

func TestRotatingASuperuserOfEachEngineRunsAJobWithTheOldPassword(t *testing.T) {
	for _, eng := range []string{"redis", "mongo", "mysql", "mariadb"} {
		t.Run(eng, func(t *testing.T) {
			a, rt, _ := newTestApp(t)
			ctx := context.Background()
			d, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: eng, Name: "shop", Domain: "shop.example.com"})
			if err != nil {
				t.Fatal(err)
			}
			_, before, _ := a.RevealSuperuser(ctx, d)
			if err := a.RotateSuperuser(ctx, d); err != nil {
				t.Fatal(err)
			}
			d, _ = a.Store.GetDatabase(ctx, d.ID)
			_, after, _ := a.RevealSuperuser(ctx, d)
			x, _ := rt.lastJob("exec")
			if after == before || x.Spec.Params.AdminPassword != before || !strings.Contains(x.Spec.Script, after) || !strings.Contains(x.Spec.Script, d.AdminUser) {
				t.Fatalf("before %q after %q job %+v", before, after, x)
			}
			rt.jobFn = func(spec job.Spec, networks []string) (string, string, int) {
				return "", "ERROR near '" + spec.Script + "'", 1
			}
			err = a.RotateSuperuser(ctx, d)
			if err == nil || strings.Contains(err.Error(), after) || strings.Contains(err.Error(), "ERROR near") {
				t.Fatalf("a failed rotation leaked the script or the tool's output: %v", err)
			}
			if d2, _ := a.Store.GetDatabase(ctx, d.ID); d2.AdminPasswordEnc != d.AdminPasswordEnc {
				t.Fatal("the stored password changed although the rotation failed")
			}
		})
	}
}
