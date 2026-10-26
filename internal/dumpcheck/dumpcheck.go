// Package dumpcheck vets the first bytes of a Postgres backup file before it is restored.
package dumpcheck

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// HeadSize is how much of a file the caller should fetch for Inspect.
const HeadSize = 64 << 10

// Header describes what Inspect learned. For plain SQL only Format and Gzip are set.
type Header struct {
	Format                                       string // "custom" | "sql"
	Gzip                                         bool
	ArchiveVersion, ServerVersion, PgDumpVersion string
}

// Archive versions whose header layout Inspect knows (PostgreSQL 10 through 18).
const minArchive, maxArchive = 12, 16

// Inspect checks the head of the object stored under key. head may be shorter than the file.
func Inspect(key string, head []byte) (Header, error) {
	if len(head) == 0 {
		return Header{}, errors.New("empty file")
	}
	switch {
	case strings.HasSuffix(key, ".dump"):
		return inspectCustom(head)
	case strings.HasSuffix(key, ".sql.gz"):
		return inspectSQL(head, true)
	case strings.HasSuffix(key, ".sql"):
		return inspectSQL(head, false)
	}
	return Header{}, errors.New("unsupported file type (want .dump, .sql or .sql.gz)")
}

// Major is the major version of a Postgres version string: "17.2" is 17, "14beta1" is 14, "9.6.1" is 9.
func Major(version string) int {
	n := 0
	for n < len(version) && version[n] >= '0' && version[n] <= '9' {
		n++
	}
	m, _ := strconv.Atoi(version[:n])
	return m
}

var errTruncated = errors.New("dump header is truncated")

// archiveReader reads the primitives of pg_dump's archive header (pg_backup_archiver.c).
type archiveReader struct {
	b       []byte
	intSize int
	err     error
}

func (r *archiveReader) byte() byte {
	if len(r.b) == 0 {
		r.err = errTruncated
		return 0
	}
	c := r.b[0]
	r.b = r.b[1:]
	return c
}

// int is a sign byte followed by intSize little-endian magnitude bytes.
func (r *archiveReader) int() int {
	neg := r.byte() != 0
	v := 0
	for i := 0; i < r.intSize; i++ {
		v |= int(r.byte()) << (8 * i)
	}
	if neg {
		return -v
	}
	return v
}

// str is a length (int) and that many bytes; a negative length is NULL.
func (r *archiveReader) str() string {
	n := r.int()
	if r.err != nil || n < 0 {
		return ""
	}
	if n > len(r.b) {
		r.err = errTruncated
		return ""
	}
	s := string(r.b[:n])
	r.b = r.b[n:]
	return s
}

func inspectCustom(head []byte) (Header, error) {
	if !bytes.HasPrefix(head, []byte("PGDMP")) {
		return Header{}, errors.New("not a pg_dump custom-format file (missing PGDMP signature)")
	}
	r := &archiveReader{b: head[5:], intSize: 4}
	vmaj, vmin, vrev := r.byte(), r.byte(), r.byte()
	r.intSize = int(r.byte())
	offSize := r.byte()
	format := r.byte()
	if r.err != nil {
		return Header{}, r.err
	}
	if vmaj != 1 || vmin < minArchive || vmin > maxArchive {
		return Header{}, fmt.Errorf("unsupported pg_dump archive version %d.%d.%d", vmaj, vmin, vrev)
	}
	if r.intSize < 1 || r.intSize > 8 || offSize < 1 || offSize > 8 {
		return Header{}, errors.New("not a pg_dump custom-format file (bad header sizes)")
	}
	if format != 1 {
		return Header{}, errors.New("not a pg_dump custom-format file (wrong archive format)")
	}
	if vmin >= 15 {
		r.byte() // compression algorithm
	} else {
		r.int() // compression level
	}
	for i := 0; i < 7; i++ { // creation time: sec, min, hour, mday, mon, year, isdst
		r.int()
	}
	r.str() // database name
	h := Header{Format: "custom", ArchiveVersion: fmt.Sprintf("1.%d.%d", vmin, vrev)}
	h.ServerVersion = firstField(r.str())
	h.PgDumpVersion = firstField(r.str())
	if r.err != nil {
		return Header{}, r.err
	}
	if Major(h.ServerVersion) == 0 || Major(h.PgDumpVersion) == 0 {
		return Header{}, errors.New("pg_dump header has no server version")
	}
	return h, nil
}

