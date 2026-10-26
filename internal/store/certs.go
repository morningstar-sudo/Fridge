package store

import (
	"context"
	"time"
)

// migration5 created db_certs, the per-route certificate; migration 6 replaces it: a database presents one certificate,
// both to Traefik's clients and to its own sidecar (see below), so the table is dropped.
const migration5 = `
CREATE TABLE db_certs (
	database_id BIGINT PRIMARY KEY REFERENCES databases(id) ON DELETE CASCADE,
	cert_pem TEXT NOT NULL,
	key_enc TEXT NOT NULL,
	not_after TIMESTAMPTZ NOT NULL
);`

// Route is what Traefik needs for one running database: the certificate it presents for the database's domain.
// That is the database's own server certificate, deliberately the same one the database presents behind the proxy:
// SCRAM channel binding (libpq's default) compares the hash of the certificate the client saw with the one the server
// holds, so the two must be the same certificate. CertPEM and KeyEnc are empty for a node without node TLS.
type Route struct {
	DatabaseID      int64
	Engine, Domain  string
	NodeKey         string
	CertPEM, KeyEnc string
	NotAfter        time.Time
}

// ListRoutes returns the running databases that have a node, with their certificates.
func (s *Store) ListRoutes(ctx context.Context) ([]Route, error) {
	rows, err := s.pool.Query(ctx, `SELECT d.id, d.engine, d.domain, d.node_key, COALESCE(c.server_cert_pem,''), COALESCE(c.server_key_enc,''), COALESCE(c.not_after,'epoch')
		FROM databases d LEFT JOIN db_tls c ON c.database_id = d.id
		WHERE d.status = 'running' AND d.node_key <> '' ORDER BY d.id`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []Route
	for rows.Next() {
		var r Route
		if err := rows.Scan(&r.DatabaseID, &r.Engine, &r.Domain, &r.NodeKey, &r.CertPEM, &r.KeyEnc, &r.NotAfter); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// migration6: the private TLS material of one database node. The database presents ServerCert (also the route's
// certificate: it names both the domain and the node's service); it accepts a client
// only if that client holds the key of ClientCert (its sole trusted root), and only the node's own edge sidecar and
// jobs are given that key. Keys are encrypted like every other secret.
const migration6 = `
DROP TABLE db_certs;
CREATE TABLE db_tls (
	database_id BIGINT PRIMARY KEY REFERENCES databases(id) ON DELETE CASCADE,
	server_cert_pem TEXT NOT NULL,
	server_key_enc TEXT NOT NULL,
	client_cert_pem TEXT NOT NULL,
	client_key_enc TEXT NOT NULL,
	not_after TIMESTAMPTZ NOT NULL
);`

type DBTLS struct {
	ServerCertPEM, ServerKeyEnc string
	ClientCertPEM, ClientKeyEnc string
	CACertPEM                   string // the CA that signed ServerCertPEM; empty = the instance-wide CA
	NotAfter                    time.Time
}

// SetDBTLS stores (or replaces) the TLS material of database id.
func (s *Store) SetDBTLS(ctx context.Context, id int64, t DBTLS) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO db_tls(database_id, server_cert_pem, server_key_enc, client_cert_pem, client_key_enc, not_after, ca_cert_pem) VALUES($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (database_id) DO UPDATE SET server_cert_pem=EXCLUDED.server_cert_pem, server_key_enc=EXCLUDED.server_key_enc,
			client_cert_pem=EXCLUDED.client_cert_pem, client_key_enc=EXCLUDED.client_key_enc, not_after=EXCLUDED.not_after, ca_cert_pem=EXCLUDED.ca_cert_pem`,
		id, t.ServerCertPEM, t.ServerKeyEnc, t.ClientCertPEM, t.ClientKeyEnc, t.NotAfter, t.CACertPEM)
	return mapErr(err)
}

// GetDBTLS returns ErrNotFound for a database without node TLS (a node made before it existed).
func (s *Store) GetDBTLS(ctx context.Context, id int64) (DBTLS, error) {
	var t DBTLS
	err := s.pool.QueryRow(ctx, `SELECT server_cert_pem, server_key_enc, client_cert_pem, client_key_enc, not_after, ca_cert_pem FROM db_tls WHERE database_id=$1`, id).
		Scan(&t.ServerCertPEM, &t.ServerKeyEnc, &t.ClientCertPEM, &t.ClientKeyEnc, &t.NotAfter, &t.CACertPEM)
	return t, mapErr(err)
}

// migration7: every tenant (a user account) gets a CA of their own, created the first time they get a database. The
// certificates of their databases are signed by it, so the CA a user downloads and trusts vouches for their databases
// and for no one else's. ca_cert_pem on db_tls records which CA signed a node (” = the instance-wide CA, for nodes
// made before tenant CAs existed).
const migration7 = `
CREATE TABLE tenant_cas (
	user_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
	cert_pem TEXT NOT NULL,
	key_enc TEXT NOT NULL
);
ALTER TABLE db_tls ADD ca_cert_pem TEXT NOT NULL DEFAULT '';`

type TenantCA struct {
	UserID          int64
	CertPEM, KeyEnc string
}

// GetTenantCA returns ErrNotFound for a user who has none yet.
func (s *Store) GetTenantCA(ctx context.Context, userID int64) (TenantCA, error) {
	c := TenantCA{UserID: userID}
	err := s.pool.QueryRow(ctx, `SELECT cert_pem, key_enc FROM tenant_cas WHERE user_id=$1`, userID).Scan(&c.CertPEM, &c.KeyEnc)
	return c, mapErr(err)
}

// CreateTenantCA stores c unless the user already has one (two requests racing): it returns whichever is stored.
func (s *Store) CreateTenantCA(ctx context.Context, c TenantCA) (TenantCA, error) {
	if _, err := s.pool.Exec(ctx, `INSERT INTO tenant_cas(user_id, cert_pem, key_enc) VALUES($1,$2,$3) ON CONFLICT (user_id) DO NOTHING`, c.UserID, c.CertPEM, c.KeyEnc); err != nil {
		return TenantCA{}, mapErr(err)
	}
	return s.GetTenantCA(ctx, c.UserID)
}
