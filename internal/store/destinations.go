package store

import (
	"context"
	"time"
)

type Destination struct {
	ID                                                                int64
	Name, Provider, Endpoint, Region, Bucket, AccessKey, SecretKeyEnc string
	PathStyle                                                         bool
	CreatedAt                                                         time.Time
}

const destCols = `id, name, provider, endpoint, region, bucket, access_key, secret_key_enc, path_style, created_at`

func (d *Destination) scan(r interface{ Scan(...any) error }) error {
	return r.Scan(&d.ID, &d.Name, &d.Provider, &d.Endpoint, &d.Region, &d.Bucket, &d.AccessKey, &d.SecretKeyEnc, &d.PathStyle, &d.CreatedAt)
}

func (s *Store) CreateDestination(ctx context.Context, d *Destination) error {
	err := s.pool.QueryRow(ctx, `INSERT INTO destinations(name, provider, endpoint, region, bucket, access_key, secret_key_enc, path_style)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id, created_at`,
		d.Name, d.Provider, d.Endpoint, d.Region, d.Bucket, d.AccessKey, d.SecretKeyEnc, d.PathStyle).Scan(&d.ID, &d.CreatedAt)
	return mapErr(err)
}

func (s *Store) GetDestination(ctx context.Context, id int64) (Destination, error) {
	var d Destination
	err := d.scan(s.pool.QueryRow(ctx, `SELECT `+destCols+` FROM destinations WHERE id=$1`, id))
	return d, mapErr(err)
}

func (s *Store) ListDestinations(ctx context.Context) ([]Destination, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+destCols+` FROM destinations ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Destination
	for rows.Next() {
		var d Destination
		if err := d.scan(rows); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
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

// DeleteDestination returns ErrInUse while a schedule still references it.
func (s *Store) DeleteDestination(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM destinations WHERE id=$1`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
