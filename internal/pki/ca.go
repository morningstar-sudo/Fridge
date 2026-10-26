// Package pki holds the internal CA that signs Traefik's per-domain TLS certs.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	caCertFile = "ca.crt"
	caKeyFile  = "ca.key"
	caValidFor = 10 * 365 * 24 * time.Hour
)

// CA is the internal certificate authority.
type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	key     *ecdsa.PrivateKey
}

// LoadOrCreate loads the CA from dir, creating it if both files are absent.
// A partial or mismatched CA is an error: regenerating would invalidate every
// certificate the team has already installed.
func LoadOrCreate(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	crtPath := filepath.Join(dir, caCertFile)
	keyPath := filepath.Join(dir, caKeyFile)

	crtPEM, crtErr := os.ReadFile(crtPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	switch {
	case errors.Is(crtErr, os.ErrNotExist) && errors.Is(keyErr, os.ErrNotExist):
		return create(dir)
	case crtErr != nil:
		return nil, crtErr
	case keyErr != nil:
		return nil, keyErr
	}
	return parse(crtPEM, keyPEM)
}

// CertPEM returns the CA certificate in PEM form.
func (c *CA) CertPEM() []byte { return c.certPEM }

// Issue creates a ServerAuth leaf certificate for domain, valid for validFor.
// It returns the cert and an unencrypted PKCS#8 private key, both PEM.
func (c *CA) Issue(domain string, validFor time.Duration) (certPEM, keyPEM []byte, err error) {
	if domain == "" {
		return nil, nil, errors.New("pki: empty domain")
	}
	return c.IssueNames([]string{domain}, validFor)
}

// IssueNames is Issue for several names: an IP literal goes into the IP SANs, anything else into the DNS SANs.
// The first name is the common name.
func (c *CA) IssueNames(names []string, validFor time.Duration) (certPEM, keyPEM []byte, err error) {
	if len(names) == 0 || names[0] == "" {
		return nil, nil, errors.New("pki: no names")
	}
	var dns []string
	var ips []net.IP
	for _, n := range names {
		if n == "" {
			return nil, nil, errors.New("pki: empty name")
		}
		if ip := net.ParseIP(n); ip != nil {
			ips = append(ips, ip)
		} else {
			dns = append(dns, n)
		}
	}
	if validFor <= 0 {
		return nil, nil, errors.New("pki: validFor must be positive")
	}
	now := time.Now()
	notAfter := now.Add(validFor)
	if notAfter.After(c.cert.NotAfter) {
		return nil, nil, fmt.Errorf("pki: leaf would outlive CA (CA expires %s)", c.cert.NotAfter.Format(time.RFC3339))
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     dns,
		IPAddresses:  ips,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

func create(dir string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Fridge Internal CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(caValidFor),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	crtPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	// O_EXCL: never overwrite a file that appeared since we checked.
	if err := writeNew(filepath.Join(dir, caKeyFile), keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := writeNew(filepath.Join(dir, caCertFile), crtPEM, 0o644); err != nil {
		return nil, err
	}
	return parse(crtPEM, keyPEM)
}

func parse(crtPEM, keyPEM []byte) (*CA, error) {
	block, _ := pem.Decode(crtPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("pki: ca.crt is not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parse ca.crt: %w", err)
	}
	if !cert.IsCA {
		return nil, errors.New("pki: ca.crt is not a CA certificate")
	}

	kblock, _ := pem.Decode(keyPEM)
	if kblock == nil {
		return nil, errors.New("pki: ca.key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(kblock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parse ca.key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("pki: ca.key is not an ECDSA key")
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, errors.New("pki: ca.key does not match ca.crt")
	}
	return &CA{cert: cert, certPEM: crtPEM, key: key}, nil
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func writeNew(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Remaining is max, or less when the CA itself expires sooner: the longest a leaf can live (a day of margin).
func (c *CA) Remaining(max time.Duration) time.Duration {
	left := time.Until(c.cert.NotAfter) - 24*time.Hour
	if left < max {
		return left
	}
	return max
}

// SelfSignedClient makes a certificate that signs itself and is also its own trust anchor: a server whose client CA
// file holds only this certificate accepts exactly the holder of its key and nobody else.
func SelfSignedClient(commonName string, validFor time.Duration) (certPEM, keyPEM []byte, err error) {
	if commonName == "" || validFor <= 0 {
		return nil, nil, errors.New("pki: bad self-signed request")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validFor),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}
