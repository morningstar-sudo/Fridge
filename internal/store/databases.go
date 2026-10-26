package store

import (
	"context"
	"time"
)

type Database struct {
	ID                                                                                    int64
	Name, Engine, Version, Domain, User, PasswordEnc, HostID, ContainerID, Volume, Status string
	CreatedAt                                                                             time.Time
	OwnerID                                                                               *int64
	OwnerName                                                                             string // read-only
	AdminUser, AdminPasswordEnc, NodeKey                                                  string
	DomainID                                                                              *int64
}

const dbCols = `id, name, engine, version, domain, db_user, password_enc, host_id, container_id, volume, status, created_at, owner_id, COALESCE((SELECT username FROM users WHERE users.id = databases.owner_id), ''), admin_user, admin_password_enc, node_key, domain_id`

func (d *Database) scan(r interface{ Scan(...any) error }) error {
	return r.Scan(&d.ID, &d.Name, &d.Engine, &d.Version, &d.Domain, &d.User, &d.PasswordEnc, &d.HostID, &d.ContainerID, &d.Volume, &d.Status, &d.CreatedAt, &d.OwnerID, &d.OwnerName, &d.AdminUser, &d.AdminPasswordEnc, &d.NodeKey, &d.DomainID)
}

func (s *Store) CreateDatabase(ctx context.Context, d *Database) error {
	if d.HostID == "" {
		d.HostID = "local"
	}
	if d.Status == "" {
		d.Status = "creating"
	}
	err := s.pool.QueryRow(ctx, `INSERT INTO databases(name, engine, version, domain, db_user, password_enc, host_id, container_id, volume, status, owner_id, admin_user, admin_password_enc, node_key, domain_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15) RETURNING id, created_at`,
		d.Name, d.Engine, d.Version, d.Domain, d.User, d.PasswordEnc, d.HostID, d.ContainerID, d.Volume, d.Status, d.OwnerID, d.AdminUser, d.AdminPasswordEnc, d.NodeKey, d.DomainID).Scan(&d.ID, &d.CreatedAt)
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

// ListDatabasesVisible returns non-deleted databases the user owns or is a member of.
func (s *Store) ListDatabasesVisible(ctx context.Context, userID int64) ([]Database, error) {
	return list(ctx, s, func(d *Database, r scanner) error { return d.scan(r) },
		`SELECT `+dbCols+` FROM databases WHERE status<>'deleted'
		AND (owner_id=$1 OR id IN (SELECT database_id FROM database_members WHERE user_id=$1)) ORDER BY id`, userID)
}

func (s *Store) IsMember(ctx context.Context, dbID, userID int64) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM database_members WHERE database_id=$1 AND user_id=$2)`, dbID, userID).Scan(&ok)
	return ok, err
}

type Member struct {
	UserID   int64
	Username string
	AddedAt  time.Time
}

func (s *Store) AddMember(ctx context.Context, dbID, userID int64, addedBy *int64) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO database_members(database_id, user_id, added_by) VALUES($1,$2,$3)`, dbID, userID, addedBy)
	return mapErr(err)
}

func (s *Store) RemoveMember(ctx context.Context, dbID, userID int64) error {
	return s.exec1(ctx, `DELETE FROM database_members WHERE database_id=$1 AND user_id=$2`, dbID, userID)
}

func (s *Store) ListMembers(ctx context.Context, dbID int64) ([]Member, error) {
	return list(ctx, s, func(m *Member, r scanner) error { return r.Scan(&m.UserID, &m.Username, &m.AddedAt) },
		`SELECT m.user_id, u.username, m.added_at FROM database_members m JOIN users u ON u.id=m.user_id WHERE m.database_id=$1 ORDER BY m.added_at, m.user_id`, dbID)
}

func (s *Store) SetDatabaseAdminPassword(ctx context.Context, id int64, passwordEnc string) error {
	return s.exec1(ctx, `UPDATE databases SET admin_password_enc=$2 WHERE id=$1`, id, passwordEnc)
}
