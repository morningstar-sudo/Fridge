// Package app orchestrates databases: validation, containers, certs, routes, state.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"fridge/internal/audit"
	"fridge/internal/container"
	"fridge/internal/objstore"
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
	CreateNetwork(ctx context.Context, name string) error
	NetworkExists(ctx context.Context, name string) (bool, error)
	RemoveNetwork(ctx context.Context, name string) error
	ConnectNetwork(ctx context.Context, netName, containerName string, aliases []string) error
	DisconnectNetwork(ctx context.Context, netName, containerName string) error
	ContainerNetworks(ctx context.Context, containerName string) ([]string, error)
	FridgeSubnets(ctx context.Context) ([]netip.Prefix, error)
}

var _ Runtime = (*container.Client)(nil)

type App struct {
	Store         *store.Store
	Box           *secret.Box
	RT            Runtime
	CA            *pki.CA
	TraefikDir    string
	APIBackend    string
	HealthTimeout time.Duration
	Alert         func(ctx context.Context, text string)
	Audit         *audit.Logger                                                    // nil = no audit trail
	Sched         *Scheduler                                                       // may be nil in tests
	OpenObjects   func(d store.Destination, secretKey string) (ObjectStore, error) // nil = real S3
	now           func() time.Time                                                 // test clock; nil = time.Now
	backupTimeout time.Duration                                                    // test override; 0 = backupTimeout
	wg            sync.WaitGroup

	// ControlNetwork (FRIDGE_NETWORK) holds only traefik/fridge/fridge-meta; each DB node gets its own
	// internal network shared with TraefikContainer (FRIDGE_TRAEFIK_CONTAINER).
	ControlNetwork, TraefikContainer string

	netMu      sync.Mutex     // serializes node-network repair (Reconcile/Heal) with removeNode's teardown
	healFailed map[int64]bool // under netMu: DBs whose last heal failed (alert once per OK→failed)
	suLocks    sync.Map       // db id -> *sync.Mutex: one superuser rotation per node at a time
}

// auditSystem records an action taken by Fridge itself (scheduler, retention).
func (a *App) auditSystem(ctx context.Context, action string, dbID int64, targetType string, targetID int64, ok bool, detail map[string]any) {
	a.Audit.Record(ctx, store.AuditEntry{Actor: "system", Action: action, TargetType: targetType,
		TargetID: strconv.FormatInt(targetID, 10), DBID: &dbID, Success: ok, Detail: detail})
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

// ObjectStore is the S3 surface the app needs; *objstore.Store satisfies it.
type ObjectStore interface {
	Test(ctx context.Context) error
	Put(ctx context.Context, key string, r io.Reader, meta map[string]string) (int64, error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Meta(ctx context.Context, key string) (map[string]string, error)
	List(ctx context.Context, prefix, q string, limit int) ([]objstore.Object, bool, error)
	Delete(ctx context.Context, key string) error
}

var _ ObjectStore = (*objstore.Store)(nil)

// Ready reports whether the metadata DB and the container runtime respond.
func (a *App) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := a.Store.Ping(ctx); err != nil {
		return err
	}
	return a.RT.Ping(ctx)
}
