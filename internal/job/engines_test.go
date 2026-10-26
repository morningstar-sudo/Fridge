package job

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fridge/internal/dumpcheck"
	"fridge/internal/engine"
)

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func paramsFor(eng string) engine.Params {
	return engine.Params{Name: "alice_shop", User: "alice_app", AdminUser: "su_0123456789abcdef", AdminPassword: "sekret", Version: "99", Host: "fridge-db-0123456789abcdef"}
}

func TestMySQLDumpIsGzippedAndRestoreDropsDefiners(t *testing.T) {
	dump := "-- MySQL dump 10.13\nCREATE DEFINER=`root`@`localhost` PROCEDURE p() BEGIN END;\n-- Dump completed on 2026-10-26\n"
	log := tools(t, map[string]string{"mysqldump": "printf '%s' '" + dump + "'", "mysql": "cat > \"$LOG.stdin-mysql\""})
	t.Setenv("LOG", log)
	b := bucket(t)
	code, out, errs := run(t, Spec{Op: "dump", Engine: "mysql", Params: paramsFor("mysql"), Key: "k/x.sql.gz"})
	if code != 0 {
		t.Fatalf("dump exit %d: %s", code, errs)
	}
	zr, err := gzip.NewReader(bytes.NewReader(b.objs["k/x.sql.gz"]))
	if err != nil {
		t.Fatalf("the object is not gzip: %v", err)
	}
	var plain bytes.Buffer
	plain.ReadFrom(zr)
	if plain.String() != dump {
		t.Fatalf("object = %q", plain.String())
	}
	if r := resultOf(t, out); r["size"] != float64(len(b.objs["k/x.sql.gz"])) {
		t.Fatalf("result %v (size is of the stored object)", r)
	}
	got, _ := os.ReadFile(log)
	if !strings.Contains(string(got), "mysqldump -h fridge-db-0123456789abcdef -P 3306 -u su_0123456789abcdef") || !strings.Contains(string(got), "PW=") {
		t.Fatalf("mysqldump ran as %q", got)
	}

	if code, _, errs := run(t, Spec{Op: "restore", Engine: "mysql", Params: paramsFor("mysql"), Key: "k/x.sql.gz"}); code != 0 {
		t.Fatalf("restore exit %d: %s", code, errs)
	}
	in, _ := os.ReadFile(log + ".stdin-mysql")
	if strings.Contains(string(in), "DEFINER") || !strings.Contains(string(in), "CREATE  PROCEDURE p()") {
		t.Fatalf("mysql read %q", in)
	}
}

func TestExecWaitsForTheNodeThenRunsTheScriptOnce(t *testing.T) {
	readyWait = 10 * time.Second
	t.Cleanup(func() { readyWait = 2 * time.Minute })
	dir := t.TempDir()
	// ping: refused twice (the node is still starting), then answered; the admin shell appends what it is fed
	log := tools(t, map[string]string{"mysql": `
if [ "$*" != "${*%select 1*}" ]; then
  n=$(cat "` + dir + `/n" 2>/dev/null || echo 0); echo $((n+1)) > "` + dir + `/n"
  [ "$n" -ge 2 ] && exit 0 || exit 1
fi
cat >> "` + dir + `/script"`})
	_ = log
	s := Spec{Op: "exec", Engine: "mysql", Params: paramsFor("mysql"), Script: "CREATE USER 'a'@'%' IDENTIFIED BY 'x';\n"}
	if code, _, errs := run(t, s); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "script"))
	if string(got) != s.Script {
		t.Fatalf("the shell was fed %q (once, after the node answered)", got)
	}
	if n, _ := os.ReadFile(filepath.Join(dir, "n")); !strings.HasPrefix(string(n), "3") {
		t.Fatalf("pings = %q", n)
	}
}

func TestExecFailsWhenTheToolFailsOrSaysError(t *testing.T) {
	readyWait = time.Second
	t.Cleanup(func() { readyWait = 2 * time.Minute })
	tools(t, map[string]string{"mysql": `cat >/dev/null; case "$*" in *"select 1"*) exit 0;; esac; exit 1`})
	if code, _, errs := run(t, Spec{Op: "exec", Engine: "mysql", Params: paramsFor("mysql"), Script: "x"}); code != 1 || !strings.Contains(errs, "admin script failed") {
		t.Fatalf("a failing shell: %d %q", code, errs)
	}
	// redis-cli exits 0 on a refused command: only its output says so
	tools(t, map[string]string{"redis-cli": `cat >/dev/null; case "$*" in *ping*) echo PONG;; *) echo "(error) ERR unknown command";; esac`})
	if code, _, errs := run(t, Spec{Op: "exec", Engine: "redis", Params: paramsFor("redis"), Script: "FOO"}); code != 1 || !strings.Contains(errs, "refused") {
		t.Fatalf("redis-cli saying error: %d %q", code, errs)
	}
	if code, _, errs := run(t, Spec{Op: "exec", Engine: "redis", Params: paramsFor("redis")}); code != 1 || !strings.Contains(errs, "no script") {
		t.Fatalf("no script: %d %q", code, errs)
	}
	// a node that never answers
	tools(t, map[string]string{"mysql": "exit 1"})
	if code, _, errs := run(t, Spec{Op: "exec", Engine: "mysql", Params: paramsFor("mysql"), Script: "x"}); code != 1 || !strings.Contains(errs, "did not accept an admin connection") {
		t.Fatalf("a node that is down: %d %q", code, errs)
	}
}

