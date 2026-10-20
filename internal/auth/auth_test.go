package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"fridge/internal/scram"
	"fridge/internal/store"
	"fridge/internal/store/storetest"
)

const (
	ip1       = "10.0.0.1"
	ip2       = "10.0.0.2"
	setupTok  = "setup-token-123"
	goodPass  = "correct horse battery"
	otherPass = "another long password"
)

var (
	credMu    sync.Mutex
	credCache = map[string]cred{}
)

type cred struct {
	Credentials
	salted []byte
}

// creds derives (once per password) real 600000-iteration credentials.
func creds(pw string) cred {
	credMu.Lock()
	defer credMu.Unlock()
	if c, ok := credCache[pw]; ok {
		return c
	}
	salt := []byte("0123456789abcdef")
	salted := scram.SaltedPassword(pw, salt, MinIterations)
	st, sv := scram.Keys(salted)
	c := cred{Credentials{salt, MinIterations, st, sv}, salted}
	credCache[pw] = c
	return c
}

type env struct {
	t   *testing.T
	s   *Service
	now time.Time
	ctx context.Context
}

func newEnv(t *testing.T) *env {
	e := &env{t: t, now: time.Now(), ctx: context.Background()}
	e.s = mustNew(t, storetest.New(t), setupTok)
	e.s.Now = func() time.Time { return e.now }
	return e
}

