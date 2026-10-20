package api

import (
	"encoding/hex"
	"net/http"
	"time"

	"fridge/internal/auth"
	"fridge/internal/store"
)

type credJSON struct {
	Salt       []byte `json:"salt"`
	Iterations int    `json:"iterations"`
	StoredKey  []byte `json:"stored_key"`
	ServerKey  []byte `json:"server_key"`
}

func (c credJSON) creds() auth.Credentials {
	return auth.Credentials{Salt: c.Salt, Iterations: c.Iterations, StoredKey: c.StoredKey, ServerKey: c.ServerKey}
}

type userJSON struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

func (s *server) ip(r *http.Request) string { return clientIP(r, s.trusted) }

func noContent(w http.ResponseWriter, err error) {
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

func link(r *http.Request, tok string) map[string]string {
	return map[string]string{"link": "https://" + r.Host + "/#/invite/" + tok}
}

func (s *server) publicRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/setup/status", func(w http.ResponseWriter, r *http.Request) {
		need, err := s.au.SetupNeeded(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"needed": need, "token_configured": s.au.TokenConfigured()})
	})
	m.HandleFunc("POST /api/setup", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			SetupToken  string   `json:"setup_token"`
			Username    string   `json:"username"`
			Credentials credJSON `json:"credentials"`
		}
		if !decode(w, r, &in) {
			return
		}
		u, err := s.au.Setup(r.Context(), in.SetupToken, in.Username, in.Credentials.creds())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 201, userJSON{u.ID, u.Username, u.Role})
	})
	m.HandleFunc("POST /api/auth/scram/start", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ClientFirst string `json:"client_first"`
		}
		if !decode(w, r, &in) {
			return
		}
		sf, id, _, err := s.au.LoginStart(r.Context(), in.ClientFirst, s.ip(r))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"server_first": sf, "scram_id": id})
	})
	m.HandleFunc("POST /api/auth/scram/finish", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ScramID     string `json:"scram_id"`
			ClientFinal string `json:"client_final"`
			WantToken   bool   `json:"want_token"`
		}
		if !decode(w, r, &in) {
			return
		}
		ua := r.UserAgent()
		if len(ua) > 256 {
			ua = ua[:256]
		}
		sf, sid, u, _, err := s.au.LoginFinish(r.Context(), in.ScramID, in.ClientFinal, s.ip(r), ua)
		if err != nil {
			fail(w, err)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: sid, Path: "/", HttpOnly: true, Secure: true,
			SameSite: http.SameSiteStrictMode, MaxAge: int(auth.SessionAbsTTL / time.Second)})
		out := map[string]any{"server_final": sf, "user": userJSON{u.ID, u.Username, u.Role}}
		if in.WantToken {
			out["session_id"] = sid
		}
		writeJSON(w, 200, out)
	})
	m.HandleFunc("POST /api/invites/lookup", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Token string `json:"token"`
		}
		if !decode(w, r, &in) {
			return
		}
		name, purpose, err := s.au.InviteInfo(r.Context(), in.Token)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"username": name, "purpose": purpose})
	})
	m.HandleFunc("POST /api/invites/accept", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Token       string   `json:"token"`
			Credentials credJSON `json:"credentials"`
		}
		if !decode(w, r, &in) {
			return
		}
		u, err := s.au.AcceptInvite(r.Context(), in.Token, in.Credentials.creds())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, userJSON{u.ID, u.Username, u.Role})
	})
}

