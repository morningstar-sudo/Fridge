package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"fridge/internal/pki"
)

const (
	webCertValidFor = 365 * 24 * time.Hour
	webCertRenewAt  = 30 * 24 * time.Hour // reissue when less than this is left
)

// webCertNames are the names the web UI is reached by: localhost, 127.0.0.1 and FRIDGE_PUBLIC_HOSTS.
func webCertNames(hosts string) []string {
	names := []string{"localhost", "127.0.0.1"}
	for _, h := range strings.Split(hosts, ",") {
		if h = strings.TrimSpace(h); h != "" && !slices.Contains(names, h) {
			names = append(names, h)
		}
	}
	return names
}

// sans lists a certificate's names, sorted, IPs in their canonical text form.
func sans(c *x509.Certificate) []string {
	out := slices.Clone(c.DNSNames)
	for _, ip := range c.IPAddresses {
		out = append(out, ip.String())
	}
	slices.Sort(out)
	return out
}

// ensureWebCert makes sure certFile/keyFile hold a certificate signed by ca for exactly names, with at least
// webCertRenewAt left; otherwise it issues a new one and replaces the files (key first, then the cert, each
// atomically). It reports whether it wrote anything.
func ensureWebCert(ca *pki.CA, certFile, keyFile string, names []string) (bool, error) {
	if current(ca, certFile, keyFile, names) {
		return false, nil
	}
	certPEM, keyPEM, err := ca.IssueNames(names, webCertValidFor)
	if err != nil {
		return false, err
	}
	if err := writeAtomic(keyFile, keyPEM, 0o600); err != nil {
		return false, err
	}
	return true, writeAtomic(certFile, certPEM, 0o644)
}

func current(ca *pki.CA, certFile, keyFile string, names []string) bool {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile) // also proves the key belongs to the cert
	if err != nil {
		return false
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || time.Until(leaf.NotAfter) < webCertRenewAt {
		return false
	}
	want := make([]string, 0, len(names))
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			n = ip.String()
		}
		want = append(want, n)
	}
	slices.Sort(want)
	if !slices.Equal(want, sans(leaf)) {
		return false
	}
	roots := x509.NewCertPool()
	block, _ := pem.Decode(ca.CertPEM())
	if block == nil {
		return false
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	roots.AddCert(caCert)
	_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err == nil
}

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp, perm)); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
