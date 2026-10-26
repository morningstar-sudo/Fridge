package app

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"fridge/internal/engine"
	"fridge/internal/mysqlproxy"
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
		if info.Route != "sni" { // routed by the MySQL router, not by Traefik
			continue
		}
		keyPEM, err := a.Box.Decrypt(r.KeyEnc)
		if err != nil {
			return proxy.Config{}, fmt.Errorf("certificate for database %d: %w", r.DatabaseID, err)
		}
		cfg.DBs = append(cfg.DBs, proxy.DB{ID: r.DatabaseID, EntryPoint: info.EntryPoint, ALPN: info.ALPN, Domain: r.Domain,
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

// mysqlFront is the one certificate MySQL/MariaDB clients are shown (the protocol has no SNI to choose by): it names every
// MySQL/MariaDB domain, is signed by the instance CA and is reissued when the set of names changes or it is 30 days from expiry.
type mysqlFront struct {
	mu       sync.Mutex
	names    string
	cert     []byte
	key      []byte
	notAfter time.Time
}

func (a *App) mysqlFrontend(domains []string) (certPEM, keyPEM []byte, err error) {
	slices.Sort(domains)
	domains = slices.Compact(domains)
	if len(domains) == 0 {
		domains = []string{"mysql.fridge.invalid"} // nothing to serve yet; the router still needs a certificate
	}
	names := strings.Join(domains, ",")
	f := &a.front
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.names == names && time.Until(f.notAfter) > 30*24*time.Hour {
		return f.cert, f.key, nil
	}
	c, k, err := a.CA.IssueNames(domains, a.CA.Remaining(90*24*time.Hour))
	if err != nil {
		return nil, nil, err
	}
	na, err := certNotAfter(c)
	if err != nil {
		return nil, nil, err
	}
	f.names, f.cert, f.key, f.notAfter = names, c, k, na
	return c, k, nil
}

// MySQLRouterConfig is what the MySQL router should serve right now: each running MySQL/MariaDB database's users (app
// user and superuser) with where to find the database and the node's client certificate, and the one frontend certificate.
func (a *App) MySQLRouterConfig(ctx context.Context) (mysqlproxy.Document, error) {
	dbs, err := a.Store.ListDatabases(ctx)
	if err != nil {
		return mysqlproxy.Document{}, err
	}
	doc := mysqlproxy.Document{Users: map[string]mysqlproxy.Node{}}
	var domains []string
	for _, d := range dbs {
		eng, info, err := engine.Get(d.Engine)
		if err != nil || info.Route != "mysql" || d.Status != "running" || d.NodeKey == "" {
			continue
		}
		t, err := a.nodeTLS(ctx, d)
		if err != nil {
			log.Printf("mysql router config: %v", err)
			continue
		}
		p := nodeParams(d)
		n := mysqlproxy.Node{Addr: fmt.Sprintf("%s:%d", swarm.DBService(d.NodeKey), eng.Spec(p).Port),
			Cert: t.ClientCert, Key: t.ClientKey, CA: t.CA, ServerName: swarm.DBService(d.NodeKey)}
		doc.Users[d.User] = n
		if d.AdminUser != "" {
			doc.Users[d.AdminUser] = n
		}
		domains = append(domains, d.Domain)
	}
	c, k, err := a.mysqlFrontend(domains)
	if err != nil {
		return mysqlproxy.Document{}, err
	}
	doc.Frontend = mysqlproxy.Pair{Cert: string(c), Key: string(k)}
	return doc, nil
}
