package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"fridge/internal/app"
	"fridge/internal/store"
)

// adminRoutes: members, domains and per-node superuser credentials.
func (s *server) adminRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/databases/{db}/members", s.listMembers)
	m.HandleFunc("POST /api/databases/{db}/members", s.addMember)
	m.HandleFunc("DELETE /api/databases/{db}/members/{user_id}", s.removeMember)
	m.HandleFunc("GET /api/domains", s.listDomains)
	m.HandleFunc("POST /api/domains", s.addDomain)
	m.HandleFunc("DELETE /api/domains/{id}", s.removeDomain)
	m.HandleFunc("POST /api/databases/{db}/superuser/reveal", s.revealSuperuser)
	m.HandleFunc("POST /api/databases/{db}/superuser/rotate", s.rotateSuperuser)
}

func (s *server) listMembers(w http.ResponseWriter, r *http.Request) {
	d, ok := s.dbOf(w, r)
	if !ok {
		return
	}
	ms, err := s.a.ListMembers(r.Context(), d)
	if err != nil {
		fail(w, err)
		return
	}
	type member struct {
		UserID   int64     `json:"user_id"`
		Username string    `json:"username"`
		AddedAt  time.Time `json:"added_at"`
	}
	writeJSON(w, 200, mapList(ms, func(m store.Member) member { return member{m.UserID, m.Username, m.AddedAt} }))
}

func (s *server) addMember(w http.ResponseWriter, r *http.Request) {
	want(r, "db.member_add")
	d, ok := s.ownerOf(w, r)
	if !ok {
		return
	}
	var in struct {
		Username string `json:"username"`
	}
	if !decode(w, r, &in) {
		return
	}
	note(r, "username", in.Username)
	switch err := s.a.AddMember(r.Context(), d, in.Username, me(r).UserID); {
	case errors.Is(err, app.ErrInvalid): // one message for every bad input: usernames cannot be probed
		writeJSON(w, 400, map[string]string{"error": "không thêm được"})
	case err != nil:
		fail(w, err)
	default:
		w.WriteHeader(204)
	}
}

func (s *server) removeMember(w http.ResponseWriter, r *http.Request) {
	want(r, "db.member_remove")
	d, ok := s.ownerOf(w, r)
	if !ok {
		return
	}
	note(r, "user_id", r.PathValue("user_id"))
	uid, err := strconv.ParseInt(r.PathValue("user_id"), 10, 64)
	if err != nil {
		fail(w, store.ErrNotFound)
		return
	}
	noContent(w, s.a.RemoveMember(r.Context(), d, uid))
}

type domainJSON struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	InUse     int       `json:"in_use"`
	CreatedAt time.Time `json:"created_at"`
}

func domainOut(d store.Domain) domainJSON {
	return domainJSON{d.ID, d.Name, d.OwnerName, d.InUse, d.CreatedAt}
}

// listDomains: a user's own domains; an admin's view is everyone's.
func (s *server) listDomains(w http.ResponseWriter, r *http.Request) {
	p := me(r)
	var owner *int64
	if !p.IsAdmin() {
		owner = &p.UserID
	}
	ds, err := s.a.Store.ListDomains(r.Context(), owner)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, mapList(ds, domainOut))
}

func (s *server) addDomain(w http.ResponseWriter, r *http.Request) {
	p := me(r)
	var in struct {
		Name string `json:"name"`
	}
	wantNow(r, "domain.register", "domain", "")
	if !decode(w, r, &in) {
		return
	}
	note(r, "name", in.Name)
	d, err := s.a.RegisterDomain(r.Context(), p.UserID, in.Name)
	switch {
	case errors.Is(err, store.ErrConflict): // never say who holds it
		writeJSON(w, 409, map[string]string{"error": "domain không đăng ký được"})
	case err != nil:
		fail(w, err)
	default:
		target(r, "domain", itoa(d.ID), nil)
		d.OwnerName = p.Username
		writeJSON(w, 201, domainOut(d))
	}
}

func (s *server) removeDomain(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p := me(r)
	want(r, "domain.remove")
	d, err := s.a.Store.GetDomain(r.Context(), id)
	if err == nil && !p.IsAdmin() && d.OwnerID != p.UserID {
		err = store.ErrNotFound
	}
	if err == nil { // visible to the caller from here on
		target(r, "domain", itoa(id), nil)
		note(r, "name", d.Name)
		err = s.a.Store.DeleteDomain(r.Context(), id)
	}
	noContent(w, err)
}

func (s *server) revealSuperuser(w http.ResponseWriter, r *http.Request) {
	want(r, "superuser.reveal")
	d, ok := s.ownerOf(w, r)
	if !ok {
		return
	}
	u, pw, err := s.a.RevealSuperuser(r.Context(), d)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"user": u, "password": pw})
}

func (s *server) rotateSuperuser(w http.ResponseWriter, r *http.Request) {
	want(r, "superuser.rotate")
	if d, ok := s.ownerOf(w, r); ok {
		noContent(w, s.a.RotateSuperuser(r.Context(), d))
	}
}
