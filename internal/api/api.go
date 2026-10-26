// Package api exposes the app layer over HTTP with session authentication.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path"
	"strconv"
	"time"

	"fridge/internal/app"
	"fridge/internal/auth"
	"fridge/internal/engine"
	"fridge/internal/store"
)

type server struct {
	a       *app.App
	au      *auth.Service
	trusted func(net.IP) bool
	locked  lockedSeen
}

// Options configures New.
type Options struct {
	Web          http.Handler         // nil: 404 for paths outside /api, /healthz, /readyz
	TrustedProxy func(ip net.IP) bool // nil: loopback only
}

// New builds the HTTP handler: public routes, then everything else behind session auth.
// Every resource route checks the caller's rights: invisible resources are 404, forbidden actions on visible ones 403.
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
	api.HandleFunc("GET /api/databases/{db}/connection", s.connection)
	api.HandleFunc("GET /api/databases/{db}/ca", s.databaseCA)
	api.HandleFunc("GET /api/destinations", s.listDests)
	api.HandleFunc("POST /api/destinations", s.createDest)
	api.HandleFunc("PUT /api/destinations/{id}", s.updateDest)
	api.HandleFunc("PATCH /api/destinations/{id}", s.shareDest)
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
	api.HandleFunc("GET /api/settings/endpoint-allowlist", s.getEndpointAllow)
	api.HandleFunc("PUT /api/settings/endpoint-allowlist", s.putEndpointAllow)
	s.adminRoutes(api)
	s.auditRoutes(api)
	s.imageRoutes(api)
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

// me is the authenticated caller; every route behind requireLogin has one.
func me(r *http.Request) auth.Principal {
	p, _ := principal(r)
	return p
}

// dbOf resolves {db} (all digits = id, otherwise the full prefixed name); 404 unless the caller may access it.
func (s *server) dbOf(w http.ResponseWriter, r *http.Request) (store.Database, bool) {
	v := r.PathValue("db")
	var d store.Database
	var err error
	if id, perr := strconv.ParseInt(v, 10, 64); perr == nil {
		d, err = s.a.Store.GetDatabase(r.Context(), id)
	} else {
		d, err = s.a.Store.GetDatabaseByName(r.Context(), v)
	}
	if err == nil {
		err = s.canAccess(r, d)
	}
	if err != nil {
		fail(w, err)
		return d, false
	}
	target(r, "database", d.Name, &d.ID)
	return d, true
}

// canAccess: nil, store.ErrNotFound (no access) or a lookup error.
func (s *server) canAccess(r *http.Request, d store.Database) error {
	ok, err := s.a.CanAccessDB(r.Context(), me(r), d)
	if err == nil && !ok {
		err = store.ErrNotFound
	}
	return err
}

// ownerOf is dbOf plus 403 unless the caller owns d (or is admin).
func (s *server) ownerOf(w http.ResponseWriter, r *http.Request) (store.Database, bool) {
	d, ok := s.dbOf(w, r)
	if ok && !app.IsDBOwner(me(r), d) {
		fail(w, auth.ErrForbidden)
		return d, false
	}
	return d, ok
}

// destOf loads {id}: 404 unless visible to the caller, 403 if manage is needed and the caller cannot manage it.
func (s *server) destOf(w http.ResponseWriter, r *http.Request, manage bool) (store.Destination, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return store.Destination{}, false
	}
	d, err := s.a.Store.GetDestination(r.Context(), id)
	p := me(r)
	switch {
	case err != nil:
	case !app.CanManageDestination(p, d) && !app.CanUseDestination(p, d):
		err = store.ErrNotFound
	case manage && !app.CanManageDestination(p, d):
		err = auth.ErrForbidden
	}
	if err != nil {
		fail(w, err)
		return d, false
	}
	target(r, "destination", itoa(d.ID), nil)
	return d, true
}

// usableDest writes 400 "destination not usable" unless both the caller and db's owner may use destination id.
// Missing and foreign destinations look the same.
func (s *server) usableDest(w http.ResponseWriter, r *http.Request, db store.Database, id int64) bool {
	d, err := s.a.Store.GetDestination(r.Context(), id)
	ok := false
	if err == nil {
		ok, err = s.a.CanUseDestinationFor(r.Context(), me(r), db, d)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		fail(w, err)
		return false
	}
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "destination not usable"})
		return false
	}
	return true
}

