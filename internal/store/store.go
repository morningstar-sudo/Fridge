// Package store is the Postgres-backed metadata store.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
	ErrInUse    = errors.New("in use")
	// ErrInvalid: CHECK constraint or integrity trigger rejected the row.
	ErrInvalid = errors.New("invalid")
	// ErrLastAdmin: the change would leave no active admin.
	ErrLastAdmin = errors.New("last admin")
)

type Store struct{ pool *pgxpool.Pool }

// Open connects, pings and applies pending migrations.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error                   { s.pool.Close(); return nil }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Pool exposes the pool to sibling store code and tests.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

var migrations = []string{
	`CREATE TABLE databases (
		id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		name TEXT NOT NULL,
		engine TEXT NOT NULL,
		version TEXT NOT NULL,
		domain TEXT NOT NULL,
		tls_mode TEXT NOT NULL DEFAULT 'internal',
		db_user TEXT NOT NULL DEFAULT '',
		password_enc TEXT NOT NULL DEFAULT '',
		host_id TEXT NOT NULL DEFAULT 'local',
		container_id TEXT NOT NULL DEFAULT '',
		volume TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'creating',
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		deleted_at TIMESTAMPTZ
	);
	CREATE UNIQUE INDEX db_name ON databases(name) WHERE status NOT IN ('error','deleted');
	CREATE UNIQUE INDEX db_domain ON databases(engine, domain) WHERE status NOT IN ('error','deleted');

	CREATE TABLE destinations (
		id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		provider TEXT NOT NULL,
		endpoint TEXT NOT NULL DEFAULT '',
		region TEXT NOT NULL DEFAULT '',
		bucket TEXT NOT NULL DEFAULT '',
		access_key TEXT NOT NULL DEFAULT '',
		secret_key_enc TEXT NOT NULL DEFAULT '',
		path_style BOOLEAN NOT NULL DEFAULT false,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now()
	);

	CREATE TABLE backup_schedules (
		id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		database_id BIGINT NOT NULL REFERENCES databases(id),
		destination_id BIGINT NOT NULL REFERENCES destinations(id) ON DELETE RESTRICT,
		cron TEXT NOT NULL,
		prefix TEXT NOT NULL DEFAULT 'fridge',
		keep_latest INTEGER NOT NULL DEFAULT 7,
		enabled BOOLEAN NOT NULL DEFAULT true
	);

	CREATE TABLE backups (
		id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		database_id BIGINT NOT NULL REFERENCES databases(id),
		destination_id INTEGER NOT NULL,
		schedule_id BIGINT,
		trigger TEXT NOT NULL,
		engine TEXT NOT NULL,
		engine_version TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'running',
		started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		finished_at TIMESTAMPTZ,
		size_bytes BIGINT NOT NULL DEFAULT 0,
		s3_key TEXT NOT NULL DEFAULT '',
		log TEXT NOT NULL DEFAULT '',
		error TEXT NOT NULL DEFAULT '',
		verified_at TIMESTAMPTZ,
		verify_status TEXT NOT NULL DEFAULT ''
	);
	CREATE UNIQUE INDEX one_running_backup ON backups(database_id) WHERE status='running';

	CREATE TABLE restores (
		id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		destination_id INTEGER NOT NULL,
		s3_key TEXT NOT NULL,
		source_engine_version TEXT NOT NULL DEFAULT '',
		target_database_id BIGINT REFERENCES databases(id),
		mode TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'running',
		log TEXT NOT NULL DEFAULT '',
		started_by TEXT NOT NULL DEFAULT '',
		started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		finished_at TIMESTAMPTZ
	);

	CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);`,
	// restores needs error + split source engine/version (Task 3).
	`ALTER TABLE restores RENAME COLUMN source_engine_version TO source_version;
	ALTER TABLE restores ADD COLUMN source_engine TEXT NOT NULL DEFAULT '', ADD COLUMN error TEXT NOT NULL DEFAULT '';`,
	// one object key per backup row: a same-second rerun must conflict, not overwrite (Task 9).
	`CREATE UNIQUE INDEX backups_s3_key ON backups(s3_key);`,
	migration4,
}

func (s *Store) migrate(ctx context.Context) error { return s.migrateTo(ctx, len(migrations)) }

// migrateTo applies migrations 1..n that are not yet recorded.
func (s *Store) migrateTo(ctx context.Context, n int) error {
	if _, err := s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	for i, m := range migrations[:n] {
		v := i + 1
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('fridge-migrate'))`); err != nil {
				return err
			}
			var done bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, v).Scan(&done); err != nil || done {
				return err
			}
			if _, err := tx.Exec(ctx, m); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES($1)`, v)
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %d: %w", v, err)
		}
	}
	return nil
}

// mapErr translates pg errors to the package sentinels.
func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		switch pe.Code {
		case "23505":
			return fmt.Errorf("%w: %s", ErrConflict, pe.Message)
		case "23514", "P0001":
			return fmt.Errorf("%w: %s", ErrInvalid, pe.Message)
		case "23503", "23001": // 23001: ON DELETE RESTRICT (reported instead of 23503 by PG 18)
			return fmt.Errorf("%w: %s", ErrInUse, pe.Message)
		}
	}
	return err
}
