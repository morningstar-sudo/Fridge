package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fridge/internal/objstore"
	"github.com/minio/minio-go/v7"
	"io"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"fridge/internal/container"
	"fridge/internal/pki"
	"fridge/internal/secret"
	"fridge/internal/store"
	"fridge/internal/store/storetest"
)

type removeCall struct{ ID, Volume string }

type execCall struct {
	ID    string
	Cmd   []string
	Stdin string
}

type fakeRuntime struct {
	mu        sync.Mutex
	runs      []container.RunSpec
	removes   []removeCall
	execs     []execCall
	healthErr error
	pingErr   error
	alerts    []string
	onWait    func()
	execFn    func(cmd []string, stdin io.Reader, stdout, stderr io.Writer) int
	hang      bool // Exec blocks until ctx is done

	// network state: network -> attached containers; ops logs Run/Remove/network calls in order.
	nets       map[string]map[string]bool
	ops        []string
	connectErr error
}

func (f *fakeRuntime) Ping(ctx context.Context) error { return f.pingErr }

func (f *fakeRuntime) Run(ctx context.Context, s container.RunSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, s)
	f.ops = append(f.ops, "run "+s.Name+" "+s.Network)
	if f.nets[s.Network] != nil {
		f.nets[s.Network][s.Name] = true
	}
	return "ctr-" + s.Name, nil
}

func (f *fakeRuntime) CreateNetwork(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "create "+name)
	if f.nets == nil {
		f.nets = map[string]map[string]bool{}
	}
	if f.nets[name] == nil {
		f.nets[name] = map[string]bool{}
	}
	return nil
}

func (f *fakeRuntime) RemoveNetwork(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "rmnet "+name)
	if len(f.nets[name]) > 0 { // like Docker: attached endpoints block removal
		return errors.New("network has active endpoints")
	}
	delete(f.nets, name)
	return nil
}

func (f *fakeRuntime) ConnectNetwork(ctx context.Context, net, ctr string, aliases []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "connect "+net+" "+ctr+" "+strings.Join(aliases, ","))
	if f.connectErr != nil {
		return f.connectErr
	}
	if f.nets[net] == nil {
		return errors.New("no such network " + net)
	}
	f.nets[net][ctr] = true // already connected: no-op
	return nil
}

func (f *fakeRuntime) DisconnectNetwork(ctx context.Context, net, ctr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "disconnect "+net+" "+ctr)
	delete(f.nets[net], ctr)
	return nil
}

func (f *fakeRuntime) ContainerNetworks(ctx context.Context, ctr string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n, cs := range f.nets {
		if cs[ctr] {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeRuntime) FridgeSubnets(ctx context.Context) ([]netip.Prefix, error) { return nil, nil }

func (f *fakeRuntime) WaitHealthy(ctx context.Context, id string, d time.Duration) error {
	if f.onWait != nil {
		f.onWait()
	}
	return f.healthErr
}

func (f *fakeRuntime) Exec(ctx context.Context, id string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if f.hang {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	c := execCall{ID: id, Cmd: cmd}
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return -1, err
		}
		c.Stdin, stdin = string(b), bytes.NewReader(b)
	}
	f.mu.Lock()
	f.execs = append(f.execs, c)
	f.mu.Unlock()
	if f.execFn == nil {
		return 0, nil
	}
	return f.execFn(cmd, stdin, stdout, stderr), nil
}

func (f *fakeRuntime) Remove(ctx context.Context, id, volume string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err // like the real client: a dead ctx removes nothing
	}
	f.removes = append(f.removes, removeCall{id, volume})
	f.ops = append(f.ops, "remove "+id)
	for _, cs := range f.nets {
		delete(cs, strings.TrimPrefix(id, "ctr-"))
	}
	return nil
}

type fakeObj struct {
	data []byte
	meta map[string]string
}

type fakeObjects struct {
	mu     sync.Mutex
	objs   map[string]fakeObj
	putErr error
}

func (f *fakeObjects) Test(ctx context.Context) error { return nil }