// ---- response shapes (never include PasswordEnc / SecretKeyEnc) ----

type lastBackupJSON struct {
	Status     string     `json:"status"`
	FinishedAt *time.Time `json:"finished_at"`
}

type dbJSON struct {
	ID               int64           `json:"id"`
	Name             string          `json:"name"`
	Engine           string          `json:"engine"`
	Version          string          `json:"version"`
	Domain           string          `json:"domain"`
	TLSMode          string          `json:"tls_mode"`
	User             string          `json:"user"`
	Status           string          `json:"status"`
	CreatedAt        time.Time       `json:"created_at"`
	Owner            string          `json:"owner"`
	ConnectionString string          `json:"connection_string"` // password always masked
	AdminUser        string          `json:"admin_user,omitempty"`
	Legacy           bool            `json:"legacy,omitempty"`
	LastBackup       *lastBackupJSON `json:"last_backup"`
}

// dbOut renders d for p; the superuser name only for its owner/admin.
func dbOut(p auth.Principal, d store.Database, last map[int64]store.Backup) dbJSON {
	cs, err := app.MaskedConnString(d)
	if err != nil {
		log.Printf("api: connection string of %s: %v", d.Name, err)
	}
	o := dbJSON{ID: d.ID, Name: d.Name, Engine: d.Engine, Version: d.Version, Domain: d.Domain, TLSMode: d.TLSMode,
		User: d.User, Status: d.Status, CreatedAt: d.CreatedAt, Owner: d.OwnerName, ConnectionString: cs, Legacy: d.AdminUser == ""}
	if app.IsDBOwner(p, d) {
		o.AdminUser = d.AdminUser
	}
	if b, ok := last[d.ID]; ok {
		o.LastBackup = &lastBackupJSON{b.Status, b.FinishedAt}
	}
	return o
}

type destJSON struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Provider  string    `json:"provider"`
	Endpoint  string    `json:"endpoint,omitempty"`
	Region    string    `json:"region"`
	Bucket    string    `json:"bucket,omitempty"`
	AccessKey string    `json:"access_key,omitempty"`
	PathStyle bool      `json:"path_style"`
	CreatedAt time.Time `json:"created_at"`
	Owner     string    `json:"owner"`
	Shared    bool      `json:"shared"`
}

// destOut hides where a shared destination points from users who cannot manage it.
func destOut(p auth.Principal, d store.Destination) destJSON {
	o := destJSON{d.ID, d.Name, d.Provider, d.Endpoint, d.Region, d.Bucket, d.AccessKey, d.PathStyle, d.CreatedAt, d.OwnerName, d.Shared}
	if !app.CanManageDestination(p, d) {
		o.Endpoint, o.Bucket, o.AccessKey = "", "", ""
	}
	return o
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
	p := me(r)
	var in struct {
		Engine     string `json:"engine"`
		Name       string `json:"name"`
		Domain     string `json:"domain"`
		Version    string `json:"version"`
		TLSMode    string `json:"tls_mode"`
		DBUser     string `json:"db_user"`
		DBPassword string `json:"db_password"`
	}
	wantNow(r, "db.create", "database", "")
	if !decode(w, r, &in) {
		return
	}
	note(r, "engine", in.Engine)
	note(r, "name", in.Name)
	d, err := s.a.CreateDatabase(r.Context(), app.CreateRequest{Engine: in.Engine, Name: in.Name, Domain: in.Domain, Version: in.Version,
		TLSMode: in.TLSMode, OwnerID: p.UserID, DBUser: in.DBUser, DBPassword: in.DBPassword})
	if err != nil {
		fail(w, err)
		return
	}
	target(r, "database", d.Name, &d.ID)
	d.OwnerName = p.Username
	writeJSON(w, 201, dbOut(p, d, nil))
}

func (s *server) listDBs(w http.ResponseWriter, r *http.Request) {
	p := me(r)
	ds, err := s.a.VisibleDatabases(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	last, err := s.a.Store.LastBackups(r.Context(), mapList(ds, func(d store.Database) int64 { return d.ID }))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, mapList(ds, func(d store.Database) dbJSON { return dbOut(p, d, last) }))
}

func (s *server) getDB(w http.ResponseWriter, r *http.Request) {
	d, ok := s.dbOf(w, r)
	if !ok {
		return
	}
	last, err := s.a.Store.LastBackups(r.Context(), []int64{d.ID})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, dbOut(me(r), d, last))
}

