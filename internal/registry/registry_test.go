package registry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

const accepted = "application/vnd.oci.image.index.v1+json"

// fake is a Docker Hub stand-in: a token endpoint, an index with two platforms, one manifest, one config blob.
func fake(t *testing.T, env []string) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("scope") != "repository:library/postgres:pull" || r.URL.Query().Get("service") != "registry.docker.io" {
			http.Error(w, "bad scope", 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"token": "tok"})
	})
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, "unauthorized", 401)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/v2/library/postgres/manifests/18", authed(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), accepted) {
			http.Error(w, "accept", 406)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"manifests": []map[string]any{
			{"digest": "sha256:other", "platform": map[string]string{"os": "linux", "architecture": "s390x"}},
			{"digest": "sha256:mine", "platform": map[string]string{"os": "linux", "architecture": runtime.GOARCH}},
		}})
	}))
	mux.HandleFunc("/v2/library/postgres/manifests/sha256:mine", authed(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"config": map[string]string{"digest": "sha256:cfg"}})
	}))
	mux.HandleFunc("/v2/library/postgres/blobs/sha256:cfg", authed(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"config": map[string]any{"Env": env}})
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, AuthURL: srv.URL}
}

func TestRegistryRemoteEnv(t *testing.T) {
	c := fake(t, []string{"PATH=/usr/bin", "PG_VERSION=18.2"})
	v, err := c.RemoteEnv(t.Context(), "postgres:18", "PG_VERSION")
	if err != nil || v != "18.2" {
		t.Fatalf("%q %v", v, err)
	}
	if _, err := c.RemoteEnv(t.Context(), "postgres:18", "NOPE"); err == nil {
		t.Fatal("missing variable accepted")
	}
	if _, err := c.RemoteEnv(t.Context(), "postgres:99", "PG_VERSION"); err == nil {
		t.Fatal("unknown tag accepted")
	}
	// no token, no manifest: the registry answers 401
	noTok := &Client{BaseURL: c.BaseURL, AuthURL: "http://127.0.0.1:1"}
	if _, err := noTok.RemoteEnv(t.Context(), "postgres:18", "PG_VERSION"); err == nil {
		t.Fatal("token failure ignored")
	}
}

func TestRegistrySplit(t *testing.T) {
	for in, want := range map[string][2]string{"postgres:18": {"library/postgres", "18"}, "postgres": {"library/postgres", "latest"},
		"bitnami/redis:7": {"bitnami/redis", "7"}, "localhost:5000/x": {"localhost:5000/x", "latest"}} {
		if r, tag := split(in); r != want[0] || tag != want[1] {
			t.Errorf("split(%q) = %q, %q", in, r, tag)
		}
	}
}
