package engine

import (
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

func TestPostgresRestoreCmd(t *testing.T) {
	e, _, err := Get("postgres")
	if err != nil {
		t.Fatal(err)
	}
	p := Params{Name: "n", User: "u", Password: "p", Version: "17"}

	cmd, gz, err := e.RestoreCmd(p, "a/b.dump")
	if err != nil || cmd[0] != "pg_restore" || gz {
		t.Fatalf("a/b.dump: cmd=%v gunzip=%v err=%v", cmd, gz, err)
	}
	cmd, gz, err = e.RestoreCmd(p, "x.sql.gz")
	if err != nil || cmd[0] != "psql" || !gz {
		t.Fatalf("x.sql.gz: cmd=%v gunzip=%v err=%v", cmd, gz, err)
	}
	if _, _, err := e.RestoreCmd(p, "x.zip"); err == nil {
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
