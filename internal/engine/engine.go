// Package engine holds the database engine catalog. Phase 1 registers only postgres.
package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Params identifies one database instance for a given engine.
// AdminUser is the node superuser; legacy single-user nodes pass AdminUser = User.
type Params struct {
	Name, User, Password, Version, AdminUser, AdminPassword string
	Host                                                    string // when set the client tools connect over TCP to Host:Port (jobs); otherwise over the local socket
	Port                                                    int
}

// Spec describes the container an engine runs.
type Spec struct {
	Image       string
	Env         []string
	VolumePath  string
	Port        int
	Healthcheck []string
}

// Engine builds container specs, backup/restore commands and connection strings.
type Engine interface {
	Spec(p Params) Spec
	DumpCmd(p Params) []string
	DumpExt() string
	// setRole: the caller prefixes stdin with SET ROLE <User> so objects belong to the app user.
	RestoreCmd(p Params, key string) (cmd []string, gunzip, setRole bool, err error)
	ConnString(p Params, domain string) string // app user
	AppUserSQL(user, password, db string) string
	SetPasswordSQL(user, password string) string
	AdminCmd(p Params) []string // superuser SQL shell; SQL (and passwords) go on stdin, never argv
	// AdminDSN is a connection URL for running SQL as the node superuser at host:port (the edge sidecar).
	AdminDSN(p Params, host string, port int) string
}

// Info is the catalog entry for an engine.
type Info struct {
	Name       string
	Versions   []string
	Default    string
	EntryPoint string
	PublicPort int
}

type entry struct {
	eng  Engine
	info Info
}

var registry = map[string]entry{}

func register(e Engine, info Info) {
	registry[info.Name] = entry{eng: e, info: info}
}

// Get returns the engine registered under name, or an error if it is not registered.
func Get(name string) (Engine, Info, error) {
	e, ok := registry[name]
	if !ok {
		return nil, Info{}, fmt.Errorf("engine %q is not registered", name)
	}
	return e.eng, e.info, nil
}

// List returns all registered engines, sorted by name.
func List() []Info {
	out := make([]Info, 0, len(registry))
	for _, e := range registry {
		out = append(out, e.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ResolveVersion maps "" to the default version and rejects versions outside the catalog.
func ResolveVersion(name, version string) (string, error) {
	_, info, err := Get(name)
	if err != nil {
		return "", err
	}
	if version == "" {
		return info.Default, nil
	}
	for _, v := range info.Versions {
		if v == version {
			return v, nil
		}
	}
	return "", fmt.Errorf("version %q is not supported for %s (supported: %s)",
		version, name, strings.Join(info.Versions, ", "))
}

// CompareVersions compares dot-separated numeric versions component by component,
// over the common prefix only: returns -1, 0 or 1.
// ponytail: numeric components only; non-numeric parts count as 0.
func CompareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, _ := strconv.Atoi(as[i])
		y, _ := strconv.Atoi(bs[i])
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
