package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"fridge/internal/app"
	"fridge/internal/store"
)

const pwRoot = "root password 1234"

// tenants: admin root1, users alice and bob; alice owns alice_app on her domain alice.lan.
type tenants struct {
	h                http.Handler
	a                *app.App
	root, alice, bob string // session ids
	aliceDB, alicePW string // alice_app and its app password
}

func newTenants(t *testing.T) *tenants {
	t.Helper()
	h, _, a := newRawSrv(t, "setup-tok", Options{})
	if w := req(h, "POST", "/api/setup", jb(map[string]any{"setup_token": "setup-tok", "username": "root1", "credentials": credsFor(pwRoot).c})); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	e := &tenants{h: h, a: a, aliceDB: "alice_app", alicePW: "Alice-app-pw.123"}
	_, e.root = loginAs(t, h, "root1", pwRoot)
	inviteUser(t, h, e.root, "alice", "user", pwAlice)
	inviteUser(t, h, e.root, "bob", "user", pwBob)
	_, e.alice = loginAs(t, h, "alice", pwAlice)
	_, e.bob = loginAs(t, h, "bob", pwBob)
	e.want(t, e.alice, "POST", "/api/domains", `{"name":"alice.lan"}`, 201)
	e.want(t, e.alice, "POST", "/api/databases", jb(map[string]string{"engine": "postgres", "name": "app", "domain": "alice.lan", "db_password": e.alicePW}), 201)
	return e
}

func (e *tenants) do(sid, method, path, body string) *httptest.ResponseRecorder {
	return req(e.h, method, path, body, bearer(sid)...)
}

func (e *tenants) want(t *testing.T, sid, method, path, body string, code int) *httptest.ResponseRecorder {
	t.Helper()
	w := e.do(sid, method, path, body)
	if w.Code != code {
		t.Fatalf("%s %s: want %d, got %d %s", method, path, code, w.Code, w.Body.String())
	}
	return w
}

// id decodes the "id" of a JSON object response.
func id(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var v struct{ ID int64 }
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || v.ID == 0 {
		t.Fatal(w.Body.String())
	}
	return strconv.FormatInt(v.ID, 10)
}

// dest creates a destination owned by sid's user; an admin may share it.
func (e *tenants) dest(t *testing.T, sid, name string, shared bool) string {
	t.Helper()
	d := id(t, e.want(t, sid, "POST", "/api/destinations", jb(map[string]string{"name": name, "endpoint": "https://s3.example.com", "bucket": "bk-" + name, "access_key": "AK-" + name, "secret_key": "x"}), 201))
	if shared {
		e.want(t, e.root, "PATCH", "/api/destinations/"+d, `{"shared":true}`, 204)
	}
	return d
}

type dbView struct {
	Name, Owner      string
	ConnectionString string `json:"connection_string"`
	AdminUser        string `json:"admin_user"`
	LastBackup       *struct {
		Status     string
		FinishedAt *string `json:"finished_at"`
	} `json:"last_backup"`
}

func (e *tenants) list(t *testing.T, sid string) map[string]dbView {
	t.Helper()
	var v []dbView
	json.Unmarshal(e.want(t, sid, "GET", "/api/databases", "", 200).Body.Bytes(), &v)
	out := map[string]dbView{}
	for _, d := range v {
		out[d.Name] = d
	}
	return out
}

func TestDBVisibility(t *testing.T) {
	e := newTenants(t)
	if _, ok := e.list(t, e.bob)["alice_app"]; ok {
		t.Fatal("bob sees alice_app")
	}
	e.want(t, e.bob, "GET", "/api/databases/alice_app", "", 404)
	if d, ok := e.list(t, e.root)["alice_app"]; !ok || d.Owner != "alice" {
		t.Fatalf("admin view %+v", d)
	}
	if d := e.list(t, e.alice)["alice_app"]; d.Owner != "alice" {
		t.Fatalf("owner view %+v", d)
	}
}

func TestDBListLastBackup(t *testing.T) {
	e := newTenants(t)
	e.want(t, e.alice, "POST", "/api/domains", `{"name":"two.alice.lan"}`, 201)
	e.want(t, e.alice, "POST", "/api/databases", `{"engine":"postgres","name":"two","domain":"two.alice.lan"}`, 201)
	dA := e.dest(t, e.alice, "da", false)
	e.want(t, e.alice, "POST", "/api/databases/alice_app/backups", `{"destination_id":`+dA+`}`, 202)
	e.a.Wait()
	l := e.list(t, e.alice)
	if lb := l["alice_app"].LastBackup; lb == nil || lb.Status != "ok" || lb.FinishedAt == nil {
		t.Fatalf("%+v", l["alice_app"])
	}
	if l["alice_two"].LastBackup != nil {
		t.Fatalf("%+v", l["alice_two"])
	}
	if !strings.Contains(e.want(t, e.alice, "GET", "/api/databases", "", 200).Body.String(), `"last_backup":null`) {
		t.Fatal("last_backup must be null, not absent")
	}
}

