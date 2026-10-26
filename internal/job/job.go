// Package job is what runs inside a swarm job container: dump a database to S3, restore an S3 file into one,
// or check a backup file. It exists because the nodes of a swarm have no Fridge agent and `docker exec` only
// works on the machine that holds the container; a job runs anywhere, joins the database's network, and moves
// the data to and from S3 itself, so nothing large ever passes through the manager.
package job

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"fridge/internal/dumpcheck"
	"fridge/internal/engine"
	"fridge/internal/objstore"
	"fridge/internal/store"
)

// EnvSpec is the environment variable that carries the Spec (it holds credentials: managers only can read it).
const EnvSpec = "FRIDGE_JOB"

// ResultPrefix starts the one line on stdout that carries a job's result as JSON.
const ResultPrefix = "FRIDGE_RESULT "

type S3 struct {
	Endpoint, Region, Bucket, AccessKey, SecretKey string
	PathStyle                                      bool
}

type Guard struct {
	Blocked []string // CIDRs
	Allow   []string
}

type Spec struct {
	Op     string // "dump" | "restore" | "check"
	Params engine.Params
	Engine string
	S3     S3
	Key    string
	Meta   map[string]string // dump: metadata stored with the object
	Guard  *Guard

	tlsDir string // where Run put the node's client certificate (not part of the spec)
}

