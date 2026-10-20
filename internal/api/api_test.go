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
	"strconv"
	"strings"
	"testing"
	"time"

	"fridge/internal/app"
	"fridge/internal/container"
	"fridge/internal/objstore"
	"fridge/internal/pki"
	"fridge/internal/secret"
	"fridge/internal/store"
	"fridge/internal/store/storetest"
)

type fakeRT struct{ pingErr error }

func (f *fakeRT) Ping(context.Context) error { return f.pingErr }
func (f *fakeRT) Run(_ context.Context, s container.RunSpec) (string, error) {
	return "ctr-" + s.Name, nil
}
func (f *fakeRT) WaitHealthy(context.Context, string, time.Duration) error { return nil }
func (f *fakeRT) Exec(_ context.Context, _ string, _ []string, _ io.Reader, _, _ io.Writer) (int, error) {
	return 0, nil
}
func (f *fakeRT) Remove(context.Context, string, string) error { return nil }

type fakeObjs struct {
	testErr error
	list    []objstore.Object
	meta    map[string]string
}

func (f *fakeObjs) Test(context.Context) error { return f.testErr }
func (f *fakeObjs) Put(_ context.Context, _ string, r io.Reader, _ map[string]string) (int64, error) {
	return io.Copy(io.Discard, r)
}
func (f *fakeObjs) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (f *fakeObjs) Meta(context.Context, string) (map[string]string, error) { return f.meta, nil }
func (f *fakeObjs) List(context.Context, string, string, int) ([]objstore.Object, bool, error) {
	return f.list, true, nil
}
func (f *fakeObjs) Delete(context.Context, string) error { return nil }
func (f *fakeObjs) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "http://presigned", nil
}

const tok = "s3cret-token"

func newSrv(t *testing.T) (http.Handler, *fakeRT, *app.App) {
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
	a := &app.App{Store: storetest.New(t), Box: box, RT: rt, CA: ca, TraefikDir: t.TempDir(), HealthTimeout: time.Second}
	a.OpenObjects = func(store.Destination, string) (app.ObjectStore, error) { return &fakeObjs{}, nil }
	return New(a, tok), rt, a
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

func TestEmptyTokenRejectsAll(t *testing.T) {
	_, _, a := newSrv(t)
	h := New(a, "")
	r := httptest.NewRequest("GET", "/api/engines", nil)
	r.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
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
	if strings.Contains(l, "password_enc") || strings.Contains(l, "connection_string") || !strings.Contains(l, `"abc"`) {
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
	json.Unmarshal(do(h, "GET", "/api/databases/abc", "", true).Body.Bytes(), &a)
	json.Unmarshal(do(h, "GET", "/api/databases/"+strconv.FormatInt(a.ID, 10), "", true).Body.Bytes(), &b)
	if a.ID == 0 || a.ID != b.ID || a.ConnectionString == "" {
		t.Fatal(a, b)
	}
}

func TestBackupNowAccepted(t *testing.T) {
	h, _, a := newSrv(t)
	do(h, "POST", "/api/databases", `{"engine":"postgres","name":"abc","domain":"abc.com"}`, true)
	do(h, "POST", "/api/destinations", `{"name":"d","endpoint":"https://s3.example.com","bucket":"b","access_key":"AK","secret_key":"x"}`, true)
	w := do(h, "POST", "/api/databases/abc/backups", `{}`, true)
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
