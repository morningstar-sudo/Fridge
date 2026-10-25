package api

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"fridge/internal/auth"
	"fridge/internal/scram"
)

const (
	pwAlice = "alice password 123"
	pwBob   = "bob password 1234"
)

var (
	credMu    sync.Mutex
	credCache = map[string]credSalted{}
)

type credSalted struct {
	c      credJSON
	salted []byte
}

func credsFor(pw string) credSalted {
	credMu.Lock()
	defer credMu.Unlock()
	if c, ok := credCache[pw]; ok {
		return c
	}
	salt := []byte("0123456789abcdef")
	salted := scram.SaltedPassword(pw, salt, auth.MinIterations)
	st, sv := scram.Keys(salted)
	c := credSalted{credJSON{salt, auth.MinIterations, st, sv}, salted}
	credCache[pw] = c
	return c
}

func jb(v any) string { b, _ := json.Marshal(v); return string(b) }

// req sends a request; hdr is a flat key,value list.
func req(h http.Handler, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			r.Host = hdr[i+1]
		}
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func bearer(sid string) []string { return []string{"Authorization", "Bearer " + sid} }

func setupAlice(t *testing.T, h http.Handler) {
	t.Helper()
	w := req(h, "POST", "/api/setup", jb(map[string]any{"setup_token": "setup-tok", "username": "alice", "credentials": credsFor(pwAlice).c}))
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
}

// login runs the SCRAM exchange and returns the finish response.
func login(t *testing.T, h http.Handler, user, pw string, want bool) *httptest.ResponseRecorder {
	t.Helper()
	cs := credsFor(pw)
	n := make([]byte, 12)
	rand.Read(n)
	c := scram.NewClient(user, "", base64.RawURLEncoding.EncodeToString(n))
	w := req(h, "POST", "/api/auth/scram/start", jb(map[string]string{"client_first": c.First()}))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var st struct{ ServerFirst, ScramID string }
	var raw map[string]string
	json.Unmarshal(w.Body.Bytes(), &raw)
	st.ServerFirst, st.ScramID = raw["server_first"], raw["scram_id"]
	fin, err := c.FinalWithSalted(st.ServerFirst, cs.salted)
	if err != nil {
		t.Fatal(err)
	}
	return req(h, "POST", "/api/auth/scram/finish", jb(map[string]any{"scram_id": st.ScramID, "client_final": fin, "want_token": want}))
}

