package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestServerRequiresEnv(t *testing.T) {
	for _, k := range []string{"FRIDGE_DATABASE_URL", "FRIDGE_SECRET_KEY", "FRIDGE_TOKEN"} {
		t.Setenv(k, "")
	}
	t.Setenv("FRIDGE_TOKEN", "x")
	err := runServer()
	if err == nil || !strings.Contains(err.Error(), "FRIDGE_DATABASE_URL, FRIDGE_SECRET_KEY") || strings.Contains(err.Error(), "FRIDGE_TOKEN") {
		t.Fatalf("got %v", err)
	}
	t.Setenv("FRIDGE_DATABASE_URL", "postgres://x")
	t.Setenv("FRIDGE_SECRET_KEY", "short")
	if err := runServer(); err == nil || !strings.Contains(err.Error(), "FRIDGE_SECRET_KEY") {
		t.Fatalf("bad key: got %v", err)
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
