//go:build integration

package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"fridge/internal/app"
	"fridge/internal/audit"
	"fridge/internal/auth"
	"fridge/internal/engine"
	"fridge/internal/objstore"
	"fridge/internal/pki"
	"fridge/internal/secret"
	"fridge/internal/store"
	"fridge/internal/store/storetest"
	"fridge/internal/swarm"
)

// Real swarm + an S3 (MinIO): users, ownership, permissions, per-node users and network isolation through the HTTP API.
// It runs inside the test swarm (scripts/swarm-test/gotest.sh), which provides the Docker socket, the overlays,
// the metadata Postgres and the S3.
func TestE2EOwnershipFlow(t *testing.T) {
	serverImg, jobImg := os.Getenv("FRIDGE_SERVER_IMAGE"), os.Getenv("FRIDGE_JOB_IMAGE")
	ep, ak, sk, bucket := os.Getenv("FRIDGE_IT_S3_ENDPOINT"), os.Getenv("FRIDGE_IT_S3_ACCESS"), os.Getenv("FRIDGE_IT_S3_SECRET"), os.Getenv("FRIDGE_IT_S3_BUCKET")
	if serverImg == "" || jobImg == "" || ep == "" || ak == "" || sk == "" || bucket == "" {
		t.Skip("FRIDGE_SERVER_IMAGE, FRIDGE_JOB_IMAGE and FRIDGE_IT_S3_* are not set")
	}
	ctx := context.Background()
	egress := "fridge_egress"
	rt, err := swarm.New(swarm.Config{EdgeNetwork: "fridge_edge", EgressNetwork: egress, ServerImage: serverImg})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if err := rt.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rt.EnsureNetworks(ctx); err != nil {
		t.Fatal(err)
	}

	key := make([]byte, 32)
	rand.Read(key)
	box, err := secret.NewBox(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := pki.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := storetest.New(t)
	a := &app.App{Store: st, Box: box, RT: rt, CA: ca, HealthTimeout: 4 * time.Minute, JobImage: jobImg, EgressNetwork: egress,
		Alert: func(ctx context.Context, text string) { t.Logf("alert: %s", text) }, Audit: &audit.Logger{Store: st}}
	au, err := auth.New(st, key, "setup-tok")
	if err != nil {
		t.Fatal(err)
	}
	e := &tenants{h: New(a, au, Options{}), a: a}

	// Nodes made here are removed even when the test fails half way.
	t.Cleanup(func() {
		dbs, _ := st.ListDatabases(ctx)
		for _, d := range dbs {
			if d.NodeKey != "" {
				_ = rt.RemoveNode(ctx, swarm.NodeRef{Key: d.NodeKey, HostID: d.HostID, Volume: d.Volume})
			}
		}
	})

	// sql runs a statement as the app user of node d, through its edge sidecar (what a client behind Traefik gets).
	sql := func(d store.Database, query string) (string, error) {
		t.Helper()
		pw, err := box.Decrypt(d.PasswordEnc)
		if err != nil {
			t.Fatal(err)
		}
		eng, _, _ := engine.Get(d.Engine)
		p := engine.Params{Name: d.Name, User: d.User, Version: d.Version, AdminUser: d.User, AdminPassword: pw}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(cctx, eng.AdminDSN(p, swarm.EdgeService(d.NodeKey), swarm.EdgePort))
		if err != nil {
			t.Fatalf("connect to %s: %v", d.Name, err)
		}
		defer c.Close(cctx)
		var out string
		rows, err := c.Query(cctx, query, pgx.QueryExecModeSimpleProtocol)
		if err != nil {
			return "", err
		}
		defer rows.Close()
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				return "", err
			}
			if len(vals) > 0 {
				out = strings.TrimSpace(strings.Join([]string{out, toString(vals[0])}, ""))
			}
		}
		return out, rows.Err()
	}
	must := func(d store.Database, query string) string {
		t.Helper()
		out, err := sql(d, query)
		if err != nil {
			t.Fatalf("%q on %s: %v", query, d.Name, err)
		}
		return out
	}
	nodeCount := func() int {
		ns, err := rt.ListNodes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(ns)
	}
	view := func(sid, db string) dbView {
		var v dbView
		json.Unmarshal(e.want(t, sid, "GET", "/api/databases/"+db, "", 200).Body.Bytes(), &v)
		return v
	}

	// ---- accounts ----
	e.want(t, "", "POST", "/api/setup", jb(map[string]any{"setup_token": "setup-tok", "username": "root1", "credentials": credsFor(pwRoot).c}), 201)
	_, e.root = loginAs(t, e.h, "root1", pwRoot)
	inviteUser(t, e.h, e.root, "alice", "user", pwAlice)
	inviteUser(t, e.h, e.root, "bob", "user", pwBob)
	_, e.alice = loginAs(t, e.h, "alice", pwAlice)
	_, e.bob = loginAs(t, e.h, "bob", pwBob)

	// The S3 sits on a Fridge network, which users may not point a destination at: the admin exempts exactly that host.
	u, err := url.Parse(ep)
	if err != nil {
		t.Fatal(err)
	}
	e.want(t, e.alice, "POST", "/api/destinations", jb(map[string]any{"name": "blocked", "endpoint": ep, "bucket": bucket, "access_key": ak, "secret_key": sk, "path_style": true}), 400)
	e.want(t, e.root, "PUT", "/api/settings/endpoint-allowlist", jb(map[string]any{"hosts": []string{u.Hostname()}}), 200)
	destBody := func(name string) string {
		return jb(map[string]any{"name": name, "endpoint": ep, "bucket": bucket, "access_key": ak, "secret_key": sk, "path_style": true})
	}

	// ---- alice: her own destination, domains, a database, a backup ----
	dAlice := id(t, e.want(t, e.alice, "POST", "/api/destinations", destBody("alice-s3"), 201))
	for _, d := range []string{"alice.lan", "shop.alice.lan", "copy.alice.lan"} {
		e.want(t, e.alice, "POST", "/api/domains", jb(map[string]string{"name": d}), 201)
	}
	e.want(t, e.alice, "POST", "/api/databases", jb(map[string]string{"engine": "postgres", "name": "app", "domain": "alice.lan", "version": "16", "db_password": e.alicePW}), 201)
	aliceApp := mustDB(t, e, "alice_app")
	must(aliceApp, "create table t(x int); insert into t values (1),(2),(3)")
	var bk struct {
		S3Key string `json:"s3_key"`
	}
	json.Unmarshal(e.want(t, e.alice, "POST", "/api/databases/alice_app/backups", jb(map[string]any{"destination_id": mustAtoi(t, dAlice)}), 202).Body.Bytes(), &bk)
	a.Wait()
	if v := e.list(t, e.alice)["alice_app"]; v.LastBackup == nil || v.LastBackup.Status != "ok" {
		t.Fatalf("alice's backup: %+v", v.LastBackup)
	}
	t.Cleanup(func() {
		if o, err := objstore.New(store.Destination{Endpoint: ep, Bucket: bucket, AccessKey: ak, PathStyle: true}, sk, nil); err == nil {
			o.Delete(ctx, bk.S3Key)
			o.Delete(ctx, "upload/junk.dump")
		}
	})

	// ---- bob sees nothing of hers; as a member he can back up but not add people ----
	e.want(t, e.bob, "GET", "/api/databases/alice_app", "", 404)
	e.want(t, e.bob, "PUT", "/api/destinations/"+dAlice, destBody("x"), 404)
	if _, ok := e.list(t, e.bob)["alice_app"]; ok {
		t.Fatal("bob lists alice_app")
	}
	dS := id(t, e.want(t, e.root, "POST", "/api/destinations", destBody("shared-s3"), 201))
	e.want(t, e.root, "PATCH", "/api/destinations/"+dS, `{"shared":true}`, 204)
	e.want(t, e.alice, "POST", "/api/databases/alice_app/members", `{"username":"bob"}`, 204)
	time.Sleep(1100 * time.Millisecond) // object keys carry the second: a backup in the same second as the first would collide
	e.want(t, e.bob, "POST", "/api/databases/alice_app/backups", jb(map[string]any{"destination_id": mustAtoi(t, dS)}), 202)
	a.Wait()
	e.want(t, e.bob, "POST", "/api/databases/alice_app/members", `{"username":"alice"}`, 403)

	// ---- the same suffix for two owners, running side by side ----
	e.want(t, e.alice, "POST", "/api/databases", jb(map[string]string{"engine": "postgres", "name": "shop", "domain": "shop.alice.lan", "version": "16"}), 201)
	e.want(t, e.bob, "POST", "/api/domains", `{"name":"bob.lan"}`, 201)
	e.want(t, e.bob, "POST", "/api/databases", jb(map[string]string{"engine": "postgres", "name": "shop", "domain": "bob.lan", "version": "16"}), 201)
	aliceShop, bobShop := mustDB(t, e, "alice_shop"), mustDB(t, e, "bob_shop")
	if aliceShop.Status != "running" || bobShop.Status != "running" {
		t.Fatalf("shops: %s %s", aliceShop.Status, bobShop.Status)
	}

	// ---- schedules: shared destination yes, someone else's private one no ----
	sched := func(dest string) string {
		return jb(map[string]any{"destination_id": mustAtoi(t, dest), "cron": "0 3 * * *", "keep_latest": 3, "enabled": true})
	}
	e.want(t, e.bob, "POST", "/api/databases/bob_shop/schedules", sched(dS), 201)
	e.want(t, e.bob, "POST", "/api/databases/bob_shop/schedules", sched(dAlice), 400)

	// ---- restore: new node owned by alice, objects owned by its app user; bob cannot see the restore ----
	var rs struct {
		ID int64 `json:"id"`
	}
	body := jb(map[string]any{"destination_id": mustAtoi(t, dAlice), "key": bk.S3Key, "new": map[string]string{"name": "copy", "domain": "copy.alice.lan", "version": "17"}})
	json.Unmarshal(e.want(t, e.alice, "POST", "/api/restores", body, 202).Body.Bytes(), &rs)
	a.Wait()
	r, err := st.GetRestore(ctx, rs.ID)
	if err != nil || r.Status != "ok" || r.TargetDatabaseID == nil {
		t.Fatalf("restore: %+v %v", r, err)
	}
	copyDB, err := st.GetDatabase(ctx, *r.TargetDatabaseID)
	if err != nil || copyDB.Name != "alice_copy" || copyDB.Version != "17" {
		t.Fatalf("restored node: %+v %v", copyDB, err)
	}
	if v := view(e.alice, "alice_copy"); v.Owner != "alice" {
		t.Fatalf("owner of the restored node: %q", v.Owner)
	}
	if got := must(copyDB, "select count(*) from t"); got != "3" {
		t.Fatalf("rows after restore: %q", got)
	}
	if got := must(copyDB, "select tableowner from pg_tables where tablename='t'"); got != copyDB.User {
		t.Fatalf("restored table owner %q, want the app user %q", got, copyDB.User)
	}
	e.want(t, e.bob, "GET", "/api/restores/"+itoa(rs.ID), "", 404)

	// ---- a junk file is refused before anything is built ----
	o, err := objstore.New(store.Destination{Endpoint: ep, Bucket: bucket, AccessKey: ak, PathStyle: true}, sk, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Put(ctx, "upload/junk.dump", strings.NewReader("this is not a dump"), nil); err != nil {
		t.Fatal(err)
	}
	before := nodeCount()
	junk := jb(map[string]any{"destination_id": mustAtoi(t, dAlice), "key": "upload/junk.dump", "new": map[string]string{"name": "junk", "domain": "copy.alice.lan"}})
	if w := e.want(t, e.alice, "POST", "/api/restores", junk, 400); !strings.Contains(w.Body.String(), "pg_dump") {
		t.Fatalf("junk refusal does not say why: %s", w.Body.String())
	}
	if after := nodeCount(); after != before {
		t.Fatalf("a refused restore changed the node count %d -> %d", before, after)
	}

	// ---- the app user is no superuser and cannot run programs on the server ----
	if got := must(aliceApp, "select rolsuper::text from pg_roles where rolname = current_user"); got != "false" {
		t.Fatalf("app user rolsuper = %q", got)
	}
	must(aliceApp, "create table mine(a int)")
	if _, err := sql(aliceApp, "copy (select 1) to program 'id'"); err == nil || !strings.Contains(err.Error(), "permission denied") && !strings.Contains(err.Error(), "must be superuser") {
		t.Fatalf("COPY TO PROGRAM as the app user: %v", err)
	}

	// ---- one superuser per node; rotating one leaves the other alone ----
	appV, shopV := view(e.alice, "alice_app"), view(e.alice, "alice_shop")
	if !strings.HasPrefix(appV.AdminUser, "su_") || !strings.HasPrefix(shopV.AdminUser, "su_") || appV.AdminUser == shopV.AdminUser {
		t.Fatalf("superusers %q %q", appV.AdminUser, shopV.AdminUser)
	}
	reveal := func(db string) string {
		var r struct{ Password string }
		json.Unmarshal(e.want(t, e.alice, "POST", "/api/databases/"+db+"/superuser/reveal", "", 200).Body.Bytes(), &r)
		return r.Password
	}
	appPW, shopPW := reveal("alice_app"), reveal("alice_shop")
	if appPW == "" || appPW == shopPW {
		t.Fatal("superuser passwords empty or equal")
	}
	e.want(t, e.alice, "POST", "/api/databases/alice_shop/superuser/rotate", "", 204)
	if reveal("alice_shop") == shopPW || reveal("alice_app") != appPW {
		t.Fatal("rotate changed the wrong superuser")
	}
	e.want(t, e.bob, "POST", "/api/databases/alice_shop/superuser/reveal", "", 404)

	// ---- network isolation: each node has its own overlay; a job on one cannot see the other ----
	if swarm.Network(aliceShop.NodeKey) == swarm.Network(bobShop.NodeKey) {
		t.Fatal("two nodes share a network")
	}
	ready := func(from string, target string) int {
		res, err := rt.RunJob(ctx, swarm.JobSpec{Name: "isready", Image: jobImg, Entrypoint: []string{"/usr/lib/postgresql/17/bin/pg_isready"},
			Args: []string{"-h", target, "-U", "x", "-t", "3"}, Networks: []string{from}, Timeout: 2 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		return res.ExitCode
	}
	if ready(swarm.Network(aliceShop.NodeKey), swarm.DBService(bobShop.NodeKey)) == 0 {
		t.Fatal("alice_shop's network reached bob_shop")
	}
	if ready(swarm.Network(aliceShop.NodeKey), swarm.DBService(aliceShop.NodeKey)) != 0 {
		t.Fatal("alice_shop's own network does not reach it")
	}
	if ready(swarm.Network(aliceShop.NodeKey), swarm.EdgeService(bobShop.NodeKey)) == 0 {
		t.Fatal("alice_shop's network reached bob_shop's sidecar")
	}
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	default:
		b, _ := json.Marshal(x)
		return strings.Trim(string(b), `"`)
	}
}
