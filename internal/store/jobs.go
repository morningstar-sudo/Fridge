package store

import (
	"context"
	"time"
)

type Schedule struct {
	ID, DatabaseID, DestinationID int64
	Cron, Prefix                  string
	KeepLatest                    int
	Enabled                       bool
}

type Backup struct {
	ID, DatabaseID, DestinationID                             int64
	ScheduleID                                                *int64
	Trigger, Engine, EngineVersion, Status, S3Key, Log, Error string
	SizeBytes                                                 int64
	StartedAt                                                 time.Time
	FinishedAt                                                *time.Time
	StartedByID                                               *int64
}

type Restore struct {
	ID, DestinationID                                            int64
	S3Key, SourceEngine, SourceVersion, Mode, Status, Log, Error string
	TargetDatabaseID                                             *int64
	StartedAt                                                    time.Time
	FinishedAt                                                   *time.Time
	StartedByID                                                  *int64
}

type scanner interface{ Scan(...any) error }

const (
	schedCols   = `id, database_id, destination_id, cron, prefix, keep_latest, enabled`
	backupCols  = `id, database_id, destination_id, schedule_id, trigger, engine, engine_version, status, s3_key, log, error, size_bytes, started_at, finished_at, started_by_id`
	restoreCols = `id, destination_id, s3_key, source_engine, source_version, mode, status, log, error, target_database_id, started_at, finished_at, started_by_id`
)

func (x *Schedule) scan(r scanner) error {
	return r.Scan(&x.ID, &x.DatabaseID, &x.DestinationID, &x.Cron, &x.Prefix, &x.KeepLatest, &x.Enabled)
}

func (x *Backup) scan(r scanner) error {
	return r.Scan(&x.ID, &x.DatabaseID, &x.DestinationID, &x.ScheduleID, &x.Trigger, &x.Engine, &x.EngineVersion, &x.Status, &x.S3Key, &x.Log, &x.Error, &x.SizeBytes, &x.StartedAt, &x.FinishedAt, &x.StartedByID)
}

func (x *Restore) scan(r scanner) error {
	return r.Scan(&x.ID, &x.DestinationID, &x.S3Key, &x.SourceEngine, &x.SourceVersion, &x.Mode, &x.Status, &x.Log, &x.Error, &x.TargetDatabaseID, &x.StartedAt, &x.FinishedAt, &x.StartedByID)
}

