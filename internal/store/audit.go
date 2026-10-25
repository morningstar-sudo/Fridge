package store

import (
	"context"
	"encoding/json"
	"time"
)

type AuditEntry struct {
	ID         int64
	At         time.Time
	ActorID    *int64
	Actor, IP  string
	Action     string
	TargetType string
	TargetID   string
	DBID       *int64
	Success    bool
	Detail     map[string]any
}

// AuditFilter selects audit entries, newest first. Zero fields do not filter.
type AuditFilter struct {
	ActorID, DBID *int64
	Actor         string // username, matched exactly against the actor column
	Action        string // prefix, e.g. "db."
	From, To      *time.Time
	BeforeID      int64  // page backwards: only entries with a smaller id
	Limit         int    // default 100, at most 500
	VisibleTo     *int64 // nil = everything (admin); otherwise the user's own actions plus anything on databases they own
}

const auditCols = `id, at, actor_id, actor, ip, action, target_type, target_id, db_id, success, detail`

func (e *AuditEntry) scan(r scanner) error {
	var raw []byte
	if err := r.Scan(&e.ID, &e.At, &e.ActorID, &e.Actor, &e.IP, &e.Action, &e.TargetType, &e.TargetID, &e.DBID, &e.Success, &raw); err != nil {
		return err
	}
	return json.Unmarshal(raw, &e.Detail)
}

func (s *Store) InsertAudit(ctx context.Context, e AuditEntry) error {
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	raw, err := json.Marshal(e.Detail)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO audit_log(actor_id, actor, ip, action, target_type, target_id, db_id, success, detail)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, e.ActorID, e.Actor, e.IP, e.Action, e.TargetType, e.TargetID, e.DBID, e.Success, raw)
	return mapErr(err)
}

func (s *Store) ListAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	if f.Limit > 500 {
		f.Limit = 500
	}
	return list(ctx, s, (*AuditEntry).scan, `SELECT `+auditCols+` FROM audit_log
		WHERE ($1::bigint IS NULL OR actor_id=$1)
		AND ($2::bigint IS NULL OR db_id=$2)
		AND ($3='' OR actor=$3)
		AND ($4='' OR starts_with(action, $4))
		AND ($5::timestamptz IS NULL OR at >= $5)
		AND ($6::timestamptz IS NULL OR at <= $6)
		AND ($7=0 OR id < $7)
		AND ($8::bigint IS NULL OR actor_id=$8 OR db_id IN (SELECT id FROM databases WHERE owner_id=$8))
		ORDER BY id DESC LIMIT $9`,
		f.ActorID, f.DBID, f.Actor, f.Action, f.From, f.To, f.BeforeID, f.VisibleTo, f.Limit)
}

// PurgeAudit deletes entries older than olderThan; the table guard refuses anything under a year old.
func (s *Store) PurgeAudit(ctx context.Context, olderThan time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM audit_log WHERE at < $1`, olderThan)
	return int(tag.RowsAffected()), mapErr(err)
}

// Job is a running background operation.
type Job struct {
	Kind      string // "backup" | "restore" | "dumpcheck" | "image_pull"
	ID        int64
	DBName    string
	StartedBy string // username, or "system"
	StartedAt time.Time
}

func (j *Job) scan(r scanner) error {
	return r.Scan(&j.Kind, &j.ID, &j.DBName, &j.StartedBy, &j.StartedAt)
}

// RunningJobs lists running backups, restores and dump checks, oldest first.
func (s *Store) RunningJobs(ctx context.Context) ([]Job, error) {
	return list(ctx, s, (*Job).scan, `
		SELECT 'backup', b.id, d.name, COALESCE(u.username,'system'), b.started_at
			FROM backups b JOIN databases d ON d.id=b.database_id LEFT JOIN users u ON u.id=b.started_by_id WHERE b.status='running'
		UNION ALL
		SELECT 'restore', r.id, COALESCE(d.name,''), COALESCE(u.username,'system'), r.started_at
			FROM restores r LEFT JOIN databases d ON d.id=r.target_database_id LEFT JOIN users u ON u.id=r.started_by_id WHERE r.status='running'
		UNION ALL
		SELECT 'dumpcheck', c.id, '', COALESCE(u.username,'system'), c.started_at
			FROM dump_checks c LEFT JOIN users u ON u.id=c.started_by_id WHERE c.status='running'
		ORDER BY 5, 1, 2`)
}

type OnlineSession struct {
	Session
	Username string
}

func (o *OnlineSession) scan(r scanner) error {
	return r.Scan(&o.IDHash, &o.UserID, &o.CreatedAt, &o.LastSeenAt, &o.ExpiresAt, &o.IP, &o.UserAgent, &o.Username)
}

// OnlineSessions lists sessions seen since the given time, most recently seen first.
func (s *Store) OnlineSessions(ctx context.Context, since time.Time) ([]OnlineSession, error) {
	return list(ctx, s, (*OnlineSession).scan, `SELECT s.id_hash, s.user_id, s.created_at, s.last_seen_at, s.expires_at, s.ip, s.user_agent, u.username
		FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.last_seen_at >= $1 ORDER BY s.last_seen_at DESC`, since)
}
