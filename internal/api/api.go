// Package api exposes the app layer over HTTP with session authentication.
package api

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"fridge/internal/app"
	"fridge/internal/auth"
	"fridge/internal/engine"
	"fridge/internal/objstore"
	"fridge/internal/store"
)

type server struct {
	a       *app.App
	au      *auth.Service
	trusted func(net.IP) bool
}

// Options configures New.
type Options struct {
	Web          http.Handler         // nil: 404 for paths outside /api, /healthz, /readyz
	TrustedProxy func(ip net.IP) bool // nil: loopback only
}

// New builds the HTTP handler: public routes, then everything else behind session auth.
func New(a *app.App, au *auth.Service, o Options) http.Handler {
	s := &server{a: a, au: au, trusted: o.TrustedProxy}
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	m.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if a.Ready(r.Context()) != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	api := http.NewServeMux()
	api.HandleFunc("GET /api/engines", s.engines)
	api.HandleFunc("POST /api/databases", s.createDB)
	api.HandleFunc("GET /api/databases", s.listDBs)
	api.HandleFunc("GET /api/databases/{db}", s.getDB)
	api.HandleFunc("GET /api/destinations", s.listDests)
	api.HandleFunc("POST /api/destinations", s.createDest)
	api.HandleFunc("PUT /api/destinations/{id}", s.updateDest)
	api.HandleFunc("DELETE /api/destinations/{id}", s.deleteDest)
	api.HandleFunc("POST /api/destinations/{id}/test", s.testDest)
	api.HandleFunc("GET /api/destinations/{id}/objects", s.objects)
	api.HandleFunc("GET /api/databases/{db}/schedules", s.listScheds)
	api.HandleFunc("POST /api/databases/{db}/schedules", s.createSched)
	api.HandleFunc("PUT /api/schedules/{id}", s.updateSched)
	api.HandleFunc("DELETE /api/schedules/{id}", s.deleteSched)
	api.HandleFunc("GET /api/databases/{db}/backups", s.listBackups)
	api.HandleFunc("POST /api/databases/{db}/backups", s.startBackup)
	api.HandleFunc("GET /api/backups/{id}", s.getBackup)
	api.HandleFunc("GET /api/backups/{id}/download", s.download)
	api.HandleFunc("POST /api/restores", s.startRestore)
	api.HandleFunc("GET /api/restores/{id}", s.getRestore)
	api.HandleFunc("GET /api/settings/allowlist", s.getAllow)
	api.HandleFunc("PUT /api/settings/allowlist", s.putAllow)
	m.HandleFunc("GET /api/ca", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write(a.CA.CertPEM())
	})
	s.publicRoutes(m)
	s.loginRoutes(api)
	m.Handle("/api/", s.requireLogin(api))
	if o.Web != nil {
		m.Handle("/", o.Web)
	}
	return headers(m)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	code := 500
	switch {
	case errors.Is(err, app.ErrInvalid):
		code = 400
	case errors.Is(err, store.ErrNotFound):
		code = 404
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrInUse), errors.Is(err, store.ErrLastAdmin), errors.Is(err, auth.ErrSetupDone):
		code = 409
	case errors.Is(err, auth.ErrInvalid), errors.Is(err, auth.ErrInviteInvalid):
		code = 400
	case errors.Is(err, auth.ErrBadCredentials), errors.Is(err, auth.ErrUnauthenticated):
		code = 401
	case errors.Is(err, auth.ErrForbidden), errors.Is(err, auth.ErrBadSetupToken):
		code = 403
	case errors.Is(err, auth.ErrLocked), errors.Is(err, auth.ErrBusy):
		code = 429
	}
	msg := err.Error()
	if code == 500 {
		log.Printf("api: internal error: %v", err)
		msg = "internal error"
	}
	writeJSON(w, code, map[string]string{"error": msg})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON body"})
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, store.ErrNotFound)
		return 0, false
	}
	return id, true
}