func TestConnectionMasked(t *testing.T) {
	e := newTenants(t)
	for _, b := range []string{
		e.want(t, e.alice, "GET", "/api/databases/alice_app", "", 200).Body.String(),
		e.want(t, e.alice, "GET", "/api/databases", "", 200).Body.String(),
	} {
		if !strings.Contains(b, "postgresql://alice_app:****@alice.lan") || strings.Contains(b, e.alicePW) || strings.Contains(b, "db_password") {
			t.Fatal(b)
		}
	}
	var c struct {
		ConnectionString string `json:"connection_string"`
	}
	json.Unmarshal(e.want(t, e.alice, "GET", "/api/databases/alice_app/connection", "", 200).Body.Bytes(), &c)
	if !strings.Contains(c.ConnectionString, "alice_app:"+e.alicePW+"@") {
		t.Fatal(c)
	}
	e.want(t, e.bob, "GET", "/api/databases/alice_app/connection", "", 404)
	e.want(t, e.alice, "GET", "/api/databases/alice_app/connection?role=nope", "", 400)
}

func TestDomainsAPI(t *testing.T) {
	e := newTenants(t)
	w := e.want(t, e.bob, "POST", "/api/domains", `{"name":"alice.lan"}`, 409)
	if strings.TrimSpace(w.Body.String()) != `{"error":"domain không đăng ký được"}` {
		t.Fatal(w.Body.String())
	}
	e.want(t, e.bob, "POST", "/api/domains", `{"name":"not a host"}`, 400)
	bobDom := id(t, e.want(t, e.bob, "POST", "/api/domains", `{"name":"Bob.LAN"}`, 201))
	if b := e.want(t, e.bob, "GET", "/api/domains", "", 200).Body.String(); strings.Contains(b, "alice.lan") || !strings.Contains(b, `"bob.lan"`) {
		t.Fatal(b)
	}
	e.want(t, e.alice, "POST", "/api/databases", `{"engine":"postgres","name":"x","domain":"bob.lan"}`, 400)

	var ds []domainJSON
	json.Unmarshal(e.want(t, e.alice, "GET", "/api/domains", "", 200).Body.Bytes(), &ds)
	if len(ds) != 1 || ds[0].Name != "alice.lan" || ds[0].InUse != 1 {
		t.Fatal(ds)
	}
	aliceDom := strconv.FormatInt(ds[0].ID, 10)
	e.want(t, e.alice, "DELETE", "/api/domains/"+aliceDom, "", 409)
	e.want(t, e.alice, "DELETE", "/api/domains/"+bobDom, "", 404)
	if b := e.want(t, e.root, "GET", "/api/domains", "", 200).Body.String(); !strings.Contains(b, `"owner":"alice"`) || !strings.Contains(b, `"owner":"bob"`) {
		t.Fatal(b)
	}
	e.want(t, e.bob, "DELETE", "/api/domains/"+bobDom, "", 204)
}

func TestMembersAPI(t *testing.T) {
	e := newTenants(t)
	e.want(t, e.alice, "POST", "/api/databases/alice_app/members", `{"username":"bob"}`, 204)
	d, ok := e.list(t, e.bob)["alice_app"]
	if !ok || !strings.Contains(d.ConnectionString, ":****@") {
		t.Fatalf("%+v", d)
	}
	e.want(t, e.bob, "GET", "/api/databases/alice_app", "", 200)
	if b := e.want(t, e.bob, "GET", "/api/databases/alice_app/members", "", 200).Body.String(); !strings.Contains(b, `"username":"bob"`) {
		t.Fatal(b)
	}
	e.want(t, e.bob, "POST", "/api/databases/alice_app/members", `{"username":"carol"}`, 403)
	bob, _ := e.a.Store.GetUserByName(context.Background(), "bob")
	bobID := strconv.FormatInt(bob.ID, 10)
	e.want(t, e.bob, "DELETE", "/api/databases/alice_app/members/"+bobID, "", 403)
	for _, n := range []string{"nobody", "bob", "alice"} {
		w := e.want(t, e.alice, "POST", "/api/databases/alice_app/members", jb(map[string]string{"username": n}), 400)
		if strings.TrimSpace(w.Body.String()) != `{"error":"không thêm được"}` {
			t.Fatal(n, w.Body.String())
		}
	}
	e.want(t, e.alice, "DELETE", "/api/databases/alice_app/members/"+bobID, "", 204)
	e.want(t, e.bob, "GET", "/api/databases/alice_app", "", 404)
}

