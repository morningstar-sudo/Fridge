package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"fridge/internal/auth"
	"fridge/internal/store"
)

func (a *App) VisibleDatabases(ctx context.Context, p auth.Principal) ([]store.Database, error) {
	if p.IsAdmin() {
		return a.Store.ListDatabases(ctx)
	}
	return a.Store.ListDatabasesVisible(ctx, p.UserID)
}

// IsDBOwner reports whether p may administer d (admin or owner).
func IsDBOwner(p auth.Principal, d store.Database) bool {
	return p.IsAdmin() || (d.OwnerID != nil && *d.OwnerID == p.UserID)
}

func (a *App) CanAccessDB(ctx context.Context, p auth.Principal, d store.Database) (bool, error) {
	if IsDBOwner(p, d) {
		return true, nil
	}
	return a.Store.IsMember(ctx, d.ID, p.UserID)
}

func (a *App) VisibleDestinations(ctx context.Context, p auth.Principal) ([]store.Destination, error) {
	if p.IsAdmin() {
		return a.Store.ListDestinations(ctx)
	}
	return a.Store.ListDestinationsVisible(ctx, p.UserID)
}

func CanManageDestination(p auth.Principal, d store.Destination) bool {
	return p.IsAdmin() || (d.OwnerID != nil && *d.OwnerID == p.UserID)
}

func CanUseDestination(p auth.Principal, d store.Destination) bool {
	return (CanManageDestination(p, d) || d.Shared) && d.DeletedAt == nil
}

func (a *App) SetDestinationShared(ctx context.Context, id int64, shared bool) error {
	return a.Store.SetDestinationShared(ctx, id, shared)
}

// ownerDestination picks the lowest-id live destination the DB's owner owns or that is shared.
func (a *App) ownerDestination(ctx context.Context, db store.Database) (store.Destination, bool, error) {
	dests, err := a.Store.ListDestinations(ctx) // live only, ordered by id
	if err != nil {
		return store.Destination{}, false, err
	}
	for _, d := range dests {
		if d.Shared || (d.OwnerID != nil && db.OwnerID != nil && *d.OwnerID == *db.OwnerID) {
			return d, true, nil
		}
	}
	return store.Destination{}, false, nil
}

type Restorable struct {
	Key          string
	Size         int64
	LastModified time.Time
	DatabaseName string // empty for foreign files
}

func (a *App) dbName(ctx context.Context, id int64) string {
	d, err := a.Store.GetDatabase(ctx, id)
	if err != nil {
		return ""
	}
	return d.Name
}

// ListRestorable lists files p may restore from: the whole bucket for managers,
// only own-visible ok backups for users of a shared destination.
func (a *App) ListRestorable(ctx context.Context, p auth.Principal, destID int64, prefix, q string) ([]Restorable, bool, error) {
	dest, err := a.Store.GetDestination(ctx, destID)
	if err != nil {
		return nil, false, err
	}
	if !CanManageDestination(p, dest) && !CanUseDestination(p, dest) {
		return nil, false, store.ErrNotFound
	}
	var out []Restorable
	if CanManageDestination(p, dest) {
		objs, trunc, err := a.ListObjects(ctx, destID, prefix, q)
		if err != nil {
			return nil, false, err
		}
		for _, o := range objs {
			r := Restorable{Key: o.Key, Size: o.Size, LastModified: o.LastModified}
			if b, err := a.Store.GetBackupByKey(ctx, destID, o.Key); err == nil {
				r.DatabaseName = a.dbName(ctx, b.DatabaseID)
			} else if !errors.Is(err, store.ErrNotFound) {
				return nil, false, err
			}
			out = append(out, r)
		}
		return out, trunc, nil
	}
	bs, err := a.Store.ListOKBackupsOnDestination(ctx, destID, &p.UserID)
	if err != nil {
		return nil, false, err
	}
	q = strings.ToLower(q)
	names := map[int64]string{}
	for _, b := range bs {
		if !strings.HasPrefix(b.S3Key, prefix) || !strings.Contains(strings.ToLower(b.S3Key), q) {
			continue
		}
		n, ok := names[b.DatabaseID]
		if !ok {
			n = a.dbName(ctx, b.DatabaseID)
			names[b.DatabaseID] = n
		}
		t := b.StartedAt
		if b.FinishedAt != nil {
			t = *b.FinishedAt
		}
		out = append(out, Restorable{Key: b.S3Key, Size: b.SizeBytes, LastModified: t, DatabaseName: n})
	}
	return out, false, nil
}

// CanRestoreKey: managers may restore any file; others only keys of backups of databases they can access.
func (a *App) CanRestoreKey(ctx context.Context, p auth.Principal, destID int64, key string) (bool, error) {
	dest, err := a.Store.GetDestination(ctx, destID)
	if err != nil {
		return false, err
	}
	if CanManageDestination(p, dest) {
		return true, nil
	}
	b, err := a.Store.GetBackupByKey(ctx, destID, key)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	d, err := a.Store.GetDatabase(ctx, b.DatabaseID)
	if err != nil {
		return false, err
	}
	return a.CanAccessDB(ctx, p, d)
}
