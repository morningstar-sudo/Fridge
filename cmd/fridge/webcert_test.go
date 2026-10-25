package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"fridge/internal/pki"
)

func newCA(t *testing.T) *pki.CA {
	t.Helper()
	ca, err := pki.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func TestWebCertNames(t *testing.T) {
	got := webCertNames(" fridge.lan, 192.168.1.10 ,,localhost,fridge.lan")
	want := []string{"localhost", "127.0.0.1", "fridge.lan", "192.168.1.10"}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%v", got)
		}
	}
	if n := webCertNames(""); len(n) != 2 {
		t.Fatalf("%v", n)
	}
}

func TestEnsureWebCert(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "certs", "web.crt"), filepath.Join(dir, "certs", "web.key")
	ca := newCA(t)
	names := webCertNames("fridge.lan")
	read := func() (string, string) {
		c, _ := os.ReadFile(cert)
		k, _ := os.ReadFile(key)
		return string(c), string(k)
	}

	if changed, err := ensureWebCert(ca, cert, key, names); err != nil || !changed {
		t.Fatalf("first call: %v %v", changed, err)
	}
	c1, k1 := read()
	if fi, _ := os.Stat(key); fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", fi.Mode().Perm())
	}
	// same names: untouched
	if changed, err := ensureWebCert(ca, cert, key, names); err != nil || changed {
		t.Fatalf("second call: %v %v", changed, err)
	}
	if c2, k2 := read(); c2 != c1 || k2 != k1 {
		t.Fatal("files changed although nothing did")
	}
	// the order of names does not matter
	if changed, _ := ensureWebCert(ca, cert, key, []string{"fridge.lan", "127.0.0.1", "localhost"}); changed {
		t.Fatal("reissued for a reordering")
	}
	// other names: reissued
	if changed, err := ensureWebCert(ca, cert, key, webCertNames("fridge.lan,10.0.0.5")); err != nil || !changed {
		t.Fatalf("new host: %v %v", changed, err)
	}
	if c3, _ := read(); c3 == c1 {
		t.Fatal("certificate not replaced")
	}
	// a certificate with under 30 days left is renewed
	c3, k3 := read()
	soon, soonKey, err := ca.IssueNames(webCertNames("fridge.lan,10.0.0.5"), 29*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(cert, soon, 0o644)
	os.WriteFile(key, soonKey, 0o600)
	if changed, err := ensureWebCert(ca, cert, key, webCertNames("fridge.lan,10.0.0.5")); err != nil || !changed {
		t.Fatalf("29 days left: %v %v", changed, err)
	}
	if c4, k4 := read(); c4 == string(soon) || (c4 == c3 && k4 == k3) {
		t.Fatal("expiring certificate kept")
	}
	// a certificate from another CA, or a key that does not fit, is replaced
	other, otherKey, _ := newCA(t).IssueNames(names, 365*24*time.Hour)
	os.WriteFile(cert, other, 0o644)
	os.WriteFile(key, otherKey, 0o600)
	if changed, err := ensureWebCert(ca, cert, key, names); err != nil || !changed {
		t.Fatalf("foreign CA: %v %v", changed, err)
	}
	cur, _ := read()
	os.WriteFile(key, otherKey, 0o600)
	if changed, err := ensureWebCert(ca, cert, key, names); err != nil || !changed {
		t.Fatalf("mismatched key: %v %v", changed, err)
	}
	if now, _ := read(); now == cur {
		t.Fatal("certificate kept with a key that does not fit")
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "certs", "*.tmp")); len(matches) > 0 {
		t.Fatalf("leftover temp files: %v", matches)
	}
}