func TestDestinationOwnership(t *testing.T) {
	e := newTenants(t)
	dA := e.dest(t, e.alice, "da", false)
	if b := e.want(t, e.bob, "GET", "/api/destinations", "", 200).Body.String(); strings.Contains(b, `"da"`) {
		t.Fatal(b)
	}
	put := `{"name":"x","endpoint":"https://s3.example.com","bucket":"b"}`
	e.want(t, e.bob, "PUT", "/api/destinations/"+dA, put, 404)
	e.want(t, e.bob, "DELETE", "/api/destinations/"+dA, "", 404)
	e.want(t, e.bob, "POST", "/api/destinations/"+dA+"/test", "", 404)
	e.want(t, e.bob, "GET", "/api/destinations/"+dA+"/objects", "", 404)
	e.want(t, e.alice, "PATCH", "/api/destinations/"+dA, `{"shared":true}`, 403)
	if w := e.want(t, e.root, "PATCH", "/api/destinations/"+dA, `{"shared":true}`, 400); !strings.Contains(w.Body.String(), "only admin destinations can be shared") {
		t.Fatal(w.Body.String())
	}
	e.want(t, e.root, "PATCH", "/api/destinations/"+dA, `{"shared":false}`, 204)

	dAdmin := e.dest(t, e.root, "dadmin", true)
	var ds []map[string]any
	json.Unmarshal(e.want(t, e.bob, "GET", "/api/destinations", "", 200).Body.Bytes(), &ds)
	if len(ds) != 1 || ds[0]["name"] != "dadmin" || ds[0]["shared"] != true || ds[0]["owner"] != "root1" {
		t.Fatal(ds)
	}
	for _, k := range []string{"endpoint", "bucket", "access_key"} {
		if _, ok := ds[0][k]; ok {
			t.Fatal("shared destination leaks", k, ds)
		}
	}
	e.want(t, e.bob, "PUT", "/api/destinations/"+dAdmin, put, 403)
	e.want(t, e.bob, "DELETE", "/api/destinations/"+dAdmin, "", 403)
	e.want(t, e.bob, "POST", "/api/destinations/"+dAdmin+"/test", "", 403)
	if b := e.want(t, e.root, "GET", "/api/destinations", "", 200).Body.String(); !strings.Contains(b, `"bucket":"bk-da"`) || !strings.Contains(b, `"owner":"alice"`) {
		t.Fatal(b)
	}
	e.want(t, e.alice, "PUT", "/api/destinations/"+dA, put, 200)
}

func TestScheduleDestinationRules(t *testing.T) {
	e := newTenants(t)
	dA := e.dest(t, e.alice, "da", false)
	dS := e.dest(t, e.root, "ds", true)
	e.want(t, e.bob, "POST", "/api/domains", `{"name":"bob.lan"}`, 201)
	e.want(t, e.bob, "POST", "/api/databases", `{"engine":"postgres","name":"x","domain":"bob.lan"}`, 201)
	sched := func(dest string) string {
		return `{"destination_id":` + dest + `,"cron":"0 3 * * *","keep_latest":3,"enabled":true}`
	}
	e.want(t, e.bob, "POST", "/api/databases/bob_x/schedules", sched(dA), 400)
	e.want(t, e.bob, "POST", "/api/databases/bob_x/schedules", sched("999"), 400)
	sc := id(t, e.want(t, e.bob, "POST", "/api/databases/bob_x/schedules", sched(dS), 201))
	e.want(t, e.bob, "PUT", "/api/schedules/"+sc, sched(dA), 400)
	e.want(t, e.bob, "PUT", "/api/schedules/"+sc, sched(dS), 200)

	// a member cannot route the owner's data into a bucket only the member controls
	dB := e.dest(t, e.bob, "db", false)
	e.want(t, e.alice, "POST", "/api/databases/alice_app/members", `{"username":"bob"}`, 204)
	e.want(t, e.bob, "POST", "/api/databases/alice_app/schedules", sched(dB), 400)
	msc := id(t, e.want(t, e.bob, "POST", "/api/databases/alice_app/schedules", sched(dS), 201))
	e.want(t, e.bob, "PUT", "/api/schedules/"+msc, sched(dB), 400)
	e.want(t, e.bob, "POST", "/api/databases/alice_app/backups", `{"destination_id":`+dB+`}`, 400)
	// nor can the owner pick the member's bucket
	e.want(t, e.alice, "POST", "/api/databases/alice_app/schedules", sched(dB), 400)
}

