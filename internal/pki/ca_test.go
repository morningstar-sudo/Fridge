package pki

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIssueVerifiesAgainstCA(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ca.Issue("abc.com", 365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(keyPEM) == 0 {
		t.Fatal("empty key PEM")
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("CA PEM not parseable")
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("leaf PEM not decodable")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	opts := x509.VerifyOptions{Roots: pool, DNSName: "abc.com"}
	if _, err := leaf.Verify(opts); err != nil {
		t.Fatalf("verify abc.com: %v", err)
	}
	opts.DNSName = "xyz.com"
	if _, err := leaf.Verify(opts); err == nil {
		t.Fatal("verify xyz.com: want error, got nil")
	}
}

func TestLoadOrCreateIsStable(t *testing.T) {
	dir := t.TempDir()
	ca1, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	ca2, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(ca1.CertPEM()) != string(ca2.CertPEM()) {
		t.Fatal("CertPEM differs between loads")
	}
	info, err := os.Stat(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("ca.key mode = %o, want 600", mode)
	}
}

func TestLoadOrCreateCreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "pki")
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.crt")); err != nil {
		t.Fatal(err)
	}
}

func TestLoadOrCreateRejectsBrokenState(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	crt := filepath.Join(dir, "ca.crt")
	key := filepath.Join(dir, "ca.key")
	crtBytes, _ := os.ReadFile(crt)
	keyBytes, _ := os.ReadFile(key)

	// Key missing: must not regenerate.
	os.Remove(key)
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("missing ca.key: want error")
	}
	os.WriteFile(key, keyBytes, 0o600)

	// Corrupt cert.
	os.WriteFile(crt, []byte("garbage"), 0o644)
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("corrupt ca.crt: want error")
	}
	os.WriteFile(crt, crtBytes, 0o644)

	// Mismatched key: a different CA's key.
	otherDir := t.TempDir()
	if _, err := LoadOrCreate(otherDir); err != nil {
		t.Fatal(err)
	}
	otherKey, _ := os.ReadFile(filepath.Join(otherDir, "ca.key"))
	os.WriteFile(key, otherKey, 0o600)
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("mismatched ca.key: want error")
	}
}
