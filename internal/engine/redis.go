package engine

import (
	"fmt"
	"io"
	"net/url"
	"strings"
)

type redis struct{}

func init() {
	register(redis{}, Info{
		Name:       "redis",
		Versions:   []string{"6.2", "7.2", "7.4", "8.0"},
		Default:    "7.4",
		EntryPoint: "redis",
		PublicPort: 6379,
		Route:      "sni",
	})
}

// A Redis node is a whole instance: its "database" is the keyspace. The superuser and the app user are ACL users; the
// ACL file lives in the data volume, so `ACL SAVE` (password rotation, the app user) survives restarts.
func (redis) Spec(p Params) Spec {
	s := Spec{
		Image: "redis:" + p.Version,
		Env: []string{
			"FRIDGE_ADMIN_USER=" + p.AdminUser,
			"FRIDGE_ADMIN_PASSWORD=" + p.AdminPassword,
		},
		VolumePath: "/data",
		Port:       6379,
		// the TLS port answers once redis-server runs; the entrypoint's setup happens before that
		Healthcheck: []string{"CMD-SHELL", "bash -c 'exec 3<>/dev/tcp/127.0.0.1/6379'"},
	}
	if p.TLS != nil {
		s.Env = append(s.Env, "FRIDGE_TLS_SERVER_CERT="+p.TLS.ServerCert, "FRIDGE_TLS_SERVER_KEY="+p.TLS.ServerKey, "FRIDGE_TLS_CLIENT_CA="+p.TLS.ClientCert)
		s.Command = []string{"sh", "-c", redisEntrypoint}
	}
	return s
}

// redisEntrypoint writes the certificates and the configuration from the environment, creates the ACL file on the first
// start only (a later start must keep the rotated passwords), and hands over to the image's own entrypoint. Only the node's
// own client certificate is trusted (tls-ca-cert-file), and the default user is off.
const redisEntrypoint = `set -e
d=/var/lib/fridge-tls
mkdir -p "$d"
printf '%s\n' "$FRIDGE_TLS_SERVER_CERT" > "$d/server.crt"
printf '%s\n' "$FRIDGE_TLS_SERVER_KEY" > "$d/server.key"
printf '%s\n' "$FRIDGE_TLS_CLIENT_CA" > "$d/client-ca.crt"
printf 'port 0\ntls-port 6379\ntls-cert-file %s/server.crt\ntls-key-file %s/server.key\ntls-ca-cert-file %s/client-ca.crt\ntls-auth-clients yes\nprotected-mode no\naclfile /data/users.acl\n' "$d" "$d" "$d" > "$d/redis.conf"
chown -R redis:redis "$d"
chmod 600 "$d/server.key"
mkdir -p /data
if [ ! -f /data/users.acl ]; then
  printf 'user default off\nuser %s on >%s ~* &* +@all\n' "$FRIDGE_ADMIN_USER" "$FRIDGE_ADMIN_PASSWORD" > /data/users.acl
  chown redis:redis /data/users.acl
fi
exec docker-entrypoint.sh redis-server "$d/redis.conf"`

// cli is redis-cli as the node superuser: TLS with the node's client certificate when a job has put it in TLSDir.
func (redis) cli(p Params) []string {
	a := []string{"redis-cli"}
	if p.TLSDir != "" {
		a = append(a, "--tls", "--cacert", p.TLSDir+"/ca.crt", "--cert", p.TLSDir+"/client.crt", "--key", p.TLSDir+"/client.key")
	}
	if p.Host != "" {
		a = append(a, "-h", p.Host, "-p", fmt.Sprint(orPort(p.Port, 6379)))
	}
	return append(a, "--user", p.AdminUser, "--no-auth-warning")
}

func orPort(port, def int) int {
	if port == 0 {
		return def
	}
	return port
}

func (redis) ClientEnv(p Params) []string { return []string{"REDISCLI_AUTH=" + p.AdminPassword} }

func (r redis) DumpCmd(p Params) []string { return append(r.cli(p), "--rdb", "-") }
func (redis) DumpExt() string             { return ".rdb" }
func (redis) DumpGzip() bool              { return false }

// RestoreCmd accepts only an RDB file: it cannot be loaded into a running node, so the file is put into the data
// volume before the node starts (Preload) and there is no restore tool to run.
func (redis) RestoreCmd(p Params, key string) ([]string, bool, bool, error) {
	if strings.HasSuffix(key, ".rdb") {
		return nil, false, false, nil
	}
	return nil, false, false, fmt.Errorf("unsupported restore file %q (want .rdb)", key)
}
func (redis) RestoreFilter() func(io.Reader) io.Reader { return nil }

// Preload: redis loads dump.rdb from its data directory at start; the file belongs to the redis user (uid 999 in the image).
func (redis) Preload(key string) (string, int) {
	if strings.HasSuffix(key, ".rdb") {
		return "dump.rdb", 999
	}
	return "", 0
}

func (r redis) AdminCmd(p Params) []string { return append(r.cli(p), "--no-raw") }
func (redis) AdminFailed(out string) bool  { return strings.Contains(out, "(error)") } // redis-cli exits 0 on a refused command

// PingCmd says PONG only for an authenticated admin: redis-cli also exits 0 after "NOAUTH".
func (r redis) PingCmd(p Params) []string {
	return append([]string{"sh", "-c", `"$@" | grep -q PONG`, "sh"}, append(r.cli(p), "ping")...)
}

func (redis) AdminDSN(Params, string, int) string { return "" }
func (redis) Upstream() string                    { return "tls" }

func (redis) ConnString(p Params, domain string) string {
	return fmt.Sprintf("rediss://%s@%s:6379", url.UserPassword(p.User, p.Password), domain)
}

// redisArg quotes one token for redis-cli's line parser (double quotes, backslash escapes).
func redisArg(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// AppUserSQL is the redis-cli script that creates the app user: everything except what changes the instance itself
// (CONFIG, SHUTDOWN, replication, ACL, DEBUG, modules). The node is the tenant's alone, so its keyspace is theirs to flush.
func (redis) AppUserSQL(user, password, db string) string {
	return fmt.Sprintf("ACL SETUSER %s on %s ~* &* +@all -@admin -debug -module -acl\nACL SAVE\n", redisArg(user), redisArg(">"+password))
}

func (redis) SetPasswordSQL(user, password string) string {
	return fmt.Sprintf("ACL SETUSER %s resetpass %s\nACL SAVE\n", redisArg(user), redisArg(">"+password))
}
