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

// The container's bootstrap user is the node superuser; the app user is created afterwards via AppUserSQL.
func (postgres) Spec(p Params) Spec {
	volume := "/var/lib/postgresql/data"
	// Image 18 moved PGDATA up one level.
	if major, _ := strconv.Atoi(strings.SplitN(p.Version, ".", 2)[0]); major >= 18 {
		volume = "/var/lib/postgresql"
	}
	return Spec{
		Image: "postgres:" + p.Version,
		Env: []string{
			"POSTGRES_USER=" + p.AdminUser,
			"POSTGRES_PASSWORD=" + p.AdminPassword,
			"POSTGRES_DB=" + p.Name,
		},
		VolumePath:  volume,
		Port:        5432,
		Healthcheck: []string{"CMD-SHELL", fmt.Sprintf("pg_isready -h 127.0.0.1 -U %s -d %s", p.AdminUser, p.Name)},
	}
}

// Exec inside the container needs no password: the image's local socket is trust.
func (postgres) DumpCmd(p Params) []string {
	return []string{"pg_dump", "-U", p.AdminUser, "-d", p.Name, "-Fc"}
}

func (postgres) DumpExt() string { return ".dump" }

// Restores run as the superuser; objects land owned by the app role (--role / SET ROLE).
func (postgres) RestoreCmd(p Params, key string) ([]string, bool, bool, error) {
	switch {
	case strings.HasSuffix(key, ".dump"):
		return []string{"pg_restore", "-U", p.AdminUser, "-d", p.Name,
			"--no-owner", "--no-acl", "--exit-on-error", "--role=" + p.User}, false, false, nil
	case strings.HasSuffix(key, ".sql"):
		return postgres{}.AdminCmd(p), false, true, nil
	case strings.HasSuffix(key, ".sql.gz"):
		return postgres{}.AdminCmd(p), true, true, nil
	}
	return nil, false, false, fmt.Errorf("unsupported restore file %q (want .dump, .sql or .sql.gz)", key)
}

func (postgres) AdminCmd(p Params) []string {
	return []string{"psql", "-U", p.AdminUser, "-d", p.Name, "-v", "ON_ERROR_STOP=1", "-q"}
}

func (postgres) ConnString(p Params, domain string) string {
	return fmt.Sprintf("postgresql://%s@%s:5432/%s?sslmode=require",
		url.UserPassword(p.User, p.Password), domain, p.Name)
}

func (postgres) AppUserSQL(user, password, db string) string {
	u := QuoteIdent(user)
	return fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD %s NOSUPERUSER NOCREATEDB NOCREATEROLE;\nALTER DATABASE %s OWNER TO %s;\nALTER SCHEMA public OWNER TO %s;\n",
		u, quoteLit(password), QuoteIdent(db), u, u)
}

func (postgres) SetPasswordSQL(user, password string) string {
	return fmt.Sprintf("ALTER ROLE %s PASSWORD %s;", QuoteIdent(user), quoteLit(password))
}

// QuoteIdent double-quotes a SQL identifier.
func QuoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// quoteLit single-quotes a SQL literal (standard_conforming_strings, the default since 9.1).
func quoteLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
