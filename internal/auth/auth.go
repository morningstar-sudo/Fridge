// Package auth implements first-run setup, SCRAM-SHA-256 login (server side),
// stateful sessions, invites and user administration.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"regexp"
	"sync"
	"time"

	"fridge/internal/scram"
	"fridge/internal/store"
)

const (
	SessionIdleTTL  = 12 * time.Hour
	SessionAbsTTL   = 7 * 24 * time.Hour
	TouchInterval   = time.Minute
	ScramStateTTL   = 60 * time.Second
	InviteTTL       = 48 * time.Hour
	ReauthTTL       = 2 * time.Minute
	LoginMaxFails   = 5
	LoginFailWindow = time.Minute
	LoginLockout    = 5 * time.Minute
	MinIterations   = 600000
	MaxIterations   = 10000000

	saltLen, keyLen = 16, 32
	maxPending      = 10000
	maxPendingPerIP = 20
	MinSecretLen    = 32
	sweepThreshold  = 1024
	maxAttemptName  = 64
)

var (
	ErrInvalid         = errors.New("invalid input")
	ErrSetupDone       = errors.New("setup already done")
	ErrBadSetupToken   = errors.New("bad setup token")
	ErrBadCredentials  = errors.New("bad credentials")
	ErrLocked          = errors.New("too many failed attempts")
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrForbidden       = errors.New("forbidden")
	ErrLastAdmin       = store.ErrLastAdmin
	ErrInviteInvalid   = errors.New("invite invalid or expired")
	ErrBusy            = errors.New("too many pending logins")
)

var usernameRe = regexp.MustCompile(`^[a-z][a-z0-9]{2,15}$`)

func ValidUsername(name string) bool { return usernameRe.MatchString(name) }

type Principal struct {
	UserID      int64
	Username    string
	Role        string
	SessionHash []byte
}

func (p Principal) IsAdmin() bool { return p.Role == "admin" }

type Credentials struct {
	Salt                 []byte
	Iterations           int
	StoredKey, ServerKey []byte
}

func (c Credentials) Validate() error {
	if len(c.Salt) != saltLen || c.Iterations < MinIterations || c.Iterations > MaxIterations ||
		len(c.StoredKey) != keyLen || len(c.ServerKey) != keyLen {
		return ErrInvalid
	}
	return nil
}

// Attempt identifies a login attempt for auditing; UserID is nil when the user does not exist.
type Attempt struct {
	Username string
	UserID   *int64
}

type pending struct {
	srv      *scram.Server
	user     store.User // zero ID = fake (unknown or disabled user)
	att      Attempt    // UserID set whenever the account exists, even if disabled
	username string
	ip       string
	reauth   bool
	expires  time.Time
}

type failState struct {
	times       []time.Time
	lockedUntil time.Time
}

type reauthTok struct {
	userID  int64
	session string
	expires time.Time
}

type Service struct {
	Store      *store.Store
	Secret     []byte
	SetupToken string
	Now        func() time.Time // nil = time.Now

	// ponytail: RAM state, single instance only; move to the DB for multiple replicas.
	mu      sync.Mutex
	scram   map[string]*pending
	fails   map[string]*failState
	reauths map[string]reauthTok
}

func New(st *store.Store, secret []byte, setupToken string) (*Service, error) {
	if len(secret) < MinSecretLen {
		return nil, errors.New("auth: secret must be at least 32 bytes")
	}
	return &Service{Store: st, Secret: secret, SetupToken: setupToken,
		scram: map[string]*pending{}, fails: map[string]*failState{}, reauths: map[string]reauthTok{}}, nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// newToken returns 32 random bytes as base64url (no padding) and its SHA-256.
func newToken() (tok string, hash []byte) {
	b := make([]byte, 32)
	rand.Read(b) // never fails since Go 1.24
	tok = base64.RawURLEncoding.EncodeToString(b)
	return tok, SessionHash(tok)
}

// SessionHash is the SHA-256 of a session ID (also used for invite and reauth tokens).
func SessionHash(sessionID string) []byte {
	h := sha256.Sum256([]byte(sessionID))
	return h[:]
}

// locked reports whether key is locked. Caller holds s.mu.
func (s *Service) locked(key string) bool {
	f := s.fails[key]
	return f != nil && s.now().Before(f.lockedUntil)
}

// recordFail counts a failed attempt. Caller holds s.mu.
func (s *Service) recordFail(key string) {
	now := s.now()
	f := s.fails[key]
	if f == nil {
		f = &failState{}
		s.fails[key] = f
	}
	kept := f.times[:0]
	for _, t := range f.times {
		if now.Sub(t) < LoginFailWindow {
			kept = append(kept, t)
		}
	}
	f.times = append(kept, now)
	if len(f.times) >= LoginMaxFails {
		f.lockedUntil = now.Add(LoginLockout)
		f.times = nil
	}
}

// sweep drops expired RAM state. Caller holds s.mu.
func (s *Service) sweep() {
	now := s.now()
	for k, p := range s.scram {
		if now.After(p.expires) {
			delete(s.scram, k)
		}
	}
	for k, r := range s.reauths {
		if now.After(r.expires) {
			delete(s.reauths, k)
		}
	}
	if len(s.fails) > sweepThreshold {
		for k, f := range s.fails {
			if !now.Before(f.lockedUntil) && (len(f.times) == 0 || now.Sub(f.times[len(f.times)-1]) >= LoginFailWindow) {
				delete(s.fails, k)
			}
		}
	}
}
