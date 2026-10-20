package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
