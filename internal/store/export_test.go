package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// OpenRaw opens a pool without running migrations.
func OpenRaw(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool}, nil
}

// MigrateTo applies migrations 1..n.
func (s *Store) MigrateTo(ctx context.Context, n int) error { return s.migrateTo(ctx, n) }

var MapErr = mapErr
