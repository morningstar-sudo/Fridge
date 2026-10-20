package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type Destination struct {
	ID                                                                int64
	Name, Provider, Endpoint, Region, Bucket, AccessKey, SecretKeyEnc string
	PathStyle                                                         bool
	CreatedAt                                                         time.Time
	OwnerID                                                           *int64
	OwnerName                                                         string // read-only
	Shared                                                            bool
	DeletedAt                                                         *time.Time
}

const destCols = `id, name, provider, endpoint, region, bucket, access_key, secret_key_enc, path_style, created_at, owner_id, COALESCE((SELECT username FROM users WHERE users.id = destinations.owner_id), ''), shared, deleted_at`

func (d *Destination) scan(r interface{ Scan(...any) error }) error {
	return r.Scan(&d.ID, &d.Name, &d.Provider, &d.Endpoint, &d.Region, &d.Bucket, &d.AccessKey, &d.SecretKeyEnc, &d.PathStyle, &d.CreatedAt, &d.OwnerID, &d.OwnerName, &d.Shared, &d.DeletedAt)
}

func (s *Store) CreateDestination(ctx context.Context, d *Destination) error {
	err := s.pool.QueryRow(ctx, `INSERT INTO destinations(name, provider, endpoint, region, bucket, access_key, secret_key_enc, path_style, owner_id, shared)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id, created_at`,
		d.Name, d.Provider, d.Endpoint, d.Region, d.Bucket, d.AccessKey, d.SecretKeyEnc, d.PathStyle, d.OwnerID, d.Shared).Scan(&d.ID, &d.CreatedAt)
	return mapErr(err)
}

func (s *Store) GetDestination(ctx context.Context, id int64) (Destination, error) {
	var d Destination
	err := d.scan(s.pool.QueryRow(ctx, `SELECT `+destCols+` FROM destinations WHERE id=$1`, id))
	return d, mapErr(err)
}

func (s *Store) UpdateDestination(ctx context.Context, d Destination) error {
	tag, err := s.pool.Exec(ctx, `UPDATE destinations SET name=$2, provider=$3, endpoint=$4, region=$5, bucket=$6, access_key=$7, secret_key_enc=$8, path_style=$9 WHERE id=$1`,
		d.ID, d.Name, d.Provider, d.Endpoint, d.Region, d.Bucket, d.AccessKey, d.SecretKeyEnc, d.PathStyle)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanDest(d *Destination, r scanner) error { return d.scan(r) }

func (s *Store) ListDestinations(ctx context.Context) ([]Destination, error) {
	return list(ctx, s, scanDest, `SELECT `+destCols+` FROM destinations WHERE deleted_at IS NULL ORDER BY id`)
}

func (s *Store) ListDestinationsVisible(ctx context.Context, userID int64) ([]Destination, error) {
	return list(ctx, s, scanDest, `SELECT `+destCols+` FROM destinations WHERE (owner_id=$1 OR shared) AND deleted_at IS NULL ORDER BY id`, userID)
}

func (s *Store) SetDestinationShared(ctx context.Context, id int64, shared bool) error {
	return s.exec1(ctx, `UPDATE destinations SET shared=$2 WHERE id=$1 AND deleted_at IS NULL`, id, shared)
}

// DeleteDestination soft-deletes (credentials wiped, row kept for backup history).
// ErrInUse while a schedule references it; ErrNotFound if missing or already deleted.
func (s *Store) DeleteDestination(ctx context.Context, id int64) error {
	return mapErr(pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// FOR UPDATE conflicts with the key-share lock a concurrent schedule insert takes.
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM destinations WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&one); err != nil {
			return err
		}
		var used bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM backup_schedules WHERE destination_id=$1)`, id).Scan(&used); err != nil {
			return err
		}
		if used {
			return ErrInUse
		}
		_, err := tx.Exec(ctx, `UPDATE destinations SET deleted_at=now(), secret_key_enc='', access_key='', shared=false WHERE id=$1`, id)
		return err
	}))
}