func TestPreloadWritesTheFileForTheEngineToFind(t *testing.T) {
	root := t.TempDir()
	preloadRoot = root
	var chowned int
	chown = func(path string, uid, gid int) error { chowned = uid; return nil }
	t.Cleanup(func() { preloadRoot, chown = "/data", os.Chown })
	b := bucket(t)
	b.objs["k/x.rdb"] = []byte("REDIS0011-data-0123456789")
	if code, _, errs := run(t, Spec{Op: "preload", Engine: "redis", Params: paramsFor("redis"), Key: "k/x.rdb"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	got, err := os.ReadFile(filepath.Join(root, "dump.rdb"))
	if err != nil || string(got) != "REDIS0011-data-0123456789" || chowned != 999 {
		t.Fatalf("file %q (%v), owner %d", got, err, chowned)
	}
	if code, _, errs := run(t, Spec{Op: "preload", Engine: "postgres", Params: paramsFor("postgres"), Key: "k/x.dump"}); code != 1 || !strings.Contains(errs, "does not take a preloaded file") {
		t.Fatalf("an engine that loads files into a running node: %d %q", code, errs)
	}
}

func TestCheckOther(t *testing.T) {
	sql := "-- MySQL dump 10.13  Distrib 8.4.0\n-- Server version\t8.4.0\n/*!40101 SET x=1 */;\nCREATE TABLE t (a int);\n-- Dump completed on 2026-10-26 10:00:00\n"
	archive := append(append([]byte{}, dumpcheck.ArchiveMagic[:]...), bytes.Repeat([]byte{7}, 200)...)
	rdb := append([]byte("REDIS0011"), bytes.Repeat([]byte{1}, 100)...)
	rdb = append(rdb, 0xff, 1, 2, 3, 4, 5, 6, 7, 8)
	cases := []struct {
		name, eng, key string
		data           []byte
		wantErr        string
	}{
		{"mysql", "mysql", "a/mysql-8.4/x.sql.gz", gz(t, sql), ""},
		{"mysql cut off", "mysql", "a/mysql-8.4/x.sql.gz", gz(t, strings.TrimSuffix(sql, "-- Dump completed on 2026-10-26 10:00:00\n")), "cut off"},
		{"mysql, truncated gzip", "mysql", "a/x.sql.gz", gz(t, sql)[:40], ""}, // 40 bytes: head still parses, the stream does not
		{"mariadb", "mariadb", "a/x.sql", []byte(strings.ReplaceAll(sql, "MySQL", "MariaDB")), ""},
		{"mongo", "mongo", "a/x.archive.gz", gz(t, string(archive)), ""},
		{"mongo, corrupt", "mongo", "a/x.archive.gz", append(gz(t, string(archive))[:30], bytes.Repeat([]byte{9}, 30)...), "corrupt"},
		{"redis", "redis", "a/x.rdb", rdb, ""},
		{"redis cut off", "redis", "a/x.rdb", rdb[:60], "cut off"},
	}
	for _, c := range cases {
		b := bucket(t)
		b.objs[c.key] = c.data
		code, out, errs := run(t, Spec{Op: "check", Engine: c.eng, Key: c.key, Params: engine.Params{Version: "99"}})
		if c.name == "mysql, truncated gzip" {
			if code == 0 {
				t.Errorf("%s: passed", c.name)
			}
			continue
		}
		if c.wantErr == "" {
			if code != 0 {
				t.Errorf("%s: exit %d: %s", c.name, code, errs)
			} else if r := resultOf(t, out); r["bytes"] != float64(len(c.data)) {
				t.Errorf("%s: result %v", c.name, r)
			}
			continue
		}
		if code != 1 || !strings.Contains(errs, c.wantErr) {
			t.Errorf("%s: exit %d: %s", c.name, code, errs)
		}
	}
}

// The client tools learn where the node's certificate is from their arguments (redis-cli, mongo tools, mysql) or from
// the environment (libpq): whichever the engine uses, the files must exist and be named.
func TestClientArgumentsNameTheCertificateFiles(t *testing.T) {
	log := tools(t, map[string]string{"redis-cli": `echo "ARGS $*" >> "$LOG"; case "$*" in *--cert*) for f in $(echo "$*" | tr ' ' '\n' | grep '^/'); do [ -s "$f" ] || echo "MISSING $f" >> "$LOG"; done;; esac; echo PONG`})
	t.Setenv("LOG", log)
	p := paramsFor("redis")
	p.TLS = &engine.TLS{ClientCert: "CC", ClientKey: "CK", CA: "CA"}
	if code, _, errs := run(t, Spec{Op: "ping", Engine: "redis", Params: p}); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	got, _ := os.ReadFile(log)
	if !strings.Contains(string(got), "--tls --cacert /") || !strings.Contains(string(got), "--cert /") || strings.Contains(string(got), "MISSING") {
		t.Fatalf("redis-cli ran as %q", got)
	}
}
