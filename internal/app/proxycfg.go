package app

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"fridge/internal/engine"
	"fridge/internal/proxy"
	"fridge/internal/swarm"
)

// ProxyConfig is what Traefik should be routing right now: every running database, the web/API router and its
// certificate. Traefik polls it, so changing a database, the allowlist or a certificate needs no further step.
// A database's route certificate is its node server certificate (see store.Route for why they must be one).
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
		if r.CertPEM == "" {
			log.Printf("proxy config: database %d has no node certificate and is not routed", r.DatabaseID)
			continue
		}
		_, info, err := engine.Get(r.Engine)
		if err != nil {
			return proxy.Config{}, err
		}
		keyPEM, err := a.Box.Decrypt(r.KeyEnc)
		if err != nil {
			return proxy.Config{}, fmt.Errorf("certificate for database %d: %w", r.DatabaseID, err)
		}
		cfg.DBs = append(cfg.DBs, proxy.DB{ID: r.DatabaseID, EntryPoint: info.EntryPoint, Domain: r.Domain,
			Backend: fmt.Sprintf("%s:%d", swarm.EdgeService(r.NodeKey), swarm.EdgePort), CertPEM: r.CertPEM, KeyPEM: keyPEM})
	}
	return cfg, nil
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
