package store

import (
	"context"
	"time"
)

type Database struct {
	ID                                                                                             int64
	Name, Engine, Version, Domain, TLSMode, User, PasswordEnc, HostID, ContainerID, Volume, Status string
	CreatedAt                                                                                      time.Time
}

const dbCols = `id, name, engine, version, domain, tls_mode, db_user, password_enc, host_id, container_id, volume, status, created_at`

func (d *Database) scan(r interface{ Scan(...any) error }) error {
	return r.Scan(&d.ID, &d.Name, &d.Engine, &d.Version, &d.Domain, &d.TLSMode, &d.User, &d.PasswordEnc, &d.HostID, &d.ContainerID, &d.Volume, &d.Status, &d.CreatedAt)
}

func (s *Store) CreateDatabase(ctx context.Context, d *Database) error {
	if d.HostID == "" {
		d.HostID = "local"
	}
	if d.Status == "" {
		d.Status = "creating"
	}
	err := s.pool.QueryRow(ctx, `INSERT INTO databases(name, engine, version, domain, tls_mode, db_user, password_enc, host_id, container_id, volume, status)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id, created_at`,
		d.Name, d.Engine, d.Version, d.Domain, d.TLSMode, d.User, d.PasswordEnc, d.HostID, d.ContainerID, d.Volume, d.Status).Scan(&d.ID, &d.CreatedAt)
	return mapErr(err)
}

func (s *Store) GetDatabase(ctx context.Context, id int64) (Database, error) {
	var d Database
	err := d.scan(s.pool.QueryRow(ctx, `SELECT `+dbCols+` FROM databases WHERE id=$1`, id))
	return d, mapErr(err)
}

func (s *Store) GetDatabaseByName(ctx context.Context, name string) (Database, error) {
	var d Database
	err := d.scan(s.pool.QueryRow(ctx, `SELECT `+dbCols+` FROM databases WHERE name=$1 AND status NOT IN ('error','deleted')`, name))
	return d, mapErr(err)
}

func (s *Store) ListDatabases(ctx context.Context) ([]Database, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+dbCols+` FROM databases WHERE status<>'deleted' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Database
	for rows.Next() {
		var d Database
		if err := d.scan(rows); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) SetDatabaseState(ctx context.Context, id int64, status, containerID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE databases SET status=$2, container_id=$3 WHERE id=$1`, id, status, containerID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
