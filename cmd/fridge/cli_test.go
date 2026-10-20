package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseFrom(t *testing.T) {
	d, k, err := parseFrom("minio2:fridge/postgres-17/abc/a.dump")
	if err != nil || d != "minio2" || k != "fridge/postgres-17/abc/a.dump" {
		t.Fatalf("got %q %q %v", d, k, err)
	}
	for _, bad := range []string{"nokey", ":k", "d:"} {
		if _, _, err := parseFrom(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

type rec struct {
	method, path, auth, body string
}

func run(t *testing.T, h http.HandlerFunc, args ...string) (int, string, string, []rec) {
	t.Helper()
	var recs []rec
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		recs = append(recs, rec{r.Method, r.URL.Path, r.Header.Get("Authorization"), string(b)})
		h(w, r)
	}))
	defer srv.Close()
	t.Setenv("FRIDGE_URL", srv.URL)
	t.Setenv("FRIDGE_TOKEN", "tok")
	var out, errb bytes.Buffer
	code := runCLI(args, &out, &errb)
	return code, out.String(), errb.String(), recs
}

func TestCreateCallsAPI(t *testing.T) {
	code, out, errs, recs := run(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		w.Write([]byte(`{"id":1,"name":"abc","connection_string":"postgres://u:p@abc.com:5432/abc"}`))
	}, "create", "postgres", "abc", "--domain", "abc.com", "--version", "16")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if len(recs) != 1 || recs[0].method != "POST" || recs[0].path != "/api/databases" || recs[0].auth != "Bearer tok" {
		t.Fatalf("recs %+v", recs)
	}
	var got, want map[string]string
	json.Unmarshal([]byte(recs[0].body), &got)
	json.Unmarshal([]byte(`{"engine":"postgres","name":"abc","domain":"abc.com","version":"16"}`), &want)
	if len(got) != len(want) || got["engine"] != "postgres" || got["name"] != "abc" || got["domain"] != "abc.com" || got["version"] != "16" {
		t.Fatalf("body %s", recs[0].body)
	}
	if !strings.Contains(out, "postgres://u:p@abc.com:5432/abc") {
		t.Fatalf("stdout %q", out)
	}
	if strings.Contains(out+errs, "tok") {
		t.Fatal("token leaked")
	}
}

