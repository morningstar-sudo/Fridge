package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fridge/internal/objstore"
	"io"
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

type fakeRuntime struct {
	mu        sync.Mutex
	runs      []container.RunSpec
	removes   []removeCall
	healthErr error
	pingErr   error
	alerts    []string
	onWait    func()
	execFn    func(cmd []string, stdin io.Reader, stdout, stderr io.Writer) int
}

func (f *fakeRuntime) Ping(ctx context.Context) error { return f.pingErr }

func (f *fakeRuntime) Run(ctx context.Context, s container.RunSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, s)
	return "ctr-" + s.Name, nil
}

func (f *fakeRuntime) WaitHealthy(ctx context.Context, id string, d time.Duration) error {
	if f.onWait != nil {
		f.onWait()
	}
	return f.healthErr
}

func (f *fakeRuntime) Exec(ctx context.Context, id string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
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
	return f.objs[key].meta, nil
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
