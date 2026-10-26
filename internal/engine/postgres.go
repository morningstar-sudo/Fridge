package engine

import (
	"fmt"
	"net"
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
	s := Spec{
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
	if p.TLS != nil {
		s.Env = append(s.Env, "FRIDGE_TLS_SERVER_CERT="+p.TLS.ServerCert, "FRIDGE_TLS_SERVER_KEY="+p.TLS.ServerKey, "FRIDGE_TLS_CLIENT_CA="+p.TLS.ClientCert)
		s.Command = []string{"sh", "-c", tlsEntrypoint}
	}
	return s
}

// hostArgs are the connection flags of the client tools: none for the local socket, -h/-p for a job.
func hostArgs(p Params) []string {
	if p.Host == "" {
		return nil
	}
	port := p.Port
	if port == 0 {
		port = 5432
	}
	return []string{"-h", p.Host, "-p", strconv.Itoa(port)}
}

// Over TCP (jobs) the password comes in PGPASSWORD; on the local socket the image is trust.
func (postgres) DumpCmd(p Params) []string {
	return append([]string{"pg_dump", "-U", p.AdminUser, "-d", p.Name, "-Fc"}, hostArgs(p)...)
}

func (postgres) DumpExt() string { return ".dump" }

// Restores run as the superuser; objects land owned by the app role (--role / SET ROLE).
func (postgres) RestoreCmd(p Params, key string) ([]string, bool, bool, error) {
	switch {
	case strings.HasSuffix(key, ".dump"):
		return append([]string{"pg_restore", "-U", p.AdminUser, "-d", p.Name,
			"--no-owner", "--no-acl", "--exit-on-error", "--role=" + p.User}, hostArgs(p)...), false, false, nil
	case strings.HasSuffix(key, ".sql"):
		return postgres{}.AdminCmd(p), false, true, nil
	case strings.HasSuffix(key, ".sql.gz"):
		return postgres{}.AdminCmd(p), true, true, nil
	}
	return nil, false, false, fmt.Errorf("unsupported restore file %q (want .dump, .sql or .sql.gz)", key)
}

func (postgres) AdminCmd(p Params) []string {
	return append([]string{"psql", "-U", p.AdminUser, "-d", p.Name, "-v", "ON_ERROR_STOP=1", "-q"}, hostArgs(p)...)
}

// AdminDSN connects as the node superuser; the sidecar speaks plain TCP, TLS is the proxy's job.
func (postgres) AdminDSN(p Params, host string, port int) string {
	u := url.URL{Scheme: "postgres", User: url.UserPassword(p.AdminUser, p.AdminPassword), Host: net.JoinHostPort(host, strconv.Itoa(port)),
		Path: "/" + p.Name, RawQuery: "sslmode=disable&connect_timeout=5"}
	return u.String()
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

// tlsEntrypoint starts the stock entrypoint with TLS required and a pg_hba.conf that lets in only a client presenting
// the node's own client certificate (the one certificate in ssl_ca_file) AND the right password. The files are written
// from the environment on every start, so nothing sensitive sits in the image or the volume. Local (socket) access stays
// trusted for the entrypoint's own init scripts and the healthcheck.
const tlsEntrypoint = `set -e
d=/var/lib/fridge-tls
mkdir -p "$d"
printf '%s\n' "$FRIDGE_TLS_SERVER_CERT" > "$d/server.crt"
printf '%s\n' "$FRIDGE_TLS_SERVER_KEY" > "$d/server.key"
printf '%s\n' "$FRIDGE_TLS_CLIENT_CA" > "$d/client-ca.crt"
printf 'local all all trust\nhostssl all all all scram-sha-256 clientcert=verify-ca\nhost all all all reject\n' > "$d/pg_hba.conf"
chown -R postgres:postgres "$d"
chmod 600 "$d/server.key"
exec docker-entrypoint.sh postgres -c ssl=on -c ssl_cert_file="$d/server.crt" -c ssl_key_file="$d/server.key" -c ssl_ca_file="$d/client-ca.crt" -c hba_file="$d/pg_hba.conf" -c password_encryption=scram-sha-256`
