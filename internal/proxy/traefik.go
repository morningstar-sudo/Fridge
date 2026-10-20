// Package proxy writes Traefik file-provider config: one dynamic YAML per DB
// (TCP router by TLS SNI) plus a base file with the IP allowlists and API router.
package proxy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Route is one database's TCP route. Backend is "host:port".
type Route struct {
	ID                                             int64
	EntryPoint, Domain, Backend, CertFile, KeyFile string
}

// WriteRoute writes <dir>/dynamic/db-<id>.yml.
func WriteRoute(dir string, r Route) error {
	name := fmt.Sprintf("db-%d", r.ID)
	doc := map[string]any{
		"tcp": map[string]any{
			"routers": map[string]any{
				name: map[string]any{
					"entryPoints": []string{r.EntryPoint},
					"rule":        fmt.Sprintf("HostSNI(`%s`)", r.Domain),
					"service":     name,
					"middlewares": []string{"allowlist-tcp@file"},
					"tls":         map[string]any{},
				},
			},
			"services": map[string]any{
				name: map[string]any{
					"loadBalancer": map[string]any{
						"servers": []any{map[string]any{"address": r.Backend}},
					},
				},
			},
		},
		"tls": map[string]any{
			"certificates": []any{map[string]any{"certFile": r.CertFile, "keyFile": r.KeyFile}},
		},
	}
	return writeYAML(filepath.Join(dir, "dynamic", name+".yml"), doc)
}

// RemoveRoute deletes the route file; a missing file is not an error.
func RemoveRoute(dir string, id int64) error {
	return removeIfExists(filepath.Join(dir, "dynamic", fmt.Sprintf("db-%d.yml", id)))
}

// WriteCert writes the PEM pair to <dir>/certs and returns their absolute paths.
// The key is written 0600, the cert 0644.
func WriteCert(dir string, id int64, certPEM, keyPEM []byte) (certFile, keyFile string, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	certFile = filepath.Join(abs, "certs", fmt.Sprintf("db-%d.crt", id))
	keyFile = filepath.Join(abs, "certs", fmt.Sprintf("db-%d.key", id))
	if err := writeFileAtomic(certFile, certPEM, 0o644); err != nil {
		return "", "", err
	}
	if err := writeFileAtomic(keyFile, keyPEM, 0o600); err != nil {
		return "", "", err
	}
	return certFile, keyFile, nil
}

// RemoveCert deletes both cert files; missing files are not an error.
func RemoveCert(dir string, id int64) error {
	for _, ext := range []string{".crt", ".key"} {
		if err := removeIfExists(filepath.Join(dir, "certs", fmt.Sprintf("db-%d%s", id, ext))); err != nil {
			return err
		}
	}
	return nil
}

// WriteBase writes <dir>/dynamic/_base.yml: the allowlist middlewares for TCP and
// HTTP (same CIDRs) and the API router/service pointing at apiBackend.
func WriteBase(dir string, cidrs []string, apiBackend string) error {
	allowlist := map[string]any{
		"ipAllowList": map[string]any{"sourceRange": cidrs},
	}
	doc := map[string]any{
		"tcp": map[string]any{
			"middlewares": map[string]any{"allowlist-tcp": allowlist},
		},
		"http": map[string]any{
			"middlewares": map[string]any{"allowlist-http": allowlist},
			"routers": map[string]any{
				"fridge-api": map[string]any{
					"entryPoints": []string{"api"},
					"rule":        "PathPrefix(`/`)",
					"service":     "fridge-api",
					"middlewares": []string{"allowlist-http@file"},
				},
			},
			"services": map[string]any{
				"fridge-api": map[string]any{
					"loadBalancer": map[string]any{
						"servers": []any{map[string]any{"url": apiBackend}},
					},
				},
			},
		},
	}
	return writeYAML(filepath.Join(dir, "dynamic", "_base.yml"), doc)
}

func writeYAML(path string, v any) error {
	b, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b, 0o644)
}

// writeFileAtomic writes to a temp file in the same directory, then renames it
// over path. The temp name ends in .tmp so Traefik never loads it.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