// dbOf resolves {db}: all digits = id, otherwise name.
func (s *server) dbOf(w http.ResponseWriter, r *http.Request) (store.Database, bool) {
	v := r.PathValue("db")
	var d store.Database
	var err error
	if id, perr := strconv.ParseInt(v, 10, 64); perr == nil {
		d, err = s.a.Store.GetDatabase(r.Context(), id)
	} else {
		d, err = s.a.Store.GetDatabaseByName(r.Context(), v)
	}
	if err != nil {
		fail(w, err)
		return d, false
	}
	return d, true
}

// ---- response shapes (never include PasswordEnc / SecretKeyEnc) ----

type dbJSON struct {
	ID               int64     `json:"id"`
	Name             string    `json:"name"`
	Engine           string    `json:"engine"`
	Version          string    `json:"version"`
	Domain           string    `json:"domain"`
	TLSMode          string    `json:"tls_mode"`
	User             string    `json:"user"`
	Status           string    `json:"status"`
	CreatedAt        time.Time `json:"created_at"`
	ConnectionString string    `json:"connection_string,omitempty"`
}

func (s *server) dbOut(d store.Database, conn bool) (dbJSON, error) {
	o := dbJSON{d.ID, d.Name, d.Engine, d.Version, d.Domain, d.TLSMode, d.User, d.Status, d.CreatedAt, ""}
	if conn {
		cs, err := s.a.ConnString(d)
		if err != nil {
			return o, err
		}
		o.ConnectionString = cs
	}
	return o, nil
}

type destJSON struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Provider  string    `json:"provider"`
	Endpoint  string    `json:"endpoint"`
	Region    string    `json:"region"`
	Bucket    string    `json:"bucket"`
	AccessKey string    `json:"access_key"`
	PathStyle bool      `json:"path_style"`
	CreatedAt time.Time `json:"created_at"`
}

func destOut(d store.Destination) destJSON {
	return destJSON{d.ID, d.Name, d.Provider, d.Endpoint, d.Region, d.Bucket, d.AccessKey, d.PathStyle, d.CreatedAt}
}

type destIn struct {
	Name      string `json:"name"`
	Provider  string `json:"provider"`
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	PathStyle bool   `json:"path_style"`
}

func (in destIn) dest(id int64) store.Destination {
	return store.Destination{ID: id, Name: in.Name, Provider: in.Provider, Endpoint: in.Endpoint, Region: in.Region,
		Bucket: in.Bucket, AccessKey: in.AccessKey, PathStyle: in.PathStyle}
}

type schedJSON struct {
	ID            int64  `json:"id"`
	DatabaseID    int64  `json:"database_id"`
	DestinationID int64  `json:"destination_id"`
	Cron          string `json:"cron"`
	Prefix        string `json:"prefix"`
	KeepLatest    int    `json:"keep_latest"`
	Enabled       bool   `json:"enabled"`
}

func schedOut(x store.Schedule) schedJSON {
	return schedJSON{x.ID, x.DatabaseID, x.DestinationID, x.Cron, x.Prefix, x.KeepLatest, x.Enabled}
}

type backupJSON struct {
	ID            int64      `json:"id"`
	DatabaseID    int64      `json:"database_id"`
	DestinationID int64      `json:"destination_id"`
	ScheduleID    *int64     `json:"schedule_id"`
	Trigger       string     `json:"trigger"`
	Engine        string     `json:"engine"`
	EngineVersion string     `json:"engine_version"`
	Status        string     `json:"status"`
	S3Key         string     `json:"s3_key"`
	SizeBytes     int64      `json:"size_bytes"`
	Log           string     `json:"log"`
	Error         string     `json:"error"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
}

func backupOut(b store.Backup) backupJSON {
	return backupJSON{b.ID, b.DatabaseID, b.DestinationID, b.ScheduleID, b.Trigger, b.Engine, b.EngineVersion,
		b.Status, b.S3Key, b.SizeBytes, b.Log, b.Error, b.StartedAt, b.FinishedAt}
}

type restoreJSON struct {
	ID               int64      `json:"id"`
	DestinationID    int64      `json:"destination_id"`
	S3Key            string     `json:"s3_key"`
	SourceEngine     string     `json:"source_engine"`
	SourceVersion    string     `json:"source_version"`
	Mode             string     `json:"mode"`
	Status           string     `json:"status"`
	Log              string     `json:"log"`
	Error            string     `json:"error"`
	TargetDatabaseID *int64     `json:"target_database_id"`
	StartedAt        time.Time  `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at"`
}

