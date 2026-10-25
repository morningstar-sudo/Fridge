// Package registry reads image metadata from Docker Hub without a Docker daemon.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"
)

type Client struct {
	BaseURL, AuthURL string // default https://registry-1.docker.io and https://auth.docker.io
	HTTP             *http.Client
}

const accept = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func or(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// split turns "postgres:18" into ("library/postgres", "18"); "owner/app" stays as it is, no tag means latest.
func split(ref string) (repo, tag string) {
	repo, tag = ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		repo, tag = ref[:i], ref[i+1:]
	}
	if !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	return repo, tag
}

func (c *Client) get(ctx context.Context, u, token, accept string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("GET %s: %s", req.URL.Path, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

type descriptor struct {
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType"`
	Platform  struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform"`
}

// RemoteEnv returns the value of environment variable key in the config of the image ref (this machine's
// linux architecture): for a postgres image PG_VERSION is the exact server version a pull would bring.
func (c *Client) RemoteEnv(ctx context.Context, ref, key string) (string, error) {
	repo, tag := split(ref)
	var tok struct {
		Token string `json:"token"`
	}
	authURL := or(c.AuthURL, "https://auth.docker.io") + "/token?service=registry.docker.io&scope=" + url.QueryEscape("repository:"+repo+":pull")
	if err := c.get(ctx, authURL, "", "", &tok); err != nil {
		return "", fmt.Errorf("registry token: %w", err)
	}
	base := or(c.BaseURL, "https://registry-1.docker.io") + "/v2/" + repo
	var m struct {
		Manifests []descriptor `json:"manifests"`
		Config    descriptor   `json:"config"`
	}
	if err := c.get(ctx, base+"/manifests/"+url.PathEscape(tag), tok.Token, accept, &m); err != nil {
		return "", err
	}
	if len(m.Manifests) > 0 { // an index: pick this machine's image
		var digest string
		for _, d := range m.Manifests {
			if d.Platform.OS == "linux" && d.Platform.Architecture == runtime.GOARCH {
				digest = d.Digest
				break
			}
		}
		if digest == "" {
			return "", fmt.Errorf("no linux/%s image for %s", runtime.GOARCH, ref)
		}
		m.Manifests, m.Config = nil, descriptor{}
		if err := c.get(ctx, base+"/manifests/"+digest, tok.Token, accept, &m); err != nil {
			return "", err
		}
	}
	if m.Config.Digest == "" {
		return "", fmt.Errorf("manifest of %s has no config", ref)
	}
	var cfg struct {
		Config struct {
			Env []string `json:"Env"`
		} `json:"config"`
	}
	if err := c.get(ctx, base+"/blobs/"+m.Config.Digest, tok.Token, "", &cfg); err != nil {
		return "", err
	}
	for _, e := range cfg.Config.Env {
		if v, ok := strings.CutPrefix(e, key+"="); ok {
			return v, nil
		}
	}
	return "", fmt.Errorf("%s has no %s", ref, key)
}
