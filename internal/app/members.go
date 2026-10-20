package app

import (
	"context"
	"errors"

	"fridge/internal/store"
)

// AddMember gives every failure the same error so callers cannot probe usernames.
func (a *App) AddMember(ctx context.Context, d store.Database, username string, addedBy int64) error {
	fail := invalid("không thêm được")
	u, err := a.Store.GetUserByName(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		return fail
	} else if err != nil {
		return err
	}
	if d.OwnerID != nil && *d.OwnerID == u.ID {
		return fail
	}
	if err := a.Store.AddMember(ctx, d.ID, u.ID, &addedBy); errors.Is(err, store.ErrConflict) {
		return fail
	} else if err != nil {
		return err
	}
	return nil
}

func (a *App) RemoveMember(ctx context.Context, d store.Database, userID int64) error {
	return a.Store.RemoveMember(ctx, d.ID, userID)
}

func (a *App) ListMembers(ctx context.Context, d store.Database) ([]store.Member, error) {
	return a.Store.ListMembers(ctx, d.ID)
}