func restoreOut(x store.Restore) restoreJSON {
	return restoreJSON{x.ID, x.DestinationID, x.S3Key, x.SourceEngine, x.SourceVersion, x.Mode, x.Status, x.Log,
		x.Error, x.TargetDatabaseID, x.StartedAt, x.FinishedAt}
}

func mapList[T, U any](in []T, f func(T) U) []U {
	out := make([]U, 0, len(in))
	for _, v := range in {
		out = append(out, f(v))
	}
	return out
}

// ---- handlers ----

func (s *server) engines(w http.ResponseWriter, r *http.Request) {
	type info struct {
		Name       string   `json:"name"`
		Versions   []string `json:"versions"`
		Default    string   `json:"default"`
		EntryPoint string   `json:"entrypoint"`
		PublicPort int      `json:"public_port"`
	}
	writeJSON(w, 200, mapList(engine.List(), func(i engine.Info) info {
		return info{i.Name, i.Versions, i.Default, i.EntryPoint, i.PublicPort}
	}))
}

func (s *server) createDB(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Engine  string `json:"engine"`
		Name    string `json:"name"`
		Domain  string `json:"domain"`
		Version string `json:"version"`
		TLSMode string `json:"tls_mode"`
	}
	if !decode(w, r, &in) {
		return
	}
	d, err := s.a.CreateDatabase(r.Context(), app.CreateRequest{Engine: in.Engine, Name: in.Name, Domain: in.Domain, Version: in.Version, TLSMode: in.TLSMode})
	if err != nil {
		fail(w, err)
		return
	}
	o, err := s.dbOut(d, true)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, o)
}

func (s *server) listDBs(w http.ResponseWriter, r *http.Request) {
	ds, err := s.a.Store.ListDatabases(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, mapList(ds, func(d store.Database) dbJSON { o, _ := s.dbOut(d, false); return o }))
}

func (s *server) getDB(w http.ResponseWriter, r *http.Request) {
	d, ok := s.dbOf(w, r)
	if !ok {
		return
	}
	o, err := s.dbOut(d, true)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, o)
}

func (s *server) listDests(w http.ResponseWriter, r *http.Request) {
	ds, err := s.a.Store.ListDestinations(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, mapList(ds, destOut))
}

func (s *server) createDest(w http.ResponseWriter, r *http.Request) {
	var in destIn
	if !decode(w, r, &in) {
		return
	}
	d, err := s.a.CreateDestination(r.Context(), in.dest(0), in.SecretKey)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, destOut(d))
}

func (s *server) updateDest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in destIn
	if !decode(w, r, &in) {
		return
	}
	if err := s.a.UpdateDestination(r.Context(), in.dest(id), in.SecretKey); err != nil {
		fail(w, err)
		return
	}
	d, err := s.a.Store.GetDestination(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, destOut(d))
}

func (s *server) deleteDest(w http.ResponseWriter, r *http.Request) {
	if id, ok := pathID(w, r); ok {
		del(w, s.a.DeleteDestination(r.Context(), id))
	}
}

func del(w http.ResponseWriter, err error) {
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

func (s *server) testDest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := s.a.Store.GetDestination(r.Context(), id); err != nil {
		fail(w, err) // lookup failures (404/500) are not a bad destination
		return
	}
	if err := s.a.TestDestination(r.Context(), id); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *server) objects(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	objs, trunc, err := s.a.ListObjects(r.Context(), id, r.URL.Query().Get("prefix"), r.URL.Query().Get("q"))
	if err != nil {
		fail(w, err)
		return
	}
	type obj struct {
		Key          string    `json:"key"`
		Size         int64     `json:"size"`
		LastModified time.Time `json:"last_modified"`
	}
	writeJSON(w, 200, map[string]any{
		"objects":   mapList(objs, func(o objstore.Object) obj { return obj{o.Key, o.Size, o.LastModified} }),
		"truncated": trunc,
	})
}

