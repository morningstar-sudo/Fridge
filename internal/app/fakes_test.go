package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"fridge/internal/job"
	"fridge/internal/objstore"
	"fridge/internal/pki"
	"fridge/internal/secret"
	"fridge/internal/store"
	"fridge/internal/store/storetest"
	"fridge/internal/swarm"
)

// removeCall is what RemoveNode was asked to remove; ID mimics the old container id so assertions read the same.
type removeCall struct{ ID, Volume string }

type runCall struct {
	Name, Image string // Name is the volume (= the node's old container name)
	Env         []string
	Spec        swarm.NodeSpec
}

type jobCall struct {
	Name     string
	Spec     job.Spec
	Networks []string
	NodeID   string
	Volumes  []string
	User     string
}

// fakeRuntime is a swarm that only keeps notes. Jobs succeed and, for a dump, leave a small object in objs,
// unless jobFn says otherwise.
type fakeRuntime struct {
	mu        sync.Mutex
	subnets   []netip.Prefix // what Subnets reports
	runs      []runCall
	removes   []removeCall
	jobs      []jobCall
	healthErr error
	pingErr   error
	pickErr   error
	createErr error
	alerts    []string
	onWait    func()
	jobFn     func(spec job.Spec, networks []string) (stdout, stderr string, code int)
	hang      bool // RunJob blocks until ctx is done
	objs      *fakeObjects
	nodes     []swarm.NodeState // what ListNodes reports
}

func (f *fakeRuntime) Ping(ctx context.Context) error { return f.pingErr }

func (f *fakeRuntime) PickHost(ctx context.Context) (swarm.Host, error) {
	return swarm.Host{ID: "host1", Hostname: "n1"}, f.pickErr
}

func (f *fakeRuntime) CreateNode(ctx context.Context, n swarm.NodeSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	f.runs = append(f.runs, runCall{Name: n.VolumeName, Image: n.Image, Env: n.Env, Spec: n})
	return nil
}

func (f *fakeRuntime) NodeReady(ctx context.Context, key string, timeout time.Duration) error {
	if f.onWait != nil {
		f.onWait()
	}
	return f.healthErr
}

func (f *fakeRuntime) RemoveNode(ctx context.Context, ref swarm.NodeRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err // like the real client: a dead ctx removes nothing
	}
	f.removes = append(f.removes, removeCall{"ctr-" + ref.Volume, ref.Volume})
	return nil
}

func (f *fakeRuntime) NodeLogs(ctx context.Context, key string) string { return "" }

func (f *fakeRuntime) ListNodes(ctx context.Context) ([]swarm.NodeState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]swarm.NodeState(nil), f.nodes...), nil
}

func (f *fakeRuntime) Subnets(ctx context.Context) ([]netip.Prefix, error) { return f.subnets, nil }

// result renders a job's FRIDGE_RESULT stdout line.
func result(v any) string {
	b, _ := json.Marshal(v)
	return job.ResultPrefix + string(b) + "\n"
}

func (f *fakeRuntime) RunJob(ctx context.Context, j swarm.JobSpec) (swarm.JobResult, error) {
	if f.hang {
		<-ctx.Done()
		return swarm.JobResult{}, ctx.Err()
	}
	var spec job.Spec
	for _, e := range j.Env {
		if v, ok := strings.CutPrefix(e, job.EnvSpec+"="); ok {
			if err := json.Unmarshal([]byte(v), &spec); err != nil {
				return swarm.JobResult{}, err
			}
		}
	}
	f.mu.Lock()
	f.jobs = append(f.jobs, jobCall{Name: j.Name, Spec: spec, Networks: j.Networks, NodeID: j.NodeID, Volumes: j.Volumes, User: j.User})
	fn := f.jobFn
	f.mu.Unlock()
	if fn != nil {
		out, errs, code := fn(spec, j.Networks)
		return swarm.JobResult{ExitCode: code, Stdout: out, Stderr: errs}, nil
	}
	if spec.Op == "dump" && f.objs != nil {
		f.objs.put(spec.Key, []byte("DUMP"), spec.Meta)
		return swarm.JobResult{Stdout: result(map[string]any{"size": 4})}, nil
	}
	return swarm.JobResult{}, nil
}

// lastJob is the most recent job of this kind (zero value if none).
func (f *fakeRuntime) lastJob(op string) (jobCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.jobs) - 1; i >= 0; i-- {
		if f.jobs[i].Spec.Op == op {
			return f.jobs[i], true
		}
	}
	return jobCall{}, false
}

type adminCall struct{ DSN, SQL string }

// fakeAdmin records the SQL a node would have run as its superuser.
type fakeAdmin struct {
	fn    func(dsn, sql string) error // runs before the call is recorded (may be slow or fail)
	mu    sync.Mutex
	calls []adminCall
	err   error
}

func (f *fakeAdmin) Exec(ctx context.Context, dsn, sql string) error {
	if f.fn != nil {
		if err := f.fn(dsn, sql); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, adminCall{dsn, sql})
	return f.err
}

func (f *fakeAdmin) last() adminCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return adminCall{}
	}
	return f.calls[len(f.calls)-1]
}

func adminOf(a *App) *fakeAdmin { return a.Admin.(*fakeAdmin) }

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
		HealthTimeout: time.Second, APIBackend: "http://fridge:8080",
		Admin: &fakeAdmin{}, JobImage: "fridge-job:test", EgressNetwork: "fridge_egress", DBNetwork: "fridge_db",
	}
	a.WebCert, a.WebKey = writeWebCert(t, ca)
	a.Alert = func(ctx context.Context, text string) {
		rt.mu.Lock()
		rt.alerts = append(rt.alerts, text)
		rt.mu.Unlock()
	}
	objs := &fakeObjects{objs: map[string]fakeObj{}}
	rt.objs = objs
	a.OpenObjects = func(d store.Destination, sk string, _ *objstore.Guard) (ObjectStore, error) { return objs, nil }
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

func (f *fakeObjects) ReadHead(ctx context.Context, key string, n int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.objs[key]
	if !ok {
		return nil, minio.ErrorResponse{Code: "NoSuchKey"}
	}
	return o.data[:min(n, len(o.data))], nil
}

// dumpFile is one of the real pg_dump files in internal/dumpcheck/testdata.
func dumpFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "dumpcheck", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f *fakeObjects) put(key string, data []byte, meta map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objs[key] = fakeObj{data, meta}
}

// dumps makes dump jobs behave as the real one: with code 0 the object holds data and the job reports its size;
// otherwise nothing is stored and the job fails with stderr.
func (f *fakeRuntime) dumps(data, stderr string, code int) {
	f.jobFn = func(spec job.Spec, _ []string) (string, string, int) {
		if code != 0 {
			return "", stderr, code
		}
		f.objs.put(spec.Key, []byte(data), spec.Meta)
		return result(map[string]any{"size": len(data)}), stderr, 0
	}
}

// writeWebCert issues a web UI certificate into a temp dir and returns the certificate and key paths.
func writeWebCert(t *testing.T, ca *pki.CA) (certFile, keyFile string) {
	t.Helper()
	certPEM, keyPEM, err := ca.IssueNames([]string{"fridge.test"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "web.crt"), filepath.Join(dir, "web.key")
	if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}
