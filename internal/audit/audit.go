// Package audit records who did what. Recording never fails the caller's operation.
package audit

import (
	"context"
	"log"
	"time"

	"fridge/internal/auth"
	"fridge/internal/store"
)

type Logger struct{ Store *store.Store }

// Record stores e; errors are logged, never returned. A nil Logger records nothing.
// Callers must keep passwords, credentials, secrets and tokens out of e.Detail.
func (l *Logger) Record(ctx context.Context, e store.AuditEntry) {
	if l == nil || l.Store == nil {
		return
	}
	// A client that hangs up must not lose the record of what it already did.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := l.Store.InsertAudit(ctx, e); err != nil {
		log.Printf("audit: %s by %s: %v", e.Action, e.Actor, err)
	}
}

// Actor is the (id, name) pair for an authenticated caller.
func Actor(p auth.Principal) (*int64, string) {
	id := p.UserID
	return &id, p.Username
}
