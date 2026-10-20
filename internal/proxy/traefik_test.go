package proxy

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func readYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

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

func assertNoTmp(t *testing.T, dir string) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, "*", "*.tmp"))
	if len(matches) > 0 {
		t.Fatalf("leftover temp files: %v", matches)
	}
}

func TestWriteRoute(t *testing.T) {
	dir := t.TempDir()
	r := Route{
		ID: 7, EntryPoint: "pg", Domain: "abc.com", Backend: "fridge-postgres-abc:5432",
		CertFile: "/traefik/certs/db-7.crt", KeyFile: "/traefik/certs/db-7.key",
	}
	if err := WriteRoute(dir, r); err != nil {
		t.Fatal(err)
	}
	doc := readYAML(t, filepath.Join(dir, "dynamic", "db-7.yml"))

	if got := dig(t, doc, "tcp", "routers", "db-7", "rule"); got != "HostSNI(`abc.com`)" {
		t.Errorf("rule = %v", got)
	}
	if got := dig(t, doc, "tcp", "routers", "db-7", "entryPoints", 0); got != "pg" {
		t.Errorf("entryPoints[0] = %v", got)
	}
	if got := dig(t, doc, "tcp", "routers", "db-7", "middlewares", 0); got != "allowlist-tcp@file" {
		t.Errorf("middlewares[0] = %v", got)
	}
	if got := dig(t, doc, "tcp", "routers", "db-7", "service"); got != "db-7" {
		t.Errorf("service = %v", got)
	}
	if _, ok := dig(t, doc, "tcp", "routers", "db-7", "tls").(map[string]any); !ok {
		t.Errorf("router missing tls: {}")
	}
	if got := dig(t, doc, "tcp", "services", "db-7", "loadBalancer", "servers", 0, "address"); got != "fridge-postgres-abc:5432" {
		t.Errorf("address = %v", got)
	}
	if got := dig(t, doc, "tls", "certificates", 0, "certFile"); got != "/traefik/certs/db-7.crt" {
		t.Errorf("certFile = %v", got)
	}
	if got := dig(t, doc, "tls", "certificates", 0, "keyFile"); got != "/traefik/certs/db-7.key" {
		t.Errorf("keyFile = %v", got)
	}
	assertNoTmp(t, dir)
}

func TestRemoveRouteIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := WriteRoute(dir, Route{ID: 7, EntryPoint: "pg", Domain: "abc.com", Backend: "x:5432"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := RemoveRoute(dir, 7); err != nil {
			t.Fatalf("RemoveRoute call %d: %v", i+1, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "dynamic", "db-7.yml")); !os.IsNotExist(err) {
		t.Errorf("route file still present: %v", err)
	}
}

func TestWriteCert(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, err := WriteCert(dir, 7, []byte("CERT"), []byte("KEY"))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(certFile) || !filepath.IsAbs(keyFile) {
		t.Fatalf("paths not absolute: %s %s", certFile, keyFile)
	}
	if filepath.Base(certFile) != "db-7.crt" || filepath.Base(keyFile) != "db-7.key" {
		t.Errorf("unexpected names: %s %s", certFile, keyFile)
	}
	if fi, err := os.Stat(keyFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("key perm: %v %v", fi, err)
	}
	if fi, err := os.Stat(certFile); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("cert perm: %v %v", fi, err)
	}
	assertNoTmp(t, dir)

	if err := RemoveCert(dir, 7); err != nil {
		t.Fatal(err)
	}
	if err := RemoveCert(dir, 7); err != nil {
		t.Fatalf("RemoveCert not idempotent: %v", err)
	}
	if _, err := os.Stat(certFile); !os.IsNotExist(err) {
		t.Errorf("cert still present: %v", err)
	}
}

func TestWriteBase(t *testing.T) {
	dir := t.TempDir()
	cidrs := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
	if err := WriteBase(dir, cidrs, "http://fridge:8080"); err != nil {
		t.Fatal(err)
	}
	doc := readYAML(t, filepath.Join(dir, "dynamic", "_base.yml"))

	for _, path := range [][]any{
		{"tcp", "middlewares", "allowlist-tcp", "ipAllowList", "sourceRange"},
		{"http", "middlewares", "allowlist-http", "ipAllowList", "sourceRange"},
	} {
		list, ok := dig(t, doc, path...).([]any)
		if !ok || len(list) != len(cidrs) {
			t.Fatalf("%v: got %v", path, dig(t, doc, path...))
		}
		for i, c := range cidrs {
			if list[i] != c {
				t.Errorf("%v[%d] = %v, want %s", path, i, list[i], c)
			}
		}
	}
	if got := dig(t, doc, "http", "services", "fridge-api", "loadBalancer", "servers", 0, "url"); got != "http://fridge:8080" {
		t.Errorf("api url = %v", got)
	}
	if got := dig(t, doc, "http", "routers", "fridge-api", "middlewares", 0); got != "allowlist-http@file" {
		t.Errorf("api middleware = %v", got)
	}
	if got := dig(t, doc, "http", "routers", "fridge-api", "rule"); got != "PathPrefix(`/`)" {
		t.Errorf("api rule = %v", got)
	}
	assertNoTmp(t, dir)
}