func (f *fakeObjects) Put(ctx context.Context, key string, r io.Reader, meta map[string]string) (int64, error) {
	if f.putErr != nil {
		return 0, f.putErr
	}
	b, err := io.ReadAll(r) // like a real upload: a reader error aborts, nothing is stored
	if err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objs[key] = fakeObj{b, meta}
	return int64(len(b)), nil
}

func (f *fakeObjects) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objs[key]
	if !ok {
		return nil, errors.New("no such key")
	}
	return io.NopCloser(bytes.NewReader(o.data)), nil
}

func (f *fakeObjects) Meta(ctx context.Context, key string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objs[key]
	if !ok {
		return nil, minio.ErrorResponse{Code: "NoSuchKey"} // what the real objstore surfaces
	}
	return o.meta, nil
}

func (f *fakeObjects) List(ctx context.Context, prefix, q string, limit int) ([]objstore.Object, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []objstore.Object
	for k, o := range f.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, objstore.Object{Key: k, Size: int64(len(o.data))})
		}
	}
	o, tr := objstore.FilterSort(out, q, limit)
	return o, tr, nil
}

func (f *fakeObjects) Delete(ctx context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objs, key)
	return nil
}

func (f *fakeObjects) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return "http://s3/" + key, nil
}

func (f *fakeObjects) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objs[key]
	return ok
}

func newTestApp(t *testing.T) (*App, *fakeRuntime, *fakeObjects) {
	t.Helper()
	key := make([]byte, 32)
	rand.Read(key)
	box, err := secret.NewBox(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := pki.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rt := &fakeRuntime{}
	a := &App{
		Store: storetest.New(t), Box: box, RT: rt, CA: ca,
		TraefikDir: t.TempDir(), HealthTimeout: time.Second,
		ControlNetwork: "fridge-net", TraefikContainer: "fridge-traefik",
	}
	a.Alert = func(ctx context.Context, text string) {
		rt.mu.Lock()
		rt.alerts = append(rt.alerts, text)
		rt.mu.Unlock()
	}
	objs := &fakeObjects{objs: map[string]fakeObj{}}
	a.OpenObjects = func(d store.Destination, sk string) (ObjectStore, error) { return objs, nil }
	return a, rt, objs
}

// mkUser inserts a user (no setup, so owner_id stays nullable in app tests).
func mkUser(t *testing.T, a *App, name, role string) int64 {
	t.Helper()
	u := store.User{Username: name, Role: role, Salt: make([]byte, 16), Iterations: 600000, StoredKey: make([]byte, 32), ServerKey: make([]byte, 32)}
	if err := a.Store.CreateUser(context.Background(), &u); err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// own returns the id of the default test owner "root1", creating it on first use.
func own(t *testing.T, a *App) int64 {
	t.Helper()
	if u, err := a.Store.GetUserByName(context.Background(), "root1"); err == nil {
		return u.ID
	}
	return mkUser(t, a, "root1", "admin")
}

func regDomain(a *App, owner int64, domain string) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if _, err := a.Store.GetDomainByName(context.Background(), domain); err == nil {
		return
	}
	if err := a.Store.CreateDomain(context.Background(), &store.Domain{OwnerID: owner, Name: domain}); err != nil {
		panic(err)
	}
}

// createDB registers the request's domain for its owner first (tests that don't care about domain ownership).
func createDB(a *App, ctx context.Context, req CreateRequest) (store.Database, error) {
	if req.OwnerID > 0 && validDomain(strings.ToLower(strings.TrimSpace(req.Domain))) {
		regDomain(a, req.OwnerID, req.Domain)
	}
	return a.CreateDatabase(ctx, req)
}

func startRestore(a *App, ctx context.Context, req RestoreRequest) (store.Restore, error) {
	if req.New != nil && req.OwnerID > 0 && validDomain(strings.ToLower(strings.TrimSpace(req.New.Domain))) {
		regDomain(a, req.OwnerID, req.New.Domain)
	}
	return a.StartRestore(ctx, req)
}