// firstField turns "17.2 (Debian 17.2-1.pgdg120+1)" into "17.2".
func firstField(s string) string {
	f, _, _ := strings.Cut(s, " ")
	return f
}

func inspectSQL(head []byte, gz bool) (Header, error) {
	return inspectSQLWith(head, gz, looksLikeSQL, "pg_dump")
}

func inspectSQLWith(head []byte, gz bool, looks func([]byte) bool, what string) (Header, error) {
	h := Header{Format: "sql", Gzip: gz}
	text := head
	if gz {
		zr, err := gzip.NewReader(bytes.NewReader(head))
		if err != nil {
			return Header{}, errors.New("not a gzip file")
		}
		// The head is a prefix of the file: running out of input is expected.
		text, err = io.ReadAll(zr)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return Header{}, fmt.Errorf("corrupt gzip data: %w", err)
		}
		if len(text) == 0 {
			return Header{}, errors.New("empty file")
		}
	}
	if i := bytes.IndexByte(text, 0); i >= 0 {
		return Header{}, errors.New("not a SQL text file (contains NUL bytes)")
	}
	if !validPrefix(text) {
		return Header{}, errors.New("not a SQL text file (invalid UTF-8)")
	}
	if !looks(text) {
		return Header{}, fmt.Errorf("not a %s SQL file", what)
	}
	return h, nil
}

// validPrefix is utf8.Valid, except that the text may end inside a multi-byte rune (it is a cut-off head).
func validPrefix(text []byte) bool {
	for i := len(text) - 1; i >= 0 && i >= len(text)-utf8.UTFMax; i-- {
		if utf8.RuneStart(text[i]) { // the last rune begins here: excuse it only if it is incomplete
			if !utf8.FullRune(text[i:]) {
				text = text[:i]
			}
			break
		}
	}
	return utf8.Valid(text)
}

func looksLikeSQL(text []byte) bool {
	if bytes.Contains(text, []byte("-- PostgreSQL database dump")) {
		return true
	}
	for _, line := range bytes.Split(text, []byte("\n")) {
		for _, kw := range []string{"SET", "CREATE", "COPY", "INSERT", "BEGIN"} {
			if bytes.HasPrefix(bytes.TrimSpace(line), []byte(kw)) {
				return true
			}
		}
	}
	return false
}

// TextChecker is an io.Writer that fails on NUL bytes and invalid UTF-8, however the text is chunked.
// Call Close at the end of the input: a rune cut off there is an error too.
type TextChecker struct {
	N     int64 // bytes seen
	carry []byte
}

var (
	errNUL  = errors.New("not a SQL text file (contains NUL bytes)")
	errUTF8 = errors.New("not a SQL text file (invalid UTF-8)")
)

func (c *TextChecker) Write(p []byte) (int, error) {
	c.N += int64(len(p))
	if bytes.IndexByte(p, 0) >= 0 {
		return 0, errNUL
	}
	buf := append(c.carry, p...)
	cut := len(buf)
	for i := len(buf) - 1; i >= 0 && i >= len(buf)-utf8.UTFMax; i-- {
		if utf8.RuneStart(buf[i]) {
			if !utf8.FullRune(buf[i:]) {
				cut = i
			}
			break
		}
	}
	if !utf8.Valid(buf[:cut]) {
		return 0, errUTF8
	}
	c.carry = append(c.carry[:0], buf[cut:]...)
	return len(p), nil
}

func (c *TextChecker) Close() error {
	if len(c.carry) > 0 {
		return errUTF8
	}
	return nil
}
