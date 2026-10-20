// Package app orchestrates databases: validation, containers, certs, routes, state.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sync"
	"time"

	"fridge/internal/container"
	"fridge/internal/pki"
	"fridge/internal/proxy"
	"fridge/internal/secret"
	"fridge/internal/store"
)

var ErrInvalid = errors.New("invalid")

type Runtime interface {
	Ping(ctx context.Context) error
	Run(ctx context.Context, s container.RunSpec) (string, error)
	WaitHealthy(ctx context.Context, id string, timeout time.Duration) error
	Exec(ctx context.Context, id string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	Remove(ctx context.Context, id, volume string) error
}

type App struct {
	Store         *store.Store
	Box           *secret.Box
	RT            Runtime
	CA            *pki.CA
	TraefikDir    string
	APIBackend    string
	HealthTimeout time.Duration
	Alert         func(ctx context.Context, text string)
	Sched         *Scheduler // may be nil in tests
	wg            sync.WaitGroup
}

// Wait blocks until all background jobs finish.
func (a *App) Wait() { a.wg.Wait() }

var defaultAllowlist = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}

func (a *App) Allowlist(ctx context.Context) ([]string, error) {
	v, ok, err := a.Store.GetSetting(ctx, "allowlist")
	if err != nil {
		return nil, err
	}
	if !ok {
		return defaultAllowlist, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, fmt.Errorf("corrupt allowlist setting: %w", err)
	}
	return out, nil
}

func (a *App) SetAllowlist(ctx context.Context, cidrs []string) error {
	if len(cidrs) == 0 {
		return fmt.Errorf("%w: allowlist must not be empty", ErrInvalid)
	}
	for _, c := range cidrs {
		if _, err := netip.ParsePrefix(c); err != nil {
			return fmt.Errorf("%w: bad CIDR %q", ErrInvalid, c)
		}
	}
	b, _ := json.Marshal(cidrs)
	if err := a.Store.SetSetting(ctx, "allowlist", string(b)); err != nil {
		return err
	}
	return proxy.WriteBase(a.TraefikDir, cidrs, a.APIBackend)
}