// Objects is the part of an S3 bucket a job uses; *objstore.Store satisfies it.
type Objects interface {
	Put(ctx context.Context, key string, r io.Reader, meta map[string]string) (int64, error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	ReadHead(ctx context.Context, key string, n int) ([]byte, error)
}

// open is swapped by tests for an in-memory bucket.
var open = func(s Spec) (Objects, error) { return s.objects() }

func (s Spec) objects() (*objstore.Store, error) {
	d := store.Destination{Endpoint: s.S3.Endpoint, Region: s.S3.Region, Bucket: s.S3.Bucket, AccessKey: s.S3.AccessKey, PathStyle: s.S3.PathStyle}
	var g *objstore.Guard
	if s.Guard != nil {
		g = &objstore.Guard{Allow: s.Guard.Allow}
		for _, c := range s.Guard.Blocked {
			p, err := netip.ParsePrefix(c)
			if err != nil {
				return nil, err
			}
			g.Blocked = append(g.Blocked, p)
		}
	}
	return objstore.New(d, s.S3.SecretKey, g)
}

// FromEnv reads the Spec from the environment.
func FromEnv() (Spec, error) {
	var s Spec
	raw := os.Getenv(EnvSpec)
	if raw == "" {
		return s, errors.New(EnvSpec + " is not set")
	}
	return s, json.Unmarshal([]byte(raw), &s)
}

func result(stdout io.Writer, v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(stdout, "%s%s\n", ResultPrefix, b)
}

// Run executes the job and returns the process exit code. Progress and errors go to stderr, the result to stdout.
func Run(ctx context.Context, s Spec, stdout, stderr io.Writer) int {
	if t := s.Params.TLS; t != nil && t.ClientCert != "" {
		dir, err := writeTLS(t)
		if err != nil {
			fmt.Fprintln(stderr, "job failed:", err)
			return 1
		}
		defer os.RemoveAll(dir)
		s.tlsDir = dir
	}
	var err error
	switch s.Op {
	case "dump":
		err = dump(ctx, s, stdout, stderr)
	case "restore":
		err = restore(ctx, s, stderr)
	case "check":
		err = check(ctx, s, stdout, stderr)
	default:
		err = fmt.Errorf("unknown job %q", s.Op)
	}
	if err != nil {
		fmt.Fprintln(stderr, "job failed:", err)
		return 1
	}
	return 0
}

// pgBin finds a client tool of the database's major version (the job image carries 13 to 18 side by side).
func pgBin(version, name string) string {
	major := version
	if i := strings.IndexAny(major, ".-"); i > 0 {
		major = major[:i]
	}
	p := filepath.Join("/usr/lib/postgresql", major, "bin", name)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return name // not in the job image (tests, a dev machine): whatever is on PATH
}

func command(ctx context.Context, s Spec, argv []string, stderr io.Writer) *exec.Cmd {
	c := exec.CommandContext(ctx, pgBin(s.Params.Version, argv[0]), argv[1:]...)
	c.Env = append(os.Environ(), "PGPASSWORD="+s.Params.AdminPassword)
	if s.tlsDir != "" { // the database takes only a client holding this node's certificate, and the client checks the server's name
		c.Env = append(c.Env, "PGSSLMODE=verify-full", "PGSSLCERT="+filepath.Join(s.tlsDir, "client.crt"),
			"PGSSLKEY="+filepath.Join(s.tlsDir, "client.key"), "PGSSLROOTCERT="+filepath.Join(s.tlsDir, "ca.crt"))
	}
	c.Stderr = stderr
	return c
}

func dump(ctx context.Context, s Spec, stdout, stderr io.Writer) error {
	eng, _, err := engine.Get(s.Engine)
	if err != nil {
		return err
	}
	objs, err := open(s)
	if err != nil {
		return err
	}
	c := command(ctx, s, eng.DumpCmd(s.Params), stderr)
	pr, pw := io.Pipe()
	c.Stdout = pw
	done := make(chan error, 1)
	go func() {
		err := c.Run()
		if err != nil {
			err = fmt.Errorf("dump failed: %w", err)
		}
		pw.CloseWithError(err) // nil = plain EOF
		done <- err
	}()
	size, putErr := objs.Put(ctx, s.Key, pr, s.Meta)
	if putErr != nil {
		pr.CloseWithError(putErr) // unblock the dump writer
	} else {
		pr.Close()
	}
	if err := errors.Join(<-done, putErr); err != nil {
		return err
	}
	result(stdout, map[string]any{"size": size})
	return nil
}

func restore(ctx context.Context, s Spec, stderr io.Writer) error {
	eng, _, err := engine.Get(s.Engine)
	if err != nil {
		return err
	}
	argv, gunzip, setRole, err := eng.RestoreCmd(s.Params, s.Key)
	if err != nil {
		return err
	}
	objs, err := open(s)
	if err != nil {
		return err
	}
	rc, err := objs.Get(ctx, s.Key)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer rc.Close()
	var in io.Reader = rc
	if gunzip {
		zr, err := gzip.NewReader(rc)
		if err != nil {
			return fmt.Errorf("not a gzip file: %w", err)
		}
		defer zr.Close()
		in = zr
	}
	if setRole { // objects of a plain SQL file must belong to the app user, not the superuser
		in = io.MultiReader(strings.NewReader("SET ROLE "+engine.QuoteIdent(s.Params.User)+";\n"), in)
	}
	c := command(ctx, s, argv, stderr)
	c.Stdin = in
	if err := c.Run(); err != nil {
		return fmt.Errorf("restore failed: %w", err)
	}
	return nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// check reads the whole object: plain SQL in Go, a custom archive through the real pg_restore writing to /dev/null.
func check(ctx context.Context, s Spec, stdout, stderr io.Writer) (err error) {
	res := map[string]any{}
	defer func() { result(stdout, res) }()
	objs, err := open(s)
	if err != nil {
		return err
	}
	head, err := objs.ReadHead(ctx, s.Key, dumpcheck.HeadSize)
	if err != nil {
		return fmt.Errorf("read head: %w", err)
	}
	hdr, err := dumpcheck.Inspect(s.Key, head)
	if err != nil {
		return err
	}
	res["format"] = hdr.Format
	rc, err := objs.Get(ctx, s.Key)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer rc.Close()
	in := &countReader{r: rc}
	defer func() { res["bytes"] = in.n }()

	if hdr.Format == "sql" {
		var r io.Reader = in
		if hdr.Gzip {
			zr, err := gzip.NewReader(in)
			if err != nil {
				return fmt.Errorf("not a gzip file: %w", err)
			}
			defer zr.Close()
			r = zr
		}
		var tc dumpcheck.TextChecker
		_, err := io.Copy(&tc, r)
		if err == nil {
			err = tc.Close()
		}
		if hdr.Gzip {
			res["sql_bytes"] = tc.N
		}
		return err
	}

	res["source_version"], res["pg_dump_version"] = hdr.ServerVersion, hdr.PgDumpVersion
	var out tailCounter
	c := exec.CommandContext(ctx, pgBin(newestMajor(s.Params.Version, hdr), "pg_restore"), "-v", "-f", "/dev/null")
	c.Stdin = in
	c.Stderr = io.MultiWriter(&out, stderr)
	runErr := c.Run()
	res["toc_entries"] = out.items
	if runErr != nil {
		return fmt.Errorf("pg_restore: %w", runErr)
	}
	return nil
}

// newestMajor picks the pg_restore that can read the archive: the archive's pg_dump version, or the job's
// configured version when that is newer.
func newestMajor(configured string, h dumpcheck.Header) string {
	if dumpcheck.Major(configured) > dumpcheck.Major(h.PgDumpVersion) {
		return configured
	}
	return h.PgDumpVersion
}

// tailCounter keeps the last 8 KiB of what it is fed and counts pg_restore's per-entry lines.
type tailCounter struct {
	mu    sync.Mutex
	items int
	line  []byte
}

func (t *tailCounter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, b := range p {
		if b != '\n' {
			if len(t.line) < 512 {
				t.line = append(t.line, b)
			}
			continue
		}
		if isTOCLine(t.line) {
			t.items++
		}
		t.line = t.line[:0]
	}
	return len(p), nil
}

// isTOCLine: pg_restore -v names every archive entry it works through, one line each
// ("processing item" when restoring into a database, "creating ..." / "processing data ..." when writing a script).
// ponytail: best effort — the count is for display only.
func isTOCLine(l []byte) bool {
	for _, p := range []string{"pg_restore: creating ", "pg_restore: processing data ", "pg_restore: executing ", "pg_restore: processing item "} {
		if bytes.HasPrefix(l, []byte(p)) {
			return true
		}
	}
	return false
}

// writeTLS puts the node's client certificate, key and the CA that signed the server certificate in a private temp
// directory (libpq reads them from files) and returns it.
func writeTLS(t *engine.TLS) (string, error) {
	dir, err := os.MkdirTemp("", "fridge-tls")
	if err != nil {
		return "", err
	}
	for name, content := range map[string]string{"client.crt": t.ClientCert, "client.key": t.ClientKey, "ca.crt": t.CA} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
	}
	return dir, nil
}
