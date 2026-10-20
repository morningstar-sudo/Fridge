package engine

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type postgres struct{}

func init() {
	register(postgres{}, Info{
		Name:       "postgres",
		Versions:   []string{"13", "14", "15", "16", "17", "18"},
		Default:    "17",
		EntryPoint: "pg",
		PublicPort: 5432,
	})
}

func (postgres) Spec(p Params) Spec {
	volume := "/var/lib/postgresql/data"
	// Image 18 moved PGDATA up one level.
	if major, _ := strconv.Atoi(strings.SplitN(p.Version, ".", 2)[0]); major >= 18 {
		volume = "/var/lib/postgresql"
	}
	return Spec{
		Image: "postgres:" + p.Version,
		Env: []string{
			"POSTGRES_USER=" + p.User,
			"POSTGRES_PASSWORD=" + p.Password,
			"POSTGRES_DB=" + p.Name,
		},
		VolumePath:  volume,
		Port:        5432,
		Healthcheck: []string{"CMD-SHELL", fmt.Sprintf("pg_isready -h 127.0.0.1 -U %s -d %s", p.User, p.Name)},
	}
}

// Exec inside the container needs no password: the image's local socket is trust.
func (postgres) DumpCmd(p Params) []string {
	return []string{"pg_dump", "-U", p.User, "-d", p.Name, "-Fc"}
}

func (postgres) DumpExt() string { return ".dump" }

func (postgres) RestoreCmd(p Params, key string) ([]string, bool, error) {
	switch {
	case strings.HasSuffix(key, ".dump"):
		return []string{"pg_restore", "-U", p.User, "-d", p.Name,
			"--no-owner", "--no-acl", "--exit-on-error"}, false, nil
	case strings.HasSuffix(key, ".sql"):
		return psql(p), false, nil
	case strings.HasSuffix(key, ".sql.gz"):
		return psql(p), true, nil
	}
	return nil, false, fmt.Errorf("unsupported restore file %q (want .dump, .sql or .sql.gz)", key)
}

func psql(p Params) []string {
	return []string{"psql", "-U", p.User, "-d", p.Name, "-v", "ON_ERROR_STOP=1", "-q"}
}

func (postgres) ConnString(p Params, domain string) string {
	return fmt.Sprintf("postgresql://%s@%s:5432/%s?sslmode=require",
		url.UserPassword(p.User, p.Password), domain, p.Name)
}
