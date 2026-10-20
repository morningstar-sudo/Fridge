package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

type DumpCheck struct {
	ID, DestinationID         int64
	S3Key, Status, Log, Error string
	Result                    map[string]any
	StartedByID               *int64
	StartedAt                 time.Time
	FinishedAt                *time.Time
}

func (s *Store) StartDumpCheck(ctx context.Context, c *DumpCheck) error {
	c.Status = "running"
	return mapErr(s.pool.QueryRow(ctx, `INSERT INTO dump_checks(destination_id, s3_key, status, started_by_id)
		VALUES($1,$2,'running',$3) RETURNING id, started_at`, c.DestinationID, c.S3Key, c.StartedByID).Scan(&c.ID, &c.StartedAt))
}

func (s *Store) FinishDumpCheck(ctx context.Context, id int64, status string, result map[string]any, log, errText string) error {
	if result == nil {
		result = map[string]any{}
	}
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return s.exec1(ctx, `UPDATE dump_checks SET status=$2, result=$3, log=$4, error=$5, finished_at=now() WHERE id=$1`, id, status, b, log, errText)
}

func (s *Store) GetDumpCheck(ctx context.Context, id int64) (DumpCheck, error) {
	var c DumpCheck
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT id, destination_id, s3_key, status, result, log, error, started_by_id, started_at, finished_at FROM dump_checks WHERE id=$1`, id).
		Scan(&c.ID, &c.DestinationID, &c.S3Key, &c.Status, &raw, &c.Log, &c.Error, &c.StartedByID, &c.StartedAt, &c.FinishedAt)
	if err != nil {
		return c, mapErr(err)
	}
	return c, json.Unmarshal(raw, &c.Result)
}

type Domain struct {
	ID, OwnerID     int64
	OwnerName, Name string
	CreatedAt       time.Time
	InUse           int // active databases (status not error/deleted)
}

const domainCols = `d.id, COALESCE(d.owner_id,0), COALESCE(u.username,''), d.name, d.created_at,
	(SELECT count(*) FROM databases b WHERE b.domain_id=d.id AND b.status NOT IN ('error','deleted'))`
const domainFrom = ` FROM domains d LEFT JOIN users u ON u.id=d.owner_id `

func scanDomain(d *Domain, r scanner) error {
	return r.Scan(&d.ID, &d.OwnerID, &d.OwnerName, &d.Name, &d.CreatedAt, &d.InUse)
}

func (s *Store) CreateDomain(ctx context.Context, d *Domain) error {
	return mapErr(s.pool.QueryRow(ctx, `INSERT INTO domains(owner_id, name) VALUES($1,$2) RETURNING id, created_at`, d.OwnerID, d.Name).Scan(&d.ID, &d.CreatedAt))
}

func (s *Store) ListDomains(ctx context.Context, ownerID *int64) ([]Domain, error) {
	return list(ctx, s, scanDomain, `SELECT `+domainCols+domainFrom+`WHERE ($1::bigint IS NULL OR d.owner_id=$1) ORDER BY d.id`, ownerID)
}

func (s *Store) GetDomain(ctx context.Context, id int64) (Domain, error) {
	var d Domain
	err := scanDomain(&d, s.pool.QueryRow(ctx, `SELECT `+domainCols+domainFrom+`WHERE d.id=$1`, id))
	return d, mapErr(err)
}

func (s *Store) GetDomainByName(ctx context.Context, name string) (Domain, error) {
	var d Domain
	err := scanDomain(&d, s.pool.QueryRow(ctx, `SELECT `+domainCols+domainFrom+`WHERE d.name=$1`, name))
	return d, mapErr(err)
}

// DeleteDomain: ErrInUse while an active database uses it; error/deleted ones are detached by the FK.
func (s *Store) DeleteDomain(ctx context.Context, id int64) error {
	return mapErr(pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM domains WHERE id=$1 FOR UPDATE`, id).Scan(&one); err != nil {
			return err
		}
		var used bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM databases WHERE domain_id=$1 AND status NOT IN ('error','deleted'))`, id).Scan(&used); err != nil {
			return err
		}
		if used {
			return ErrInUse
		}
		_, err := tx.Exec(ctx, `DELETE FROM domains WHERE id=$1`, id)
		return err
	}))
}

type ImageCheck struct {
	ImageRef, LocalDigest, LocalVersion, RemoteDigest, RemoteVersion, Error string
	CheckedAt                                                               time.Time
}

func (s *Store) UpsertImageCheck(ctx context.Context, c ImageCheck) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO image_checks(image_ref, local_digest, local_version, remote_digest, remote_version, error, checked_at)
		VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (image_ref) DO UPDATE SET local_digest=EXCLUDED.local_digest,
		local_version=EXCLUDED.local_version, remote_digest=EXCLUDED.remote_digest, remote_version=EXCLUDED.remote_version,
		error=EXCLUDED.error, checked_at=EXCLUDED.checked_at`,
		c.ImageRef, c.LocalDigest, c.LocalVersion, c.RemoteDigest, c.RemoteVersion, c.Error, c.CheckedAt)
	return mapErr(err)
}

func (s *Store) ListImageChecks(ctx context.Context) ([]ImageCheck, error) {
	return list(ctx, s, func(c *ImageCheck, r scanner) error {
		return r.Scan(&c.ImageRef, &c.LocalDigest, &c.LocalVersion, &c.RemoteDigest, &c.RemoteVersion, &c.Error, &c.CheckedAt)
	}, `SELECT image_ref, local_digest, local_version, remote_digest, remote_version, error, checked_at FROM image_checks ORDER BY image_ref`)
}
