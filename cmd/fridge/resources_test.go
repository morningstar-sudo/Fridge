package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// fake runs the CLI against a fake API whose answers are keyed by "METHOD /path".
func fake(t *testing.T, answers map[string]string, stdin string, args ...string) (int, string, string, []rec) {
	t.Helper()
	code, out, errs, recs := runStdin(t, stdin, func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		a, ok := answers[key]
		if !ok {
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"no fake for ` + key + `"}`))
			return
		}
		status := 200
		if a == "" {
			status = 204
		} else if strings.HasPrefix(a, "201:") {
			status, a = 201, a[4:]
		} else if strings.HasPrefix(a, "202:") {
			status, a = 202, a[4:]
		}
		w.WriteHeader(status)
		w.Write([]byte(a))
	}, args...)
	return code, out, errs, recs
}

func paths(recs []rec) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r.method+" "+r.path)
	}
	return out
}

func TestCheckCommand(t *testing.T) {
	pollEvery = 0
	defer func() { pollEvery = 2e9 }()
	n := 0
	code, out, errs, recs := runStdin(t, "", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/destinations":
			w.Write([]byte(`[{"id":7,"name":"dS"}]`))
		case "POST /api/dump-checks":
			w.WriteHeader(202)
			w.Write([]byte(`{"id":3,"status":"running"}`))
		case "GET /api/dump-checks/3":
			n++
			st := "running"
			if n >= 2 {
				st = "ok"
			}
			w.Write([]byte(`{"id":3,"status":"` + st + `"}`))
		default:
			w.WriteHeader(404)
		}
	}, "check", "--from", "dS:fridge/x.dump", "--wait")
	if code != 0 || !strings.Contains(out, "id=3 status=ok") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	var body struct {
		DestinationID int64  `json:"destination_id"`
		Key           string `json:"key"`
	}
	json.Unmarshal([]byte(recs[1].body), &body)
	if recs[1].path != "/api/dump-checks" || body.DestinationID != 7 || body.Key != "fridge/x.dump" {
		t.Fatalf("%+v", recs)
	}
	if n < 2 {
		t.Fatalf("polled %d times", n)
	}
	// a failed check is a failed command
	code, _, errs, _ = runStdin(t, "", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.WriteHeader(202)
			w.Write([]byte(`{"id":4,"status":"failed","error":"pg_restore exited 1"}`))
			return
		}
		w.Write([]byte(`[{"id":7,"name":"dS"}]`))
	}, "check", "--from", "7:k.dump", "--wait")
	if code != 1 || !strings.Contains(errs, "pg_restore exited 1") {
		t.Fatalf("a failed check must fail the command: %d %q", code, errs)
	}
	if code, _, _, _ := runStdin(t, "", nil, "check", "--from", "nokey"); code != 2 {
		t.Fatalf("bad --from: %d", code)
	}
}

func TestMembersCommands(t *testing.T) {
	code, out, errs, recs := fake(t, map[string]string{
		"POST /api/databases/alice_app/members": "",
	}, "", "members", "add", "alice_app", "bob")
	if code != 0 || !strings.Contains(out, "bob is now a member") || recs[0].body != `{"username":"bob"}` {
		t.Fatalf("add: %d %q %q %+v", code, out, errs, recs)
	}
	code, out, _, recs = fake(t, map[string]string{
		"GET /api/databases/alice_app/members": `[{"user_id":5,"username":"bob","added_at":"2026-10-01T00:00:00Z"},{"user_id":6,"username":"carol","added_at":"2026-10-02T00:00:00Z"}]`,
	}, "", "members", "ls", "alice_app")
	if code != 0 || !strings.Contains(out, "bob") || !strings.Contains(out, "carol") {
		t.Fatalf("ls: %d %q %v", code, out, paths(recs))
	}
	code, out, errs, recs = fake(t, map[string]string{
		"GET /api/databases/alice_app/members":      `[{"user_id":5,"username":"bob"},{"user_id":6,"username":"carol"}]`,
		"DELETE /api/databases/alice_app/members/6": "",
	}, "", "members", "rm", "alice_app", "carol")
	if code != 0 || strings.Join(paths(recs), ",") != "GET /api/databases/alice_app/members,DELETE /api/databases/alice_app/members/6" {
		t.Fatalf("rm: %d %q %q %v", code, out, errs, paths(recs))
	}
	code, _, errs, recs = fake(t, map[string]string{"GET /api/databases/alice_app/members": `[{"user_id":5,"username":"bob"}]`}, "", "members", "rm", "alice_app", "zed")
	if code != 1 || !strings.Contains(errs, "not a member") || len(recs) != 1 {
		t.Fatalf("rm unknown: %d %q %v", code, errs, paths(recs))
	}
}

func TestDomainsCommands(t *testing.T) {
	code, out, _, recs := fake(t, map[string]string{"POST /api/domains": `201:{"id":9,"name":"shop.lan","owner":"alice","in_use":0}`}, "", "domains", "add", "shop.lan")
	if code != 0 || !strings.Contains(out, "shop.lan registered (id=9)") || recs[0].body != `{"name":"shop.lan"}` {
		t.Fatalf("add: %d %q %+v", code, out, recs)
	}
	code, out, _, _ = fake(t, map[string]string{"GET /api/domains": `[{"id":9,"name":"shop.lan","owner":"alice","in_use":2}]`}, "", "domains", "ls")
	if code != 0 || !strings.Contains(out, "shop.lan") || !strings.Contains(out, "alice") {
		t.Fatalf("ls: %d %q", code, out)
	}
	code, _, _, recs = fake(t, map[string]string{
		"GET /api/domains":      `[{"id":9,"name":"shop.lan"},{"id":10,"name":"b.lan"}]`,
		"DELETE /api/domains/9": "",
	}, "", "domains", "rm", "SHOP.lan")
	if code != 0 || strings.Join(paths(recs), ",") != "GET /api/domains,DELETE /api/domains/9" {
		t.Fatalf("rm: %d %v", code, paths(recs))
	}
	if code, _, errs, _ := fake(t, map[string]string{"GET /api/domains": `[]`}, "", "domains", "rm", "none.lan"); code != 1 || !strings.Contains(errs, "not found") {
		t.Fatalf("rm unknown: %d %q", code, errs)
	}
}

func TestDestShare(t *testing.T) {
	code, _, _, recs := fake(t, map[string]string{"GET /api/destinations": `[{"id":3,"name":"shared-s3"}]`, "PATCH /api/destinations/3": ""}, "", "dest", "share", "shared-s3", "on")
	if code != 0 || recs[1].method != "PATCH" || recs[1].body != `{"shared":true}` {
		t.Fatalf("%d %+v", code, recs)
	}
	_, _, _, recs = fake(t, map[string]string{"PATCH /api/destinations/3": ""}, "", "dest", "share", "3", "off")
	if len(recs) != 1 || recs[0].body != `{"shared":false}` {
		t.Fatalf("%+v", recs)
	}
	if code, _, _, _ := fake(t, nil, "", "dest", "share", "3", "maybe"); code != 2 {
		t.Fatalf("%d", code)
	}
}

func TestShowAndSuperuser(t *testing.T) {
	db := `{"id":1,"name":"alice_app","engine":"postgres","version":"17","domain":"alice.lan","status":"running","connection_string":"postgresql://alice_app:****@alice.lan:5432/alice_app","admin_user":"su_abc"}`
	// show: masked, no /connection call
	code, out, _, recs := fake(t, map[string]string{"GET /api/databases/alice_app": db}, "", "show", "alice_app")
	if code != 0 || !strings.Contains(out, ":****@") || strings.Join(paths(recs), ",") != "GET /api/databases/alice_app" {
		t.Fatalf("show: %d %q %v", code, out, paths(recs))
	}
	// show --reveal: the real string, via /connection
	code, out, _, recs = fake(t, map[string]string{
		"GET /api/databases/alice_app":            db,
		"GET /api/databases/alice_app/connection": `{"connection_string":"postgresql://alice_app:s3cret@alice.lan:5432/alice_app"}`,
	}, "", "show", "alice_app", "--reveal")
	if code != 0 || !strings.Contains(out, "s3cret") || strings.Contains(out, "****") || !contains(paths(recs), "GET /api/databases/alice_app/connection") {
		t.Fatalf("reveal: %d %q %v", code, out, paths(recs))
	}
	// superuser show: only the name, nothing secret fetched
	code, out, _, recs = fake(t, map[string]string{"GET /api/databases/alice_app": db}, "", "superuser", "show", "alice_app")
	if code != 0 || strings.TrimSpace(out) != "su_abc" || len(recs) != 1 {
		t.Fatalf("superuser show: %d %q %v", code, out, paths(recs))
	}
	// superuser show --reveal
	code, out, _, recs = fake(t, map[string]string{"POST /api/databases/alice_app/superuser/reveal": `{"user":"su_abc","password":"P4ss"}`}, "", "superuser", "show", "alice_app", "--reveal")
	if code != 0 || !strings.Contains(out, "su_abc") || !strings.Contains(out, "P4ss") || len(recs) != 1 {
		t.Fatalf("superuser reveal: %d %q %v", code, out, paths(recs))
	}
	// rotate
	code, _, _, recs = fake(t, map[string]string{"POST /api/databases/alice_app/superuser/rotate": ""}, "", "superuser", "rotate", "alice_app")
	if code != 0 || len(recs) != 1 {
		t.Fatalf("rotate: %d %v", code, paths(recs))
	}
	// there is no --superuser flag on show
	if code, _, _, _ := fake(t, map[string]string{"GET /api/databases/alice_app": db}, "", "show", "alice_app", "--superuser"); code != 2 {
		t.Fatalf("--superuser accepted: %d", code)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestAuditCommand(t *testing.T) {
	var gotQuery url.Values
	code, out, errs, _ := runStdin(t, "", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		if r.URL.Path != "/api/audit" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Write([]byte(`{"entries":[{"at":"2026-10-25T10:00:00Z","actor":"bob","action":"backup.start","target_type":"backup","target_id":"4","db":"alice_app","success":true}],"next_before":null}`))
	}, "audit", "--user", "bob", "--db", "alice_app", "--since", "24h", "--action", "backup.")
	if code != 0 || !strings.Contains(out, "backup.start") || !strings.Contains(out, "bob") || !strings.Contains(out, "backup:4") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	if gotQuery.Get("actor") != "bob" || gotQuery.Get("db") != "alice_app" || gotQuery.Get("action") != "backup." {
		t.Fatalf("query %v", gotQuery)
	}
	from := gotQuery.Get("from")
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`).MatchString(from) {
		t.Fatalf("from %q", from)
	}
	// --csv goes to the .csv route and prints the file as it is
	code, out, _, recs := fake(t, map[string]string{"GET /api/audit.csv": "at,actor\n2026-10-25T10:00:00Z,bob\n"}, "", "audit", "--csv")
	if code != 0 || out != "at,actor\n2026-10-25T10:00:00Z,bob\n" || recs[0].path != "/api/audit.csv" {
		t.Fatalf("csv: %d %q %v", code, out, paths(recs))
	}
	// days, and nonsense
	if code, _, _, _ := runStdin(t, "", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"entries":[]}`)) }, "audit", "--since", "7d"); code != 0 {
		t.Fatalf("7d: %d", code)
	}
	if code, _, _, _ := runStdin(t, "", nil, "audit", "--since", "soon"); code != 2 {
		t.Fatalf("soon: %d", code)
	}
}

func TestCredentialFlags(t *testing.T) {
	create := map[string]string{"POST /api/databases": `201:{"id":1,"name":"alice_shop","connection_string":"postgresql://alice_x:****@shop.lan:5432/alice_shop"}`}
	// random credentials exclude the other two flags
	for _, extra := range []string{"--db-user", "--db-password-stdin"} {
		args := []string{"create", "postgres", "shop", "--domain", "shop.lan", "--random-credentials", extra}
		if extra == "--db-user" {
			args = append(args, "x")
		}
		if code, _, _, recs := fake(t, create, "pw\n", args...); code != 2 || len(recs) != 0 {
			t.Errorf("%s with --random-credentials: exit %d, %d requests", extra, code, len(recs))
		}
	}
	// generated: 8 [a-z0-9] user, 24 [A-Za-z0-9-_.~] password, shown once
	code, out, _, recs := fake(t, create, "", "create", "postgres", "shop", "--domain", "shop.lan", "--random-credentials")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var body map[string]string
	json.Unmarshal([]byte(recs[0].body), &body)
	if !regexp.MustCompile(`^[a-z0-9]{8}$`).MatchString(body["db_user"]) || !regexp.MustCompile(`^[A-Za-z0-9_.~-]{24}$`).MatchString(body["db_password"]) {
		t.Fatalf("body %v", body)
	}
	if !strings.Contains(out, body["db_password"]) || !strings.Contains(out, "created alice_shop") || strings.Count(out, body["db_password"]) != 1 {
		t.Fatalf("out %q", out)
	}
	// two runs differ
	_, _, _, recs2 := fake(t, create, "", "create", "postgres", "shop", "--domain", "shop.lan", "--random-credentials")
	var body2 map[string]string
	json.Unmarshal([]byte(recs2[0].body), &body2)
	if body2["db_password"] == body["db_password"] {
		t.Fatal("same password twice")
	}
	// --db-user and --db-password-stdin
	code, out, _, recs = fake(t, create, "chosen-password.1\n", "create", "postgres", "shop", "--domain", "shop.lan", "--db-user", "rw", "--db-password-stdin")
	body = nil
	json.Unmarshal([]byte(recs[0].body), &body)
	if code != 0 || body["db_user"] != "rw" || body["db_password"] != "chosen-password.1" || strings.Contains(out, "chosen-password.1") {
		t.Fatalf("explicit: %d %v %q", code, body, out)
	}
	// without any flag the request carries neither field
	_, _, _, recs = fake(t, create, "", "create", "postgres", "shop", "--domain", "shop.lan")
	if strings.Contains(recs[0].body, "db_user") || strings.Contains(recs[0].body, "db_password") {
		t.Fatalf("body %s", recs[0].body)
	}
	// restore takes the same flags, in its "new" object
	rs := map[string]string{"GET /api/destinations": `[{"id":3,"name":"d"}]`, "POST /api/restores": `202:{"id":8,"status":"running"}`}
	code, out, _, recs = fake(t, rs, "", "restore", "--from", "d:k.dump", "--name", "copy", "--domain", "copy.lan", "--random-credentials")
	var rb struct {
		New map[string]string
	}
	json.Unmarshal([]byte(recs[1].body), &rb)
	if code != 0 || len(rb.New["db_user"]) != 8 || len(rb.New["db_password"]) != 24 || !strings.Contains(out, rb.New["db_password"]) {
		t.Fatalf("restore: %d %v %q", code, rb, out)
	}
	// create --reveal asks for the real string
	rv := map[string]string{
		"POST /api/databases":                      create["POST /api/databases"],
		"GET /api/databases/alice_shop/connection": `{"connection_string":"postgresql://alice_x:realpw@shop.lan:5432/alice_shop"}`,
	}
	if _, out, _, _ := fake(t, rv, "", "create", "postgres", "shop", "--domain", "shop.lan", "--reveal"); !strings.Contains(out, "realpw") {
		t.Fatalf("--reveal: %q", out)
	}
}

func TestRandomString(t *testing.T) {
	seen := map[byte]bool{}
	for i := 0; i < 200; i++ {
		for _, c := range []byte(randomString(passwordSet, 24)) {
			if !strings.ContainsRune(passwordSet, rune(c)) {
				t.Fatalf("%q outside the alphabet", c)
			}
			seen[c] = true
		}
	}
	if len(seen) < len(passwordSet)-4 {
		t.Fatalf("only %d of %d characters ever drawn", len(seen), len(passwordSet))
	}
}
