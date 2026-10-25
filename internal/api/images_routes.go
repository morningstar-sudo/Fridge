package api

import (
	"context"
	"net/http"
	"time"

	"fridge/internal/app"
	"fridge/internal/auth"
)

func (s *server) imageRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/images", s.listImages)
	m.HandleFunc("POST /api/images/pull", s.pullImages)
	m.HandleFunc("GET /api/images/pull/{job}", s.pullStatus)
	m.HandleFunc("GET /api/images/prune", s.prunePreview)
	m.HandleFunc("POST /api/images/prune", s.pruneImages)
	m.HandleFunc("POST /api/images/check-updates", s.checkUpdates)
}

type imageJSON struct {
	Ref         string      `json:"ref"`
	ShortDigest string      `json:"short_digest"`
	Version     string      `json:"version"`
	Size        int64       `json:"size"`
	Created     *time.Time  `json:"created"`
	Present     bool        `json:"present"`
	UsedBy      []string    `json:"used_by"`
	LastUsed    *time.Time  `json:"last_used"`
	Update      *updateJSON `json:"update"`
}

type updateJSON struct {
	LocalVersion  string    `json:"local_version"`
	RemoteVersion string    `json:"remote_version"`
	Available     bool      `json:"available"` // the registry's image differs from the one on disk
	Error         string    `json:"error,omitempty"`
	CheckedAt     time.Time `json:"checked_at"`
}

func imageOut(r app.ImageRow) imageJSON {
	o := imageJSON{Ref: r.Ref, ShortDigest: r.ShortDigest, Version: r.Version, Size: r.Size, Present: r.Present,
		UsedBy: r.UsedBy, LastUsed: r.LastUsed}
	if o.UsedBy == nil {
		o.UsedBy = []string{}
	}
	if !r.Created.IsZero() {
		o.Created = &r.Created
	}
	if u := r.Update; u != nil {
		o.Update = &updateJSON{u.LocalVersion, u.RemoteVersion, u.Error == "" && u.RemoteDigest != "" && u.RemoteDigest != u.LocalDigest, u.Error, u.CheckedAt}
	}
	return o
}

// adminOnly writes 403 unless the caller is an admin.
func adminOnly(w http.ResponseWriter, r *http.Request) bool {
	if !me(r).IsAdmin() {
		fail(w, auth.ErrForbidden)
		return false
	}
	return true
}

func (s *server) listImages(w http.ResponseWriter, r *http.Request) {
	if !adminOnly(w, r) {
		return
	}
	rows, err := s.a.ListImages(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, mapList(rows, imageOut))
}

func (s *server) pullImages(w http.ResponseWriter, r *http.Request) {
	wantNow(r, "image.pull", "image", "")
	if !adminOnly(w, r) {
		return
	}
	var in struct {
		Refs []string `json:"refs"`
	}
	if !decode(w, r, &in) {
		return
	}
	note(r, "refs", in.Refs)
	job, err := s.a.PullImages(in.Refs, me(r).Username)
	if err != nil {
		fail(w, err)
		return
	}
	target(r, "image", job, nil)
	writeJSON(w, 202, map[string]string{"job": job})
}

func (s *server) pullStatus(w http.ResponseWriter, r *http.Request) {
	if !adminOnly(w, r) {
		return
	}
	st, err := s.a.PullStatus(r.PathValue("job"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"status": st})
}

func (s *server) prunePreview(w http.ResponseWriter, r *http.Request) {
	if !adminOnly(w, r) {
		return
	}
	rows, total, err := s.a.PrunePreview(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"images": mapList(rows, imageOut), "bytes": total})
}

func (s *server) pruneImages(w http.ResponseWriter, r *http.Request) {
	wantNow(r, "image.prune", "image", "")
	if !adminOnly(w, r) {
		return
	}
	var in struct {
		Refs []string `json:"refs"`
	}
	if !decode(w, r, &in) {
		return
	}
	removed, err := s.a.Prune(r.Context(), in.Refs)
	if removed == nil {
		removed = []string{}
	}
	note(r, "removed", removed)
	if err != nil && len(removed) == 0 {
		fail(w, err)
		return
	}
	out := map[string]any{"removed": removed}
	if err != nil { // some went, some did not
		out["errors"] = err.Error()
	}
	writeJSON(w, 200, out)
}

func (s *server) checkUpdates(w http.ResponseWriter, r *http.Request) {
	if !adminOnly(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := s.a.CheckImageUpdates(ctx); err != nil {
		fail(w, err)
		return
	}
	s.listImages(w, r)
}
