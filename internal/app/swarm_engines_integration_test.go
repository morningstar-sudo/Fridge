//go:build integration

package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"fridge/internal/engine"
	"fridge/internal/job"
	"fridge/internal/store"
	"fridge/internal/swarm"
)

// What the data path of every engine other than Postgres (TestSwarmNodeLifecycle covers it) does in a real swarm:
// create, take a backup, check the file, restore into a second node, rotate the superuser, and the one property all of
// them share: a node answers only to a client holding its own certificate.
func TestSwarmEngines(t *testing.T) {
	e := newSwarmEnv(t)
	for _, c := range []struct {
		eng, seed, query, want string
		// refused runs a client that must NOT get in: the engine's own tool, speaking TLS but with no client certificate
		refused string
	}{
		{"redis", "SET greeting hello\nSET n 3\n", "GET greeting\n", "hello",
			`redis-cli --tls --insecure -h "$HOST" --user "$SU" --no-auth-warning ping | grep -q PONG`},
		{"mongo", `db.getSiblingDB("{db}").items.insertMany([{a: 1}, {a: 2}, {a: 3}])` + "\n", `print("count=" + db.getSiblingDB("{db}").items.countDocuments())` + "\n", "count=3",
			`mongosh --quiet --host "$HOST" --tls --tlsAllowInvalidCertificates -u "$SU" -p "$PW" --authenticationDatabase admin --eval 'db.runCommand({ping:1}).ok' | grep -qx 1`},
		{"mysql", "USE `{db}`;\nCREATE TABLE items (a int);\nINSERT INTO items VALUES (1),(2),(3);\n", "SELECT COUNT(*) AS n FROM `{db}`.items;\n", "3",
			`mysql -h "$HOST" -u "$SU" --ssl-mode=REQUIRED -e 'select 1' | grep -q 1`},
		{"mariadb", "USE `{db}`;\nCREATE TABLE items (a int);\nINSERT INTO items VALUES (1),(2),(3);\n", "SELECT COUNT(*) AS n FROM `{db}`.items;\n", "3",
			`mariadb -h "$HOST" -u "$SU" --ssl --skip-ssl-verify-server-cert -e 'select 1' | grep -q 1`},
	} {
		t.Run(c.eng, func(t *testing.T) { engineLifecycle(t, e, c.eng, c.seed, c.query, c.want, c.refused) })
	}
}

