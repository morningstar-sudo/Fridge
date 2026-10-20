package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type User struct {
	ID                   int64
	Username, Role       string
	Salt                 []byte
	Iterations           int
	StoredKey, ServerKey []byte
	Disabled             bool
	CreatedAt            time.Time
	CreatedBy            *int64
}

type Invite struct {
	ID                int64
	TokenHash         []byte
	Username, Role    string
	Purpose           string
	UserID, CreatedBy *int64
	ExpiresAt         time.Time
	UsedAt            *time.Time
	CreatedAt         time.Time
}

type Session struct {
	IDHash                           []byte
	UserID                           int64
	CreatedAt, LastSeenAt, ExpiresAt time.Time
	IP, UserAgent                    string
}

const (
	userCols    = `id, username, role, scram_salt, scram_iterations, stored_key, server_key, disabled, created_at, created_by`
	inviteCols  = `id, token_hash, username, role, purpose, user_id, created_by, expires_at, used_at, created_at`
	sessionCols = `id_hash, user_id, created_at, last_seen_at, expires_at, ip, user_agent`
)

func (u *User) scan(r scanner) error {
	return r.Scan(&u.ID, &u.Username, &u.Role, &u.Salt, &u.Iterations, &u.StoredKey, &u.ServerKey, &u.Disabled, &u.CreatedAt, &u.CreatedBy)
}

func (i *Invite) scan(r scanner) error {
	return r.Scan(&i.ID, &i.TokenHash, &i.Username, &i.Role, &i.Purpose, &i.UserID, &i.CreatedBy, &i.ExpiresAt, &i.UsedAt, &i.CreatedAt)
}

func (x *Session) scan(r scanner) error {
	return r.Scan(&x.IDHash, &x.UserID, &x.CreatedAt, &x.LastSeenAt, &x.ExpiresAt, &x.IP, &x.UserAgent)
}

const insertUser = `INSERT INTO users(username, role, scram_salt, scram_iterations, stored_key, server_key, created_by)
	VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id, disabled, created_at`

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

// CreateFirstAdmin creates the first user (role admin) and hands every existing, unowned
// database/destination/domain to it. ErrConflict if any user already exists.
func (s *Store) CreateFirstAdmin(ctx context.Context, u *User) error {
	u.Role = "admin"
	return mapErr(pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('fridge.setup'))`); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrConflict
		}
		if err := tx.QueryRow(ctx, insertUser, u.Username, u.Role, u.Salt, u.Iterations, u.StoredKey, u.ServerKey, u.CreatedBy).
			Scan(&u.ID, &u.Disabled, &u.CreatedAt); err != nil {
			return err
		}
		for _, q := range []string{
			`UPDATE databases SET owner_id=$1 WHERE owner_id IS NULL`,
			`UPDATE destinations SET owner_id=$1 WHERE owner_id IS NULL`,
			`INSERT INTO domains(owner_id, name) SELECT DISTINCT $1::bigint, domain FROM databases WHERE domain <> '' ON CONFLICT (name) DO NOTHING`,
			`UPDATE databases d SET domain_id = dm.id FROM domains dm WHERE dm.name = d.domain AND d.domain_id IS NULL AND $1::bigint IS NOT NULL`,
		} {
			if _, err := tx.Exec(ctx, q, u.ID); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `ALTER TABLE databases ALTER owner_id SET NOT NULL;
			ALTER TABLE destinations ALTER owner_id SET NOT NULL;
			ALTER TABLE domains ALTER owner_id SET NOT NULL`)
		return err
	}))
}

func (s *Store) CreateUser(ctx context.Context, u *User) error {
	return mapErr(s.pool.QueryRow(ctx, insertUser, u.Username, u.Role, u.Salt, u.Iterations, u.StoredKey, u.ServerKey, u.CreatedBy).
		Scan(&u.ID, &u.Disabled, &u.CreatedAt))
}

func (s *Store) GetUser(ctx context.Context, id int64) (User, error) {
	var u User
	err := u.scan(s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
	return u, mapErr(err)
}

func (s *Store) GetUserByName(ctx context.Context, name string) (User, error) {
	var u User
	err := u.scan(s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE username=$1`, name))
	return u, mapErr(err)
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	return list(ctx, s, (*User).scan, `SELECT `+userCols+` FROM users ORDER BY id`)
}

