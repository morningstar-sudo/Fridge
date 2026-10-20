package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"fridge/internal/scram"
	"fridge/internal/store"
)

func limiterKey(username, ip string) string { return username + "|" + ip }

func (s *Service) SetupNeeded(ctx context.Context) (bool, error) {
	n, err := s.Store.CountUsers(ctx)
	return n == 0, err
}

func (s *Service) TokenConfigured() bool { return s.SetupToken != "" }

// ponytail: no rate limit on setup-token guesses; the token is high-entropy and setup is one-shot.
func (s *Service) Setup(ctx context.Context, setupToken, username string, c Credentials) (store.User, error) {
	if s.SetupToken == "" || subtle.ConstantTimeCompare([]byte(setupToken), []byte(s.SetupToken)) != 1 {
		return store.User{}, ErrBadSetupToken
	}
	if !ValidUsername(username) || c.Validate() != nil {
		return store.User{}, ErrInvalid
	}
	u := store.User{Username: username, Salt: c.Salt, Iterations: c.Iterations, StoredKey: c.StoredKey, ServerKey: c.ServerKey}
	if err := s.Store.CreateFirstAdmin(ctx, &u); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.User{}, ErrSetupDone
		}
		return store.User{}, err
	}
	return u, nil
}

func (s *Service) fakeSalt(username string) []byte {
	h := hmac.New(sha256.New, s.Secret)
	h.Write([]byte("fridge-fake-salt:" + username))
	return h.Sum(nil)[:saltLen]
}

func clip(name string) string {
	if len(name) > maxAttemptName {
		return name[:maxAttemptName]
	}
	return name
}

// start parses client-first, applies the lock and registers SCRAM state. Unknown or
// disabled users (and malformed usernames) get a deterministic fake salt.
func (s *Service) start(ctx context.Context, clientFirst, ip string, reauthFor *Principal) (string, string, Attempt, error) {
	var att Attempt
	_, username, err := scram.StartServer(clientFirst, make([]byte, saltLen), MinIterations, "")
	if err != nil {
		return "", "", att, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	att.Username = clip(username)
	if reauthFor != nil && username != reauthFor.Username {
		return "", "", att, ErrForbidden
	}
	var u store.User
	if ValidUsername(username) {
		got, err := s.Store.GetUserByName(ctx, username)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return "", "", att, err
		default:
			att.UserID = &got.ID
			if !got.Disabled {
				u = got
			}
		}
	}
	salt, iter := s.fakeSalt(username), MinIterations
	if u.ID != 0 {
		salt, iter = u.Salt, u.Iterations
	}
	srv, _, err := scram.StartServer(clientFirst, salt, iter, randNonce())
	if err != nil {
		return "", "", att, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if reauthFor != nil && u.ID != reauthFor.UserID {
		return "", "", att, ErrForbidden
	}
	id, _ := newToken()
	key := limiterKey(username, ip)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locked(key) {
		return "", "", att, ErrLocked
	}
	s.sweep()
	// ponytail: O(n) scan, n <= maxPending; keep a per-IP counter map if this gets hot.
	perIP := 0
	for _, p := range s.scram {
		if p.ip == ip {
			perIP++
		}
	}
	if perIP >= maxPendingPerIP {
		return "", "", att, ErrBusy
	}
	if len(s.scram) >= maxPending { // evict the oldest rather than refuse everyone
		var oldK string
		var oldT time.Time
		for k, p := range s.scram {
			if oldK == "" || p.expires.Before(oldT) {
				oldK, oldT = k, p.expires
			}
		}
		delete(s.scram, oldK)
	}
	s.scram[id] = &pending{srv: srv, user: u, att: att, username: username, ip: ip, reauth: reauthFor != nil, expires: s.now().Add(ScramStateTTL)}
	return srv.ServerFirst, id, att, nil
}

func randNonce() string {
	t, _ := newToken()
	return t[:24]
}

func (s *Service) LoginStart(ctx context.Context, clientFirst, ip string) (string, string, Attempt, error) {
	return s.start(ctx, clientFirst, ip, nil)
}

// finish consumes the SCRAM state and verifies the proof; failures count against the limiter.
func (s *Service) finish(ctx context.Context, scramID, clientFinal, ip string, reauth bool) (string, store.User, Attempt, error) {
	var att Attempt
	s.mu.Lock()
	p := s.scram[scramID]
	delete(s.scram, scramID) // single use
	s.mu.Unlock()
	if p == nil || p.reauth != reauth || p.ip != ip || s.now().After(p.expires) {
		return "", store.User{}, att, ErrBadCredentials
	}
	att = p.att
	key := limiterKey(p.username, ip)
	// Check the lock and spend one attempt atomically, so parallel finishes of
	// pre-started states cannot exceed the budget. Success refunds below.
	s.mu.Lock()
	if s.locked(key) {
		s.mu.Unlock()
		return "", store.User{}, att, ErrLocked
	}
	s.recordFail(key)
	s.mu.Unlock()
	// Same DB lookup on every path (id 0 -> ErrNotFound) so timing does not reveal existence.
	var id int64
	if p.user.ID != 0 {
		id = p.user.ID
	}
	var u store.User
	stored, server := make([]byte, keyLen), make([]byte, keyLen)
	rand.Read(stored)
	rand.Read(server)
	cur, err := s.Store.GetUser(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", store.User{}, att, err
	}
	if err == nil && !cur.Disabled {
		u = cur
		stored, server = cur.StoredKey, cur.ServerKey
	}
	sf, err := p.srv.Finish(clientFinal, stored, server)
	if err != nil || u.ID == 0 {
		return "", store.User{}, att, ErrBadCredentials
	}
	s.mu.Lock()
	delete(s.fails, key)
	s.mu.Unlock()
	return sf, u, att, nil
}

func (s *Service) LoginFinish(ctx context.Context, scramID, clientFinal, ip, userAgent string) (string, string, store.User, Attempt, error) {
	sf, u, att, err := s.finish(ctx, scramID, clientFinal, ip, false)
	if err != nil {
		return "", "", store.User{}, att, err
	}
	id, hash := newToken()
	now := s.now()
	sess := store.Session{IDHash: hash, UserID: u.ID, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(SessionAbsTTL), IP: ip, UserAgent: userAgent}
	if err := s.Store.CreateSession(ctx, &sess); err != nil {
		return "", "", store.User{}, att, err
	}
	return sf, id, u, att, nil
}

func (s *Service) ReauthStart(ctx context.Context, p Principal, clientFirst, ip string) (string, string, error) {
	sf, id, _, err := s.start(ctx, clientFirst, ip, &p)
	return sf, id, err
}

func (s *Service) ReauthFinish(ctx context.Context, p Principal, scramID, clientFinal, ip string) (string, string, error) {
	sf, u, _, err := s.finish(ctx, scramID, clientFinal, ip, true)
	if err != nil {
		return "", "", err
	}
	if u.ID != p.UserID {
		return "", "", ErrBadCredentials
	}
	tok, hash := newToken()
	s.mu.Lock()
	s.reauths[string(hash)] = reauthTok{userID: p.UserID, session: string(p.SessionHash), expires: s.now().Add(ReauthTTL)}
	s.mu.Unlock()
	return sf, tok, nil
}

// consumeReauth validates and burns a reauth token.
func (s *Service) consumeReauth(p Principal, token string) bool {
	k := string(SessionHash(token))
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.reauths[k]
	delete(s.reauths, k)
	return ok && r.userID == p.UserID && r.session == string(p.SessionHash) && !s.now().After(r.expires)
}