func engineLifecycle(t *testing.T, e *swarmEnv, name, seed, query, want, refused string) {
	ctx := context.Background()
	a, rt := e.a, e.rt
	eng, _, _ := engine.Get(name)

	// adminJob runs op as the node's superuser (or as user/pw) on the database network, with the node's certificate.
	adminJob := func(d store.Database, tls store.Database, op, script, pw string) (map[string]any, string, error) {
		t.Helper()
		p := nodeParams(d)
		p.AdminPassword = pw
		p.Host = swarm.DBService(d.NodeKey)
		p.Port = eng.Spec(p).Port
		tl, err := a.nodeTLS(ctx, tls)
		if err != nil {
			t.Fatal(err)
		}
		p.TLS = tl
		return a.runJob(ctx, op, job.Spec{Op: op, Engine: name, Params: p, Script: script}, []string{e.dbNet}, 4*time.Minute)
	}
	su := func(d store.Database) string {
		_, pw, err := a.RevealSuperuser(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		return pw
	}
	run := func(d store.Database, script string) string {
		t.Helper()
		out, stderr, err := adminJob(d, d, "exec", strings.ReplaceAll(script, "{db}", d.Name), su(d))
		if err != nil {
			t.Fatalf("%s on %s: %v: %s", name, d.Name, err, stderr)
		}
		s, _ := out["output"].(string)
		return s
	}

	// ---- create: the node runs, the superuser and the app user exist ----
	d1, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: name, Name: name + "1", Domain: "it1." + name + ".lan"})
	if err != nil {
		t.Fatal(err)
	}
	if d1.Status != "running" {
		t.Fatalf("node: %+v", d1)
	}
	st := nodeState(t, rt, d1.NodeKey)
	if !st.DB || st.Edge != (eng.Upstream() != "none") {
		t.Fatalf("swarm state %+v (a sidecar only for engines without their own router)", st)
	}
	run(d1, seed)
	if got := run(d1, query); !strings.Contains(got, want) {
		t.Fatalf("query on the seeded node printed %q, want %q", got, want)
	}

	// ---- isolation: no certificate, no way in ----
	probe := func(host string, user, pw, cmd string) bool { // true = got in
		t.Helper()
		res, err := rt.RunJob(ctx, swarm.JobSpec{Name: "probe", Image: e.jobImg, Entrypoint: []string{"sh", "-c", cmd},
			Env: []string{"HOST=" + host, "SU=" + user, "PW=" + pw, "REDISCLI_AUTH=" + pw, "MYSQL_PWD=" + pw}, Networks: []string{e.dbNet}, Timeout: 3 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("probe %s: exit %d %q", name, res.ExitCode, strings.TrimSpace(res.Stderr))
		return res.ExitCode == 0
	}
	if probe(swarm.DBService(d1.NodeKey), d1.AdminUser, su(d1), refused) {
		t.Fatalf("a client without the node's certificate got into %s", d1.Name)
	}
	if _, _, err := adminJob(d1, d1, "ping", "", su(d1)); err != nil {
		t.Fatalf("the node's own certificate and password are refused: %v", err)
	}
	if _, _, err := adminJob(d1, d1, "ping", "", "wrong-"+su(d1)); err == nil {
		t.Fatal("a wrong password was accepted")
	}

	// ---- backup, deep check, restore into a second node ----
	bk, err := a.StartBackup(ctx, d1.ID, e.dest.ID, nil, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if bk, err = a.Store.GetBackup(ctx, bk.ID); err != nil || bk.Status != "ok" || bk.SizeBytes <= 0 {
		t.Fatalf("backup: %+v %v", bk, err)
	}
	t.Cleanup(func() { e.objs.Delete(ctx, bk.S3Key) })
	if meta, err := e.objs.Meta(ctx, bk.S3Key); err != nil || meta["fridge-engine"] != name || meta["fridge-db"] != d1.Name {
		t.Fatalf("object meta %v %v", meta, err)
	}
	chk, err := a.StartDumpCheck(ctx, e.dest.ID, bk.S3Key, own(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if chk = waitCheck(t, a, chk.ID); chk.Status != "ok" {
		t.Fatalf("deep check: %+v", chk)
	}

	r, err := startRestore(a, ctx, RestoreRequest{OwnerID: own(t, a), DestinationID: e.dest.ID, Key: bk.S3Key,
		New: &CreateRequest{OwnerID: own(t, a), Name: name + "2", Domain: "it2." + name + ".lan"}})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if r, err = a.Store.GetRestore(ctx, r.ID); err != nil || r.Status != "ok" || r.TargetDatabaseID == nil {
		t.Fatalf("restore: %+v %v", r, err)
	}
	d2, _ := a.Store.GetDatabase(ctx, *r.TargetDatabaseID)
	if d2.NodeKey == d1.NodeKey || d2.Engine != name {
		t.Fatalf("second node %+v", d2)
	}
	if got := run(d2, query); !strings.Contains(got, want) {
		t.Fatalf("the restored node printed %q, want %q", got, want)
	}

	// ---- a node does not answer to the certificate of another ----
	if _, _, err := adminJob(d1, d2, "ping", "", su(d1)); err == nil {
		t.Fatal("node 1 accepted node 2's certificate")
	}
	if _, _, err := adminJob(d2, d1, "ping", "", su(d2)); err == nil {
		t.Fatal("node 2 accepted node 1's certificate")
	}

	// ---- rotating the superuser ----
	old := su(d1)
	if err := a.RotateSuperuser(ctx, d1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := adminJob(d1, d1, "ping", "", old); err == nil {
		t.Fatal("the old superuser password still works")
	}
	d1, _ = a.Store.GetDatabase(ctx, d1.ID)
	if _, _, err := adminJob(d1, d1, "ping", "", su(d1)); err != nil {
		t.Fatalf("the new superuser password does not work: %v", err)
	}
	if got := run(d1, query); !strings.Contains(got, want) {
		t.Fatalf("after the rotation the node printed %q", got)
	}

	// ---- removal takes the services and the volume; the other node is untouched ----
	if err := rt.RemoveNode(ctx, swarm.NodeRef{Key: d2.NodeKey, HostID: d2.HostID, Volume: d2.Volume}); err != nil {
		t.Fatal(err)
	}
	ns, _ := rt.ListNodes(ctx)
	for _, n := range ns {
		if n.Key == d2.NodeKey {
			t.Fatalf("node 2 left behind: %+v", n)
		}
	}
	if st := nodeState(t, rt, d1.NodeKey); !st.DB {
		t.Fatalf("node 1 was disturbed: %+v", st)
	}
}
