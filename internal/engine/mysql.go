package engine

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// mysqlEngine is MySQL and MariaDB: one wire protocol, one set of tools (named differently), a few dialect differences.
// Their clients are routed by user name (internal/mysqlproxy), so there is no sidecar and no SNI.
type mysqlEngine struct{ mariadb bool }

func init() {
	register(mysqlEngine{}, Info{Name: "mysql", Versions: []string{"8.0", "8.4"}, Default: "8.4", EntryPoint: "mysql", PublicPort: 3306, Route: "mysql"})
	register(mysqlEngine{mariadb: true}, Info{Name: "mariadb", Versions: []string{"10.6", "10.11", "11.4", "11.8"}, Default: "11.4", EntryPoint: "mysql", PublicPort: 3306, Route: "mysql"})
}

func (m mysqlEngine) Spec(p Params) Spec {
	s := Spec{VolumePath: "/var/lib/mysql", Port: 3306,
		Healthcheck: []string{"CMD-SHELL", "bash -c 'exec 3<>/dev/tcp/127.0.0.1/3306'"}} // the init server has no network: this is the real one
	if m.mariadb {
		s.Image = "mariadb:" + p.Version
		s.Env = []string{"MARIADB_RANDOM_ROOT_PASSWORD=yes", "MARIADB_DATABASE=" + p.Name}
	} else {
		s.Image = "mysql:" + p.Version
		s.Env = []string{"MYSQL_RANDOM_ROOT_PASSWORD=yes", "MYSQL_ROOT_HOST=localhost", "MYSQL_DATABASE=" + p.Name}
	}
	// the superuser is created by the init script the entrypoint writes from these (root keeps a random password and stays local)
	s.Env = append(s.Env, "FRIDGE_ADMIN_USER="+p.AdminUser, "FRIDGE_ADMIN_PASSWORD="+p.AdminPassword)
	if p.TLS != nil {
		s.Env = append(s.Env, "FRIDGE_TLS_SERVER_CERT="+p.TLS.ServerCert, "FRIDGE_TLS_SERVER_KEY="+p.TLS.ServerKey, "FRIDGE_TLS_CLIENT_CA="+p.TLS.ClientCert)
		s.Command = []string{"sh", "-c", m.entrypoint(p.Version)}
	}
	return s
}

// entrypoint: certificates and the superuser's init script from the environment, then the image's entrypoint with TLS
// required for every network connection and only the node's own client certificate trusted. Users are REQUIRE X509, so a
// connection also has to present that certificate. MySQL 8.4 ships the native password plugin disabled (the router speaks it).
func (m mysqlEngine) entrypoint(version string) string {
	create := "CREATE USER '%s'@'%%' IDENTIFIED WITH mysql_native_password BY '%s' REQUIRE X509;"
	daemon, native := "mysqld", ""
	if m.mariadb {
		create = "CREATE USER '%s'@'%%' IDENTIFIED BY '%s' REQUIRE X509;"
		daemon = "mariadbd"
	} else if majorMinorAtLeast(version, 8, 4) {
		native = " --mysql-native-password=ON"
	}
	return `set -e
d=/var/lib/fridge-tls
mkdir -p "$d"
printf '%s\n' "$FRIDGE_TLS_SERVER_CERT" > "$d/server.crt"
printf '%s\n' "$FRIDGE_TLS_SERVER_KEY" > "$d/server.key"
printf '%s\n' "$FRIDGE_TLS_CLIENT_CA" > "$d/client-ca.crt"
chown -R mysql:mysql "$d"
chmod 600 "$d/server.key"
ls -ldn "$d" "$d"/* >&2; id >&2 # ponytail: temporary, why mysqld got EACCES on the CI runner
q() { printf '%s' "$1" | sed "s/\\\\/\\\\\\\\/g; s/'/''/g"; }
mkdir -p /docker-entrypoint-initdb.d
printf "` + create + `\nGRANT ALL PRIVILEGES ON *.* TO '%s'@'%%' WITH GRANT OPTION;\n" "$(q "$FRIDGE_ADMIN_USER")" "$(q "$FRIDGE_ADMIN_PASSWORD")" "$(q "$FRIDGE_ADMIN_USER")" > /docker-entrypoint-initdb.d/00-fridge.sql
exec docker-entrypoint.sh ` + daemon + ` --ssl-cert="$d/server.crt" --ssl-key="$d/server.key" --ssl-ca="$d/client-ca.crt" --require-secure-transport=ON --skip-name-resolve --character-set-server=utf8mb4` + native
}

func majorMinorAtLeast(version string, major, minor int) bool {
	a, b, _ := strings.Cut(version, ".")
	x, _ := strconv.Atoi(a)
	y, _ := strconv.Atoi(b)
	return x > major || x == major && y >= minor
}

func (mysqlEngine) ClientEnv(p Params) []string { return []string{"MYSQL_PWD=" + p.AdminPassword} }

