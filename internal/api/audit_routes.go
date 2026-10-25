package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"fridge/internal/auth"
	"fridge/internal/store"
)

func (s *server) auditRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/audit", s.listAudit)
	m.HandleFunc("GET /api/audit.csv", s.auditCSV)
	m.HandleFunc("GET /api/activity", s.activity)
}

var errBadQuery = errors.New("bad query parameter")

// auditFilter reads the query string. empty = the caller named a database they cannot see.
func (s *server) auditFilter(r *http.Request) (f store.AuditFilter, empty bool, err error) {
	q := r.URL.Query()
	p := me(r)
	if !p.IsAdmin() {
		f.VisibleTo = &p.UserID
	}
	f.Actor, f.Action = q.Get("actor"), q.Get("action")
	num := func(k string) (int64, bool, error) {
		if q.Get(k) == "" {
			return 0, false, nil
		}
		n, err := strconv.ParseInt(q.Get(k), 10, 64)
		if err != nil || n < 0 {
			return 0, false, errBadQuery
		}
		return n, true, nil
	}
	aid, hasAID, e1 := num("actor_id")
	before, _, e2 := num("before")
	limit, _, e3 := num("limit")
	if err := errors.Join(e1, e2, e3); err != nil {
		return f, false, errBadQuery
	}
	if hasAID {
		f.ActorID = &aid
	}
	f.BeforeID = before
	f.Limit = int(min(limit, 500))
	if f.Limit == 0 {
		f.Limit = 100
	}
	for k, dst := range map[string]**time.Time{"from": &f.From, "to": &f.To} {
		if q.Get(k) == "" {
			continue
		}
		t, e := time.Parse(time.RFC3339, q.Get(k))
		if e != nil {
			return f, false, errBadQuery
		}
		*dst = &t
	}
	if v := q.Get("db"); v != "" {
		var d store.Database
		var e error
		if id, perr := strconv.ParseInt(v, 10, 64); perr == nil {
			d, e = s.a.Store.GetDatabase(r.Context(), id)
		} else {
			d, e = s.a.Store.GetDatabaseByName(r.Context(), v)
		}
		if e == nil {
			e = s.canAccess(r, d)
		}
		if errors.Is(e, store.ErrNotFound) {
			return f, true, nil // same answer as for an empty history: existence is not revealed
		}
		if e != nil {
			return f, false, e
		}
		f.DBID = &d.ID
	}
	return f, false, nil
}

type auditJSON struct {
	ID         int64          `json:"id"`
	At         time.Time      `json:"at"`
	ActorID    *int64         `json:"actor_id"`
	Actor      string         `json:"actor"`
	IP         string         `json:"ip"`
	Action     string         `json:"action"`
	TargetType string         `json:"target_type"`
	TargetID   string         `json:"target_id"`
	DBID       *int64         `json:"db_id"`
	DB         string         `json:"db"`
	Success    bool           `json:"success"`
	Detail     map[string]any `json:"detail"`
}

// auditOut renders entries with database names (cached per call).
func (s *server) auditOut(ctx context.Context, es []store.AuditEntry) ([]auditJSON, error) {
	names := map[int64]string{}
	out := make([]auditJSON, 0, len(es))
	for _, e := range es {
		name := ""
		if e.DBID != nil {
			var ok bool
			if name, ok = names[*e.DBID]; !ok {
				d, err := s.a.Store.GetDatabase(ctx, *e.DBID)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return nil, err
				}
				name = d.Name
				names[*e.DBID] = name
			}
		}
		out = append(out, auditJSON{e.ID, e.At, e.ActorID, e.Actor, e.IP, e.Action, e.TargetType, e.TargetID, e.DBID, name, e.Success, e.Detail})
	}
	return out, nil
}

func (s *server) badQuery(w http.ResponseWriter, err error) {
	if errors.Is(err, errBadQuery) {
		writeJSON(w, 400, map[string]string{"error": "invalid query parameter"})
		return
	}
	fail(w, err)
}

func (s *server) listAudit(w http.ResponseWriter, r *http.Request) {
	f, empty, err := s.auditFilter(r)
	if err != nil {
		s.badQuery(w, err)
		return
	}
	var es []store.AuditEntry
	if !empty {
		if es, err = s.a.Store.ListAudit(r.Context(), f); err != nil {
			fail(w, err)
			return
		}
	}
	out, err := s.auditOut(r.Context(), es)
	if err != nil {
		fail(w, err)
		return
	}
	var next *int64
	if len(es) > 0 && len(es) == f.Limit { // a full page: there may be more
		next = &es[len(es)-1].ID
	}
	writeJSON(w, 200, map[string]any{"entries": out, "next_before": next})
}

// csvCell defuses spreadsheet formulas in attacker-influenced text (usernames tried at login).
func csvCell(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

// auditCSV streams every entry matching the filter, 500 per query.
// ponytail: capped at 100 pages (50000 rows); narrow the filter beyond that.
func (s *server) auditCSV(w http.ResponseWriter, r *http.Request) {
	f, empty, err := s.auditFilter(r)
	if err != nil {
		s.badQuery(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition", `attachment; filename="audit.csv"`)
	cw := csv.NewWriter(w)
	cw.Write([]string{"at", "actor", "ip", "action", "target_type", "target_id", "db", "success", "detail"})
	f.Limit = 500
	for page := 0; page < 100 && !empty; page++ {
		es, err := s.a.Store.ListAudit(r.Context(), f)
		if err == nil {
			var rows []auditJSON
			if rows, err = s.auditOut(r.Context(), es); err == nil {
				for _, e := range rows {
					d, _ := json.Marshal(e.Detail)
					cw.Write([]string{e.At.UTC().Format(time.RFC3339), csvCell(e.Actor), e.IP, e.Action, e.TargetType, csvCell(e.TargetID),
						csvCell(e.DB), strconv.FormatBool(e.Success), csvCell(string(d))})
				}
			}
		}
		if err != nil { // the 200 and part of the body are out; end the file rather than send a lie
			cw.Flush()
			return
		}
		if len(es) < f.Limit {
			break
		}
		f.BeforeID = es[len(es)-1].ID
	}
	cw.Flush()
}

// activity (admin): what is running now and who is online.
func (s *server) activity(w http.ResponseWriter, r *http.Request) {
	if !me(r).IsAdmin() {
		fail(w, auth.ErrForbidden)
		return
	}
	jobs, err := s.a.Store.RunningJobs(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	jobs = append(jobs, s.a.RunningPulls()...)
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].StartedAt.Before(jobs[j].StartedAt) })
	on, err := s.a.Store.OnlineSessions(r.Context(), time.Now().Add(-5*time.Minute))
	if err != nil {
		fail(w, err)
		return
	}
	type job struct {
		Kind      string    `json:"kind"`
		ID        int64     `json:"id"`
		Database  string    `json:"database"`
		StartedBy string    `json:"started_by"`
		StartedAt time.Time `json:"started_at"`
	}
	type online struct {
		Username   string    `json:"username"`
		IP         string    `json:"ip"`
		UserAgent  string    `json:"user_agent"`
		LastSeenAt time.Time `json:"last_seen_at"`
	}
	writeJSON(w, 200, map[string]any{
		"jobs":   mapList(jobs, func(j store.Job) job { return job{j.Kind, j.ID, j.DBName, j.StartedBy, j.StartedAt} }),
		"online": mapList(on, func(o store.OnlineSession) online { return online{o.Username, o.IP, o.UserAgent, o.LastSeenAt} }),
	})
}
