package store

import (
	"context"
	"time"
)

// migration5: the TLS certificate of each database's route lives in the metadata DB, so Traefik can run on any
// node and is configured over HTTP (no shared directory). The private key is encrypted like every other secret.
const migration5 = `
CREATE TABLE db_certs (
	database_id BIGINT PRIMARY KEY REFERENCES databases(id) ON DELETE CASCADE,
	cert_pem TEXT NOT NULL,
	key_enc TEXT NOT NULL,
	not_after TIMESTAMPTZ NOT NULL
);`

// Route is what Traefik needs for one running database. CertPEM and KeyEnc are empty until a certificate is issued.
type Route struct {
	DatabaseID      int64
	Engine, Domain  string
	NodeKey         string
	CertPEM, KeyEnc string
	NotAfter        time.Time
}

// SetDBCert stores (or replaces) the certificate of database id.
func (s *Store) SetDBCert(ctx context.Context, id int64, certPEM, keyEnc string, notAfter time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO db_certs(database_id, cert_pem, key_enc, not_after) VALUES($1,$2,$3,$4)
		ON CONFLICT (database_id) DO UPDATE SET cert_pem=EXCLUDED.cert_pem, key_enc=EXCLUDED.key_enc, not_after=EXCLUDED.not_after`,
		id, certPEM, keyEnc, notAfter)
	return mapErr(err)
}

// ListRoutes returns the running databases that have a node, with their certificates.
func (s *Store) ListRoutes(ctx context.Context) ([]Route, error) {
	rows, err := s.pool.Query(ctx, `SELECT d.id, d.engine, d.domain, d.node_key, COALESCE(c.cert_pem,''), COALESCE(c.key_enc,''), COALESCE(c.not_after,'epoch')
		FROM databases d LEFT JOIN db_certs c ON c.database_id = d.id
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
