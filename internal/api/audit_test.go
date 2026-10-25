package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"fridge/internal/scram"
	"fridge/internal/store"
)

const pwCarol = "carol password 123"

type auditView struct {
	ID         int64
	ActorID    *int64 `json:"actor_id"`
	Actor      string
	IP         string
	Action     string
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	DBID       *int64 `json:"db_id"`
	DB         string
	Success    bool
	Detail     map[string]any
}

func (e *tenants) audit(t *testing.T, sid, query string) []auditView {
	t.Helper()
	var out struct{ Entries []auditView }
	if err := json.Unmarshal(e.want(t, sid, "GET", "/api/audit"+query, "", 200).Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Entries
}

// find returns the newest entry with the action (by actor when non-empty).
func find(es []auditView, action, actor string) *auditView {
	for i := range es {
		if es[i].Action == action && (actor == "" || es[i].Actor == actor) {
			return &es[i]
		}
	}
	return nil
}

func (e *tenants) member(t *testing.T) {
	t.Helper()
	e.want(t, e.alice, "POST", "/api/databases/alice_app/members", `{"username":"bob"}`, 204)
}

func TestAuditVisibility(t *testing.T) {
	e := newTenants(t)
	e.member(t)
	inviteUser(t, e.h, e.root, "carol", "user", pwCarol)
	_, carol := loginAs(t, e.h, "carol", pwCarol)
	shared := e.dest(t, e.root, "da", true)
	e.want(t, e.bob, "POST", "/api/databases/alice_app/backups", `{"destination_id":`+shared+`}`, 202)
	e.a.Wait()

	if x := find(e.audit(t, e.alice, ""), "backup.start", "bob"); x == nil || x.DB != "alice_app" || !x.Success {
		t.Fatalf("owner does not see the member's backup: %+v", x)
	}
	if find(e.audit(t, e.bob, ""), "backup.start", "bob") == nil {
		t.Fatal("bob does not see his own backup")
	}
	if find(e.audit(t, e.bob, ""), "db.create", "alice") != nil {
		t.Fatal("a member sees the owner's actions")
	}
	for _, x := range e.audit(t, carol, "") {
		if x.DB == "alice_app" || x.Actor != "carol" {
			t.Fatalf("carol sees %+v", x)
		}
	}
	if find(e.audit(t, e.root, ""), "backup.start", "bob") == nil || find(e.audit(t, e.root, ""), "db.create", "alice") == nil {
		t.Fatal("admin does not see everything")
	}
}

func TestAuditFilters(t *testing.T) {
	e := newTenants(t)
	e.member(t)
	ids := func(es []auditView) string {
		var s []string
		for _, x := range es {
			s = append(s, strconv.FormatInt(x.ID, 10))
		}
		return strings.Join(s, ",")
	}
	for _, x := range e.audit(t, e.root, "?actor=alice") {
		if x.Actor != "alice" {
			t.Fatal(x)
		}
	}
	byName, db := e.audit(t, e.root, "?db=alice_app"), ""
	for _, x := range byName {
		if x.DBID != nil {
			db = strconv.FormatInt(*x.DBID, 10)
		}
	}
	if len(byName) == 0 || db == "" || ids(byName) != ids(e.audit(t, e.root, "?db="+db)) {
		t.Fatalf("db by name %q vs by id %q", ids(byName), db)
	}
	for _, x := range e.audit(t, e.root, "?action=db.") {
		if !strings.HasPrefix(x.Action, "db.") {
			t.Fatal(x)
		}
	}
	if es := e.audit(t, e.bob, "?db=nosuch_db"); len(es) != 0 {
		t.Fatal(es)
	}
	// a database the caller cannot see answers like one with no history
	e.want(t, e.root, "POST", "/api/domains", `{"name":"root.lan"}`, 201)
	e.want(t, e.root, "POST", "/api/databases", `{"engine":"postgres","name":"hidden","domain":"root.lan"}`, 201)
	if es := e.audit(t, e.bob, "?db=root1_hidden"); len(es) != 0 {
		t.Fatal(es)
	}
	if es := e.audit(t, e.root, "?db=root1_hidden"); len(es) == 0 {
		t.Fatal("admin sees nothing")
	}
	// paging
	all := e.audit(t, e.root, "")
	if len(all) < 4 {
		t.Fatal(len(all))
	}
	var page struct {
		Entries    []auditView
		NextBefore *int64 `json:"next_before"`
	}
	json.Unmarshal(e.want(t, e.root, "GET", "/api/audit?limit=2", "", 200).Body.Bytes(), &page)
	if len(page.Entries) != 2 || page.NextBefore == nil || *page.NextBefore != page.Entries[1].ID {
		t.Fatalf("%+v", page)
	}
	if rest := e.audit(t, e.root, "?before="+strconv.FormatInt(*page.NextBefore, 10)); len(rest) != len(all)-2 {
		t.Fatalf("rest %d of %d", len(rest), len(all))
	}
	e.want(t, e.root, "GET", "/api/audit?limit=x", "", 400)
	e.want(t, e.root, "GET", "/api/audit?from=yesterday", "", 400)
}

func TestAuditRecordedForActions(t *testing.T) {
	e := newTenants(t)
	login(t, e.h, "alice", "wrong password 12345", false)
	login(t, e.h, "ghost1", "wrong password 12345", false)
	e.member(t)
	e.want(t, e.bob, "GET", "/api/databases/nosuch", "", 404)
	e.want(t, e.bob, "POST", "/api/databases/alice_app/members", `{"username":"carol"}`, 403)
	e.want(t, e.alice, "POST", "/api/databases/alice_app/superuser/reveal", "", 200)
	es := e.audit(t, e.root, "?limit=500")

	if x := find(es, "auth.login_failed", "alice"); x == nil || x.Success || x.ActorID == nil || x.Detail["reason"] != "bad_credentials" {
		t.Fatalf("known user: %+v", x)
	}
	if x := find(es, "auth.login_failed", "anonymous"); x == nil || x.ActorID != nil || x.Detail["username"] != "ghost1" {
		t.Fatalf("unknown user: %+v", x)
	}
	if x := find(es, "auth.login", "alice"); x == nil || !x.Success {
		t.Fatalf("login: %+v", x)
	}
	if x := find(es, "auth.setup", "root1"); x == nil {
		t.Fatal("setup not recorded")
	}
	inv := 0
	for _, x := range es {
		if x.Action == "invite.create" && x.Actor == "root1" && x.Success && x.Detail["role"] == "user" && (x.TargetID == "alice" || x.TargetID == "bob") {
			inv++
		}
	}
	if inv != 2 {
		t.Fatalf("invite.create entries: %d", inv)
	}
	if find(es, "invite.accept", "alice") == nil {
		t.Fatal("invite.accept not recorded")
	}
	if x := find(es, "db.create", "alice"); x == nil || !x.Success || x.DB != "alice_app" || x.Detail["name"] != "app" {
		t.Fatalf("db.create: %+v", x)
	}
	if x := find(es, "domain.register", "alice"); x == nil || !x.Success || x.Detail["name"] != "alice.lan" {
		t.Fatalf("domain.register: %+v", x)
	}
	if x := find(es, "db.member_add", "alice"); x == nil || !x.Success {
		t.Fatalf("member_add by owner: %+v", x)
	}
	if x := find(es, "db.member_add", "bob"); x == nil || x.Success || x.DB != "alice_app" {
		t.Fatalf("member_add by member: %+v", x)
	}
	if find(es, "superuser.reveal", "alice") == nil {
		t.Fatal("superuser.reveal not recorded")
	}
	for _, x := range es {
		if x.Actor == "bob" && strings.HasPrefix(x.Action, "db.") && x.Action != "db.member_add" {
			t.Fatalf("a 404 left a trace: %+v", x)
		}
	}
	for _, x := range es {
		if !regexp.MustCompile(`^[a-z_]+\.[a-z_]+$`).MatchString(x.Action) {
			t.Fatal(x.Action)
		}
	}

	// user management and sessions
	bob, _ := e.a.Store.GetUserByName(context.Background(), "bob")
	bid := strconv.FormatInt(bob.ID, 10)
	e.want(t, e.root, "PATCH", "/api/users/"+bid, `{"role":"admin"}`, 204)
	e.want(t, e.root, "PATCH", "/api/users/"+bid, `{"disabled":true}`, 204)
	e.want(t, e.root, "PATCH", "/api/users/"+bid, `{"disabled":false}`, 204)
	e.want(t, e.root, "POST", "/api/users/"+bid+"/reset", "", 201)
	e.want(t, e.alice, "POST", "/api/auth/logout", "", 204)
	es = e.audit(t, e.root, "?limit=500")
	for _, a := range []string{"user.role", "user.disable", "user.enable", "user.reset_password"} {
		if x := find(es, a, "root1"); x == nil || x.TargetID != bid || !x.Success {
			t.Fatalf("%s: %+v", a, x)
		}
	}
	if find(es, "auth.logout", "alice") == nil {
		t.Fatal("logout not recorded")
	}
	// a rejected change is a failed entry
	e.want(t, e.alice, "PATCH", "/api/users/"+bid, `{"role":"admin"}`, 401) // alice's session just ended
	e.want(t, e.root, "PATCH", "/api/users/"+bid, `{"role":"wizard"}`, 400)
	if x := find(e.audit(t, e.root, "?action=user.role"), "user.role", "root1"); x == nil || x.Success {
		t.Fatalf("%+v", x)
	}
}

func TestRevealPasswordAudited(t *testing.T) {
	e := newTenants(t)
	count := func(action string) int {
		n := 0
		for _, x := range e.audit(t, e.root, "?limit=500") {
			if x.Action == action {
				n++
			}
		}
		return n
	}
	e.want(t, e.alice, "GET", "/api/databases/alice_app", "", 200)
	e.want(t, e.alice, "GET", "/api/databases", "", 200)
	if count("db.reveal_password") != 0 {
		t.Fatal("masked reads were audited")
	}
	e.want(t, e.alice, "GET", "/api/databases/alice_app/connection", "", 200)
	x := find(e.audit(t, e.root, "?action=db."), "db.reveal_password", "alice")
	if x == nil || !x.Success || x.DB != "alice_app" {
		t.Fatalf("%+v", x)
	}
	e.want(t, e.bob, "GET", "/api/databases/alice_app/connection", "", 404)
	if count("db.reveal_password") != 1 {
		t.Fatal("a 404 was audited")
	}
	e.want(t, e.alice, "GET", "/api/databases/alice_app/connection?role=superuser", "", 200)
	if count("superuser.reveal") != 1 {
		t.Fatal("superuser connection string not audited")
	}
}

func TestAuditNoSecrets(t *testing.T) {
	e := newTenants(t)
	tokn := inviteUser(t, e.h, e.root, "carol", "user", pwCarol)
	e.want(t, e.alice, "POST", "/api/destinations", jb(map[string]string{"name": "mine", "endpoint": "https://s3.example.com", "bucket": "b", "access_key": "AK", "secret_key": "TOPSECRET"}), 201)
	e.want(t, e.alice, "GET", "/api/databases/alice_app/connection", "", 200)
	var su map[string]string
	json.Unmarshal(e.want(t, e.alice, "POST", "/api/databases/alice_app/superuser/reveal", "", 200).Body.Bytes(), &su)
	e.want(t, e.alice, "POST", "/api/databases/alice_app/superuser/rotate", "", 204)
	var su2 map[string]string
	json.Unmarshal(e.want(t, e.alice, "POST", "/api/databases/alice_app/superuser/reveal", "", 200).Body.Bytes(), &su2)

	cs := credsFor(pwAlice)
	c := scram.NewClient("alice", "", "reauthnonce0123456789ab")
	var st, rf map[string]string
	json.Unmarshal(e.want(t, e.alice, "POST", "/api/me/reauth/start", jb(map[string]string{"client_first": c.First()}), 200).Body.Bytes(), &st)
	fin, err := c.FinalWithSalted(st["server_first"], cs.salted)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(e.want(t, e.alice, "POST", "/api/me/reauth/finish", jb(map[string]string{"scram_id": st["scram_id"], "client_final": fin}), 200).Body.Bytes(), &rf)
	e.want(t, e.alice, "POST", "/api/me/password", jb(map[string]any{"reauth_token": rf["reauth_token"], "credentials": credsFor(pwCarol).c}), 204)
	e.want(t, e.alice, "POST", "/api/auth/logout", "", 204)

	var dump string
	if err := e.a.Store.Pool().QueryRow(context.Background(), `SELECT COALESCE(string_agg(audit_log::text, E'\n'), '') FROM audit_log`).Scan(&dump); err != nil || len(dump) < 100 {
		t.Fatal(len(dump), err)
	}
	for name, secret := range map[string]string{
		"destination secret": "TOPSECRET", "db password": e.alicePW, "root session": e.root, "alice session": e.alice, "bob session": e.bob,
		"invite token": tokn, "root password": pwRoot, "alice password": pwAlice, "reauth token": rf["reauth_token"],
		"superuser password": su["password"], "rotated superuser password": su2["password"],
	} {
		if secret == "" {
			t.Errorf("no value for the %s: the check proves nothing", name)
		} else if strings.Contains(dump, secret) {
			t.Errorf("audit log contains the %s", name)
		}
	}
}

func mustDB(t *testing.T, e *tenants, name string) store.Database {
	t.Helper()
	d, err := e.a.Store.GetDatabaseByName(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mustAtoi(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAuditCSV(t *testing.T) {
	e := newTenants(t)
	login(t, e.h, "ghost1", "wrong password 12345", false)
	if csvCell("=1+1") != "'=1+1" || csvCell("+x") != "'+x" || csvCell("@x") != "'@x" || csvCell("-1") != "'-1" || csvCell("plain") != "plain" || csvCell("") != "" {
		t.Fatal("csvCell")
	}
	w := e.want(t, e.root, "GET", "/api/audit.csv?limit=500", "", 200)
	if !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal(w.Header())
	}
	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rows[0], ",") != "at,actor,ip,action,target_type,target_id,db,success,detail" {
		t.Fatal(rows[0])
	}
	if want := len(e.audit(t, e.root, "?limit=500")); len(rows)-1 != want {
		t.Fatalf("%d rows, %d entries", len(rows)-1, want)
	}
	// filters apply
	rows, _ = csv.NewReader(strings.NewReader(e.want(t, e.root, "GET", "/api/audit.csv?action=db.", "", 200).Body.String())).ReadAll()
	if len(rows) < 2 {
		t.Fatal(rows)
	}
	for _, r := range rows[1:] {
		if !strings.HasPrefix(r[3], "db.") {
			t.Fatal(r)
		}
	}
	all, _ := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	for _, r := range all {
		for _, c := range r {
			if c != "" && strings.ContainsRune("=+@", rune(c[0])) {
				t.Fatalf("formula cell %q", c)
			}
		}
	}
	e.want(t, e.bob, "GET", "/api/audit.csv", "", 200) // a user may export what they may see
}

func TestActivityAdminOnly(t *testing.T) {
	e := newTenants(t)
	e.want(t, e.alice, "GET", "/api/activity", "", 403)
	rt := e.a.RT.(*fakeRT)
	rt.gate = make(chan struct{})
	shared := e.dest(t, e.root, "da", true)
	e.want(t, e.alice, "POST", "/api/databases/alice_app/backups", `{"destination_id":`+shared+`}`, 202)

	var act struct {
		Jobs []struct {
			Kind, Database string
			StartedBy      string `json:"started_by"`
		}
		Online []struct{ Username string }
	}
	json.Unmarshal(e.want(t, e.root, "GET", "/api/activity", "", 200).Body.Bytes(), &act)
	if len(act.Jobs) != 1 || act.Jobs[0].Kind != "backup" || act.Jobs[0].Database != "alice_app" || act.Jobs[0].StartedBy != "alice" {
		t.Fatalf("%+v", act.Jobs)
	}
	seen := map[string]bool{}
	for _, o := range act.Online {
		seen[o.Username] = true
	}
	if !seen["root1"] || !seen["alice"] || !seen["bob"] {
		t.Fatalf("online %+v", act.Online)
	}
	close(rt.gate)
	e.a.Wait()
	json.Unmarshal(e.want(t, e.root, "GET", "/api/activity", "", 200).Body.Bytes(), &act)
	if len(act.Jobs) != 0 {
		t.Fatalf("%+v", act.Jobs)
	}

	// a scheduled backup has no starter: "system"
	rt.gate = make(chan struct{})
	var sc struct{ ID int64 }
	json.Unmarshal(e.want(t, e.alice, "POST", "/api/databases/alice_app/schedules", jb(map[string]any{"destination_id": mustAtoi(t, shared), "cron": "0 2 * * *", "prefix": "sched", "keep_latest": 3, "enabled": true}), 201).Body.Bytes(), &sc)
	d := mustDB(t, e, "alice_app")
	if _, err := e.a.StartBackup(context.Background(), d.ID, 0, &sc.ID, "schedule", nil); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(e.want(t, e.root, "GET", "/api/activity", "", 200).Body.Bytes(), &act)
	if len(act.Jobs) != 1 || act.Jobs[0].StartedBy != "system" {
		t.Fatalf("%+v", act.Jobs)
	}
	close(rt.gate)
	e.a.Wait()
	if x := find(e.audit(t, e.root, "?action=system."), "system.backup", "system"); x == nil || !x.Success || x.DB != "alice_app" || x.ActorID != nil {
		t.Fatalf("system.backup: %+v", x)
	}
}