type schedIn struct {
	DestinationID int64  `json:"destination_id"`
	Cron          string `json:"cron"`
	Prefix        string `json:"prefix"`
	KeepLatest    int    `json:"keep_latest"`
	Enabled       bool   `json:"enabled"`
}

func (s *server) listScheds(w http.ResponseWriter, r *http.Request) {
	d, ok := s.dbOf(w, r)
	if !ok {
		return
	}
	xs, err := s.a.Store.ListSchedules(r.Context(), d.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, mapList(xs, schedOut))
}

func (s *server) createSched(w http.ResponseWriter, r *http.Request) {
	d, ok := s.dbOf(w, r)
	if !ok {
		return
	}
	var in schedIn
	if !decode(w, r, &in) {
		return
	}
	sc, err := s.a.CreateSchedule(r.Context(), store.Schedule{DatabaseID: d.ID, DestinationID: in.DestinationID,
		Cron: in.Cron, Prefix: in.Prefix, KeepLatest: in.KeepLatest, Enabled: in.Enabled})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, schedOut(sc))
}

func (s *server) updateSched(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in schedIn
	if !decode(w, r, &in) {
		return
	}
	sc := store.Schedule{ID: id, DestinationID: in.DestinationID, Cron: in.Cron, Prefix: in.Prefix, KeepLatest: in.KeepLatest, Enabled: in.Enabled}
	if err := s.a.UpdateSchedule(r.Context(), sc); err != nil {
		fail(w, err)
		return
	}
	got, err := s.a.Store.GetSchedule(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, schedOut(got))
}

func (s *server) deleteSched(w http.ResponseWriter, r *http.Request) {
	if id, ok := pathID(w, r); ok {
		del(w, s.a.DeleteSchedule(r.Context(), id))
	}
}

func (s *server) listBackups(w http.ResponseWriter, r *http.Request) {
	d, ok := s.dbOf(w, r)
	if !ok {
		return
	}
	bs, err := s.a.Store.ListBackups(r.Context(), d.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, mapList(bs, backupOut))
}

func (s *server) startBackup(w http.ResponseWriter, r *http.Request) {
	d, ok := s.dbOf(w, r)
	if !ok {
		return
	}
	var in struct {
		DestinationID int64 `json:"destination_id"`
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	b, err := s.a.StartBackup(r.Context(), d.ID, in.DestinationID, nil, "manual")
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 202, backupOut(b))
}

func (s *server) getBackup(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	b, err := s.a.Store.GetBackup(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, backupOut(b))
}

func (s *server) download(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	u, err := s.a.BackupDownloadURL(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"url": u})
}

func (s *server) startRestore(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DestinationID int64  `json:"destination_id"`
		Key           string `json:"key"`
		Engine        string `json:"engine"`
		New           *struct {
			Name    string `json:"name"`
			Domain  string `json:"domain"`
			Version string `json:"version"`
		} `json:"new"`
	}
	if !decode(w, r, &in) {
		return
	}
	req := app.RestoreRequest{DestinationID: in.DestinationID, Key: in.Key, Engine: in.Engine}
	if in.New != nil {
		req.New = &app.CreateRequest{Name: in.New.Name, Domain: in.New.Domain, Version: in.New.Version}
	}
	x, err := s.a.StartRestore(r.Context(), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 202, restoreOut(x))
}

func (s *server) getRestore(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	x, err := s.a.Store.GetRestore(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, restoreOut(x))
}

func (s *server) getAllow(w http.ResponseWriter, r *http.Request) {
	c, err := s.a.Allowlist(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string][]string{"cidrs": c})
}

func (s *server) putAllow(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CIDRs []string `json:"cidrs"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.a.SetAllowlist(r.Context(), in.CIDRs); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string][]string{"cidrs": in.CIDRs})
}
