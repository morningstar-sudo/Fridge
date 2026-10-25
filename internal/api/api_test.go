package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"fridge/internal/app"
	"fridge/internal/audit"
	"fridge/internal/auth"
	"fridge/internal/job"
	"fridge/internal/objstore"
	"fridge/internal/pki"
	"fridge/internal/secret"
	"fridge/internal/store"
	"fridge/internal/store/storetest"
	"fridge/internal/swarm"
)

// fakeRT is a swarm that only agrees: nodes come up, jobs succeed (a dump reports the size of dump).
type fakeRT struct {
	pingErr error
	dump    string        // what a backup's dump job pretends to have uploaded
	gate    chan struct{} // non-nil: every job waits until it is closed
}

func (f *fakeRT) Ping(context.Context) error { return f.pingErr }
func (f *fakeRT) PickHost(context.Context) (swarm.Host, error) {
	return swarm.Host{ID: "host1", Hostname: "n1"}, nil
}
func (f *fakeRT) CreateNode(context.Context, swarm.NodeSpec) error       { return nil }
func (f *fakeRT) NodeReady(context.Context, string, time.Duration) error { return nil }
func (f *fakeRT) RemoveNode(context.Context, swarm.NodeRef) error        { return nil }
func (f *fakeRT) ListNodes(context.Context) ([]swarm.NodeState, error)   { return nil, nil }
func (f *fakeRT) Subnets(context.Context) ([]netip.Prefix, error)        { return nil, nil }
func (f *fakeRT) RunJob(_ context.Context, j swarm.JobSpec) (swarm.JobResult, error) {
	if f.gate != nil {
		<-f.gate
	}
	size := len(f.dump)
	if size == 0 {
		size = 4
	}
	res := func(v any) string { b, _ := json.Marshal(v); return job.ResultPrefix + string(b) + "\n" }
	for _, e := range j.Env {
		if strings.Contains(e, `"Op":"check"`) {
			return swarm.JobResult{Stdout: res(map[string]any{"format": "custom", "bytes": size})}, nil
		}
	}
	return swarm.JobResult{Stdout: res(map[string]any{"size": size})}, nil
}

// noAdmin lets provisioning succeed without a database to talk to.
type noAdmin struct{}

func (noAdmin) Exec(context.Context, string, string) error { return nil }

type fakeObjs struct {
	testErr error
	list    []objstore.Object
	meta    map[string]string
	body    string // what Get returns
	head    []byte // what ReadHead returns
}

func (f *fakeObjs) Test(context.Context) error { return f.testErr }
func (f *fakeObjs) Put(_ context.Context, _ string, r io.Reader, _ map[string]string) (int64, error) {
	return io.Copy(io.Discard, r)
}
func (f *fakeObjs) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(f.body)), nil
}
func (f *fakeObjs) Meta(context.Context, string) (map[string]string, error) { return f.meta, nil }
func (f *fakeObjs) List(context.Context, string, string, int) ([]objstore.Object, bool, error) {
	return f.list, true, nil
}
func (f *fakeObjs) Delete(context.Context, string) error { return nil }

var tok string // session ID of the admin "alice", set by newSrv (tests here do not run in parallel)

func newSrv(t *testing.T) (http.Handler, *fakeRT, *app.App) {
	t.Helper()
	h, rt, a := newRawSrv(t, "setup-tok", Options{})
	setupAlice(t, h)
	_, tok = loginAs(t, h, "alice", pwAlice)
	regDomains(t, a)
	return h, rt, a
}

func newRawSrv(t *testing.T, setupToken string, o Options) (http.Handler, *fakeRT, *app.App) {
	t.Helper()
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
	rt := &fakeRT{}
	st := storetest.New(t)
	a := &app.App{Store: st, Box: box, RT: rt, Admin: noAdmin{}, JobImage: "fridge-job:test", EgressNetwork: "fridge_egress", CA: ca, TraefikDir: t.TempDir(), HealthTimeout: time.Second, Audit: &audit.Logger{Store: st}, WebCert: "/traefik/certs/web.crt", WebKey: "/traefik/certs/web.key"}
	head := dumpFile(t, "pg17.dump") // every object is restorable
	a.OpenObjects = func(store.Destination, string, *objstore.Guard) (app.ObjectStore, error) {
		return &fakeObjs{head: head}, nil
	}
	au, err := auth.New(a.Store, key, setupToken)
	if err != nil {
		t.Fatal(err)
	}
	return New(a, au, o), rt, a
}

