// Package proxy builds Traefik's dynamic configuration and serves it over HTTP (Traefik's http provider),
// so Traefik needs no shared directory and can run on any swarm node: one TCP route per database (by TLS SNI),
// the HTTPS web/API router behind an IP allowlist, and the web certificate.
package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// DB is one database's TCP route. Backend is "host:port" of its edge sidecar; the PEMs are inline.
type DB struct {
	ID              int64
	Domain, Backend string
	EntryPoint      string // the engine's Traefik entry point
	CertPEM, KeyPEM string
}

// Config is everything Traefik is told. The allowlist applies to the web UI and API only: a database's port is
// open to whoever reaches it (the routing mesh hides the client address), so it is the host firewall's job.
type Config struct {
	DBs                   []DB
	Allowlist             []string
	APIBackend            string // URL of the Fridge API
	WebCertPEM, WebKeyPEM string
}

const (
	alpnPG = "postgresql" // libpq 17 sends ALPN "postgresql" and fails without an answer
	tlsPG  = "pg"
)

// Build is the document Traefik's http provider expects.
func Build(c Config) map[string]any {
	routers, services := map[string]any{}, map[string]any{}
	certs := []any{}
	for _, d := range c.DBs {
		name := fmt.Sprintf("db-%d", d.ID)
		routers[name] = map[string]any{
			"entryPoints": []string{d.EntryPoint},
			"rule":        fmt.Sprintf("HostSNI(`%s`)", d.Domain),
			"service":     name,
			"tls":         map[string]any{"options": tlsPG},
		}
		services[name] = map[string]any{"loadBalancer": map[string]any{"servers": []any{map[string]any{"address": d.Backend}}}}
		certs = append(certs, map[string]any{"certFile": d.CertPEM, "keyFile": d.KeyPEM})
	}
	tls := map[string]any{
		"options": map[string]any{tlsPG: map[string]any{"alpnProtocols": []string{alpnPG}}},
		"stores": map[string]any{"default": map[string]any{
			"defaultCertificate": map[string]any{"certFile": c.WebCertPEM, "keyFile": c.WebKeyPEM}}},
	}
	if len(certs) > 0 {
		tls["certificates"] = certs
	}
	doc := map[string]any{
		"http": map[string]any{
			"middlewares": map[string]any{
				"allowlist-http": map[string]any{"ipAllowList": map[string]any{"sourceRange": c.Allowlist}},
				"to-https":       map[string]any{"redirectScheme": map[string]any{"scheme": "https", "port": "8443", "permanent": true}},
			},
			"routers": map[string]any{
				"fridge-web": map[string]any{
					"entryPoints": []string{"websecure"},
					"rule":        "PathPrefix(`/`)",
					"service":     "fridge-api",
					"middlewares": []string{"allowlist-http"},
					"tls":         map[string]any{},
				},
				"fridge-redirect": map[string]any{
					"entryPoints": []string{"web"},
					"rule":        "PathPrefix(`/`)",
					"service":     "fridge-api",
					"middlewares": []string{"to-https"},
				},
			},
			"services": map[string]any{
				"fridge-api": map[string]any{"loadBalancer": map[string]any{"servers": []any{map[string]any{"url": c.APIBackend}}}},
			},
		},
		"tls": tls,
	}
	if len(routers) > 0 {
		doc["tcp"] = map[string]any{"routers": routers, "services": services}
	}
	return doc
}

// Handler serves build() as JSON to callers that present the bearer token. The document carries private keys,
// so a wrong token gets nothing and an empty configured token means nothing is served at all.
func Handler(token string, build func(r *http.Request) (Config, error)) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if token == "" || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet || strings.TrimSuffix(r.URL.Path, "/") != "/traefik" {
			http.NotFound(w, r)
			return
		}
		cfg, err := build(r)
		if err != nil {
			log.Printf("proxy config: %v", err)
			http.Error(w, "unavailable", http.StatusServiceUnavailable) // Traefik keeps its last good config
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(Build(cfg))
	})
}