func TestAPIErrorExitCode(t *testing.T) {
	code, _, errs, _ := run(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"bad version"}`))
	}, "create", "postgres", "abc", "--domain", "abc.com")
	if code != 1 || !strings.Contains(errs, "bad version") {
		t.Fatalf("code %d stderr %q", code, errs)
	}
}

func TestUsageErrorExitCode(t *testing.T) {
	if code, _, _, _ := run(t, nil, "create"); code != 2 {
		t.Fatalf("code %d", code)
	}
	if code, _, _, _ := run(t, nil, "bogus"); code != 2 {
		t.Fatalf("code %d", code)
	}
}

func TestRestoreResolvesDestinationName(t *testing.T) {
	code, _, errs, recs := run(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Write([]byte(`[{"id":3,"name":"minio2"}]`))
			return
		}
		w.WriteHeader(202)
		w.Write([]byte(`{"id":9,"status":"running"}`))
	}, "restore", "--from", "minio2:fridge/postgres-17/abc/a.dump", "--name", "n1", "--domain", "n1.com")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if len(recs) != 2 || recs[0].path != "/api/destinations" || recs[1].method != "POST" || recs[1].path != "/api/restores" {
		t.Fatalf("recs %+v", recs)
	}
	var b struct {
		DestinationID int64  `json:"destination_id"`
		Key           string `json:"key"`
		New           struct{ Name, Domain string }
	}
	json.Unmarshal([]byte(recs[1].body), &b)
	if b.DestinationID != 3 || b.Key != "fridge/postgres-17/abc/a.dump" || b.New.Name != "n1" || b.New.Domain != "n1.com" {
		t.Fatalf("body %s", recs[1].body)
	}
}

// seq serves the start POST as running, then the given GET poll bodies (code "" = 200).
func seq(polls ...[2]string) http.HandlerFunc {
	i := 0
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.WriteHeader(202)
			w.Write([]byte(`{"id":7,"status":"running"}`))
			return
		}
		p := polls[min(i, len(polls)-1)]
		i++
		code := 200
		if p[0] != "" {
			code, _ = strconv.Atoi(p[0])
		}
		w.WriteHeader(code)
		w.Write([]byte(p[1]))
	}
}

func TestBackupWait(t *testing.T) {
	pollEvery = time.Millisecond
	run503 := [2]string{"503", `{"error":"restarting"}`}
	running := [2]string{"", `{"id":7,"status":"running"}`}
	ok := [2]string{"", `{"id":7,"status":"ok"}`}

	code, out, errs, _ := run(t, seq(running, running, ok), "backup", "db1", "--wait")
	if code != 0 || !strings.Contains(out, "status=ok") {
		t.Fatalf("ok: %d %q %q", code, out, errs)
	}
	code, _, errs, _ = run(t, seq(running, [2]string{"", `{"id":7,"status":"failed","error":"pg_dump exploded"}`}), "backup", "db1", "--wait")
	if code != 1 || !strings.Contains(errs, "pg_dump exploded") {
		t.Fatalf("failed: %d %q", code, errs)
	}
	code, _, errs, _ = run(t, seq(run503, running, ok), "backup", "db1", "--wait")
	if code != 0 {
		t.Fatalf("transient: %d %q", code, errs)
	}
	code, _, errs, _ = run(t, seq(run503), "backup", "db1", "--wait")
	if code != 1 || !strings.Contains(errs, "id=7") {
		t.Fatalf("persistent errors: %d %q", code, errs)
	}
	code, _, errs, _ = run(t, seq(running), "backup", "db1", "--wait", "--timeout", "30ms")
	if code != 1 || !strings.Contains(errs, "id=7") || !strings.Contains(errs, "fridge backups db1") {
		t.Fatalf("deadline: %d %q", code, errs)
	}
}

func TestFlagsAnywhere(t *testing.T) {
	for _, args := range [][]string{
		{"create", "postgres", "abc", "--version", "16", "--domain", "abc.com"},
		{"create", "--domain", "abc.com", "postgres", "abc"},
	} {
		code, _, errs, recs := run(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }, args...)
		if code != 0 || len(recs) != 1 || !strings.Contains(recs[0].body, `"name":"abc"`) || !strings.Contains(recs[0].body, `"domain":"abc.com"`) {
			t.Fatalf("%v: %d %q %+v", args, code, errs, recs)
		}
	}
}

func TestDestAdd(t *testing.T) {
	code, _, errs, recs := run(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		w.Write([]byte(`{"id":1,"name":"m"}`))
	}, "dest", "add", "m", "--endpoint", "http://s3:9000", "--bucket", "b", "--access-key", "A", "--secret-key", "S", "--region", "r", "--path-style")
	if code != 0 || len(recs) != 1 || recs[0].path != "/api/destinations" {
		t.Fatalf("%d %q %+v", code, errs, recs)
	}
	var b map[string]any
	json.Unmarshal([]byte(recs[0].body), &b)
	if b["name"] != "m" || b["endpoint"] != "http://s3:9000" || b["bucket"] != "b" || b["access_key"] != "A" ||
		b["secret_key"] != "S" || b["region"] != "r" || b["path_style"] != true {
		t.Fatalf("body %s", recs[0].body)
	}
}

func TestScheduleAdd(t *testing.T) {
	code, _, errs, recs := run(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Write([]byte(`[{"id":4,"name":"minio2"}]`))
			return
		}
		w.WriteHeader(201)
		w.Write([]byte(`{"id":2}`))
	}, "schedule", "add", "db1", "--dest", "minio2", "--cron", "5 3 * * *", "--keep", "3")
	if code != 0 || len(recs) != 2 || recs[1].path != "/api/databases/db1/schedules" {
		t.Fatalf("%d %q %+v", code, errs, recs)
	}
	var b map[string]any
	json.Unmarshal([]byte(recs[1].body), &b)
	if b["destination_id"] != float64(4) || b["cron"] != "5 3 * * *" || b["keep_latest"] != float64(3) || b["prefix"] != "fridge" || b["enabled"] != true {
		t.Fatalf("body %s", recs[1].body)
	}
}

func TestAllowlistSet(t *testing.T) {
	code, out, errs, recs := run(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"cidrs":["10.0.0.0/8","1.2.3.4/32"]}`))
	}, "allowlist", "set", "10.0.0.0/8", "1.2.3.4/32")
	if code != 0 || len(recs) != 1 || recs[0].method != "PUT" || recs[0].path != "/api/settings/allowlist" ||
		recs[0].body != `{"cidrs":["10.0.0.0/8","1.2.3.4/32"]}` || !strings.Contains(out, "1.2.3.4/32") {
		t.Fatalf("%d %q %q %+v", code, out, errs, recs)
	}
}
