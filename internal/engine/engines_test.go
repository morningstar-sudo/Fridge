package engine

import (
	"io"
	"strings"
	"testing"
)

var tlsParams = Params{Name: "alice_shop", User: "alice_app", Password: "app'pw", Version: "7.4", AdminUser: "su_0123456789abcdef", AdminPassword: "adm1n-pw",
	TLS: &TLS{ServerCert: "SC", ServerKey: "SK", ClientCert: "CC", ClientKey: "CK"}}

func TestCatalog(t *testing.T) {
	want := map[string]struct {
		def, route, upstream string
		port                 int
	}{
		"postgres": {"17", "sni", "pg", 5432}, "redis": {"7.4", "sni", "tls", 6379}, "mongo": {"7.0", "sni", "tls", 27017},
		"mysql": {"8.4", "mysql", "none", 3306}, "mariadb": {"11.4", "mysql", "none", 3306},
	}
	if got := List(); len(got) != len(want) {
		t.Fatalf("%d engines registered, want %d", len(got), len(want))
	}
	for name, w := range want {
		e, info, err := Get(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Default != w.def || info.Route != w.route || info.PublicPort != w.port || e.Upstream() != w.upstream {
			t.Errorf("%s: %+v upstream %s", name, info, e.Upstream())
		}
		if v, err := ResolveVersion(name, ""); err != nil || v != w.def {
			t.Errorf("%s default version: %q %v", name, v, err)
		}
	}
}

// Every engine's database container learns the server key and the one client certificate it trusts, never the client's key.
func TestEveryNodeTrustsOnlyItsOwnClientCertificate(t *testing.T) {
	for _, name := range []string{"postgres", "redis", "mongo", "mysql", "mariadb"} {
		e, info, _ := Get(name)
		p := tlsParams
		p.Version = info.Default
		s := e.Spec(p)
		env := strings.Join(s.Env, "\n")
		for _, want := range []string{"FRIDGE_TLS_SERVER_CERT=SC", "FRIDGE_TLS_SERVER_KEY=SK", "FRIDGE_TLS_CLIENT_CA=CC"} {
			if !strings.Contains(env, want) {
				t.Errorf("%s: env lacks %s", name, want)
			}
		}
		if strings.Contains(env, "CK") || strings.Contains(strings.Join(s.Command, " "), "CK") {
			t.Errorf("%s: the client's private key reached the database container", name)
		}
		if len(s.Command) != 3 || s.Command[0] != "sh" || s.Command[1] != "-c" || s.Port != info.PublicPort || s.Image == "" || s.VolumePath == "" || len(s.Healthcheck) != 2 {
			t.Errorf("%s: spec %+v", name, s)
		}
		if plain := e.Spec(Params{Name: "x", AdminUser: "su", Version: info.Default}); plain.Command != nil {
			t.Errorf("%s: a node without TLS keeps the image's command", name)
		}
		if e.AdminDSN(Params{}, "h", 1) != "" && name != "postgres" {
			t.Errorf("%s has a pgx DSN", name)
		}
	}
}

func TestRedis(t *testing.T) {
	e, _, _ := Get("redis")
	p := tlsParams
	p.Host, p.Port, p.TLSDir = "fridge-db-k1", 6379, "/tls"
	s := e.Spec(p)
	if s.Image != "redis:7.4" || s.VolumePath != "/data" {
		t.Fatalf("%+v", s)
	}
	for _, want := range []string{"tls-auth-clients yes", "tls-ca-cert-file", "port 0", "aclfile /data/users.acl", "user default off", "[ ! -f /data/users.acl ]"} {
		if !strings.Contains(s.Command[2], want) {
			t.Errorf("entrypoint lacks %q", want)
		}
	}
	if got := strings.Join(e.DumpCmd(p), " "); got != "redis-cli --tls --cacert /tls/ca.crt --cert /tls/client.crt --key /tls/client.key -h fridge-db-k1 -p 6379 --user su_0123456789abcdef --no-auth-warning --rdb -" {
		t.Fatalf("dump = %q", got)
	}
	if env := e.ClientEnv(p); len(env) != 1 || env[0] != "REDISCLI_AUTH=adm1n-pw" {
		t.Fatalf("env = %v", env)
	}
	if argv, gz, role, err := e.RestoreCmd(p, "x.rdb"); err != nil || argv != nil || gz || role {
		t.Fatalf("restore: %v %v %v %v", argv, gz, role, err)
	}
	if _, _, _, err := e.RestoreCmd(p, "x.sql"); err == nil {
		t.Fatal("a SQL file was accepted")
	}
	if rel, uid := e.Preload("a/b.rdb"); rel != "dump.rdb" || uid != 999 {
		t.Fatalf("preload = %q %d", rel, uid)
	}
	if rel, _ := e.Preload("a/b.dump"); rel != "" {
		t.Fatal("preload of another file type")
	}
	if got := e.AppUserSQL("alice_app", `p"w\x`, "ignored"); !strings.Contains(got, `ACL SETUSER "alice_app" on ">p\"w\\x" ~* &* +@all -@admin -debug -module -acl`) || !strings.HasSuffix(got, "ACL SAVE\n") {
		t.Fatalf("app user = %q", got)
	}
	if got := e.SetPasswordSQL("su_x", "n3w"); got != "ACL SETUSER \"su_x\" resetpass \">n3w\"\nACL SAVE\n" {
		t.Fatalf("rotate = %q", got)
	}
	if !e.AdminFailed("(error) ERR bad") || e.AdminFailed("OK\n") {
		t.Fatal("AdminFailed")
	}
	if got := e.ConnString(Params{User: "u", Password: "p@ss", Name: "n"}, "r.lan"); got != "rediss://u:p%40ss@r.lan:6379" {
		t.Fatalf("conn = %q", got)
	}
	if got := strings.Join(e.PingCmd(p), " "); !strings.HasPrefix(got, `sh -c "$@" | grep -q PONG sh redis-cli --tls`) || !strings.HasSuffix(got, "ping") {
		t.Fatalf("ping = %q", got)
	}
}

func TestMongo(t *testing.T) {
	e, _, _ := Get("mongo")
	p := tlsParams
	p.Version, p.Host, p.Port, p.TLSDir = "7.0", "fridge-db-k1", 27017, "/tls"
	s := e.Spec(p)
	if s.Image != "mongo:7.0" || s.VolumePath != "/data/db" || !strings.Contains(strings.Join(s.Env, " "), "MONGO_INITDB_ROOT_USERNAME=su_0123456789abcdef") {
		t.Fatalf("%+v", s)
	}
	for _, want := range []string{"--tlsMode requireTLS", "--tlsCAFile", "--auth", "--tlsCertificateKeyFile"} {
		if !strings.Contains(s.Command[2], want) {
			t.Errorf("entrypoint lacks %q", want)
		}
	}
	dump := strings.Join(e.DumpCmd(p), " ")
	for _, want := range []string{"mongodump --uri mongodb://su_0123456789abcdef:adm1n-pw@fridge-db-k1:27017/?", "authSource=admin", "tls=true", "tlsCertificateKeyFile=%2Ftls%2Fclient.pem", "tlsCAFile=%2Ftls%2Fca.crt", "--db alice_shop --archive --gzip"} {
		if !strings.Contains(dump, want) {
			t.Errorf("dump lacks %q: %s", want, dump)
		}
	}
	p.SourceDB = "bob_old"
	argv, gz, role, err := e.RestoreCmd(p, "k.archive.gz")
	if j := strings.Join(argv, " "); err != nil || gz || role || !strings.Contains(j, "--nsFrom bob_old.$collection$ --nsTo alice_shop.$collection$") || argv[0] != "mongorestore" {
		t.Fatalf("restore: %q %v %v %v", argv, gz, role, err)
	}
	p.SourceDB = ""
	if argv, _, _, _ := e.RestoreCmd(p, "k.archive.gz"); strings.Contains(strings.Join(argv, " "), "nsFrom") {
		t.Fatal("namespace mapping without a source database")
	}
	if _, _, _, err := e.RestoreCmd(p, "k.dump"); err == nil {
		t.Fatal("a pg_dump file was accepted")
	}
	if j := strings.Join(e.AdminCmd(p), " "); !strings.Contains(j, "mongosh --quiet --tls --tlsCertificateKeyFile /tls/client.pem --tlsCAFile /tls/ca.crt --host fridge-db-k1 --port 27017 -u su_0123456789abcdef -p adm1n-pw --authenticationDatabase admin") {
		t.Fatalf("admin = %q", j)
	}
	js := e.AppUserSQL("alice_app", `p"w`, "alice_shop")
	if !strings.Contains(js, `db.getSiblingDB("alice_shop").createUser({user: "alice_app", pwd: "p\"w", roles: [{role: "readWrite", db: "alice_shop"}, {role: "dbAdmin", db: "alice_shop"}]})`) {
		t.Fatalf("app user = %q", js)
	}
	if got := e.SetPasswordSQL("su_x", "n3w"); !strings.Contains(got, `getSiblingDB("admin").changeUserPassword("su_x", "n3w")`) {
		t.Fatalf("rotate = %q", got)
	}
	if got := e.ConnString(Params{User: "u", Password: "p/q", Name: "alice_shop"}, "m.lan"); got != "mongodb://u:p%2Fq@m.lan:27017/alice_shop?tls=true" {
		t.Fatalf("conn = %q", got)
	}
}

func TestMySQLAndMariaDB(t *testing.T) {
	my, _, _ := Get("mysql")
	ma, _, _ := Get("mariadb")
	p := tlsParams
	p.Host, p.Port, p.TLSDir = "fridge-db-k1", 3306, "/tls"

	p.Version = "8.4"
	s := my.Spec(p)
	if s.Image != "mysql:8.4" || !strings.Contains(s.Command[2], "mysqld ") || !strings.Contains(s.Command[2], "--mysql-native-password=ON") || !strings.Contains(s.Command[2], "--require-secure-transport=ON") {
		t.Fatalf("mysql 8.4: %+v", s)
	}
	p.Version = "8.0"
	if c := my.Spec(p).Command[2]; strings.Contains(c, "--mysql-native-password") {
		t.Fatal("8.0 does not know the flag that 8.4 needs")
	}
	if c := my.Spec(p).Command[2]; !strings.Contains(c, "IDENTIFIED WITH mysql_native_password BY") || !strings.Contains(c, "REQUIRE X509") {
		t.Fatalf("init script: %s", c)
	}
	p.Version = "11.4"
	s = ma.Spec(p)
	if s.Image != "mariadb:11.4" || !strings.Contains(s.Command[2], "mariadbd ") || strings.Contains(s.Command[2], "WITH mysql_native_password") || !strings.Contains(s.Command[2], "IDENTIFIED BY") {
		t.Fatalf("mariadb: %+v", s)
	}
	if env := strings.Join(my.Spec(p).Env, " "); !strings.Contains(env, "MYSQL_RANDOM_ROOT_PASSWORD=yes") || !strings.Contains(env, "MYSQL_ROOT_HOST=localhost") || !strings.Contains(env, "MYSQL_DATABASE=alice_shop") {
		t.Fatalf("env = %s", env)
	}

	if got := strings.Join(my.DumpCmd(p), " "); got != "mysqldump -h fridge-db-k1 -P 3306 -u su_0123456789abcdef --ssl-mode=VERIFY_IDENTITY --ssl-cert=/tls/client.crt --ssl-key=/tls/client.key --ssl-ca=/tls/ca.crt --single-transaction --routines --triggers --events --hex-blob --no-tablespaces --default-character-set=utf8mb4 --set-gtid-purged=OFF alice_shop" {
		t.Fatalf("mysql dump = %q", got)
	}
	if got := strings.Join(ma.DumpCmd(p), " "); !strings.HasPrefix(got, "mariadb-dump -h fridge-db-k1 -P 3306 -u su_0123456789abcdef --ssl --ssl-verify-server-cert --ssl-cert=/tls/client.crt") || strings.Contains(got, "gtid") || !strings.HasSuffix(got, " alice_shop") {
		t.Fatalf("mariadb dump = %q", got)
	}
	if !my.DumpGzip() || my.DumpExt() != ".sql.gz" {
		t.Fatal("dump is a gzipped SQL file")
	}
	argv, gz, role, err := my.RestoreCmd(p, "x.sql.gz")
	if err != nil || !gz || role || argv[0] != "mysql" || argv[len(argv)-1] != "alice_shop" {
		t.Fatalf("restore: %v %v %v %v", argv, gz, role, err)
	}
	if argv, gz, _, _ := ma.RestoreCmd(p, "x.sql"); argv[0] != "mariadb" || gz {
		t.Fatalf("mariadb restore: %v %v", argv, gz)
	}
	if _, _, _, err := my.RestoreCmd(p, "x.dump"); err == nil {
		t.Fatal("a pg_dump file was accepted")
	}
	if env := my.ClientEnv(p); len(env) != 1 || env[0] != "MYSQL_PWD=adm1n-pw" {
		t.Fatalf("env = %v", env)
	}

	got := my.AppUserSQL(`al'ice\app`, `p'w\d`, "shop`db")
	for _, want := range []string{"CREATE USER 'al''ice\\\\app'@'%' IDENTIFIED WITH mysql_native_password BY 'p''w\\\\d' REQUIRE X509;", "GRANT ALL PRIVILEGES ON `shop``db`.* TO 'al''ice\\\\app'@'%';"} {
		if !strings.Contains(got, want) {
			t.Errorf("mysql app user lacks %q in %q", want, got)
		}
	}
	if got := ma.AppUserSQL("u", "p", "d"); !strings.Contains(got, "CREATE USER 'u'@'%' IDENTIFIED BY 'p' REQUIRE X509;") {
		t.Fatalf("mariadb app user = %q", got)
	}
	if got := my.SetPasswordSQL("su_x", "n3w"); got != "ALTER USER 'su_x'@'%' IDENTIFIED WITH mysql_native_password BY 'n3w';\n" {
		t.Fatalf("rotate = %q", got)
	}
	if got := ma.SetPasswordSQL("su_x", "n3w"); got != "ALTER USER 'su_x'@'%' IDENTIFIED BY 'n3w';\n" {
		t.Fatalf("mariadb rotate = %q", got)
	}
	if got := my.ConnString(Params{User: "u", Password: "p@ss", Name: "d"}, "m.lan"); got != "mysql://u:p%40ss@m.lan:3306/d?ssl-mode=REQUIRED" {
		t.Fatalf("conn = %q", got)
	}
}

func TestMySQLRestoreFilterDropsDefiners(t *testing.T) {
	my, _, _ := Get("mysql")
	in := "/*!50013 DEFINER=`root`@`localhost` SQL SECURITY DEFINER */\n" +
		"CREATE DEFINER=`bob_app`@`%` PROCEDURE p() BEGIN END;\n" +
		"/*!50003 CREATE*/ /*!50017 DEFINER='x'@'y'*/ /*!50003 TRIGGER t BEFORE INSERT ON a FOR EACH ROW SET NEW.b = 1 */;\n" +
		"last line without a newline"
	b, err := io.ReadAll(my.RestoreFilter()(strings.NewReader(in)))
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, gone := range []string{"`root`@`localhost`", "bob_app", "'x'@'y'"} {
		if strings.Contains(out, gone) {
			t.Errorf("%s survived: %s", gone, out)
		}
	}
	for _, kept := range []string{"SQL SECURITY DEFINER", "CREATE  PROCEDURE p()", "TRIGGER t BEFORE INSERT", "last line without a newline"} {
		if !strings.Contains(out, kept) {
			t.Errorf("%q was lost: %s", kept, out)
		}
	}
}
