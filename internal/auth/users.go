package auth

import (
	"context"
	"errors"

	"fridge/internal/store"
)

const (
	purposeNew   = "new_user"
	purposeReset = "reset_password"
)

func validRole(r string) bool { return r == "admin" || r == "user" }

func (s *Service) newInvite(ctx context.Context, inv store.Invite) (string, error) {
	tok, hash := newToken()
	inv.TokenHash, inv.ExpiresAt = hash, s.now().Add(InviteTTL)
	if err := s.Store.CreateInvite(ctx, &inv); err != nil {
		return "", err
	}
	return tok, nil
}

func (s *Service) CreateInvite(ctx context.Context, p Principal, username, role string) (string, error) {
	if !p.IsAdmin() {
		return "", ErrForbidden
	}
	if !ValidUsername(username) || !validRole(role) {
		return "", ErrInvalid
	}
	if _, err := s.Store.GetUserByName(ctx, username); err == nil {
		return "", store.ErrConflict
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	return s.newInvite(ctx, store.Invite{Username: username, Role: role, Purpose: purposeNew, CreatedBy: &p.UserID})
}

func (s *Service) CreateResetInvite(ctx context.Context, createdBy *int64, userID int64) (string, error) {
	u, err := s.Store.GetUser(ctx, userID)
	if err != nil {
		return "", err
	}
	return s.newInvite(ctx, store.Invite{Username: u.Username, Role: u.Role, Purpose: purposeReset, UserID: &u.ID, CreatedBy: createdBy})
}

func (s *Service) invite(ctx context.Context, token string) (store.Invite, error) {
	inv, err := s.Store.GetInviteByHash(ctx, SessionHash(token))
	if errors.Is(err, store.ErrNotFound) {
		return inv, ErrInviteInvalid
	}
	if err != nil {
		return inv, err
	}
	if inv.UsedAt != nil || s.now().After(inv.ExpiresAt) {
		return inv, ErrInviteInvalid
	}
	return inv, nil
}

func (s *Service) InviteInfo(ctx context.Context, token string) (username, purpose string, err error) {
	inv, err := s.invite(ctx, token)
	if err != nil {
		return "", "", err
	}
	return inv.Username, inv.Purpose, nil
}

func (s *Service) AcceptInvite(ctx context.Context, token string, c Credentials) (store.User, error) {
	inv, err := s.invite(ctx, token)
	if err != nil {
		return store.User{}, err
	}
	if err := c.Validate(); err != nil {
		return store.User{}, err
	}
	if inv.Purpose == purposeNew && !ValidUsername(inv.Username) {
		return store.User{}, ErrInvalid
	}
	// Claim first: single use even under concurrent accepts.
	if err := s.Store.MarkInviteUsed(ctx, inv.ID); err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			return store.User{}, ErrInviteInvalid
		}
		return store.User{}, err
	}
	if inv.Purpose == purposeReset {
		if err := s.Store.UpdateUserCredentials(ctx, *inv.UserID, c.Salt, c.Iterations, c.StoredKey, c.ServerKey); err != nil {
			return store.User{}, err
		}
		if err := s.Store.DeleteUserSessions(ctx, *inv.UserID, nil); err != nil {
			return store.User{}, err
		}
		return s.Store.GetUser(ctx, *inv.UserID)
	}
	u := store.User{Username: inv.Username, Role: inv.Role, Salt: c.Salt, Iterations: c.Iterations,
		StoredKey: c.StoredKey, ServerKey: c.ServerKey, CreatedBy: inv.CreatedBy}
	if err := s.Store.CreateUser(ctx, &u); err != nil {
		return store.User{}, err
	}
	return u, nil
}

func (s *Service) ListUsers(ctx context.Context, p Principal) ([]store.User, error) {
	if !p.IsAdmin() {
		return nil, ErrForbidden
	}
	return s.Store.ListUsers(ctx)
}

func (s *Service) SetRole(ctx context.Context, p Principal, userID int64, role string) error {
	if !p.IsAdmin() {
		return ErrForbidden
	}
	if !validRole(role) {
		return ErrInvalid
	}
	return s.Store.SetUserRole(ctx, userID, role)
}

func (s *Service) SetDisabled(ctx context.Context, p Principal, userID int64, disabled bool) error {
	if !p.IsAdmin() {
		return ErrForbidden
	}
	if err := s.Store.SetUserDisabled(ctx, userID, disabled); err != nil {
		return err
	}
	if disabled {
		return s.Store.DeleteUserSessions(ctx, userID, nil)
	}
	return nil
}

// Promote makes the named user an admin (recovery path, no principal).
func (s *Service) Promote(ctx context.Context, username string) error {
	u, err := s.Store.GetUserByName(ctx, username)
	if err != nil {
		return err
	}
	return s.Store.SetUserRole(ctx, u.ID, "admin")
}
