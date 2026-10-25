package app

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"

	"fridge/internal/engine"
	"fridge/internal/store"
)

// RevealSuperuser returns the node superuser's credentials; a legacy node's admin is its db_user.
func (a *App) RevealSuperuser(ctx context.Context, d store.Database) (user, password string, err error) {
	if d.AdminUser == "" {
		pw, err := a.Box.Decrypt(d.PasswordEnc)
		return d.User, pw, err
	}
	pw, err := a.Box.Decrypt(d.AdminPasswordEnc)
	return d.AdminUser, pw, err
}

// RotateSuperuser sets a fresh superuser password on the running node, then stores it.
// Rotations of one node are serialized so the stored password always matches the node's last one.
func (a *App) RotateSuperuser(ctx context.Context, d store.Database) error {
	mu, _ := a.suLocks.LoadOrStore(d.ID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	if d.AdminUser == "" {
		return invalid("legacy database %s has no separate superuser", d.Name)
	}
	if d.Status != "running" {
		return invalid("database %s is %s, not running", d.Name, d.Status)
	}
	eng, _, err := engine.Get(d.Engine)
	if err != nil {
		return err
	}
	pw := rand.Text()
	enc, err := a.Box.Encrypt(pw)
	if err != nil {
		return err
	}
	_, oldPW, err := a.RevealSuperuser(ctx, d)
	if err != nil {
		return err
	}
	if err := a.execAsAdmin(ctx, d, eng, oldPW, eng.SetPasswordSQL(d.AdminUser, pw)); err != nil {
		return fmt.Errorf("rotate superuser: %w", err)
	}
	return a.Store.SetDatabaseAdminPassword(ctx, d.ID, enc)
}
