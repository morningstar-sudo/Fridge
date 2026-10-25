package objstore

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fridge/internal/store"
)

func TestGuardCheckIP(t *testing.T) {
	g := &Guard{Blocked: []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16"), netip.MustParsePrefix("172.19.5.0/24")}}
	for _, s := range []string{"127.0.0.1", "::1", "169.254.169.254", "fe80::1", "0.0.0.0", "::", "172.18.0.5", "172.19.5.9", "::ffff:127.0.0.1", "ff02::1", "::ffff:172.18.0.9"} {
		if err := g.CheckIP(netip.MustParseAddr(s)); !errors.Is(err, ErrEndpointBlocked) {
			t.Errorf("%s: %v", s, err)
		}
	}
	for _, s := range []string{"192.168.1.50", "10.1.2.3", "172.19.6.1", "8.8.8.8", "2001:db8::1"} {
		if err := g.CheckIP(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
}

func TestGuardCheckHostNames(t *testing.T) {
	ctx := context.Background()
	g := &Guard{}
	for _, h := range []string{"fridge-meta", "TRAEFIK", "fridge", "fridge-traefik", "localhost", "127.0.0.1", "::1", "169.254.169.254"} {
		if err := g.CheckHost(ctx, h); !errors.Is(err, ErrEndpointBlocked) {
			t.Errorf("%s: %v", h, err)
		}
	}
	if err := (&Guard{Allow: []string{"LocalHost"}}).CheckHost(ctx, "localhost"); err != nil {
		t.Errorf("allowed host: %v", err)
	}
	if err := g.CheckHost(ctx, "192.168.1.50"); err != nil {
		t.Errorf("private literal: %v", err)
	}
}

func TestDialGuardBlocksAtConnect(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	// the name passed any creation-time check (it is not a literal loopback); the connect still lands on 127.0.0.1
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	c := &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{Control: (&Guard{}).DialControl}).DialContext}, Timeout: 5 * time.Second}
	_, err := c.Get("http://localhost:" + port)
	if !errors.Is(err, ErrEndpointBlocked) || hits.Load() != 0 {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
	c = &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{Control: (&Guard{}).DialControl}).DialContext}, Timeout: 5 * time.Second}
	if _, err := c.Get(srv.URL); !errors.Is(err, ErrEndpointBlocked) {
		t.Fatal(err)
	}
}

func TestObjstoreNewWithGuard(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	d := store.Destination{Endpoint: srv.URL, Bucket: "bkt", AccessKey: "ak", PathStyle: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s, err := New(d, "sk", &Guard{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Test(ctx); err == nil || !strings.Contains(err.Error(), "endpoint không được phép") || hits.Load() != 0 {
		t.Fatalf("guarded: err=%v hits=%d", err, hits.Load())
	}
	// an allowlisted host skips the guard (no Control on the dialer)
	s, err = New(d, "sk", &Guard{Allow: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	s.Test(ctx)
	if hits.Load() == 0 {
		t.Fatal("allowlisted endpoint was not reached")
	}
	hits.Store(0)
	s, _ = New(d, "sk", nil)
	s.Test(ctx)
	if hits.Load() == 0 {
		t.Fatal("unguarded endpoint was not reached")
	}
}
