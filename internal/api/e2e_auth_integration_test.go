//go:build integration

package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"fridge/internal/app"
	"fridge/internal/audit"
	"fridge/internal/auth"
	"fridge/internal/container"
	"fridge/internal/objstore"
	"fridge/internal/pki"
	"fridge/internal/secret"
	"fridge/internal/store"
	"fridge/internal/store/storetest"
)

// Real Docker + an S3 (MinIO): users, ownership, permissions, per-node users and network isolation through the HTTP API.
//
//	Run: bash scripts/test-db.sh && FRIDGE_IT_S3_ENDPOINT=http://localhost:19000 FRIDGE_IT_S3_ACCESS=minioadmin \
//	  FRIDGE_IT_S3_SECRET=minioadmin FRIDGE_IT_S3_BUCKET=fridge go test -tags integration -timeout 30m ./internal/api/ -run E2E -v
func TestE2EOwnershipFlow(t *testing.T) {
	ep, ak, sk, bucket := os.Getenv("FRIDGE_IT_S3_ENDPOINT"), os.Getenv("FRIDGE_IT_S3_ACCESS"), os.Getenv("FRIDGE_IT_S3_SECRET"), os.Getenv("FRIDGE_IT_S3_BUCKET")
	if ep == "" || ak == "" || sk == "" || bucket == "" {
		t.Skip("FRIDGE_IT_S3_* not set")
	}
	ctx := context.Background()
	const network, proxyName = "fridge-it-api-net", "fridge-it-api-traefik"

	rt, err := container.New(network)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exec.Command("docker", "network", "rm", network).Run() })
	_ = rt.Remove(ctx, proxyName, proxyName)
	if _, err := rt.Run(ctx, container.RunSpec{Name: proxyName, Image: "postgres:17-alpine", Env: []string{"POSTGRES_PASSWORD=x"}, Volume: proxyName, VolumePath: "/var/lib/postgresql/data"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Remove(ctx, proxyName, proxyName) })

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
	a := &app.App{Store: st, Box: box, RT: rt, CA: ca, TraefikDir: t.TempDir(), HealthTimeout: 120 * time.Second,
		Alert:          func(ctx context.Context, text string) { t.Logf("alert: %s", text) },
		ControlNetwork: network, TraefikContainer: proxyName, Audit: &audit.Logger{Store: st},
		WebCert: "/traefik/certs/web.crt", WebKey: "/traefik/certs/web.key"}
	au, err := auth.New(st, key, "setup-tok")
	if err != nil {
		t.Fatal(err)
	}
	e := &tenants{h: New(a, au, Options{}), a: a}

	// Nodes made here are removed even when the test fails half way.
	t.Cleanup(func() {
		dbs, _ := st.ListDatabases(ctx)
		for _, d := range dbs {
			_ = rt.Remove(ctx, d.ContainerID, d.Volume)
			_ = rt.Remove(ctx, d.Volume, d.Volume)
			_ = rt.RemoveNetwork(ctx, app.NodeNetwork(d))
		}
	})

	psql := func(d store.Database, sql string) (out, errs string, code int) {
		t.Helper()
		var o, eb bytes.Buffer
		code, err := rt.Exec(ctx, d.ContainerID, []string{"psql", "-U", d.User, "-d", d.Name, "-v", "ON_ERROR_STOP=1", "-tAc", sql}, nil, &o, &eb)
		if err != nil {
			t.Fatalf("psql %q on %s: %v", sql, d.Name, err)
		}
		return strings.TrimSpace(o.String()), eb.String(), code
	}
	must := func(d store.Database, sql string) string {
		t.Helper()
		out, errs, code := psql(d, sql)
		if code != 0 {
			t.Fatalf("psql %q on %s: exit %d: %s", sql, d.Name, code, errs)
		}
		return out
	}
	dockerCount := func() int {
		out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=fridge.db_id").Output()
		if err != nil {
			t.Fatal(err)
		}
		return len(strings.Fields(string(out)))
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

	// MinIO sits on loopback, which users may not point a destination at: the admin exempts exactly that host.
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
	before := dockerCount()
	junk := jb(map[string]any{"destination_id": mustAtoi(t, dAlice), "key": "upload/junk.dump", "new": map[string]string{"name": "junk", "domain": "copy.alice.lan"}})
	if w := e.want(t, e.alice, "POST", "/api/restores", junk, 400); !strings.Contains(w.Body.String(), "pg_dump") {
		t.Fatalf("junk refusal does not say why: %s", w.Body.String())
	}
	if after := dockerCount(); after != before {
		t.Fatalf("a refused restore changed the container count %d -> %d", before, after)
	}

	// ---- the app user is no superuser and cannot run programs on the server ----
	if got := must(aliceApp, "select rolsuper from pg_roles where rolname = current_user"); got != "f" {
		t.Fatalf("app user rolsuper = %q", got)
	}
	must(aliceApp, "create table mine(a int)")
	if _, errs, code := psql(aliceApp, "copy (select 1) to program 'id'"); code == 0 || !strings.Contains(errs, "permission denied") && !strings.Contains(errs, "must be superuser") {
		t.Fatalf("COPY TO PROGRAM as the app user: exit %d %q", code, errs)
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

	// ---- network isolation ----
	for _, d := range []store.Database{aliceShop, bobShop} {
		nets, err := rt.ContainerNetworks(ctx, d.Volume)
		if err != nil || len(nets) != 1 || nets[0] != app.NodeNetwork(d) {
			t.Fatalf("%s networks = %q, %v (want only %s)", d.Name, nets, err, app.NodeNetwork(d))
		}
	}
	if app.NodeNetwork(aliceShop) == app.NodeNetwork(bobShop) {
		t.Fatal("two nodes share a network")
	}
	if code, err := rt.Exec(ctx, aliceShop.ContainerID, []string{"pg_isready", "-h", bobShop.Volume, "-t", "3"}, nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil || code == 0 {
		t.Fatalf("alice_shop reached bob_shop: code %d err %v", code, err)
	}
	if code, err := rt.Exec(ctx, aliceShop.ContainerID, []string{"pg_isready", "-h", proxyName, "-t", "3"}, nil, &bytes.Buffer{}, &bytes.Buffer{}); err != nil || code != 0 {
		t.Fatalf("the proxy cannot reach alice_shop's network: code %d err %v", code, err)
	}
}
