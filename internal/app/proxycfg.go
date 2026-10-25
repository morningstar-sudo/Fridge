package app

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"fridge/internal/engine"
	"fridge/internal/proxy"
	"fridge/internal/store"
	"fridge/internal/swarm"
)

const (
	dbCertValidity = 365 * 24 * time.Hour
	dbCertRenewIn  = 30 * 24 * time.Hour
)

// ProxyConfig is what Traefik should be routing right now: every running database, the web/API router and its
// certificate. Traefik polls it, so changing a database, the allowlist or a certificate needs no further step.
// A database without a certificate (or with one close to expiry) gets a fresh one here.
func (a *App) ProxyConfig(ctx context.Context) (proxy.Config, error) {
	allow, err := a.Allowlist(ctx)
	if err != nil {
		return proxy.Config{}, err
	}
	webCert, err := os.ReadFile(a.WebCert)
	if err != nil {
		return proxy.Config{}, fmt.Errorf("web certificate: %w", err)
	}
	webKey, err := os.ReadFile(a.WebKey)
	if err != nil {
		return proxy.Config{}, fmt.Errorf("web certificate: %w", err)
	}
	routes, err := a.Store.ListRoutes(ctx)
	if err != nil {
		return proxy.Config{}, err
	}
	cfg := proxy.Config{Allowlist: allow, APIBackend: a.APIBackend, WebCertPEM: string(webCert), WebKeyPEM: string(webKey)}
	for _, r := range routes {
		_, info, err := engine.Get(r.Engine)
		if err != nil {
			return proxy.Config{}, err
		}
		certPEM, keyPEM, err := a.routeCert(ctx, r)
		if err != nil {
			return proxy.Config{}, fmt.Errorf("certificate for database %d: %w", r.DatabaseID, err)
		}
		cfg.DBs = append(cfg.DBs, proxy.DB{ID: r.DatabaseID, EntryPoint: info.EntryPoint, Domain: r.Domain,
			Backend: fmt.Sprintf("%s:%d", swarm.EdgeService(r.NodeKey), swarm.EdgePort), CertPEM: certPEM, KeyPEM: keyPEM})
	}
	return cfg, nil
}

// routeCert returns the certificate of route r as PEM, issuing a new one when it has none or is about to expire.
func (a *App) routeCert(ctx context.Context, r store.Route) (certPEM, keyPEM string, err error) {
	if r.CertPEM != "" && r.KeyEnc != "" && time.Until(r.NotAfter) > dbCertRenewIn {
		keyPEM, err = a.Box.Decrypt(r.KeyEnc)
		return r.CertPEM, keyPEM, err
	}
	// two concurrent polls may both issue; the stored one wins and the next poll serves it
	return a.issueCert(ctx, r.DatabaseID, r.Domain)
}

// issueCert signs a certificate for domain and stores it for database id (the key encrypted).
func (a *App) issueCert(ctx context.Context, id int64, domain string) (certPEM, keyPEM string, err error) {
	c, k, err := a.CA.Issue(domain, dbCertValidity)
	if err != nil {
		return "", "", err
	}
	notAfter, err := certNotAfter(c)
	if err != nil {
		return "", "", err
	}
	enc, err := a.Box.Encrypt(string(k))
	if err != nil {
		return "", "", err
	}
	if err := a.Store.SetDBCert(ctx, id, string(c), enc, notAfter); err != nil {
		return "", "", err
	}
	return string(c), string(k), nil
}

func certNotAfter(certPEM []byte) (time.Time, error) {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return time.Time{}, errors.New("certificate is not PEM")
	}
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return time.Time{}, err
	}
	return c.NotAfter, nil
}