// connection reveals the full app connection string; ?role=superuser the superuser's (owner/admin only).
func (s *server) connection(w http.ResponseWriter, r *http.Request) {
	d, ok := s.dbOf(w, r)
	if !ok {
		return
	}
	var cs string
	var err error
	switch r.URL.Query().Get("role") {
	case "":
		want(r, "db.reveal_password")
		cs, err = s.a.ConnString(d)
	case "superuser":
		want(r, "superuser.reveal")
		if !app.IsDBOwner(me(r), d) {
			fail(w, auth.ErrForbidden)
			return
		}
		cs, err = s.a.SuperuserConnString(r.Context(), d)
	default:
		writeJSON(w, 400, map[string]string{"error": "role must be empty or superuser"})
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"connection_string": cs})
}

func (s *server) listDests(w http.ResponseWriter, r *http.Request) {
	p := me(r)
	ds, err := s.a.VisibleDestinations(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, mapList(ds, func(d store.Destination) destJSON { return destOut(p, d) }))
}

func (s *server) createDest(w http.ResponseWriter, r *http.Request) {
	var in destIn
	wantNow(r, "dest.create", "destination", "")
	if !decode(w, r, &in) {
		return
	}
	note(r, "name", in.Name)
	p := me(r)
	if err := s.a.CheckEndpoint(r.Context(), p, in.Endpoint); err != nil {
		fail(w, err)
		return
	}
	nd := in.dest(0)
	nd.OwnerID = &p.UserID // private until an admin shares it
	d, err := s.a.CreateDestination(r.Context(), nd, in.SecretKey)
	if err != nil {
		fail(w, err)
		return
	}
	target(r, "destination", itoa(d.ID), nil)
	d.OwnerName = p.Username
	writeJSON(w, 201, destOut(p, d))
}

func (s *server) updateDest(w http.ResponseWriter, r *http.Request) {
	want(r, "dest.update")
	old, ok := s.destOf(w, r, true)
	if !ok {
		return
	}
	var in destIn
	if !decode(w, r, &in) {
		return
	}
	if err := s.a.CheckEndpoint(r.Context(), me(r), in.Endpoint); err != nil {
		fail(w, err)
		return
	}
	if err := s.a.UpdateDestination(r.Context(), in.dest(old.ID), in.SecretKey); err != nil {
		fail(w, err)
		return
	}
	d, err := s.a.Store.GetDestination(r.Context(), old.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, destOut(me(r), d))
}

