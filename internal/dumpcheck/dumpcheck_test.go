package dumpcheck

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMajor(t *testing.T) {
	for in, want := range map[string]int{"17.2": 17, "14beta1": 14, "9.6.1": 9, "13": 13, "": 0, "x": 0} {
		if got := Major(in); got != want {
			t.Errorf("Major(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestInspectCustom(t *testing.T) {
	for file, major := range map[string]int{"pg14.dump": 14, "pg17.dump": 17} {
		h, err := Inspect(file, read(t, file))
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if h.Format != "custom" || h.Gzip || Major(h.ServerVersion) != major || Major(h.PgDumpVersion) != major || h.ArchiveVersion == "" {
			t.Errorf("%s: %+v", file, h)
		}
	}
	// only the head is needed
	if _, err := Inspect("x.dump", read(t, "pg17.dump")[:200]); err != nil {
		t.Fatal(err)
	}
}

func TestInspectPgDumpNewer(t *testing.T) {
	h, err := Inspect("pg13-by-pgdump17.dump", read(t, "pg13-by-pgdump17.dump"))
	if err != nil {
		t.Fatal(err)
	}
	if Major(h.ServerVersion) != 13 || Major(h.PgDumpVersion) != 17 {
		t.Fatalf("%+v", h)
	}
}

func TestInspectRejects(t *testing.T) {
	good := read(t, "pg17.dump")
	cases := []struct {
		name, key string
		head      []byte
		want      string
	}{
		{"empty", "empty.dump", nil, "empty"},
		{"png", "not-a-dump.dump", read(t, "not-a-dump.dump"), "not a pg_dump custom-format"},
		{"zip", "x.zip", []byte("PK\x03\x04"), "unsupported"},
		{"nul in sql", "x.sql", []byte("SELECT 1;\x00\n"), "NUL"},
		{"truncated header", "x.dump", good[:30], "truncated"},
		{"future archive", "x.dump", append(append([]byte{}, good[:6]...), append([]byte{99}, good[7:]...)...), "unsupported pg_dump archive version"},
		{"tar archive", "x.dump", append(append([]byte{}, good[:10]...), append([]byte{3}, good[11:]...)...), "wrong archive format"},
		{"not sql", "x.sql", []byte("hello world, this is a letter\n"), "not a pg_dump SQL"},
		{"bad utf8", "x.sql", []byte("-- PostgreSQL database dump\n\xff\xfe\n"), "UTF-8"},
		{"not gzip", "x.sql.gz", []byte("-- PostgreSQL database dump\n"), "gzip"},
	}
	for _, c := range cases {
		_, err := Inspect(c.key, c.head)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestInspectSQL(t *testing.T) {
	gz := read(t, "plain.sql.gz")
	h, err := Inspect("a.sql.gz", gz)
	if err != nil || h.Format != "sql" || !h.Gzip {
		t.Fatalf("%+v %v", h, err)
	}
	// a head cut off inside the gzip stream is fine
	if _, err := Inspect("a.sql.gz", gz[:len(gz)-30]); err != nil {
		t.Fatalf("truncated gzip head: %v", err)
	}
	// a head cut off inside a UTF-8 rune is fine, in the middle of the text it is not
	txt := []byte("-- PostgreSQL database dump\n-- caf\xc3\xa9\n")
	if _, err := Inspect("a.sql", txt[:len(txt)-2]); err != nil {
		t.Fatalf("cut rune: %v", err)
	}
	bad := append(append([]byte{}, txt[:len(txt)-3]...), []byte("\xc3 more text\n")...)
	if _, err := Inspect("a.sql", bad); err == nil {
		t.Fatal("broken rune in the middle accepted")
	}
	// plain SQL without the pg_dump banner
	if _, err := Inspect("a.sql", []byte("\n  CREATE TABLE t(id int);\n")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte("SET statement_timeout = 0;\n"))
	zw.Close()
	if _, err := Inspect("b.sql.gz", buf.Bytes()); err != nil {
		t.Fatal(err)
	}
}

func TestTextChecker(t *testing.T) {
	text := []byte("-- caf\xc3\xa9 \xe2\x82\xac end\n") // 2- and 3-byte runes
	for split := 0; split <= len(text); split++ {       // every possible chunk boundary
		var c TextChecker
		if _, err := c.Write(text[:split]); err != nil {
			t.Fatalf("split %d: %v", split, err)
		}
		if _, err := c.Write(text[split:]); err != nil {
			t.Fatalf("split %d: %v", split, err)
		}
		if err := c.Close(); err != nil || c.N != int64(len(text)) {
			t.Fatalf("split %d: close=%v n=%d", split, err, c.N)
		}
	}
	var c TextChecker
	c.Write(text[:len(text)-3]) // ends inside the euro sign...
	c.Write([]byte("\xe2\x82"))
	if err := c.Close(); err == nil {
		t.Error("a rune cut off at the end was accepted")
	}
	if _, err := (&TextChecker{}).Write([]byte("a\x00b")); err == nil {
		t.Error("NUL accepted")
	}
	if _, err := (&TextChecker{}).Write([]byte("ok \xff\xfe more")); err == nil {
		t.Error("invalid UTF-8 accepted")
	}
}