// list runs q and scans every row with scan.
func list[T any](ctx context.Context, s *Store, scan func(*T, scanner) error, q string, args ...any) ([]T, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var v T
		if err := scan(&v, rows); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func scanSchedule(x *Schedule, r scanner) error { return x.scan(r) }
func scanBackup(x *Backup, r scanner) error     { return x.scan(r) }

// exec1 runs a write that must touch exactly one row.
func (s *Store) exec1(ctx context.Context, q string, args ...any) error {
	tag, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- schedules ---

func (s *Store) CreateSchedule(ctx context.Context, sc *Schedule) error {
	return mapErr(s.pool.QueryRow(ctx, `INSERT INTO backup_schedules(database_id, destination_id, cron, prefix, keep_latest, enabled)
		VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, sc.DatabaseID, sc.DestinationID, sc.Cron, sc.Prefix, sc.KeepLatest, sc.Enabled).Scan(&sc.ID))
}

func (s *Store) GetSchedule(ctx context.Context, id int64) (Schedule, error) {
	var x Schedule
	err := x.scan(s.pool.QueryRow(ctx, `SELECT `+schedCols+` FROM backup_schedules WHERE id=$1`, id))
	return x, mapErr(err)
}

func (s *Store) ListSchedules(ctx context.Context, databaseID int64) ([]Schedule, error) {
	return list(ctx, s, scanSchedule, `SELECT `+schedCols+` FROM backup_schedules WHERE database_id=$1 ORDER BY id`, databaseID)
}

func (s *Store) ListEnabledSchedules(ctx context.Context) ([]Schedule, error) {
	return list(ctx, s, scanSchedule, `SELECT `+schedCols+` FROM backup_schedules WHERE enabled ORDER BY id`)
}

func (s *Store) UpdateSchedule(ctx context.Context, sc Schedule) error {
	return s.exec1(ctx, `UPDATE backup_schedules SET destination_id=$2, cron=$3, prefix=$4, keep_latest=$5, enabled=$6 WHERE id=$1`,
		sc.ID, sc.DestinationID, sc.Cron, sc.Prefix, sc.KeepLatest, sc.Enabled)
}

func (s *Store) DeleteSchedule(ctx context.Context, id int64) error {
	return s.exec1(ctx, `DELETE FROM backup_schedules WHERE id=$1`, id)
}

func (s *Store) DeleteSchedulesForDatabase(ctx context.Context, databaseID int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM backup_schedules WHERE database_id=$1`, databaseID)
	return mapErr(err)
}

// --- backups ---

// StartBackup inserts a running backup; ErrConflict if the database already has one.
func (s *Store) StartBackup(ctx context.Context, b *Backup) error {
	b.Status = "running"
	return mapErr(s.pool.QueryRow(ctx, `INSERT INTO backups(database_id, destination_id, schedule_id, trigger, engine, engine_version, status, s3_key, started_by_id)
		VALUES($1,$2,$3,$4,$5,$6,'running',$7,$8) RETURNING id, started_at`,
		b.DatabaseID, b.DestinationID, b.ScheduleID, b.Trigger, b.Engine, b.EngineVersion, b.S3Key, b.StartedByID).Scan(&b.ID, &b.StartedAt))
}

func (s *Store) FinishBackup(ctx context.Context, id int64, status string, size int64, log, errText string) error {
	return s.exec1(ctx, `UPDATE backups SET status=$2, size_bytes=$3, log=$4, error=$5, finished_at=now() WHERE id=$1`,
		id, status, size, log, errText)
}

func (s *Store) GetBackup(ctx context.Context, id int64) (Backup, error) {
	var x Backup
	err := x.scan(s.pool.QueryRow(ctx, `SELECT `+backupCols+` FROM backups WHERE id=$1`, id))
	return x, mapErr(err)
}

func (s *Store) ListBackups(ctx context.Context, databaseID int64) ([]Backup, error) {
	return list(ctx, s, scanBackup, `SELECT `+backupCols+` FROM backups WHERE database_id=$1 ORDER BY id DESC`, databaseID)
}

func (s *Store) OKBackupsForSchedule(ctx context.Context, scheduleID int64) ([]Backup, error) {
	return list(ctx, s, scanBackup, `SELECT `+backupCols+` FROM backups WHERE schedule_id=$1 AND status='ok' ORDER BY id DESC`, scheduleID)
}

func (s *Store) DeleteBackup(ctx context.Context, id int64) error {
	return s.exec1(ctx, `DELETE FROM backups WHERE id=$1`, id)
}

// --- restores ---

func (s *Store) StartRestore(ctx context.Context, r *Restore) error {
	r.Status = "running"
	return mapErr(s.pool.QueryRow(ctx, `INSERT INTO restores(destination_id, s3_key, source_engine, source_version, target_database_id, mode, status, started_by_id)
		VALUES($1,$2,$3,$4,$5,$6,'running',$7) RETURNING id, started_at`,
		r.DestinationID, r.S3Key, r.SourceEngine, r.SourceVersion, r.TargetDatabaseID, r.Mode, r.StartedByID).Scan(&r.ID, &r.StartedAt))
}

func (s *Store) SetRestoreTarget(ctx context.Context, id, databaseID int64) error {
	return s.exec1(ctx, `UPDATE restores SET target_database_id=$2 WHERE id=$1`, id, databaseID)
}

func (s *Store) FinishRestore(ctx context.Context, id int64, status, log, errText string) error {
	return s.exec1(ctx, `UPDATE restores SET status=$2, log=$3, error=$4, finished_at=now() WHERE id=$1`, id, status, log, errText)
}

func (s *Store) GetRestore(ctx context.Context, id int64) (Restore, error) {
	var x Restore
	err := x.scan(s.pool.QueryRow(ctx, `SELECT `+restoreCols+` FROM restores WHERE id=$1`, id))
	return x, mapErr(err)
}

// OrphanedRestoreTargets returns live nodes that a failed mode=new restore was building
// (e.g. interrupted by a restart, see FailRunning) and that were never torn down.
func (s *Store) OrphanedRestoreTargets(ctx context.Context) ([]Database, error) {
	return list(ctx, s, func(d *Database, r scanner) error { return d.scan(r) },
		`SELECT `+dbCols+` FROM databases WHERE status NOT IN ('error','deleted')
		AND id IN (SELECT target_database_id FROM restores WHERE status='failed' AND mode='new') ORDER BY id`)
}

// FailRunning marks every running backup and restore failed; returns the row count.
func (s *Store) FailRunning(ctx context.Context, reason string) (int, error) {
	n := 0
	for _, t := range []string{"backups", "restores", "dump_checks"} {
		tag, err := s.pool.Exec(ctx, `UPDATE `+t+` SET status='failed', error=$1, finished_at=now() WHERE status='running'`, reason)
		if err != nil {
			return n, err
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}

func (s *Store) GetBackupByKey(ctx context.Context, destID int64, key string) (Backup, error) {
	var x Backup
	err := x.scan(s.pool.QueryRow(ctx, `SELECT `+backupCols+` FROM backups WHERE destination_id=$1 AND s3_key=$2`, destID, key))
	return x, mapErr(err)
}

// ListOKBackupsOnDestination lists finished backups, newest first; visibleTo limits them
// to databases that user owns or is a member of (nil = all).
func (s *Store) ListOKBackupsOnDestination(ctx context.Context, destID int64, visibleTo *int64) ([]Backup, error) {
	return list(ctx, s, scanBackup, `SELECT `+backupCols+` FROM backups WHERE destination_id=$1 AND status='ok'
		AND ($2::bigint IS NULL OR database_id IN (SELECT id FROM databases WHERE owner_id=$2
			UNION SELECT database_id FROM database_members WHERE user_id=$2)) ORDER BY id DESC`, destID, visibleTo)
}
