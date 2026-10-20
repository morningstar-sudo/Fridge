package engine

import (
	"net/url"
	"strings"
	"testing"
)

func TestResolveVersion(t *testing.T) {
	if v, err := ResolveVersion("postgres", ""); err != nil || v != "17" {
		t.Fatalf(`ResolveVersion("postgres","") = %q, %v; want "17"`, v, err)
	}
	if v, err := ResolveVersion("postgres", "14"); err != nil || v != "14" {
		t.Fatalf(`ResolveVersion("postgres","14") = %q, %v; want "14"`, v, err)
	}
	if _, err := ResolveVersion("postgres", "99"); err == nil {
		t.Fatal(`ResolveVersion("postgres","99") want error`)
	}
	if _, err := ResolveVersion("mysql", "8.4"); err == nil {
		t.Fatal(`ResolveVersion("mysql","8.4") want error (not registered)`)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"16", "16.4", 0},
		{"7.2", "7.4", -1},
		{"18", "9", 1},
		{"17", "17", 0},
		{"13", "18", -1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q,%q) = %d; want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestPostgresVolumePath(t *testing.T) {
	e, _, err := Get("postgres")
	if err != nil {
		t.Fatal(err)
	}
	p := Params{Name: "abc", User: "u", Password: "p", Version: "17"}
	if got := e.Spec(p).VolumePath; got != "/var/lib/postgresql/data" {
		t.Errorf("v17 VolumePath = %q", got)
	}
	p.Version = "18"
	if got := e.Spec(p).VolumePath; got != "/var/lib/postgresql" {
		t.Errorf("v18 VolumePath = %q", got)
	}
}

// Over TCP, so the entrypoint's socket-only init server never reads as healthy.
func TestPostgresHealthcheckUsesTCP(t *testing.T) {
	e, _, _ := Get("postgres")
	hc := e.Spec(Params{Name: "abc", AdminUser: "u", Version: "17"}).Healthcheck
	if len(hc) != 2 || hc[0] != "CMD-SHELL" || hc[1] != "pg_isready -h 127.0.0.1 -U u -d abc" {
		t.Fatalf("healthcheck = %q", hc)
	}
}

func TestPostgresRestoreCmd(t *testing.T) {
	e, _, err := Get("postgres")
	if err != nil {
		t.Fatal(err)
	}
	p := Params{Name: "n", User: "u", Password: "p", Version: "17"}

	cmd, gz, _, err := e.RestoreCmd(p, "a/b.dump")
	if err != nil || cmd[0] != "pg_restore" || gz {
		t.Fatalf("a/b.dump: cmd=%v gunzip=%v err=%v", cmd, gz, err)
	}
	cmd, gz, _, err = e.RestoreCmd(p, "x.sql.gz")
	if err != nil || cmd[0] != "psql" || !gz {
		t.Fatalf("x.sql.gz: cmd=%v gunzip=%v err=%v", cmd, gz, err)
	}
	if _, _, _, err := e.RestoreCmd(p, "x.zip"); err == nil {
		t.Fatal("x.zip want error")
	}
}

func TestPostgresConnString(t *testing.T) {
	e, _, err := Get("postgres")
	if err != nil {
		t.Fatal(err)
	}
	p := Params{Name: "abc", User: "abc", Password: "p@ss/w", Version: "17"}
	got := e.ConnString(p, "abc.com")
	if !strings.Contains(got, "p%40ss%2Fw@abc.com:5432/abc?sslmode=require") {
		t.Errorf("ConnString = %q", got)
	}
}

func TestPostgresTwoUserSpec(t *testing.T) {
	e, _, _ := Get("postgres")
	p := Params{Name: "alice_shop", User: "alice_shop", Password: "app", Version: "17", AdminUser: "su_0123456789abcdef", AdminPassword: "adm"}
	env := strings.Join(e.Spec(p).Env, "\n")
	if !strings.Contains(env, "POSTGRES_USER=su_0123456789abcdef\n") || !strings.Contains(env, "POSTGRES_PASSWORD=adm\n") || !strings.Contains(env, "POSTGRES_DB=alice_shop") {
		t.Fatalf("env = %q", env)
	}
	if hc := e.Spec(p).Healthcheck[1]; hc != "pg_isready -h 127.0.0.1 -U su_0123456789abcdef -d alice_shop" {
		t.Fatalf("healthcheck = %q", hc)
	}
	if got := strings.Join(e.DumpCmd(p), " "); got != "pg_dump -U su_0123456789abcdef -d alice_shop -Fc" {
		t.Fatalf("dump = %q", got)
	}
	cmd, gz, setRole, err := e.RestoreCmd(p, "x.dump")
	if j := strings.Join(cmd, " "); err != nil || gz || setRole || !strings.Contains(j, "--role=alice_shop") || !strings.Contains(j, "-U su_0123456789abcdef") {
		t.Fatalf(".dump: %q %v %v %v", cmd, gz, setRole, err)
	}
	cmd, gz, setRole, err = e.RestoreCmd(p, "x.sql.gz")
	if err != nil || !gz || !setRole || cmd[0] != "psql" || cmd[2] != "su_0123456789abcdef" {
		t.Fatalf(".sql.gz: %q %v %v %v", cmd, gz, setRole, err)
	}
	if got := strings.Join(e.AdminCmd(p), " "); got != "psql -U su_0123456789abcdef -d alice_shop -v ON_ERROR_STOP=1 -q" {
		t.Fatalf("admin = %q", got)
	}
	sql := e.AppUserSQL(`a"b`, "it's", `d"b`)
	for _, want := range []string{`CREATE ROLE "a""b" LOGIN PASSWORD 'it''s' NOSUPERUSER NOCREATEDB NOCREATEROLE;`, `ALTER DATABASE "d""b" OWNER TO "a""b";`, `ALTER SCHEMA public OWNER TO "a""b";`} {
		if !strings.Contains(sql, want) {
			t.Errorf("AppUserSQL missing %q in %q", want, sql)
		}
	}
	if got := e.SetPasswordSQL("su_x", "o'k"); got != `ALTER ROLE "su_x" PASSWORD 'o''k';` {
		t.Fatalf("SetPasswordSQL = %q", got)
	}
	if u, _ := url.Parse(e.ConnString(p, "x.lan")); u.User.Username() != "alice_shop" {
		t.Fatalf("conn user = %q", u.User.Username())
	}
}