func (s *Store) UpdateUserCredentials(ctx context.Context, id int64, salt []byte, iter int, stored, server []byte) error {
	return s.exec1(ctx, `UPDATE users SET scram_salt=$2, scram_iterations=$3, stored_key=$4, server_key=$5 WHERE id=$1`, id, salt, iter, stored, server)
}

// guardedUserUpdate applies q (args: id, value) and rolls back with ErrLastAdmin if no
// active admin remains. The advisory lock serialises concurrent demotions.
func (s *Store) guardedUserUpdate(ctx context.Context, q string, id int64, v any) error {
	return mapErr(pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('fridge.admins'))`); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, q, id, v)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='admin' AND NOT disabled`).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrLastAdmin
		}
		return nil
	}))
}

func (s *Store) SetUserRole(ctx context.Context, id int64, role string) error {
	return s.guardedUserUpdate(ctx, `UPDATE users SET role=$2 WHERE id=$1`, id, role)
}

func (s *Store) SetUserDisabled(ctx context.Context, id int64, disabled bool) error {
	return s.guardedUserUpdate(ctx, `UPDATE users SET disabled=$2 WHERE id=$1`, id, disabled)
}

func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE role='admin' AND NOT disabled`).Scan(&n)
	return n, err
}

func (s *Store) CreateInvite(ctx context.Context, inv *Invite) error {
	return mapErr(s.pool.QueryRow(ctx, `INSERT INTO invites(token_hash, username, role, purpose, user_id, created_by, expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id, created_at`,
		inv.TokenHash, inv.Username, inv.Role, inv.Purpose, inv.UserID, inv.CreatedBy, inv.ExpiresAt).Scan(&inv.ID, &inv.CreatedAt))
}

func (s *Store) GetInviteByHash(ctx context.Context, hash []byte) (Invite, error) {
	var i Invite
	err := i.scan(s.pool.QueryRow(ctx, `SELECT `+inviteCols+` FROM invites WHERE token_hash=$1`, hash))
	return i, mapErr(err)
}

// MarkInviteUsed is single-use: ErrConflict if already used, ErrNotFound if unknown.
func (s *Store) MarkInviteUsed(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE invites SET used_at=now() WHERE id=$1 AND used_at IS NULL`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		var ok bool
		if err := s.pool.QueryRow(ctx, `SELECT true FROM invites WHERE id=$1`, id).Scan(&ok); err != nil {
			return mapErr(err)
		}
		return ErrConflict
	}
	return nil
}

func (s *Store) CreateSession(ctx context.Context, x *Session) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO sessions(id_hash, user_id, created_at, last_seen_at, expires_at, ip, user_agent)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, x.IDHash, x.UserID, x.CreatedAt, x.LastSeenAt, x.ExpiresAt, x.IP, x.UserAgent)
	return mapErr(err)
}

func (s *Store) GetSession(ctx context.Context, hash []byte) (Session, error) {
	var x Session
	err := x.scan(s.pool.QueryRow(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id_hash=$1`, hash))
	return x, mapErr(err)
}

func (s *Store) TouchSession(ctx context.Context, hash []byte, at time.Time) error {
	return s.exec1(ctx, `UPDATE sessions SET last_seen_at=$2 WHERE id_hash=$1`, hash, at)
}

func (s *Store) DeleteSession(ctx context.Context, hash []byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id_hash=$1`, hash)
	return err
}

// DeleteUserSessions removes the user's sessions except `except` (nil = delete all).
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64, except []byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1 AND ($2::bytea IS NULL OR id_hash <> $2)`, userID, except)
	return err
}

// ListSessions lists sessions newest first; userID nil = every user.
func (s *Store) ListSessions(ctx context.Context, userID *int64) ([]Session, error) {
	return list(ctx, s, (*Session).scan, `SELECT `+sessionCols+` FROM sessions WHERE ($1::bigint IS NULL OR user_id=$1) ORDER BY created_at DESC, last_seen_at DESC`, userID)
}

func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time, idle time.Duration) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < $1 OR last_seen_at < $2`, now, now.Add(-idle))
	return int(tag.RowsAffected()), err
}
