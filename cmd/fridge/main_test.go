package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestServerRequiresEnv(t *testing.T) {
	for _, k := range []string{"FRIDGE_DATABASE_URL", "FRIDGE_SECRET_KEY", "FRIDGE_SERVER_IMAGE", "FRIDGE_JOB_IMAGE"} {
		t.Setenv(k, "")
	}
	err := runServer()
	if err == nil || !strings.Contains(err.Error(), "FRIDGE_DATABASE_URL, FRIDGE_SECRET_KEY") {
		t.Fatalf("got %v", err)
	}
	t.Setenv("FRIDGE_DATABASE_URL", "postgres://x")
	t.Setenv("FRIDGE_SERVER_IMAGE", "fridge:test")
	t.Setenv("FRIDGE_JOB_IMAGE", "fridge-job:test")
	t.Setenv("FRIDGE_SECRET_KEY", "short")
	if err := runServer(); err == nil || !strings.Contains(err.Error(), "FRIDGE_SECRET_KEY") {
		t.Fatalf("bad key: got %v", err)
	}
}

type fakeSubnets struct {
	ps  []netip.Prefix
	err error
}

func (f fakeSubnets) SubnetsOf(context.Context, ...string) ([]netip.Prefix, error) {
	return f.ps, f.err
}

func TestTrustedProxies(t *testing.T) {
	ctx := context.Background()
	net1 := netip.MustParsePrefix("172.30.0.0/16")
	t.Setenv("FRIDGE_TRUSTED_PROXY", "")
	if p, err := trustedProxies(ctx, fakeSubnets{ps: []netip.Prefix{net1}}, "n"); err != nil || len(p) != 1 || p[0] != net1 {
		t.Fatal(p, err)
	}
	if p, err := trustedProxies(ctx, fakeSubnets{err: errors.New("docker down")}, "n"); err != nil || p != nil {
		t.Fatal("docker error must fall back to loopback only:", p, err)
	}
	t.Setenv("FRIDGE_TRUSTED_PROXY", "10.0.0.0/8, 192.168.0.0/16")
	if p, err := trustedProxies(ctx, fakeSubnets{ps: []netip.Prefix{net1}}, "n"); err != nil || len(p) != 2 {
		t.Fatal(p, err)
	}
	t.Setenv("FRIDGE_TRUSTED_PROXY", "bad")
	if _, err := trustedProxies(ctx, fakeSubnets{}, "n"); err == nil {
		t.Fatal("bad CIDR accepted")
	}
}

func TestHealthcheck(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	t.Setenv("FRIDGE_LISTEN", ":"+u.Port())
	if got := healthcheck(); got != 0 {
		t.Fatalf("ready: exit %d", got)
	}
	status.Store(http.StatusServiceUnavailable)
	if got := healthcheck(); got != 1 {
		t.Fatalf("not ready: exit %d", got)
	}
}