func TestBackupFallbackSkipsForeignSchedule(t *testing.T) {
	e := newTenants(t)
	ctx := context.Background()
	dB := e.dest(t, e.bob, "db", false)
	d, err := e.a.Store.GetDatabaseByName(ctx, "alice_app")
	if err != nil {
		t.Fatal(err)
	}
	// a schedule that predates the rules: alice_app -> bob's private bucket
	if err := e.a.Store.CreateSchedule(ctx, &store.Schedule{DatabaseID: d.ID, DestinationID: mustInt(t, dB), Cron: "0 1 * * *", Prefix: "fridge", KeepLatest: 1, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	e.want(t, e.alice, "POST", "/api/databases/alice_app/backups", `{}`, 400) // nothing usable: refused, not sent to bob
	dS := e.dest(t, e.root, "ds", true)
	var b struct {
		DestinationID int64 `json:"destination_id"`
	}
	json.Unmarshal(e.want(t, e.alice, "POST", "/api/databases/alice_app/backups", `{}`, 202).Body.Bytes(), &b)
	e.a.Wait()
	if strconv.FormatInt(b.DestinationID, 10) != dS {
		t.Fatalf("backup went to %d, want %s", b.DestinationID, dS)
	}
}

func TestIndirectAccessBlocked(t *testing.T) {
	e := newTenants(t)
	dA := e.dest(t, e.alice, "da", false)
	dS := e.dest(t, e.root, "ds", true)
	bk := e.want(t, e.alice, "POST", "/api/databases/alice_app/backups", `{"destination_id":`+dS+`}`, 202)
	e.a.Wait()
	var b struct {
		ID    int64
		S3Key string `json:"s3_key"`
	}
	json.Unmarshal(bk.Body.Bytes(), &b)
	bid := strconv.FormatInt(b.ID, 10)
	restore := jb(map[string]any{"destination_id": mustInt(t, dS), "key": b.S3Key, "new": map[string]string{"name": "copy", "domain": "bob.lan"}})
	e.want(t, e.bob, "POST", "/api/restores", restore, 404)
	e.want(t, e.bob, "GET", "/api/backups/"+bid, "", 404)
	e.want(t, e.bob, "GET", "/api/backups/"+bid+"/download", "", 404)
	e.want(t, e.alice, "GET", "/api/backups/"+bid+"/download", "", 200)
	if w := e.want(t, e.bob, "GET", "/api/destinations/"+dS+"/objects", "", 200); strings.Contains(w.Body.String(), b.S3Key) {
		t.Fatal(w.Body.String())
	}
	if w := e.want(t, e.alice, "GET", "/api/destinations/"+dS+"/objects", "", 200); !strings.Contains(w.Body.String(), `"database":"alice_app"`) {
		t.Fatal(w.Body.String())
	}
	e.want(t, e.bob, "POST", "/api/databases/alice_app/backups", `{}`, 404)
	e.want(t, e.bob, "GET", "/api/databases/alice_app/backups", "", 404)
	e.want(t, e.bob, "GET", "/api/databases/alice_app/schedules", "", 404)

	sc := id(t, e.want(t, e.alice, "POST", "/api/databases/alice_app/schedules", `{"destination_id":`+dS+`,"cron":"0 3 * * *","keep_latest":3}`, 201))
	e.want(t, e.bob, "PUT", "/api/schedules/"+sc, `{"destination_id":`+dS+`,"cron":"0 4 * * *","keep_latest":1}`, 404)
	e.want(t, e.bob, "DELETE", "/api/schedules/"+sc, "", 404)
	if got, err := e.a.Store.GetSchedule(context.Background(), mustInt(t, sc)); err != nil || got.Cron != "0 3 * * *" || got.KeepLatest != 3 {
		t.Fatal(got, err)
	}

	e.want(t, e.bob, "POST", "/api/domains", `{"name":"bob.lan"}`, 201)
	e.want(t, e.bob, "POST", "/api/databases", `{"engine":"postgres","name":"x","domain":"bob.lan"}`, 201)
	e.want(t, e.bob, "POST", "/api/databases/bob_x/backups", `{"destination_id":`+dA+`}`, 400)
	e.want(t, e.bob, "POST", "/api/databases/bob_x/backups", `{"destination_id":`+dS+`}`, 202)
	e.a.Wait()
}

func mustInt(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRestoreOwnership(t *testing.T) {
	e := newTenants(t)
	dS := e.dest(t, e.root, "ds", true)
	var b struct {
		S3Key string `json:"s3_key"`
	}
	json.Unmarshal(e.want(t, e.alice, "POST", "/api/databases/alice_app/backups", `{"destination_id":`+dS+`}`, 202).Body.Bytes(), &b)
	e.a.Wait()
	e.want(t, e.alice, "POST", "/api/domains", `{"name":"copy.alice.lan"}`, 201)
	body := jb(map[string]any{"destination_id": mustInt(t, dS), "key": b.S3Key, "new": map[string]string{"name": "copy", "domain": "copy.alice.lan", "db_user": "copy_rw"}})
	rid := id(t, e.want(t, e.alice, "POST", "/api/restores", body, 202))
	e.a.Wait()
	var x struct {
		Status string
		Target *int64 `json:"target_database_id"`
	}
	json.Unmarshal(e.want(t, e.alice, "GET", "/api/restores/"+rid, "", 200).Body.Bytes(), &x)
	if x.Status != "ok" || x.Target == nil {
		t.Fatal(x)
	}
	var d dbView
	json.Unmarshal(e.want(t, e.alice, "GET", "/api/databases/"+strconv.FormatInt(*x.Target, 10), "", 200).Body.Bytes(), &d)
	if d.Name != "alice_copy" || d.Owner != "alice" || !strings.Contains(d.ConnectionString, "alice_copy_rw:****@") {
		t.Fatalf("%+v", d)
	}
	e.want(t, e.bob, "GET", "/api/restores/"+rid, "", 404)
	e.want(t, e.root, "GET", "/api/restores/"+rid, "", 200)
}

func TestAllowlistAdminOnly(t *testing.T) {
	e := newTenants(t)
	e.want(t, e.alice, "GET", "/api/settings/allowlist", "", 403)
	e.want(t, e.alice, "PUT", "/api/settings/allowlist", `{"cidrs":["10.0.0.0/8"]}`, 403)
	e.want(t, e.root, "PUT", "/api/settings/allowlist", `{"cidrs":["10.0.0.0/8"]}`, 200)
	e.want(t, e.root, "GET", "/api/settings/allowlist", "", 200)
}

func TestSuperuserAccess(t *testing.T) {
	e := newTenants(t)
	var su struct{ User, Password string }
	json.Unmarshal(e.want(t, e.alice, "POST", "/api/databases/alice_app/superuser/reveal", "", 200).Body.Bytes(), &su)
	if !regexp.MustCompile(`^su_[0-9a-f]{16}$`).MatchString(su.User) || su.Password == "" {
		t.Fatal(su)
	}
	if d := e.list(t, e.alice)["alice_app"]; d.AdminUser != su.User {
		t.Fatalf("%+v", d)
	}
	var c struct {
		ConnectionString string `json:"connection_string"`
	}
	json.Unmarshal(e.want(t, e.alice, "GET", "/api/databases/alice_app/connection?role=superuser", "", 200).Body.Bytes(), &c)
	if !strings.Contains(c.ConnectionString, su.User+":"+su.Password+"@") {
		t.Fatal(c)
	}

	e.want(t, e.alice, "POST", "/api/databases/alice_app/members", `{"username":"bob"}`, 204)
	e.want(t, e.bob, "POST", "/api/databases/alice_app/superuser/reveal", "", 403)
	e.want(t, e.bob, "POST", "/api/databases/alice_app/superuser/rotate", "", 403)
	e.want(t, e.bob, "GET", "/api/databases/alice_app/connection?role=superuser", "", 403)
	if b := e.want(t, e.bob, "GET", "/api/databases/alice_app", "", 200).Body.String(); strings.Contains(b, "admin_user") || strings.Contains(b, su.User) {
		t.Fatal(b)
	}

	e.want(t, e.root, "POST", "/api/databases/alice_app/superuser/reveal", "", 200)
	e.want(t, e.alice, "POST", "/api/databases/alice_app/superuser/rotate", "", 204)
	var after struct{ User, Password string }
	json.Unmarshal(e.want(t, e.root, "POST", "/api/databases/alice_app/superuser/reveal", "", 200).Body.Bytes(), &after)
	if after.User != su.User || after.Password == su.Password {
		t.Fatal(su, after)
	}
}
