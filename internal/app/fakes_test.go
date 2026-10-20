package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"sync"
	"testing"
	"time"

	"fridge/internal/container"
	"fridge/internal/pki"
	"fridge/internal/secret"
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
	f.removes = append(f.removes, removeCall{id, volume})
	return nil
}

func newTestApp(t *testing.T) (*App, *fakeRuntime) {
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
	return a, rt
}