// conn is the connection flags every client tool takes: host, user and the node's client certificate (the server's name
// is checked against the CA's certificate: the router and jobs connect by the service name it carries).
func (m mysqlEngine) conn(p Params) []string {
	var a []string
	if p.Host != "" {
		a = append(a, "-h", p.Host, "-P", fmt.Sprint(orPort(p.Port, 3306)))
	}
	a = append(a, "-u", p.AdminUser)
	if p.TLSDir != "" {
		if m.mariadb {
			a = append(a, "--ssl", "--ssl-verify-server-cert")
		} else {
			a = append(a, "--ssl-mode=VERIFY_IDENTITY")
		}
		a = append(a, "--ssl-cert="+p.TLSDir+"/client.crt", "--ssl-key="+p.TLSDir+"/client.key", "--ssl-ca="+p.TLSDir+"/ca.crt")
	}
	return a
}

func (m mysqlEngine) tool(mysql, mariadb string) string {
	if m.mariadb {
		return mariadb
	}
	return mysql
}

// DumpCmd: one consistent snapshot with routines, triggers and events; blobs in hex so the file stays text, which is
// what the file checks rely on. The database's own name is left out (the restore picks the target).
func (m mysqlEngine) DumpCmd(p Params) []string {
	a := append([]string{m.tool("mysqldump", "mariadb-dump")}, m.conn(p)...)
	a = append(a, "--single-transaction", "--routines", "--triggers", "--events", "--hex-blob", "--no-tablespaces", "--default-character-set=utf8mb4")
	if !m.mariadb {
		a = append(a, "--set-gtid-purged=OFF")
	}
	return append(a, p.Name)
}
func (mysqlEngine) DumpExt() string { return ".sql.gz" }
func (mysqlEngine) DumpGzip() bool  { return true }

func (m mysqlEngine) RestoreCmd(p Params, key string) ([]string, bool, bool, error) {
	switch {
	case strings.HasSuffix(key, ".sql.gz"):
		return append(append([]string{m.tool("mysql", "mariadb")}, m.conn(p)...), p.Name), true, false, nil
	case strings.HasSuffix(key, ".sql"):
		return append(append([]string{m.tool("mysql", "mariadb")}, m.conn(p)...), p.Name), false, false, nil
	}
	return nil, false, false, fmt.Errorf("unsupported restore file %q (want .sql.gz or .sql)", key)
}

// definer matches DEFINER=`user`@`host` in the forms mysqldump writes (backtick, quote or bare).
var definer = regexp.MustCompile("(?i)\\bDEFINER\\s*=\\s*(`[^`]*`|'[^']*'|\\w+)@(`[^`]*`|'[^']*'|\\w+)")

// RestoreFilter drops the DEFINER clauses of a dump: the users they name do not exist in the new node, and a view or
// routine whose definer is missing cannot be created. Everything else passes untouched, line by line.
func (mysqlEngine) RestoreFilter() func(io.Reader) io.Reader {
	return func(r io.Reader) io.Reader {
		pr, pw := io.Pipe()
		go func() {
			br := bufio.NewReaderSize(r, 1<<20)
			var err error
			for {
				var line []byte
				line, err = br.ReadBytes('\n')
				if len(line) > 0 {
					if bytes.Contains(line, []byte("DEFINER")) || bytes.Contains(line, []byte("definer")) {
						line = definer.ReplaceAll(line, nil)
					}
					if _, werr := pw.Write(line); werr != nil {
						return
					}
				}
				if err != nil {
					break
				}
			}
			if err == io.EOF {
				err = nil
			}
			pw.CloseWithError(err)
		}()
		return pr
	}
}

func (mysqlEngine) Preload(string) (string, int) { return "", 0 }

// AdminCmd reads SQL from stdin and exits non-zero on the first error.
func (m mysqlEngine) AdminCmd(p Params) []string {
	return append(append([]string{m.tool("mysql", "mariadb")}, m.conn(p)...), "--batch")
}
func (mysqlEngine) AdminFailed(string) bool { return false }
func (m mysqlEngine) PingCmd(p Params) []string {
	return append(append([]string{m.tool("mysql", "mariadb")}, m.conn(p)...), "--batch", "-e", "select 1")
}

func (mysqlEngine) AdminDSN(Params, string, int) string { return "" }
func (mysqlEngine) Upstream() string                    { return "none" }

func (mysqlEngine) ConnString(p Params, domain string) string {
	return fmt.Sprintf("mysql://%s@%s:3306/%s?ssl-mode=REQUIRED", urlUser(p.User, p.Password), domain, p.Name)
}

// sqlString quotes a SQL string literal for MySQL/MariaDB (backslash escapes are on by default).
func sqlString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(s) + "'"
}

// backtick quotes an identifier.
func backtick(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

func (m mysqlEngine) auth(password string) string {
	if m.mariadb {
		return "IDENTIFIED BY " + sqlString(password)
	}
	return "IDENTIFIED WITH mysql_native_password BY " + sqlString(password)
}

// AppUserSQL: the app user owns its database and presents a client certificate like everyone else (REQUIRE X509 is
// what the node's TLS makes real: the server only trusts the node's own certificate).
func (m mysqlEngine) AppUserSQL(user, password, db string) string {
	return fmt.Sprintf("CREATE USER %s@'%%' %s REQUIRE X509;\nGRANT ALL PRIVILEGES ON %s.* TO %s@'%%';\n",
		sqlString(user), m.auth(password), backtick(db), sqlString(user))
}

func (m mysqlEngine) SetPasswordSQL(user, password string) string {
	return fmt.Sprintf("ALTER USER %s@'%%' %s;\n", sqlString(user), m.auth(password))
}
