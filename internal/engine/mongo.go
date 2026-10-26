package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
)

type mongo struct{}

func init() {
	register(mongo{}, Info{
		Name:       "mongo",
		Versions:   []string{"6.0", "7.0", "8.0"},
		Default:    "7.0",
		EntryPoint: "mongo",
		PublicPort: 27017,
		Route:      "sni",
	})
}

// The superuser is the image's root user (MONGO_INITDB_ROOT_*, in the admin database); the app user is created in the
// node's own database with readWrite and dbAdmin on it.
func (mongo) Spec(p Params) Spec {
	s := Spec{
		Image: "mongo:" + p.Version,
		Env: []string{
			"MONGO_INITDB_ROOT_USERNAME=" + p.AdminUser,
			"MONGO_INITDB_ROOT_PASSWORD=" + p.AdminPassword,
		},
		VolumePath: "/data/db",
		Port:       27017,
		// the entrypoint's init server also listens on localhost; readiness of the real one is the admin job's ping
		Healthcheck: []string{"CMD-SHELL", "bash -c 'exec 3<>/dev/tcp/127.0.0.1/27017'"},
	}
	if p.TLS != nil {
		s.Env = append(s.Env, "FRIDGE_TLS_SERVER_CERT="+p.TLS.ServerCert, "FRIDGE_TLS_SERVER_KEY="+p.TLS.ServerKey, "FRIDGE_TLS_CLIENT_CA="+p.TLS.ClientCert)
		s.Command = []string{"sh", "-c", mongoEntrypoint}
	}
	return s
}

// mongoEntrypoint: TLS is required and the only trusted client certificate is the node's own (tlsCAFile), then
// authentication on top. The certificate files are written from the environment on every start.
const mongoEntrypoint = `set -e
d=/var/lib/fridge-tls
mkdir -p "$d"
printf '%s\n' "$FRIDGE_TLS_SERVER_CERT" > "$d/server.pem"
printf '%s\n' "$FRIDGE_TLS_SERVER_KEY" >> "$d/server.pem"
printf '%s\n' "$FRIDGE_TLS_CLIENT_CA" > "$d/client-ca.crt"
chown -R mongodb:mongodb "$d"
chmod 600 "$d/server.pem"
exec docker-entrypoint.sh mongod --tlsMode requireTLS --tlsCertificateKeyFile "$d/server.pem" --tlsCAFile "$d/client-ca.crt" --auth --bind_ip_all`

func (mongo) ClientEnv(Params) []string { return nil }

// uri is a connection URI of the node superuser (authSource admin) with the node's client certificate. Passwords sit in
// argv of a tool inside a job container that nobody else can see into.
func (mongo) uri(p Params) string {
	host := p.Host
	if host == "" {
		host = "localhost"
	}
	q := url.Values{"authSource": {"admin"}}
	if p.TLSDir != "" {
		q.Set("tls", "true")
		q.Set("tlsCertificateKeyFile", p.TLSDir+"/client.pem")
		q.Set("tlsCAFile", p.TLSDir+"/ca.crt")
	}
	u := url.URL{Scheme: "mongodb", User: url.UserPassword(p.AdminUser, p.AdminPassword), Host: fmt.Sprintf("%s:%d", host, orPort(p.Port, 27017)), Path: "/", RawQuery: q.Encode()}
	return u.String()
}

func (m mongo) DumpCmd(p Params) []string {
	return []string{"mongodump", "--uri", m.uri(p), "--db", p.Name, "--archive", "--gzip"}
}
func (mongo) DumpExt() string { return ".archive.gz" }
func (mongo) DumpGzip() bool  { return false } // mongodump compresses the archive itself

// RestoreCmd maps the source database (its name came with the backup) onto the node's own, whatever it was called when dumped.
func (m mongo) RestoreCmd(p Params, key string) ([]string, bool, bool, error) {
	if !strings.HasSuffix(key, ".archive.gz") {
		return nil, false, false, fmt.Errorf("unsupported restore file %q (want .archive.gz)", key)
	}
	argv := []string{"mongorestore", "--uri", m.uri(p), "--archive", "--gzip"}
	if p.SourceDB != "" {
		argv = append(argv, "--nsFrom", p.SourceDB+".$collection$", "--nsTo", p.Name+".$collection$")
	}
	return argv, false, false, nil
}
func (mongo) RestoreFilter() func(io.Reader) io.Reader { return nil }
func (mongo) Preload(string) (string, int)             { return "", 0 }

func (m mongo) shell(p Params) []string {
	a := []string{"mongosh", "--quiet"}
	if p.TLSDir != "" {
		a = append(a, "--tls", "--tlsCertificateKeyFile", p.TLSDir+"/client.pem", "--tlsCAFile", p.TLSDir+"/ca.crt")
	}
	host := p.Host
	if host == "" {
		host = "localhost"
	}
	return append(a, "--host", host, "--port", fmt.Sprint(orPort(p.Port, 27017)), "-u", p.AdminUser, "-p", p.AdminPassword, "--authenticationDatabase", "admin")
}

// AdminCmd reads a script from stdin; a script that throws makes mongosh exit non-zero.
func (m mongo) AdminCmd(p Params) []string { return m.shell(p) }
func (mongo) AdminFailed(string) bool      { return false }
func (m mongo) PingCmd(p Params) []string {
	return append(m.shell(p), "--eval", "db.runCommand({ping:1}).ok")
}

func (mongo) AdminDSN(Params, string, int) string { return "" }
func (mongo) Upstream() string                    { return "tls" }

func (mongo) ConnString(p Params, domain string) string {
	return fmt.Sprintf("mongodb://%s@%s:27017/%s?tls=true", url.UserPassword(p.User, p.Password), domain, p.Name)
}

func jsString(s string) string { b, _ := json.Marshal(s); return string(b) }

func (mongo) AppUserSQL(user, password, db string) string {
	return fmt.Sprintf(`db.getSiblingDB(%s).createUser({user: %s, pwd: %s, roles: [{role: "readWrite", db: %s}, {role: "dbAdmin", db: %s}]})
`, jsString(db), jsString(user), jsString(password), jsString(db), jsString(db))
}

func (mongo) SetPasswordSQL(user, password string) string {
	return fmt.Sprintf("db.getSiblingDB(\"admin\").changeUserPassword(%s, %s)\n", jsString(user), jsString(password))
}