type sessionJSON struct {
	ID         string    `json:"id"`
	UserID     int64     `json:"user_id"`
	Username   string    `json:"username"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	IP         string    `json:"ip"`
	UserAgent  string    `json:"user_agent"`
	Current    bool      `json:"current"`
}

func (s *server) sessions(all bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, _ := principal(r)
		list, err := s.au.ListSessions(r.Context(), p, all)
		if err != nil {
			fail(w, err)
			return
		}
		names := map[int64]string{p.UserID: p.Username}
		if all {
			us, err := s.au.ListUsers(r.Context(), p)
			if err != nil {
				fail(w, err)
				return
			}
			for _, u := range us {
				names[u.ID] = u.Username
			}
		}
		writeJSON(w, 200, mapList(list, func(x store.Session) sessionJSON {
			return sessionJSON{hex.EncodeToString(x.IDHash), x.UserID, names[x.UserID], x.CreatedAt, x.LastSeenAt,
				x.IP, x.UserAgent, string(x.IDHash) == string(p.SessionHash)}
		}))
	}
}

func (s *server) loginRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		p, _ := principal(r)
		http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", HttpOnly: true, Secure: true,
			SameSite: http.SameSiteStrictMode, MaxAge: -1})
		noContent(w, s.au.Logout(r.Context(), p))
	})
	m.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		p, _ := principal(r)
		writeJSON(w, 200, userJSON{p.UserID, p.Username, p.Role})
	})
	m.HandleFunc("POST /api/me/reauth/start", func(w http.ResponseWriter, r *http.Request) {
		p, _ := principal(r)
		var in struct {
			ClientFirst string `json:"client_first"`
		}
		if !decode(w, r, &in) {
			return
		}
		sf, id, err := s.au.ReauthStart(r.Context(), p, in.ClientFirst, s.ip(r))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"server_first": sf, "scram_id": id})
	})
	m.HandleFunc("POST /api/me/reauth/finish", func(w http.ResponseWriter, r *http.Request) {
		p, _ := principal(r)
		var in struct {
			ScramID     string `json:"scram_id"`
			ClientFinal string `json:"client_final"`
		}
		if !decode(w, r, &in) {
			return
		}
		sf, tok, err := s.au.ReauthFinish(r.Context(), p, in.ScramID, in.ClientFinal, s.ip(r))
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]string{"server_final": sf, "reauth_token": tok})
	})
	m.HandleFunc("POST /api/me/password", func(w http.ResponseWriter, r *http.Request) {
		p, _ := principal(r)
		var in struct {
			ReauthToken string   `json:"reauth_token"`
			Credentials credJSON `json:"credentials"`
		}
		if !decode(w, r, &in) {
			return
		}
		noContent(w, s.au.ChangePassword(r.Context(), p, in.ReauthToken, in.Credentials.creds()))
	})
	m.HandleFunc("GET /api/me/sessions", s.sessions(false))
	m.HandleFunc("GET /api/sessions", s.sessions(true))
	m.HandleFunc("DELETE /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, _ := principal(r)
		h, err := hex.DecodeString(r.PathValue("id"))
		if err != nil {
			fail(w, store.ErrNotFound)
			return
		}
		noContent(w, s.au.RevokeSession(r.Context(), p, h))
	})
	m.HandleFunc("GET /api/users", func(w http.ResponseWriter, r *http.Request) {
		p, _ := principal(r)
		us, err := s.au.ListUsers(r.Context(), p)
		if err != nil {
			fail(w, err)
			return
		}
		type user struct {
			userJSON
			Disabled  bool      `json:"disabled"`
			CreatedAt time.Time `json:"created_at"`
		}
		writeJSON(w, 200, mapList(us, func(u store.User) user {
			return user{userJSON{u.ID, u.Username, u.Role}, u.Disabled, u.CreatedAt}
		}))
	})
	m.HandleFunc("POST /api/invites", func(w http.ResponseWriter, r *http.Request) {
		p, _ := principal(r)
		var in struct {
			Username string `json:"username"`
			Role     string `json:"role"`
		}
		if !decode(w, r, &in) {
			return
		}
		tok, err := s.au.CreateInvite(r.Context(), p, in.Username, in.Role)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 201, link(r, tok))
	})
	m.HandleFunc("POST /api/users/{id}/reset", func(w http.ResponseWriter, r *http.Request) {
		p, _ := principal(r)
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		if !p.IsAdmin() { // CreateResetInvite itself has no principal check
			fail(w, auth.ErrForbidden)
			return
		}
		tok, err := s.au.CreateResetInvite(r.Context(), &p.UserID, id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 201, link(r, tok))
	})
	m.HandleFunc("PATCH /api/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		p, _ := principal(r)
		id, ok := pathID(w, r)
		if !ok {
			return
		}
		var in struct {
			Role     *string `json:"role"`
			Disabled *bool   `json:"disabled"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.Role == nil && in.Disabled == nil {
			fail(w, auth.ErrInvalid)
			return
		}
		var err error
		if in.Role != nil {
			err = s.au.SetRole(r.Context(), p, id, *in.Role)
		}
		if err == nil && in.Disabled != nil {
			err = s.au.SetDisabled(r.Context(), p, id, *in.Disabled)
		}
		noContent(w, err)
	})
}