// shareDest (admin only) makes a destination usable by every user.
func (s *server) shareDest(w http.ResponseWriter, r *http.Request) {
	wantNow(r, "dest.share", "destination", r.PathValue("id"))
	if !me(r).IsAdmin() {
		fail(w, auth.ErrForbidden)
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Shared *bool `json:"shared"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Shared == nil {
		writeJSON(w, 400, map[string]string{"error": "shared is required"})
		return
	}
	note(r, "shared", *in.Shared)
	noContent(w, s.a.SetDestinationShared(r.Context(), id, *in.Shared))
}

func (s *server) deleteDest(w http.ResponseWriter, r *http.Request) {
	want(r, "dest.delete")
	if d, ok := s.destOf(w, r, true); ok {
		noContent(w, s.a.DeleteDestination(r.Context(), d.ID))
	}
}

func (s *server) testDest(w http.ResponseWriter, r *http.Request) {
	want(r, "dest.test")
	d, ok := s.destOf(w, r, true)
	if !ok {
		return
	}
	if err := s.a.TestDestination(r.Context(), d.ID); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// objects lists what the caller may restore: the whole bucket for managers, own backups on a shared destination.
func (s *server) objects(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	objs, trunc, err := s.a.ListRestorable(r.Context(), me(r), id, r.URL.Query().Get("prefix"), r.URL.Query().Get("q"))
	if err != nil {
		fail(w, err)
		return
	}
	type obj struct {
		Key          string    `json:"key"`
		Size         int64     `json:"size"`
		LastModified time.Time `json:"last_modified"`
		Database     string    `json:"database"`
	}
	writeJSON(w, 200, map[string]any{
		"objects":   mapList(objs, func(o app.Restorable) obj { return obj{o.Key, o.Size, o.LastModified, o.DatabaseName} }),
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

// schedOf loads {id}; 404 unless the caller may access the schedule's database.
func (s *server) schedOf(w http.ResponseWriter, r *http.Request) (store.Schedule, store.Database, bool) {
	var d store.Database
	id, ok := pathID(w, r)
	if !ok {
		return store.Schedule{}, d, false
	}
	sc, err := s.a.Store.GetSchedule(r.Context(), id)
	if err == nil {
		if d, err = s.a.Store.GetDatabase(r.Context(), sc.DatabaseID); err == nil {
			err = s.canAccess(r, d)
		}
	}
	if err != nil {
		fail(w, err)
		return sc, d, false
	}
	target(r, "schedule", itoa(sc.ID), &d.ID)
	return sc, d, true
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
	want(r, "schedule.create")
	d, ok := s.dbOf(w, r)
	if !ok {
		return
	}
	var in schedIn
	if !decode(w, r, &in) || !s.usableDest(w, r, d, in.DestinationID) {
		return
	}
	sc, err := s.a.CreateSchedule(r.Context(), store.Schedule{DatabaseID: d.ID, DestinationID: in.DestinationID,
		Cron: in.Cron, Prefix: in.Prefix, KeepLatest: in.KeepLatest, Enabled: in.Enabled})
	if err != nil {
		fail(w, err)
		return
	}
	target(r, "schedule", itoa(sc.ID), &d.ID)
	writeJSON(w, 201, schedOut(sc))
}

func (s *server) updateSched(w http.ResponseWriter, r *http.Request) {
	want(r, "schedule.update")
	old, d, ok := s.schedOf(w, r)
	if !ok {
		return
	}
	var in schedIn
	if !decode(w, r, &in) || !s.usableDest(w, r, d, in.DestinationID) {
		return
	}
	sc := store.Schedule{ID: old.ID, DestinationID: in.DestinationID, Cron: in.Cron, Prefix: in.Prefix, KeepLatest: in.KeepLatest, Enabled: in.Enabled}
	if err := s.a.UpdateSchedule(r.Context(), sc); err != nil {
		fail(w, err)
		return
	}
	got, err := s.a.Store.GetSchedule(r.Context(), old.ID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, schedOut(got))
}

func (s *server) deleteSched(w http.ResponseWriter, r *http.Request) {
	want(r, "schedule.delete")
	if sc, _, ok := s.schedOf(w, r); ok {
		noContent(w, s.a.DeleteSchedule(r.Context(), sc.ID))
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

// startBackup: an explicit destination must be usable by the caller; none = the app's owner-destination fallback.
func (s *server) startBackup(w http.ResponseWriter, r *http.Request) {
	p := me(r)
	want(r, "backup.start")
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
	if in.DestinationID != 0 && !s.usableDest(w, r, d, in.DestinationID) {
		return
	}
	b, err := s.a.StartBackup(r.Context(), d.ID, in.DestinationID, nil, "manual", &p.UserID)
	if err != nil {
		fail(w, err)
		return
	}
	target(r, "backup", itoa(b.ID), &d.ID)
	writeJSON(w, 202, backupOut(b))
}

// backupOf loads {id}; 404 unless the caller may access the backup's database.
func (s *server) backupOf(w http.ResponseWriter, r *http.Request) (store.Backup, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return store.Backup{}, false
	}
	b, err := s.a.Store.GetBackup(r.Context(), id)
	if err == nil {
		var d store.Database
		if d, err = s.a.Store.GetDatabase(r.Context(), b.DatabaseID); err == nil {
			err = s.canAccess(r, d)
		}
	}
	if err != nil {
		fail(w, err)
		return b, false
	}
	target(r, "backup", itoa(b.ID), &b.DatabaseID)
	return b, true
}

func (s *server) getBackup(w http.ResponseWriter, r *http.Request) {
	if b, ok := s.backupOf(w, r); ok {
		writeJSON(w, 200, backupOut(b))
	}
}

func (s *server) download(w http.ResponseWriter, r *http.Request) {
	want(r, "backup.download")
	b, ok := s.backupOf(w, r)
	if !ok {
		return
	}
	rc, b, err := s.a.OpenBackup(r.Context(), b.ID)
	if err != nil {
		fail(w, err)
		return
	}
	defer rc.Close()
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path.Base(b.S3Key)))
	if b.SizeBytes > 0 { // recorded at upload; a cut stream then shows as a short body
		h.Set("Content-Length", strconv.FormatInt(b.SizeBytes, 10))
	}
	if _, err := io.Copy(w, rc); err != nil {
		log.Printf("api: download backup %d: %v", b.ID, err)
	}
}

func (s *server) startRestore(w http.ResponseWriter, r *http.Request) {
	p := me(r)
	var in struct {
		DestinationID int64  `json:"destination_id"`
		Key           string `json:"key"`
		Engine        string `json:"engine"`
		New           *struct {
			Name       string `json:"name"`
			Domain     string `json:"domain"`
			Version    string `json:"version"`
			DBUser     string `json:"db_user"`
			DBPassword string `json:"db_password"`
		} `json:"new"`
	}
	wantNow(r, "restore.start", "restore", "")
	if !decode(w, r, &in) {
		return
	}
	note(r, "destination_id", in.DestinationID)
	ok, err := s.a.CanRestoreKey(r.Context(), p, in.DestinationID, in.Key)
	if err == nil && !ok {
		err = store.ErrNotFound
	}
	if err != nil {
		fail(w, err)
		return
	}
	req := app.RestoreRequest{DestinationID: in.DestinationID, Key: in.Key, Engine: in.Engine, OwnerID: p.UserID}
	if in.New != nil {
		req.New = &app.CreateRequest{Name: in.New.Name, Domain: in.New.Domain, Version: in.New.Version, DBUser: in.New.DBUser, DBPassword: in.New.DBPassword}
	}
	x, err := s.a.StartRestore(r.Context(), req)
	if err != nil {
		fail(w, err)
		return
	}
	target(r, "restore", itoa(x.ID), x.TargetDatabaseID)
	writeJSON(w, 202, restoreOut(x))
}

func (s *server) getRestore(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	x, err := s.a.Store.GetRestore(r.Context(), id)
	if p := me(r); err == nil && !p.IsAdmin() && (x.StartedByID == nil || *x.StartedByID != p.UserID) {
		err = store.ErrNotFound
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, restoreOut(x))
}

func (s *server) getAllow(w http.ResponseWriter, r *http.Request) {
	if !me(r).IsAdmin() {
		fail(w, auth.ErrForbidden)
		return
	}
	c, err := s.a.Allowlist(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string][]string{"cidrs": c})
}

func (s *server) putAllow(w http.ResponseWriter, r *http.Request) {
	wantNow(r, "settings.allowlist", "setting", "allowlist")
	if !me(r).IsAdmin() {
		fail(w, auth.ErrForbidden)
		return
	}
	var in struct {
		CIDRs []string `json:"cidrs"`
	}
	if !decode(w, r, &in) {
		return
	}
	note(r, "cidrs", in.CIDRs)
	if err := s.a.SetAllowlist(r.Context(), in.CIDRs); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string][]string{"cidrs": in.CIDRs})
}

// getEndpointAllow / putEndpointAllow: hosts an admin exempts from the destination SSRF guard.
func (s *server) getEndpointAllow(w http.ResponseWriter, r *http.Request) {
	if !me(r).IsAdmin() {
		fail(w, auth.ErrForbidden)
		return
	}
	h, err := s.a.EndpointAllowlist(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string][]string{"hosts": h})
}

func (s *server) putEndpointAllow(w http.ResponseWriter, r *http.Request) {
	wantNow(r, "settings.endpoint_allowlist", "setting", "s3_endpoint_allowlist")
	if !me(r).IsAdmin() {
		fail(w, auth.ErrForbidden)
		return
	}
	var in struct {
		Hosts []string `json:"hosts"`
	}
	if !decode(w, r, &in) {
		return
	}
	note(r, "hosts", in.Hosts)
	if err := s.a.SetEndpointAllowlist(r.Context(), in.Hosts); err != nil {
		fail(w, err)
		return
	}
	h, err := s.a.EndpointAllowlist(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string][]string{"hosts": h})
}

// databaseCA serves the CA certificate that vouches for this database's server certificate: its owner's own CA, so a
// client that trusts it trusts that owner's databases and nobody else's. Anyone who may see the database may have it.
func (s *server) databaseCA(w http.ResponseWriter, r *http.Request) {
	d, ok := s.dbOf(w, r)
	if !ok {
		return
	}
	pemCA, err := s.a.DatabaseCA(r.Context(), d)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="`+d.Name+`-ca.crt"`)
	io.WriteString(w, pemCA)
}
