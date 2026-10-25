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
	"fridge/internal/objstore"
	"fridge/internal/pki"
	"fridge/internal/secret"
	"fridge/internal/store"
	"fridge/internal/swarm"
)

var ErrInvalid = errors.New("invalid")

// Runtime is the swarm as the app sees it: database nodes, one-shot jobs and the network ranges to keep S3 away from.
type Runtime interface {
	Ping(ctx context.Context) error
	PickHost(ctx context.Context) (swarm.Host, error)
	CreateNode(ctx context.Context, n swarm.NodeSpec) error
	NodeReady(ctx context.Context, key string, timeout time.Duration) error
	RemoveNode(ctx context.Context, ref swarm.NodeRef) error
	ListNodes(ctx context.Context) ([]swarm.NodeState, error)
	RunJob(ctx context.Context, j swarm.JobSpec) (swarm.JobResult, error)
	Subnets(ctx context.Context) ([]netip.Prefix, error)
}

var _ Runtime = (*swarm.Client)(nil)

type App struct {
	Store           *store.Store
	Box             *secret.Box
	RT              Runtime
	Admin           AdminConn // runs SQL as a node's superuser; nil = pgx through the node's edge sidecar
	CA              *pki.CA
	WebCert, WebKey string // files holding the web UI certificate (Fridge serves them to Traefik)
	APIBackend      string // URL Traefik reaches the Fridge API at
	HealthTimeout   time.Duration
	Alert           func(ctx context.Context, text string)
	Audit           *audit.Logger                                                                       // nil = no audit trail
	Sched           *Scheduler                                                                          // may be nil in tests
	OpenObjects     func(d store.Destination, secretKey string, g *objstore.Guard) (ObjectStore, error) // nil = real S3
	now             func() time.Time                                                                    // test clock; nil = time.Now
	backupTimeout   time.Duration                                                                       // test override; 0 = backupTimeout
	wg              sync.WaitGroup

	// JobImage runs dump/restore/check jobs; EgressNetwork is the empty overlay that gives them internet.
	JobImage, EgressNetwork string

	suLocks sync.Map // db id -> *sync.Mutex: one superuser rotation per node at a time

	Img      Images   // nil: the Images page is unavailable (tests)
	Registry Registry // nil: remote versions are not looked up
	pulls    pulls
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
	return nil // Traefik polls ProxyConfig
}

// ObjectStore is the S3 surface the app needs; *objstore.Store satisfies it.
type ObjectStore interface {
	Test(ctx context.Context) error
	Put(ctx context.Context, key string, r io.Reader, meta map[string]string) (int64, error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Meta(ctx context.Context, key string) (map[string]string, error)
	ReadHead(ctx context.Context, key string, n int) ([]byte, error) // the object's first n bytes
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
