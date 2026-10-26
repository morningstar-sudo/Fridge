package mysqlproxy

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
)

// Document is the router's configuration as Fridge serves it (GET :8081/mysql) and the router reads it.
type Document struct {
	Frontend Pair            `json:"frontend"` // what clients see: one certificate, since MySQL has no SNI
	Users    map[string]Node `json:"users"`
}

// Pair is a PEM certificate and its key.
type Pair struct {
	Cert string `json:"cert"`
	Key  string `json:"key"`
}

// Node is one database: where it is and how to talk to it (its node's client certificate, the CA behind its server
// certificate, the name that certificate carries).
type Node struct {
	Addr       string `json:"addr"`
	Cert       string `json:"cert"`
	Key        string `json:"key"`
	CA         string `json:"ca"`
	ServerName string `json:"server_name"`
}

// Parse turns a Document (JSON) into a Config the Router can use.
func Parse(b []byte) (*Config, error) {
	var d Document
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("mysql router config: %w", err)
	}
	front, err := tls.X509KeyPair([]byte(d.Frontend.Cert), []byte(d.Frontend.Key))
	if err != nil {
		return nil, fmt.Errorf("mysql router config: frontend certificate: %w", err)
	}
	cfg := &Config{TLS: &tls.Config{Certificates: []tls.Certificate{front}, MinVersion: tls.VersionTLS12}, Users: map[string]Backend{}}
	for user, n := range d.Users {
		pair, err := tls.X509KeyPair([]byte(n.Cert), []byte(n.Key))
		if err != nil {
			return nil, fmt.Errorf("mysql router config: user %s: client certificate: %w", user, err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM([]byte(n.CA)) {
			return nil, fmt.Errorf("mysql router config: user %s: %w", user, errors.New("no CA certificate"))
		}
		cfg.Users[user] = Backend{Addr: n.Addr, TLS: &tls.Config{Certificates: []tls.Certificate{pair}, RootCAs: roots, ServerName: n.ServerName, MinVersion: tls.VersionTLS12}}
	}
	return cfg, nil
}
