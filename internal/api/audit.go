package api

import (
	"context"
	"encoding/hex"
	"net/http"
	"strconv"
	"sync"
	"time"

	"fridge/internal/audit"
	"fridge/internal/store"
)

// auditRec is the audit entry a request will produce. A handler names the action with want and
// the resolvers (dbOf, destOf, ...) arm it once the target is visible to the caller. After the
// handler ran, an armed entry is written with success = status < 400, so a 403 on a visible
// resource is recorded as a failure while a 404 (invisible resource) leaves no trace on it.
type auditRec struct {
	action, targetType, targetID string
	dbID                         *int64
	detail                       map[string]any
	armed                        bool
}

type auditKey struct{}

func rec(r *http.Request) *auditRec {
	a, _ := r.Context().Value(auditKey{}).(*auditRec)
	return a
}

// want names the action this request performs; it is recorded once a target has armed it.
func want(r *http.Request, action string) {
	if a := rec(r); a != nil {
		a.action = action
	}
}

// target arms the entry and sets what it is about.
func target(r *http.Request, typ, id string, dbID *int64) {
	if a := rec(r); a != nil {
		a.targetType, a.targetID, a.dbID, a.armed = typ, id, dbID, true
	}
}

// wantNow is want + target for actions with no resource to hide (create, settings, ...).
func wantNow(r *http.Request, action, typ, id string) {
	want(r, action)
	target(r, typ, id, nil)
}

// note adds a non-secret detail field.
func note(r *http.Request, k string, v any) {
	if a := rec(r); a != nil {
		if a.detail == nil {
			a.detail = map[string]any{}
		}
		a.detail[k] = v
	}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// statusWriter remembers the status code for the audit entry.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) {
	if w.code == 0 {
		w.code = c
	}
	w.ResponseWriter.WriteHeader(c)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = 200
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// audited runs next with an audit record in the context and writes it afterwards.
func (s *server) audited(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := &auditRec{}
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), auditKey{}, a)))
		if a.action == "" || !a.armed {
			return
		}
		p, _ := principal(r)
		id, name := audit.Actor(p)
		s.a.Audit.Record(r.Context(), store.AuditEntry{ActorID: id, Actor: name, IP: s.ip(r), Action: a.action,
			TargetType: a.targetType, TargetID: a.targetID, DBID: a.dbID, Success: sw.code < 400, Detail: a.detail})
	})
}

// record writes an entry straight away, for handlers whose outcome is not one status code or that have no login.
// A nil actorID with actor "" takes the caller from the request's principal.
func (s *server) record(r *http.Request, actorID *int64, actor, action, typ, id string, dbID *int64, ok bool, detail map[string]any) {
	if actor == "" {
		p, _ := principal(r)
		actorID, actor = audit.Actor(p)
	}
	s.a.Audit.Record(r.Context(), store.AuditEntry{ActorID: actorID, Actor: actor, IP: s.ip(r), Action: action,
		TargetType: typ, TargetID: id, DBID: dbID, Success: ok, Detail: detail})
}

// loginFailed records auth.login_failed for an attempt: the account's own name when it exists,
// otherwise "anonymous" with the tried name in the detail.
func (s *server) loginFailed(r *http.Request, name string, userID *int64, reason string) {
	if name == "" {
		return
	}
	if userID != nil {
		s.record(r, userID, name, "auth.login_failed", "user", itoa(*userID), nil, false, map[string]any{"reason": reason})
		return
	}
	s.record(r, nil, "anonymous", "auth.login_failed", "", "", nil, false, map[string]any{"username": name, "reason": reason})
}

// lockedSeen limits "locked" login records to one per (name, ip) per minute: a locked client can
// retry as fast as it likes and must not be able to fill the audit table.
type lockedSeen struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (l *lockedSeen) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.last) > 10000 { // ponytail: reset instead of expiring entries one by one
		l.last = nil
	}
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	if t, ok := l.last[key]; ok && now.Sub(t) < time.Minute {
		return false
	}
	l.last[key] = now
	return true
}

// shortHash is enough of a session's id hash to tell sessions apart in the log.
func shortHash(h []byte) string {
	s := hex.EncodeToString(h)
	if len(s) > 8 {
		s = s[:8]
	}
	return s
}