func do(h http.Handler, method, path, body string, auth bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if auth {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAuthRequired(t *testing.T) {
	h, _, _ := newSrv(t)
	if w := do(h, "GET", "/api/engines", "", false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	r := httptest.NewRequest("GET", "/api/engines", nil)
	r.Header.Set("Authorization", "Bearer nope")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := do(h, "GET", "/api/engines", "", true); w.Code != 200 {
		t.Fatal(w.Code)
	}
}

func TestHealthEndpoints(t *testing.T) {
	h, rt, _ := newSrv(t)
	if w := do(h, "GET", "/healthz", "", false); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := do(h, "GET", "/readyz", "", false); w.Code != 200 {
		t.Fatal(w.Code)
	}
	rt.pingErr = errors.New("docker exploded")
	w := do(h, "GET", "/readyz", "", false)
	if w.Code != 503 || strings.Contains(w.Body.String(), "exploded") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestErrorMapping(t *testing.T) {
	h, _, _ := newSrv(t)
	if w := do(h, "POST", "/api/databases", `{"engine":"postgres","name":"abc","domain":"abc.com","version":"99"}`, true); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/api/databases/nope", "", true); w.Code != 404 {
		t.Fatal(w.Code)
	}
	ok := `{"engine":"postgres","name":"abc","domain":"abc.com"}`
	if w := do(h, "POST", "/api/databases", ok, true); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := do(h, "POST", "/api/databases", ok, true); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := do(h, "POST", "/api/databases", "{", true); w.Code != 400 {
		t.Fatal(w.Code)
	}
}

func TestNoSecretsInResponses(t *testing.T) {
	h, _, _ := newSrv(t)
	body := `{"name":"d","endpoint":"https://s3.example.com","region":"us-east-1","bucket":"b","access_key":"AK","secret_key":"TOPSECRET"}`
	w := do(h, "POST", "/api/destinations", body, true)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, b := range []string{w.Body.String(), do(h, "GET", "/api/destinations", "", true).Body.String()} {
		if strings.Contains(b, "TOPSECRET") || strings.Contains(b, "secret_key") {
			t.Fatal(b)
		}
	}
	do(h, "POST", "/api/databases", `{"engine":"postgres","name":"abc","domain":"abc.com"}`, true)
	l := do(h, "GET", "/api/databases", "", true).Body.String()
	if strings.Contains(l, "password_enc") || !strings.Contains(l, `"connection_string":"postgresql://alice_abc:****@`) || !strings.Contains(l, `"alice_abc"`) {
		t.Fatal(l)
	}
}

func TestDatabaseByNameOrID(t *testing.T) {
	h, _, _ := newSrv(t)
	do(h, "POST", "/api/databases", `{"engine":"postgres","name":"abc","domain":"abc.com"}`, true)
	var a, b struct {
		ID               int64  `json:"id"`
		ConnectionString string `json:"connection_string"`
	}
	json.Unmarshal(do(h, "GET", "/api/databases/alice_abc", "", true).Body.Bytes(), &a)
	json.Unmarshal(do(h, "GET", "/api/databases/"+strconv.FormatInt(a.ID, 10), "", true).Body.Bytes(), &b)
	if a.ID == 0 || a.ID != b.ID || a.ConnectionString == "" {
		t.Fatal(a, b)
	}
}

func TestBackupNowAccepted(t *testing.T) {
	h, _, a := newSrv(t)
	do(h, "POST", "/api/databases", `{"engine":"postgres","name":"abc","domain":"abc.com"}`, true)
	do(h, "POST", "/api/destinations", `{"name":"d","endpoint":"https://s3.example.com","bucket":"b","access_key":"AK","secret_key":"x"}`, true)
	w := do(h, "POST", "/api/databases/alice_abc/backups", `{}`, true)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var out struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.ID == 0 || out.Status != "running" {
		t.Fatal(w.Body.String())
	}
	a.Wait()
}

// regDomains registers the domains the fixtures use for admin "alice".
func regDomains(t *testing.T, a *app.App) {
	t.Helper()
	u, err := a.Store.GetUserByName(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"abc.com", "abd.com", "abe.com", "copy.com"} {
		if err := a.Store.CreateDomain(context.Background(), &store.Domain{OwnerID: u.ID, Name: d}); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *fakeObjs) ReadHead(context.Context, string, int) ([]byte, error) { return f.head, nil }

// dumpFile is one of the real pg_dump files in internal/dumpcheck/testdata.
func dumpFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "dumpcheck", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