func (e *env) admin() store.User {
	e.t.Helper()
	u, err := e.s.Setup(e.ctx, setupTok, "alice", creds(goodPass).Credentials)
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

type result struct {
	c         *scram.Client
	sf        string
	sessionID string
	user      store.User
	att       Attempt
	err       error
}

// login runs a full SCRAM exchange using `salted` as the client's salted password.
func (e *env) login(user string, salted []byte, ip string) result {
	e.t.Helper()
	nonce := make([]byte, 12)
	rand.Read(nonce)
	c := scram.NewClient(user, "", base64.RawURLEncoding.EncodeToString(nonce))
	first, id, att, err := e.s.LoginStart(e.ctx, c.First(), ip)
	if err != nil {
		return result{att: att, err: err}
	}
	final, err := c.FinalWithSalted(first, salted)
	if err != nil {
		e.t.Fatal(err)
	}
	sf, sess, u, att, err := e.s.LoginFinish(e.ctx, id, final, ip, "ua")
	return result{c, sf, sess, u, att, err}
}

func (e *env) ok(user, pw string) result {
	e.t.Helper()
	r := e.login(user, creds(pw).salted, ip1)
	if r.err != nil {
		e.t.Fatalf("login %s: %v", user, r.err)
	}
	return r
}

func (e *env) principal(sessionID string) Principal {
	e.t.Helper()
	p, err := e.s.Authenticate(e.ctx, sessionID)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

var wrong = bytes.Repeat([]byte{7}, 32)

func is(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("err = %v, want %v", got, want)
	}
}

func TestSetup(t *testing.T) {
	e := newEnv(t)
	c := creds(goodPass).Credentials
	if need, _ := e.s.SetupNeeded(e.ctx); !need {
		t.Fatal("setup should be needed")
	}
	noTok := mustNew(t, e.s.Store, "")
	if noTok.TokenConfigured() || !e.s.TokenConfigured() {
		t.Fatal("TokenConfigured wrong")
	}
	_, err := noTok.Setup(e.ctx, "", "alice", c)
	is(t, err, ErrBadSetupToken)
	_, err = e.s.Setup(e.ctx, "nope", "alice", c)
	is(t, err, ErrBadSetupToken)
	u := e.admin()
	if u.Role != "admin" || u.Username != "alice" {
		t.Fatalf("user = %+v", u)
	}
	_, err = e.s.Setup(e.ctx, setupTok, "bob", c)
	is(t, err, ErrSetupDone)
	if need, _ := e.s.SetupNeeded(e.ctx); need {
		t.Fatal("setup still needed")
	}
}

func TestUsernameRules(t *testing.T) {
	e := newEnv(t)
	c := creds(goodPass).Credentials
	for _, n := range []string{"alice_x", "Al", "ab", "a-b"} {
		_, err := e.s.Setup(e.ctx, setupTok, n, c)
		is(t, err, ErrInvalid)
	}
	bad := c
	bad.Iterations = 1000
	_, err := e.s.Setup(e.ctx, setupTok, "alice", bad)
	is(t, err, ErrInvalid)
	u := e.admin()
	p := Principal{UserID: u.ID, Username: "alice", Role: "admin"}
	_, err = e.s.CreateInvite(e.ctx, p, "bob_1", "user")
	is(t, err, ErrInvalid)
	if _, err := e.s.CreateInvite(e.ctx, p, "bob", "user"); err != nil {
		t.Fatal(err)
	}
	_, err = e.s.CreateInvite(e.ctx, p, "alice", "user")
	is(t, err, store.ErrConflict)
}

func TestLoginSuccessAndSession(t *testing.T) {
	e := newEnv(t)
	e.admin()
	r := e.ok("alice", goodPass)
	if err := r.c.Verify(r.sf); err != nil {
		t.Fatal(err)
	}
	p := e.principal(r.sessionID)
	if p.Username != "alice" || !p.IsAdmin() || p.UserID != r.user.ID {
		t.Fatalf("principal = %+v", p)
	}
	_, err := e.s.Authenticate(e.ctx, "garbage")
	is(t, err, ErrUnauthenticated)
}

func TestSessionStoredHashed(t *testing.T) {
	e := newEnv(t)
	e.admin()
	r := e.ok("alice", goodPass)
	h := sha256.Sum256([]byte(r.sessionID))
	raw, _ := base64.RawURLEncoding.DecodeString(r.sessionID)
	if len(raw) != 32 {
		t.Fatalf("session id len %d", len(raw))
	}
	list, err := e.s.Store.ListSessions(e.ctx, nil)
	if err != nil || len(list) != 1 {
		t.Fatalf("%v %d", err, len(list))
	}
	x := list[0]
	if !bytes.Equal(x.IDHash, h[:]) || bytes.Equal(x.IDHash, raw) || strings.Contains(x.IP+x.UserAgent, r.sessionID) {
		t.Fatal("session not stored hashed")
	}
}

func TestLoginWrongPasswordGeneric(t *testing.T) {
	e := newEnv(t)
	e.admin()
	is(t, e.login("alice", wrong, ip1).err, ErrBadCredentials)
	is(t, e.login("nobody", wrong, ip1).err, ErrBadCredentials)

	sf := func() string {
		s, _, _, err := e.s.LoginStart(e.ctx, "n,,n=nobody,r=abc", ip2)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := sf(), sf()
	for _, s := range []string{a, b} {
		if !strings.Contains(s, ",s=") || !strings.HasSuffix(s, ",i=600000") {
			t.Fatalf("serverFirst = %q", s)
		}
	}
	salt := func(s string) string { return strings.Split(strings.Split(s, ",s=")[1], ",")[0] }
	if salt(a) != salt(b) {
		t.Fatal("fake salt not stable")
	}
	if salt(a) == base64.StdEncoding.EncodeToString(creds(goodPass).Salt) {
		t.Fatal("fake salt equals real")
	}
}

func TestLoginAttemptInfo(t *testing.T) {
	e := newEnv(t)
	u := e.admin()
	r := e.login("alice", wrong, ip1)
	if r.att.Username != "alice" || r.att.UserID == nil || *r.att.UserID != u.ID {
		t.Fatalf("att = %+v", r.att)
	}
	r = e.login("nobody", wrong, ip1)
	if r.att.Username != "nobody" || r.att.UserID != nil {
		t.Fatalf("att = %+v", r.att)
	}
	for range 4 {
		e.login("alice", wrong, ip1)
	}
	r = e.login("alice", wrong, ip1)
	is(t, r.err, ErrLocked)
	if r.att.Username != "alice" {
		t.Fatalf("att = %+v", r.att)
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	e.admin()
	for range 5 {
		is(t, e.login("alice", wrong, ip1).err, ErrBadCredentials)
	}
	is(t, e.login("alice", wrong, ip1).err, ErrLocked)
	// even the right password is refused while locked
	is(t, e.login("alice", creds(goodPass).salted, ip1).err, ErrLocked)
	if r := e.login("alice", creds(goodPass).salted, ip2); r.err != nil {
		t.Fatalf("other ip: %v", r.err)
	}
	e.now = e.now.Add(LoginLockout)
	if r := e.login("alice", creds(goodPass).salted, ip1); r.err != nil {
		t.Fatalf("after lockout: %v", r.err)
	}
}

func TestSlowFailuresDontLock(t *testing.T) {
	e := newEnv(t)
	e.admin()
	for range 8 {
		is(t, e.login("alice", wrong, ip1).err, ErrBadCredentials)
		e.now = e.now.Add(20 * time.Second)
	}
}

func TestLockBypassWithPrestartedState(t *testing.T) {
	e := newEnv(t)
	e.admin()
	c := scram.NewClient("alice", "", "nonce1")
	first, id, _, err := e.s.LoginStart(e.ctx, c.First(), ip1)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		e.login("alice", wrong, ip1)
	}
	final, _ := c.FinalWithSalted(first, creds(goodPass).salted)
	_, _, _, _, err = e.s.LoginFinish(e.ctx, id, final, ip1, "ua")
	is(t, err, ErrLocked)
}

func TestReauthSharesLimiter(t *testing.T) {
	e := newEnv(t)
	u := e.admin()
	p := Principal{UserID: u.ID, Username: "alice", Role: "admin"}
	for range 5 {
		e.login("alice", wrong, ip1)
	}
	_, _, err := e.s.ReauthStart(e.ctx, p, "n,,n=alice,r=abc", ip1)
	is(t, err, ErrLocked)

	e.now = e.now.Add(LoginLockout)
	for range 5 {
		c := scram.NewClient("alice", "", "abc")
		sf, id, err := e.s.ReauthStart(e.ctx, p, c.First(), ip1)
		if err != nil {
			t.Fatal(err)
		}
		final, _ := c.FinalWithSalted(sf, wrong)
		_, _, err = e.s.ReauthFinish(e.ctx, p, id, final, ip1)
		is(t, err, ErrBadCredentials)
	}
	_, _, _, err = e.s.LoginStart(e.ctx, "n,,n=alice,r=abc", ip1)
	is(t, err, ErrLocked)
	// reauth for someone else's username is refused
	_, _, err = e.s.ReauthStart(e.ctx, p, "n,,n=bob,r=abc", ip2)
	is(t, err, ErrForbidden)
}

func TestScramStateExpires(t *testing.T) {
	e := newEnv(t)
	e.admin()
	c := scram.NewClient("alice", "", "abc")
	first, id, _, err := e.s.LoginStart(e.ctx, c.First(), ip1)
	if err != nil {
		t.Fatal(err)
	}
	final, _ := c.FinalWithSalted(first, creds(goodPass).salted)
	e.now = e.now.Add(61 * time.Second)
	_, _, _, _, err = e.s.LoginFinish(e.ctx, id, final, ip1, "ua")
	is(t, err, ErrBadCredentials)
}

func TestScramStateSingleUse(t *testing.T) {
	e := newEnv(t)
	e.admin()
	c := scram.NewClient("alice", "", "abc")
	first, id, _, _ := e.s.LoginStart(e.ctx, c.First(), ip1)
	final, _ := c.FinalWithSalted(first, creds(goodPass).salted)
	if _, _, _, _, err := e.s.LoginFinish(e.ctx, id, final, ip1, "ua"); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, err := e.s.LoginFinish(e.ctx, id, final, ip1, "ua")
	is(t, err, ErrBadCredentials)
}

func TestSessionExpiry(t *testing.T) {
	e := newEnv(t)
	e.admin()
	r := e.ok("alice", goodPass)
	e.now = e.now.Add(12*time.Hour + time.Minute)
	_, err := e.s.Authenticate(e.ctx, r.sessionID)
	is(t, err, ErrUnauthenticated)
	if l, _ := e.s.Store.ListSessions(e.ctx, nil); len(l) != 0 {
		t.Fatal("expired session row kept")
	}

	r = e.ok("alice", goodPass)
	for range 7*24 - 1 {
		e.now = e.now.Add(time.Hour)
		e.principal(r.sessionID)
	}
	e.now = e.now.Add(2 * time.Hour)
	_, err = e.s.Authenticate(e.ctx, r.sessionID)
	is(t, err, ErrUnauthenticated)
}

func TestTouchThrottled(t *testing.T) {
	e := newEnv(t)
	e.admin()
	r := e.ok("alice", goodPass)
	before, _ := e.s.Store.GetSession(e.ctx, SessionHash(r.sessionID))
	e.now = e.now.Add(30 * time.Second)
	e.principal(r.sessionID)
	mid, _ := e.s.Store.GetSession(e.ctx, SessionHash(r.sessionID))
	if !mid.LastSeenAt.Equal(before.LastSeenAt) {
		t.Fatal("touched too early")
	}
	e.now = e.now.Add(2 * time.Minute)
	e.principal(r.sessionID)
	after, _ := e.s.Store.GetSession(e.ctx, SessionHash(r.sessionID))
	if !after.LastSeenAt.After(before.LastSeenAt) {
		t.Fatal("not touched")
	}
}

func (e *env) reauth(p Principal, pw string) string {
	e.t.Helper()
	c := scram.NewClient(p.Username, "", "rn")
	sf, id, err := e.s.ReauthStart(e.ctx, p, c.First(), ip1)
	if err != nil {
		e.t.Fatal(err)
	}
	final, _ := c.FinalWithSalted(sf, creds(pw).salted)
	_, tok, err := e.s.ReauthFinish(e.ctx, p, id, final, ip1)
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func TestLogoutAndChangePassword(t *testing.T) {
	e := newEnv(t)
	e.admin()
	r1, r2 := e.ok("alice", goodPass), e.ok("alice", goodPass)
	p := e.principal(r1.sessionID)
	nc := creds(otherPass).Credentials

	is(t, e.s.ChangePassword(e.ctx, p, "", nc), ErrForbidden)
	is(t, e.s.ChangePassword(e.ctx, p, "bogus", nc), ErrForbidden)

	tok := e.reauth(p, goodPass)
	e.now = e.now.Add(ReauthTTL + time.Second)
	is(t, e.s.ChangePassword(e.ctx, p, tok, nc), ErrForbidden)

	tok = e.reauth(p, goodPass)
	if err := e.s.ChangePassword(e.ctx, p, tok, nc); err != nil {
		t.Fatal(err)
	}
	is(t, e.s.ChangePassword(e.ctx, p, tok, nc), ErrForbidden) // single use
	e.principal(r1.sessionID)
	_, err := e.s.Authenticate(e.ctx, r2.sessionID)
	is(t, err, ErrUnauthenticated)
	is(t, e.login("alice", creds(goodPass).salted, ip2).err, ErrBadCredentials)
	if r := e.login("alice", creds(otherPass).salted, ip2); r.err != nil {
		t.Fatal(r.err)
	}

	// a reauth token is bound to its user
	tok = e.reauth(p, otherPass)
	is(t, e.s.ChangePassword(e.ctx, Principal{UserID: p.UserID + 1, Username: "x", SessionHash: p.SessionHash}, tok, nc), ErrForbidden)

	if err := e.s.Logout(e.ctx, p); err != nil {
		t.Fatal(err)
	}
	_, err = e.s.Authenticate(e.ctx, r1.sessionID)
	is(t, err, ErrUnauthenticated)
}

func (e *env) invite(admin Principal, name, role string) {
	e.t.Helper()
	tok, err := e.s.CreateInvite(e.ctx, admin, name, role)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.s.AcceptInvite(e.ctx, tok, creds(goodPass).Credentials); err != nil {
		e.t.Fatal(err)
	}
}

func TestInviteFlow(t *testing.T) {
	e := newEnv(t)
	a := e.admin()
	ap := Principal{UserID: a.ID, Username: "alice", Role: "admin"}
	_, err := e.s.CreateInvite(e.ctx, Principal{UserID: 99, Role: "user"}, "bob", "user")
	is(t, err, ErrForbidden)

	tok, err := e.s.CreateInvite(e.ctx, ap, "bob", "user")
	if err != nil {
		t.Fatal(err)
	}
	name, purpose, err := e.s.InviteInfo(e.ctx, tok)
	if err != nil || name != "bob" || purpose != "new_user" {
		t.Fatalf("%q %q %v", name, purpose, err)
	}
	_, err = e.s.AcceptInvite(e.ctx, tok, Credentials{})
	is(t, err, ErrInvalid)
	u, err := e.s.AcceptInvite(e.ctx, tok, creds(otherPass).Credentials)
	if err != nil || u.Role != "user" || u.CreatedBy == nil || *u.CreatedBy != a.ID {
		t.Fatalf("%+v %v", u, err)
	}
	if r := e.login("bob", creds(otherPass).salted, ip1); r.err != nil {
		t.Fatal(r.err)
	}
	_, err = e.s.AcceptInvite(e.ctx, tok, creds(otherPass).Credentials)
	is(t, err, ErrInviteInvalid)
	_, _, err = e.s.InviteInfo(e.ctx, "nonsense")
	is(t, err, ErrInviteInvalid)

	tok, _ = e.s.CreateInvite(e.ctx, ap, "carol", "user")
	e.now = e.now.Add(49 * time.Hour)
	_, _, err = e.s.InviteInfo(e.ctx, tok)
	is(t, err, ErrInviteInvalid)
	_, err = e.s.AcceptInvite(e.ctx, tok, creds(goodPass).Credentials)
	is(t, err, ErrInviteInvalid)
}

func TestResetInvite(t *testing.T) {
	e := newEnv(t)
	e.admin()
	r := e.ok("alice", goodPass)
	tok, err := e.s.CreateResetInvite(e.ctx, nil, r.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n, p, err := e.s.InviteInfo(e.ctx, tok); err != nil || n != "alice" || p != "reset_password" {
		t.Fatalf("%q %q %v", n, p, err)
	}
	if _, err := e.s.AcceptInvite(e.ctx, tok, creds(otherPass).Credentials); err != nil {
		t.Fatal(err)
	}
	_, err = e.s.Authenticate(e.ctx, r.sessionID)
	is(t, err, ErrUnauthenticated)
	is(t, e.login("alice", creds(goodPass).salted, ip2).err, ErrBadCredentials)
	if r := e.login("alice", creds(otherPass).salted, ip2); r.err != nil {
		t.Fatal(r.err)
	}
}

func TestLastAdminGuard(t *testing.T) {
	e := newEnv(t)
	a := e.admin()
	ap := Principal{UserID: a.ID, Username: "alice", Role: "admin"}
	is(t, e.s.SetRole(e.ctx, ap, a.ID, "user"), ErrLastAdmin)
	is(t, e.s.SetDisabled(e.ctx, ap, a.ID, true), ErrLastAdmin)
	is(t, e.s.SetRole(e.ctx, ap, a.ID, "root"), ErrInvalid)

	e.invite(ap, "bob", "user")
	bob, _ := e.s.Store.GetUserByName(e.ctx, "bob")
	bp := Principal{UserID: bob.ID, Username: "bob", Role: "user"}
	is(t, e.s.SetRole(e.ctx, bp, bob.ID, "admin"), ErrForbidden)
	is(t, e.s.SetDisabled(e.ctx, bp, a.ID, true), ErrForbidden)
	if _, err := e.s.ListUsers(e.ctx, bp); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	if us, err := e.s.ListUsers(e.ctx, ap); err != nil || len(us) != 2 {
		t.Fatalf("%v %d", err, len(us))
	}

	if err := e.s.Promote(e.ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	if u, _ := e.s.Store.GetUser(e.ctx, bob.ID); u.Role != "admin" {
		t.Fatal("not promoted")
	}
	r := e.ok("alice", goodPass)
	if err := e.s.SetDisabled(e.ctx, ap, a.ID, true); err != nil {
		t.Fatal(err)
	}
	_, err := e.s.Authenticate(e.ctx, r.sessionID)
	is(t, err, ErrUnauthenticated)
	is(t, e.login("alice", creds(goodPass).salted, ip2).err, ErrBadCredentials) // disabled looks like bad creds
}

func TestRevokeSession(t *testing.T) {
	e := newEnv(t)
	a := e.admin()
	ap := Principal{UserID: a.ID, Username: "alice", Role: "admin"}
	e.invite(ap, "bob", "user")
	ra, rb := e.ok("alice", goodPass), e.ok("bob", goodPass)
	bp := e.principal(rb.sessionID)

	is(t, e.s.RevokeSession(e.ctx, bp, SessionHash(ra.sessionID)), store.ErrNotFound)
	if l, err := e.s.ListSessions(e.ctx, bp, false); err != nil || len(l) != 1 {
		t.Fatalf("%v %d", err, len(l))
	}
	if _, err := e.s.ListSessions(e.ctx, bp, true); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	if l, _ := e.s.ListSessions(e.ctx, ap, true); len(l) != 2 {
		t.Fatalf("admin sees %d", len(l))
	}
	if err := e.s.RevokeSession(e.ctx, ap, SessionHash(rb.sessionID)); err != nil {
		t.Fatal(err)
	}
	_, err := e.s.Authenticate(e.ctx, rb.sessionID)
	is(t, err, ErrUnauthenticated)
	if n, err := e.s.PurgeExpired(e.ctx); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
}

func mustNew(t *testing.T, st *store.Store, tok string) *Service {
	t.Helper()
	s, err := New(st, bytes.Repeat([]byte{9}, MinSecretLen), tok)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewRejectsShortSecret(t *testing.T) {
	if _, err := New(nil, make([]byte, MinSecretLen-1), ""); err == nil {
		t.Fatal("short secret accepted")
	}
}

func TestConcurrentFinishBudget(t *testing.T) {
	e := newEnv(t)
	e.admin()
	type st struct{ id, final string }
	var sts []st
	for i := range 10 {
		c := scram.NewClient("alice", "", "n"+string(rune('a'+i)))
		first, id, _, err := e.s.LoginStart(e.ctx, c.First(), ip1)
		if err != nil {
			t.Fatal(err)
		}
		final, _ := c.FinalWithSalted(first, wrong)
		sts = append(sts, st{id, final})
	}
	errs := make(chan error, len(sts))
	for _, x := range sts {
		go func() {
			_, _, _, _, err := e.s.LoginFinish(e.ctx, x.id, x.final, ip1, "ua")
			errs <- err
		}()
	}
	bad, locked := 0, 0
	for range sts {
		switch err := <-errs; {
		case errors.Is(err, ErrBadCredentials):
			bad++
		case errors.Is(err, ErrLocked):
			locked++
		default:
			t.Fatal(err)
		}
	}
	if bad != LoginMaxFails || locked != 10-LoginMaxFails {
		t.Fatalf("bad=%d locked=%d", bad, locked)
	}
	_, _, _, err := e.s.LoginStart(e.ctx, "n,,n=alice,r=abc", ip1)
	is(t, err, ErrLocked)
}

func TestPendingCaps(t *testing.T) {
	e := newEnv(t)
	for range maxPendingPerIP {
		if _, _, _, err := e.s.LoginStart(e.ctx, "n,,n=nobody,r=abc", ip1); err != nil {
			t.Fatal(err)
		}
	}
	_, _, _, err := e.s.LoginStart(e.ctx, "n,,n=nobody,r=abc", ip1)
	is(t, err, ErrBusy)
	if _, _, _, err := e.s.LoginStart(e.ctx, "n,,n=nobody,r=abc", ip2); err != nil {
		t.Fatalf("other ip: %v", err)
	}
	// expired states free the per-IP budget
	e.now = e.now.Add(ScramStateTTL + time.Second)
	if _, _, _, err := e.s.LoginStart(e.ctx, "n,,n=nobody,r=abc", ip1); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalCapEvictsOldest(t *testing.T) {
	e := newEnv(t)
	e.admin()
	c := scram.NewClient("alice", "", "abc")
	first, oldest, _, _ := e.s.LoginStart(e.ctx, c.First(), "9.9.9.9")
	// fill with other-IP states directly (cheaper than 10000 starts)
	e.s.mu.Lock()
	for i := 0; len(e.s.scram) < maxPending; i++ {
		e.s.scram[string(rune(i+1000))+"x"] = &pending{ip: "bulk", expires: e.now.Add(time.Hour)}
	}
	e.s.mu.Unlock()
	if _, _, _, err := e.s.LoginStart(e.ctx, "n,,n=nobody,r=abc", ip2); err != nil {
		t.Fatalf("full map must evict, got %v", err)
	}
	final, _ := c.FinalWithSalted(first, creds(goodPass).salted)
	_, _, _, _, err := e.s.LoginFinish(e.ctx, oldest, final, "9.9.9.9", "ua")
	is(t, err, ErrBadCredentials) // oldest was evicted
}

func TestReauthTokenBoundToSession(t *testing.T) {
	e := newEnv(t)
	e.admin()
	r1, r2 := e.ok("alice", goodPass), e.ok("alice", goodPass)
	p1, p2 := e.principal(r1.sessionID), e.principal(r2.sessionID)
	tok := e.reauth(p1, goodPass)
	is(t, e.s.ChangePassword(e.ctx, p2, tok, creds(otherPass).Credentials), ErrForbidden)
}

func TestAttemptUserIDForDisabled(t *testing.T) {
	e := newEnv(t)
	a := e.admin()
	ap := Principal{UserID: a.ID, Username: "alice", Role: "admin"}
	e.invite(ap, "bob", "user")
	bob, _ := e.s.Store.GetUserByName(e.ctx, "bob")
	if err := e.s.SetDisabled(e.ctx, ap, bob.ID, true); err != nil {
		t.Fatal(err)
	}
	r := e.login("bob", creds(goodPass).salted, ip1)
	is(t, r.err, ErrBadCredentials)
	if r.att.UserID == nil || *r.att.UserID != bob.ID {
		t.Fatalf("att = %+v", r.att)
	}
	for range 4 {
		e.login("bob", wrong, ip1)
	}
	r = e.login("bob", wrong, ip1)
	is(t, r.err, ErrLocked)
	if r.att.UserID == nil {
		t.Fatalf("locked att = %+v", r.att)
	}
}
