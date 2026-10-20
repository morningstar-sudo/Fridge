package auth

import (
	"bytes"
	"context"
	"errors"

	"fridge/internal/store"
)

func (s *Service) Authenticate(ctx context.Context, sessionID string) (Principal, error) {
	hash := SessionHash(sessionID)
	sess, err := s.Store.GetSession(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, err
	}
	now := s.now()
	if now.Sub(sess.LastSeenAt) > SessionIdleTTL || now.After(sess.ExpiresAt) {
		s.Store.DeleteSession(ctx, hash)
		return Principal{}, ErrUnauthenticated
	}
	u, err := s.Store.GetUser(ctx, sess.UserID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Principal{}, err
	}
	if err != nil || u.Disabled {
		s.Store.DeleteSession(ctx, hash)
		return Principal{}, ErrUnauthenticated
	}
	if now.Sub(sess.LastSeenAt) > TouchInterval {
		if err := s.Store.TouchSession(ctx, hash, now); err != nil && !errors.Is(err, store.ErrNotFound) {
			return Principal{}, err
		}
	}
	return Principal{UserID: u.ID, Username: u.Username, Role: u.Role, SessionHash: hash}, nil
}

func (s *Service) Logout(ctx context.Context, p Principal) error {
	return s.Store.DeleteSession(ctx, p.SessionHash)
}

func (s *Service) ChangePassword(ctx context.Context, p Principal, reauthToken string, c Credentials) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if !s.consumeReauth(p, reauthToken) {
		return ErrForbidden
	}
	if err := s.Store.UpdateUserCredentials(ctx, p.UserID, c.Salt, c.Iterations, c.StoredKey, c.ServerKey); err != nil {
		return err
	}
	return s.Store.DeleteUserSessions(ctx, p.UserID, p.SessionHash)
}

func (s *Service) ListSessions(ctx context.Context, p Principal, all bool) ([]store.Session, error) {
	if all {
		if !p.IsAdmin() {
			return nil, ErrForbidden
		}
		return s.Store.ListSessions(ctx, nil)
	}
	return s.Store.ListSessions(ctx, &p.UserID)
}

// RevokeSession deletes the caller's own session, or any session for an admin.
// Not visible to the caller -> store.ErrNotFound.
func (s *Service) RevokeSession(ctx context.Context, p Principal, idHash []byte) error {
	var uid *int64
	if !p.IsAdmin() {
		uid = &p.UserID
	}
	list, err := s.Store.ListSessions(ctx, uid)
	if err != nil {
		return err
	}
	for _, x := range list {
		if bytes.Equal(x.IDHash, idHash) {
			return s.Store.DeleteSession(ctx, idHash)
		}
	}
	return store.ErrNotFound
}

func (s *Service) PurgeExpired(ctx context.Context) (int, error) {
	return s.Store.DeleteExpiredSessions(ctx, s.now(), SessionIdleTTL)
}