func loginAs(t *testing.T, h http.Handler, user, pw string) (*http.Cookie, string) {
	t.Helper()
	w := login(t, h, user, pw, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var out struct {
		SessionID string `json:"session_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	for _, c := range w.Result().Cookies() {
		if c.Name == "fridge_session" {
			return c, out.SessionID
		}
	}
	t.Fatal("no cookie")
	return nil, ""
}

// inviteUser has the admin invite and the user accept; returns the invite token.
func inviteUser(t *testing.T, h http.Handler, adminSID, name, role, pw string) string {
	t.Helper()
	w := req(h, "POST", "/api/invites", jb(map[string]string{"username": name, "role": role}), bearer(adminSID)...)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	var l struct{ Link string }
	var raw map[string]string
	json.Unmarshal(w.Body.Bytes(), &raw)
	l.Link = raw["link"]
	_, tokn, ok := strings.Cut(l.Link, "/#/invite/")
	if !ok {
		t.Fatal(l.Link)
	}
	w = req(h, "POST", "/api/invites/accept", jb(map[string]any{"token": tokn, "credentials": credsFor(pw).c}))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	return tokn
}

func TestSetupRequired(t *testing.T) {
	h, _, _ := newRawSrv(t, "setup-tok", Options{})
	w := req(h, "GET", "/api/databases", "")
	if w.Code != 401 || strings.TrimSpace(w.Body.String()) != `{"error":"setup required"}` {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := req(h, "GET", "/api/setup/status", ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"needed":true,"token_configured":true}` {
		t.Fatal(w.Code, w.Body.String())
	}
	h2, _, _ := newRawSrv(t, "", Options{})
	if w := req(h2, "GET", "/api/setup/status", ""); !strings.Contains(w.Body.String(), `"token_configured":false`) {
		t.Fatal(w.Body.String())
	}
	if w := req(h, "GET", "/api/ca", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "BEGIN CERTIFICATE") {
		t.Fatal(w.Code)
	}
	bad := jb(map[string]any{"setup_token": "wrong", "username": "alice", "credentials": credsFor(pwAlice).c})
	if w := req(h, "POST", "/api/setup", bad); w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestSetupAndLoginWeb(t *testing.T) {
	h, _, _ := newRawSrv(t, "setup-tok", Options{})
	setupAlice(t, h)
	if w := req(h, "POST", "/api/setup", jb(map[string]any{"setup_token": "setup-tok", "username": "bob", "credentials": credsFor(pwBob).c})); w.Code != 409 {
		t.Fatal(w.Code)
	}
	c, _ := loginAs(t, h, "alice", pwAlice)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie flags %+v", c)
	}
	w := req(h, "GET", "/api/me", "", "Cookie", c.Name+"="+c.Value)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"username":"alice"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := login(t, h, "alice", pwBob, false); w.Code != 401 {
		t.Fatal("wrong password:", w.Code)
	}
}

func TestWebLoginNoTokenInBody(t *testing.T) {
	h, _, _ := newRawSrv(t, "setup-tok", Options{})
	setupAlice(t, h)
	w := login(t, h, "alice", pwAlice, false)
	if w.Code != 200 || strings.Contains(w.Body.String(), "session_id") || len(w.Result().Cookies()) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	_, sid := loginAs(t, h, "alice", pwAlice)
	if sid == "" || req(h, "GET", "/api/me", "", bearer(sid)...).Code != 200 {
		t.Fatal("bearer not usable")
	}
}

func TestInviteTokenNotInPath(t *testing.T) {
	h, _, _ := newRawSrv(t, "setup-tok", Options{})
	setupAlice(t, h)
	_, sid := loginAs(t, h, "alice", pwAlice)
	w := req(h, "POST", "/api/invites", `{"username":"bob","role":"user"}`, append(bearer(sid), "Host", "fridge.lan")...)
	var raw map[string]string
	json.Unmarshal(w.Body.Bytes(), &raw)
	if !strings.HasPrefix(raw["link"], "https://fridge.lan/#/invite/") {
		t.Fatal(w.Code, w.Body.String())
	}
	_, tokn, _ := strings.Cut(raw["link"], "/#/invite/")
	w = req(h, "POST", "/api/invites/lookup", jb(map[string]string{"token": tokn}))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"username":"bob"`) || !strings.Contains(w.Body.String(), `"purpose":"new_user"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := req(h, "GET", "/api/invites/"+tokn, ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := req(h, "POST", "/api/invites/accept", jb(map[string]any{"token": tokn, "credentials": credsFor(pwBob).c})); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	loginAs(t, h, "bob", pwBob)
	if w := req(h, "POST", "/api/invites/lookup", jb(map[string]string{"token": tokn})); w.Code != 400 {
		t.Fatal("used invite:", w.Code)
	}
}

func TestCSRF(t *testing.T) {
	h, _, a := newRawSrv(t, "setup-tok", Options{})
	setupAlice(t, h)
	c, sid := loginAs(t, h, "alice", pwAlice)
	ck := c.Name + "=" + c.Value
	regDomains(t, a)
	body := `{"engine":"postgres","name":"abc","domain":"abc.com"}`
	if w := req(h, "POST", "/api/databases", body, "Cookie", ck, "Origin", "https://evil"); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := req(h, "POST", "/api/databases", body, "Cookie", ck); w.Code != 403 {
		t.Fatal("no origin:", w.Code)
	}
	if w := req(h, "POST", "/api/databases", body, "Cookie", ck, "Sec-Fetch-Site", "same-origin"); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := req(h, "POST", "/api/databases", `{"engine":"postgres","name":"abd","domain":"abd.com"}`, "Cookie", ck, "Origin", "https://example.com"); w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := req(h, "GET", "/api/databases", "", "Cookie", ck, "Origin", "https://evil"); w.Code != 200 {
		t.Fatal("GET:", w.Code)
	}
	if w := req(h, "POST", "/api/databases", `{"engine":"postgres","name":"abe","domain":"abe.com"}`, bearer(sid)...); w.Code != 201 {
		t.Fatal("bearer:", w.Code, w.Body.String())
	}
}

func TestClientIPTrustsOnlyProxy(t *testing.T) {
	pfx, err := ParseCIDRs("172.30.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	trust := TrustCIDRs(pfx)
	ip := func(peer, xff string) string {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = peer + ":4000"
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return clientIP(r, trust)
	}
	if got := ip("10.9.9.9", "1.2.3.4"); got != "10.9.9.9" {
		t.Fatal(got)
	}
	if got := ip("172.30.0.5", "1.1.1.1, 5.6.7.8"); got != "5.6.7.8" {
		t.Fatal(got)
	}
	if got := ip("127.0.0.1", "9.9.9.9"); got != "9.9.9.9" {
		t.Fatal(got)
	}
	if got := ip("172.30.0.5", "garbage"); got != "172.30.0.5" {
		t.Fatal(got)
	}
	if got := ip("172.30.0.5", ""); got != "172.30.0.5" {
		t.Fatal(got)
	}
	if _, err := ParseCIDRs("bad"); err == nil {
		t.Fatal("want error")
	}
	if p, err := ParseCIDRs(" 10.0.0.0/8, 172.20.0.0/16 ,"); err != nil || len(p) != 2 || p[0] != netip.MustParsePrefix("10.0.0.0/8") {
		t.Fatal(p, err)
	}
	if p, err := ParseCIDRs(""); err != nil || p != nil {
		t.Fatal(p, err)
	}
	if !trust(net.ParseIP("::1")) || trust(net.ParseIP("8.8.8.8")) {
		t.Fatal("loopback/other")
	}
}

func TestAdminOnlyRoutes(t *testing.T) {
	h, _, _ := newRawSrv(t, "setup-tok", Options{})
	setupAlice(t, h)
	_, asid := loginAs(t, h, "alice", pwAlice)
	inviteUser(t, h, asid, "bob", "user", pwBob)
	_, bsid := loginAs(t, h, "bob", pwBob)
	for _, c := range [][3]string{
		{"GET", "/api/users", ""}, {"GET", "/api/sessions", ""},
		{"POST", "/api/invites", `{"username":"carol","role":"user"}`},
		{"POST", "/api/users/1/reset", ""}, {"PATCH", "/api/users/1", `{"role":"user"}`},
	} {
		if w := req(h, c[0], c[1], c[2], bearer(bsid)...); w.Code != 403 {
			t.Fatal(c, w.Code, w.Body.String())
		}
	}
	w := req(h, "POST", "/api/invites", `{"username":"carol","role":"user"}`, bearer(asid)...)
	if w.Code != 201 || !strings.Contains(w.Body.String(), "/#/invite/") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := req(h, "GET", "/api/users", "", bearer(asid)...); w.Code != 200 || !strings.Contains(w.Body.String(), `"bob"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	// reset link, then PATCH disable bob kills his session
	if w := req(h, "POST", "/api/users/2/reset", "", bearer(asid)...); w.Code != 201 || !strings.Contains(w.Body.String(), "/#/invite/") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := req(h, "PATCH", "/api/users/2", `{"disabled":true}`, bearer(asid)...); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := req(h, "GET", "/api/me", "", bearer(bsid)...); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := req(h, "PATCH", "/api/users/1", `{"role":"user"}`, bearer(asid)...); w.Code != 409 {
		t.Fatal("last admin:", w.Code, w.Body.String())
	}
	if w := req(h, "PATCH", "/api/users/2", `{}`, bearer(asid)...); w.Code != 400 {
		t.Fatal(w.Code)
	}
}

func TestSessionListCurrent(t *testing.T) {
	h, _, _ := newRawSrv(t, "setup-tok", Options{})
	setupAlice(t, h)
	_, s1 := loginAs(t, h, "alice", pwAlice)
	_, s2 := loginAs(t, h, "alice", pwAlice)
	list := func(sid, path string) []map[string]any {
		w := req(h, "GET", path, "", bearer(sid)...)
		var out []map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return out
	}
	out := list(s1, "/api/me/sessions")
	if len(out) != 2 {
		t.Fatal(out)
	}
	cur := 0
	var other string
	for _, s := range out {
		if s["current"] == true {
			cur++
			h1 := auth.SessionHash(s1)
			if s["id"] != hex.EncodeToString(h1) {
				t.Fatal("current is not session 1", s)
			}
		} else {
			other = s["id"].(string)
		}
		if s["username"] != "alice" {
			t.Fatal(s)
		}
	}
	if cur != 1 {
		t.Fatal(out)
	}
	if len(list(s2, "/api/sessions")) != 2 {
		t.Fatal("admin list")
	}
	if w := req(h, "DELETE", "/api/sessions/"+other, "", bearer(s1)...); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w := req(h, "GET", "/api/me", "", bearer(s2)...); w.Code != 401 {
		t.Fatal("revoked session still valid", w.Code)
	}
	if w := req(h, "DELETE", "/api/sessions/zz", "", bearer(s1)...); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h, _, _ := newRawSrv(t, "setup-tok", Options{})
	for _, p := range []string{"/healthz", "/api/ca", "/api/databases", "/nope", "/api/setup/status"} {
		w := req(h, "GET", p, "")
		hd := w.Header()
		if hd.Get("Content-Security-Policy") != "default-src 'self'; img-src 'self' data:; frame-ancestors 'none'" || hd.Get("X-Frame-Options") != "DENY" ||
			hd.Get("Referrer-Policy") != "no-referrer" || hd.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal(p, hd)
		}
	}
	web := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("web")) })
	h2, _, _ := newRawSrv(t, "x", Options{Web: web})
	w := req(h2, "GET", "/index.html", "")
	if w.Body.String() != "web" || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal(w.Body.String())
	}
	if w := req(h, "GET", "/index.html", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestLogoutInvalidatesBearer(t *testing.T) {
	h, _, _ := newRawSrv(t, "setup-tok", Options{})
	setupAlice(t, h)
	_, sid := loginAs(t, h, "alice", pwAlice)
	w := req(h, "POST", "/api/auth/logout", "", bearer(sid)...)
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w := req(h, "GET", "/api/me", "", bearer(sid)...); w.Code != 401 {
		t.Fatal(w.Code)
	}
}

func TestReauthAndPasswordChange(t *testing.T) {
	h, _, _ := newRawSrv(t, "setup-tok", Options{})
	setupAlice(t, h)
	_, sid := loginAs(t, h, "alice", pwAlice)
	cs := credsFor(pwAlice)
	c := scram.NewClient("alice", "", "reauthnonce0123456789ab")
	w := req(h, "POST", "/api/me/reauth/start", jb(map[string]string{"client_first": c.First()}), bearer(sid)...)
	var st map[string]string
	json.Unmarshal(w.Body.Bytes(), &st)
	fin, err := c.FinalWithSalted(st["server_first"], cs.salted)
	if w.Code != 200 || err != nil {
		t.Fatal(w.Code, w.Body.String(), err)
	}
	w = req(h, "POST", "/api/me/reauth/finish", jb(map[string]string{"scram_id": st["scram_id"], "client_final": fin}), bearer(sid)...)
	var rf map[string]string
	json.Unmarshal(w.Body.Bytes(), &rf)
	if w.Code != 200 || rf["reauth_token"] == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	w = req(h, "POST", "/api/me/password", jb(map[string]any{"reauth_token": rf["reauth_token"], "credentials": credsFor(pwBob).c}), bearer(sid)...)
	if w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	loginAs(t, h, "alice", pwBob)
}
