package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// dig walks nested maps (string key) and lists (int index).
func dig(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("at %v: not a map", p)
			}
			v = m[k]
		case int:
			s, ok := v.([]any)
			if !ok || k >= len(s) {
				t.Fatalf("at %v: not a list of that length", p)
			}
			v = s[k]
		}
	}
	return v
}

// roundTrip is the document as Traefik sees it: JSON.
func roundTrip(t *testing.T, c Config) map[string]any {
	t.Helper()
	b, err := json.Marshal(Build(c))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

var base = Config{Allowlist: []string{"10.0.0.0/8", "203.0.113.0/24"}, APIBackend: "http://fridge:8080", WebCertPEM: "WEBCERT", WebKeyPEM: "WEBKEY"}

func TestBuildDatabaseRoute(t *testing.T) {
	c := base
	c.DBs = []DB{{ID: 7, EntryPoint: "pg", Domain: "abc.com", Backend: "fridge-edge-k1:5432", CertPEM: "CERT7", KeyPEM: "KEY7"}}
	doc := roundTrip(t, c)

	if got := dig(t, doc, "tcp", "routers", "db-7", "rule"); got != "HostSNI(`abc.com`)" {
		t.Errorf("rule = %v", got)
	}
	if got := dig(t, doc, "tcp", "routers", "db-7", "entryPoints", 0); got != "pg" {
		t.Errorf("entryPoint = %v", got)
	}
	if got := dig(t, doc, "tcp", "routers", "db-7", "tls", "options"); got != "pg" {
		t.Errorf("tls options = %v", got)
	}
	if _, ok := dig(t, doc, "tcp", "routers", "db-7").(map[string]any)["middlewares"]; ok {
		t.Error("the database port has no IP allowlist: the routing mesh hides client addresses")
	}
	if got := dig(t, doc, "tcp", "services", "db-7", "loadBalancer", "servers", 0, "address"); got != "fridge-edge-k1:5432" {
		t.Errorf("backend = %v", got)
	}
	if got := dig(t, doc, "tls", "options", "pg", "alpnProtocols", 0); got != "postgresql" {
		t.Errorf("alpn = %v", got)
	}
	if got := dig(t, doc, "tls", "certificates", 0, "certFile"); got != "CERT7" {
		t.Errorf("cert = %v", got)
	}
	if got := dig(t, doc, "tls", "certificates", 0, "keyFile"); got != "KEY7" {
		t.Errorf("key = %v", got)
	}
}

func TestBuildWebRouter(t *testing.T) {
	doc := roundTrip(t, base)
	if _, ok := doc["tcp"]; ok {
		t.Error("no databases: no tcp section (Traefik rejects empty routers)")
	}
	if got := dig(t, doc, "http", "middlewares", "allowlist-http", "ipAllowList", "sourceRange", 1); got != "203.0.113.0/24" {
		t.Errorf("allowlist = %v", got)
	}
	if got := dig(t, doc, "http", "routers", "fridge-web", "middlewares", 0); got != "allowlist-http" {
		t.Errorf("web middleware = %v", got)
	}
	if got := dig(t, doc, "http", "routers", "fridge-web", "entryPoints", 0); got != "websecure" {
		t.Errorf("web entrypoint = %v", got)
	}
	if got := dig(t, doc, "http", "routers", "fridge-redirect", "middlewares", 0); got != "to-https" {
		t.Errorf("redirect = %v", got)
	}
	if got := dig(t, doc, "http", "services", "fridge-api", "loadBalancer", "servers", 0, "url"); got != "http://fridge:8080" {
		t.Errorf("api backend = %v", got)
	}
	if got := dig(t, doc, "tls", "stores", "default", "defaultCertificate", "certFile"); got != "WEBCERT" {
		t.Errorf("default cert = %v", got)
	}
	if _, ok := doc["tls"].(map[string]any)["certificates"]; ok {
		t.Error("no databases: no certificates list")
	}
}

func TestHandlerAuth(t *testing.T) {
	h := Handler("s3cret-token", func(*http.Request) (Config, error) { return base, nil })
	get := func(path, auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, auth := range []string{"", "Bearer wrong", "s3cret-token", "Bearer s3cret-token "} {
		if w := get("/traefik", auth); w.Code != 401 || w.Body.Len() > 20 {
			t.Errorf("auth %q: %d %q", auth, w.Code, w.Body)
		}
	}
	w := get("/traefik", "Bearer s3cret-token")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %s", w.Code, w.Header())
	}
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || doc["http"] == nil {
		t.Fatalf("body %s: %v", w.Body, err)
	}
	if w := get("/other", "Bearer s3cret-token"); w.Code != 404 {
		t.Errorf("other path: %d", w.Code)
	}
}

func TestHandlerWithoutTokenServesNothing(t *testing.T) {
	h := Handler("", func(*http.Request) (Config, error) { return base, nil })
	r := httptest.NewRequest("GET", "/traefik", nil)
	r.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("empty token must not open the endpoint: %d", w.Code)
	}
}

func TestHandlerBuildErrorKeepsTraefikConfig(t *testing.T) {
	h := Handler("t", func(*http.Request) (Config, error) { return Config{}, errors.New("db down") })
	r := httptest.NewRequest("GET", "/traefik", nil)
	r.Header.Set("Authorization", "Bearer t")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatalf("%d", w.Code)
	}
}
