package pki

import (
	"crypto/tls"
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

func TestIssueNames(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	certPEM, _, err := ca.IssueNames([]string{"localhost", "127.0.0.1", "fridge.lan", "192.168.1.10", "::1"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.DNSNames) != 2 || leaf.DNSNames[0] != "localhost" || leaf.DNSNames[1] != "fridge.lan" {
		t.Fatalf("DNS names %v", leaf.DNSNames)
	}
	if len(leaf.IPAddresses) != 3 || leaf.IPAddresses[0].String() != "127.0.0.1" || leaf.IPAddresses[1].String() != "192.168.1.10" || leaf.IPAddresses[2].String() != "::1" {
		t.Fatalf("IPs %v", leaf.IPAddresses)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	for _, name := range []string{"localhost", "fridge.lan", "127.0.0.1", "192.168.1.10"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: name}); err != nil {
			t.Errorf("verify %s: %v", name, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "evil.lan"}); err == nil {
		t.Error("verified a name that is not in the certificate")
	}
	for _, bad := range [][]string{nil, {""}, {"a", ""}} {
		if _, _, err := ca.IssueNames(bad, time.Hour); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSelfSignedClientIsItsOwnAnchor(t *testing.T) {
	certPEM, keyPEM, err := SelfSignedClient("edge-k1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("does not verify against itself: %v", err)
	}
	other, _, _ := SelfSignedClient("edge-k2", time.Hour)
	otherRoots := x509.NewCertPool()
	otherRoots.AppendCertsFromPEM(other)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: otherRoots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
		t.Fatal("verifies against another database's certificate")
	}
}

func TestRemainingStopsAtTheCA(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := ca.Remaining(time.Hour); got != time.Hour {
		t.Fatalf("short request: %v", got)
	}
	if got := ca.Remaining(100 * 365 * 24 * time.Hour); got >= caValidFor || got < caValidFor-48*time.Hour {
		t.Fatalf("long request must stop just before the CA expires: %v", got)
	}
	if _, _, err := ca.IssueNames([]string{"x"}, ca.Remaining(100*365*24*time.Hour)); err != nil {
		t.Fatalf("the longest allowed leaf is refused: %v", err)
	}
}

func TestNewCAIsAUsableIndependentCA(t *testing.T) {
	c1, k1, err := NewCA("Fridge CA - alice")
	if err != nil {
		t.Fatal(err)
	}
	c2, k2, _ := NewCA("Fridge CA - bob")
	alice, err := Parse(c1, k1)
	if err != nil {
		t.Fatal(err)
	}
	bob, _ := Parse(c2, k2)
	leafPEM, _, err := alice.IssueNames([]string{"db.alice.lan"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode(leafPEM)
	leaf, _ := x509.ParseCertificate(b.Bytes)
	verify := func(ca *CA) error {
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM(ca.CertPEM())
		_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "db.alice.lan"})
		return err
	}
	if err := verify(alice); err != nil {
		t.Fatalf("its own CA does not verify its leaf: %v", err)
	}
	if err := verify(bob); err == nil {
		t.Fatal("another tenant's CA verifies the leaf")
	}
	if _, err := Parse(c1, k2); err == nil {
		t.Fatal("a key that does not belong to the certificate was accepted")
	}
}
