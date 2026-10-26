package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fridge/internal/auth"
	"fridge/internal/pki"
	"fridge/internal/scram"
)

const (
	testUser = "alice"
	testPass = "correct horse battery"
)

// apiLog records what the fake server saw.
type apiLog struct {
	setupNeeded atomic.Bool
	mu          sync.Mutex
	reqs        []rec
}

func (l *apiLog) add(r rec) { l.mu.Lock(); l.reqs = append(l.reqs, r); l.mu.Unlock() }
func (l *apiLog) last(path string) (rec, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.reqs) - 1; i >= 0; i-- {
		if l.reqs[i].path == path {
			return l.reqs[i], true
		}
	}
	return rec{}, false
}

// fakeServer is an HTTPS server with a certificate from ca that speaks just enough of the API:
// /api/ca, /api/setup/status, SCRAM login for alice, /api/me, /api/auth/logout and /api/setup.
func fakeServer(t *testing.T, ca *pki.CA) (*httptest.Server, *apiLog) {
	t.Helper()
	salt := []byte("0123456789abcdef")
	stored, serverKey := scram.Keys(scram.SaltedPassword(testPass, salt, auth.MinIterations))
	var scramSrv *scram.Server
	log := &apiLog{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		log.add(rec{r.Method, r.URL.Path, r.Header.Get("Authorization"), string(b)})
		switch r.URL.Path {
		case "/api/ca":
			w.Write(ca.CertPEM())
		case "/api/databases/alice_shop/ca":
			w.Write([]byte("TENANT-CA\n"))
		case "/api/setup/status":
			w.Write([]byte(fmt.Sprintf(`{"needed":%v}`, log.setupNeeded.Load())))
		case "/api/auth/scram/start":
			var in struct {
				ClientFirst string `json:"client_first"`
			}
			json.Unmarshal(b, &in)
			var err error
			if scramSrv, _, err = scram.StartServer(in.ClientFirst, salt, auth.MinIterations, "servernonce0123456789"); err != nil {
				http.Error(w, `{"error":"bad"}`, 400)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"server_first": scramSrv.ServerFirst, "scram_id": "sid1"})
		case "/api/auth/scram/finish":
			var in struct {
				ClientFinal string `json:"client_final"`
				WantToken   bool   `json:"want_token"`
			}
			json.Unmarshal(b, &in)
			sfinal, err := scramSrv.Finish(in.ClientFinal, stored, serverKey)
			if err != nil || !in.WantToken {
				w.WriteHeader(401)
				w.Write([]byte(`{"error":"bad credentials"}`))
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"server_final": sfinal, "session_id": "SESSION-1", "user": map[string]string{"username": testUser, "role": "admin"}})
		case "/api/me":
			if r.Header.Get("Authorization") == "" {
				w.WriteHeader(401)
				w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			w.Write([]byte(`{"id":1,"username":"alice","role":"admin"}`))
		case "/api/auth/logout":
			w.WriteHeader(204)
		case "/api/setup":
			w.WriteHeader(201)
			w.Write([]byte(`{"id":1,"username":"alice","role":"admin"}`))
		default:
			http.NotFound(w, r)
		}
	})
	certPEM, keyPEM, err := ca.IssueNames([]string{"127.0.0.1", "localhost"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, log
}

func newTestCA(t *testing.T) *pki.CA {
	t.Helper()
	ca, err := pki.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// cliEnv points the CLI at url with an empty config directory and no ambient session.
func cliEnv(t *testing.T, url string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FRIDGE_URL", url)
	t.Setenv("FRIDGE_CONFIG_DIR", dir)
	t.Setenv("FRIDGE_SESSION", "")
	t.Setenv("FRIDGE_CA_FILE", "")
	return dir
}

func runWith(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	old := cliStdin
	cliStdin = strings.NewReader(stdin)
	defer func() { cliStdin = old }()
	var out, errb bytes.Buffer
	code := runCLI(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestLoginTOFU(t *testing.T) {
	ca := newTestCA(t)
	srv, _ := fakeServer(t, ca)
	dir := cliEnv(t, srv.URL)
	wantFP, err := fingerprint(ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}

	// the user declines: nothing is saved
	code, _, errs := runWith(t, "n\n"+testPass+"\n", "login", "--user", testUser, "--password-stdin")
	if code != 1 || !strings.Contains(errs, "SHA256 fingerprint: "+wantFP) || !strings.Contains(errs, "Trust this CA?") || !strings.Contains(errs, "CA not trusted") {
		t.Fatalf("declined: %d %q", code, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.pem")); err == nil {
		t.Fatal("a CA the user declined was saved")
	}
	if _, err := os.Stat(filepath.Join(dir, "session")); err == nil {
		t.Fatal("a session was saved without trust")
	}

	// the user agrees: CA and session are saved
	code, out, errs := runWith(t, "y\n"+testPass+"\n", "login", "--user", testUser, "--password-stdin")
	if code != 0 || !strings.Contains(out, "logged in as alice (admin)") || !strings.Contains(errs, wantFP) {
		t.Fatalf("accepted: %d %q %q", code, out, errs)
	}
	saved, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil || !bytes.Equal(saved, ca.CertPEM()) {
		t.Fatalf("ca.pem: %v", err)
	}

	// next time there is no question
	code, out, errs = runWith(t, testPass+"\n", "login", "--user", testUser, "--password-stdin")
	if code != 0 || strings.Contains(errs, "Trust this CA") || strings.Contains(errs, "fingerprint") || !strings.Contains(out, "logged in") {
		t.Fatalf("second login: %d %q %q", code, out, errs)
	}

	// a server with another CA is refused, and the saved CA is not replaced
	other, _ := fakeServer(t, newTestCA(t))
	t.Setenv("FRIDGE_URL", other.URL)
	code, _, errs = runWith(t, "y\n"+testPass+"\n", "login", "--user", testUser, "--password-stdin", "--yes")
	if code != 1 || !strings.Contains(errs, "not signed by the CA saved") {
		t.Fatalf("changed CA: %d %q", code, errs)
	}
	if again, _ := os.ReadFile(filepath.Join(dir, "ca.pem")); !bytes.Equal(again, ca.CertPEM()) {
		t.Fatal("the saved CA was replaced")
	}
}

func TestLoginCAFlagAndYes(t *testing.T) {
	ca := newTestCA(t)
	srv, _ := fakeServer(t, ca)
	dir := cliEnv(t, srv.URL)
	caFile := filepath.Join(t.TempDir(), "given.pem")
	os.WriteFile(caFile, ca.CertPEM(), 0o644)
	code, out, errs := runWith(t, testPass+"\n", "login", "--user", testUser, "--password-stdin", "--ca-file", caFile)
	if code != 0 || strings.Contains(errs, "Trust this CA") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "ca.pem")); err != nil || !bytes.Equal(b, ca.CertPEM()) {
		t.Fatal("--ca-file not saved")
	}
	// --yes skips the question
	os.Remove(filepath.Join(dir, "ca.pem"))
	code, _, errs = runWith(t, testPass+"\n", "login", "--user", testUser, "--password-stdin", "--yes")
	if code != 0 || strings.Contains(errs, "Trust this CA?") || !strings.Contains(errs, "fingerprint") {
		t.Fatalf("--yes: %d %q", code, errs)
	}
	// a file that is no certificate is refused
	bad := filepath.Join(t.TempDir(), "bad.pem")
	os.WriteFile(bad, []byte("nope"), 0o644)
	if code, _, _ := runWith(t, testPass+"\n", "login", "--user", testUser, "--password-stdin", "--ca-file", bad); code != 1 {
		t.Fatalf("bad CA file: %d", code)
	}
}

func TestLoginSCRAMSavesSession(t *testing.T) {
	ca := newTestCA(t)
	srv, log := fakeServer(t, ca)
	dir := cliEnv(t, srv.URL)
	if code, _, errs := runWith(t, testPass+"\n", "login", "--user", testUser, "--password-stdin", "--yes"); code != 0 {
		t.Fatalf("login: %q", errs)
	}
	b, err := os.ReadFile(filepath.Join(dir, "session"))
	if err != nil {
		t.Fatal(err)
	}
	var sf sessionFile
	if err := json.Unmarshal(b, &sf); err != nil || sf.Session != "SESSION-1" || sf.URL != srv.URL {
		t.Fatalf("session file %s", b)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(filepath.Join(dir, "session")); fi.Mode().Perm() != 0o600 {
			t.Errorf("session file mode %v", fi.Mode().Perm())
		}
	}
	// the password never went over the wire
	for _, p := range []string{"/api/auth/scram/start", "/api/auth/scram/finish"} {
		if r, _ := log.last(p); strings.Contains(r.body, testPass) || r.auth != "" {
			t.Errorf("%s: %+v", p, r)
		}
	}
	if code, out, errs := runWith(t, "", "whoami"); code != 0 || !strings.Contains(out, "alice (admin)") {
		t.Fatalf("whoami: %d %q %q", code, out, errs)
	}
	if r, _ := log.last("/api/me"); r.auth != "Bearer SESSION-1" {
		t.Fatalf("whoami sent %q", r.auth)
	}
	// a wrong password is reported, and no session is saved
	os.Remove(filepath.Join(dir, "session"))
	code, _, errs := runWith(t, "wrong password 123\n", "login", "--user", testUser, "--password-stdin")
	if code != 1 || !strings.Contains(errs, "bad credentials") {
		t.Fatalf("wrong password: %d %q", code, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "session")); err == nil {
		t.Fatal("session saved after a failed login")
	}
	// logout ends the session on the server and removes the file
	runWith(t, testPass+"\n", "login", "--user", testUser, "--password-stdin")
	if code, _, errs := runWith(t, "", "logout"); code != 0 {
		t.Fatalf("logout: %q", errs)
	}
	if r, ok := log.last("/api/auth/logout"); !ok || r.auth != "Bearer SESSION-1" {
		t.Fatalf("logout request %+v", r)
	}
	if _, err := os.Stat(filepath.Join(dir, "session")); err == nil {
		t.Fatal("session file survived logout")
	}
}

func TestSessionEnvOverrides(t *testing.T) {
	ca := newTestCA(t)
	srv, log := fakeServer(t, ca)
	dir := cliEnv(t, srv.URL)
	t.Setenv("FRIDGE_CA_FILE", filepath.Join(dir, "ca.pem"))
	os.WriteFile(filepath.Join(dir, "ca.pem"), ca.CertPEM(), 0o644)
	saveSession("filesession")
	if code, _, errs := runWith(t, "", "whoami"); code != 0 {
		t.Fatal(errs)
	}
	if r, _ := log.last("/api/me"); r.auth != "Bearer filesession" {
		t.Fatalf("file session: %q", r.auth)
	}
	t.Setenv("FRIDGE_SESSION", "xyz")
	if code, _, errs := runWith(t, "", "whoami"); code != 0 {
		t.Fatal(errs)
	}
	if r, _ := log.last("/api/me"); r.auth != "Bearer xyz" {
		t.Fatalf("env session: %q", r.auth)
	}
	// a session saved for another server is never sent to this one
	t.Setenv("FRIDGE_SESSION", "")
	b, _ := json.Marshal(sessionFile{"https://elsewhere.example", "foreign"})
	os.WriteFile(filepath.Join(dir, "session"), b, 0o600)
	code, _, errs := runWith(t, "", "whoami")
	if r, _ := log.last("/api/me"); code != 1 || r.auth != "" || !strings.Contains(errs, "fridge login") {
		t.Fatalf("foreign session: %d %q auth=%q", code, errs, r.auth)
	}
}

func TestNotLoggedInMessage(t *testing.T) {
	code, _, errs, _ := run(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}, "ls")
	if code != 1 || !strings.Contains(errs, "not logged in") || !strings.Contains(errs, "fridge login") {
		t.Fatalf("%d %q", code, errs)
	}
	code, _, errs, _ = run(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"setup required"}`))
	}, "ls")
	if code != 1 || !strings.Contains(errs, "fridge setup") || !strings.Contains(errs, "browser") {
		t.Fatalf("%d %q", code, errs)
	}
}

func TestNoWayToSkipTLSVerification(t *testing.T) {
	ca := newTestCA(t)
	srv, _ := fakeServer(t, ca)
	cliEnv(t, srv.URL)
	t.Setenv("FRIDGE_SESSION", "s")
	// the CA is neither saved nor given: ordinary commands fail instead of talking to an unverified server
	code, _, errs := runWith(t, "", "ls")
	if code != 1 || !strings.Contains(errs, "certificate") {
		t.Fatalf("%d %q", code, errs)
	}
	for _, f := range []string{"--insecure", "-k", "--skip-tls-verify"} {
		if code, _, _ := runWith(t, "", "ls", f); code != 2 {
			t.Errorf("%s accepted", f)
		}
	}
}

func TestSetupCommand(t *testing.T) {
	ca := newTestCA(t)
	srv, log := fakeServer(t, ca)
	cliEnv(t, srv.URL)
	code, out, errs := runWith(t, "my very long password\n", "setup", "--setup-token", "tok123", "--user", "alice", "--password-stdin", "--yes")
	if code != 0 || !strings.Contains(out, "fridge login") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	r, ok := log.last("/api/setup")
	if !ok {
		t.Fatal("no setup request")
	}
	if strings.Contains(r.body, "my very long password") {
		t.Fatal("the password was sent")
	}
	var in struct {
		SetupToken  string `json:"setup_token"`
		Username    string
		Credentials struct {
			Salt       []byte
			Iterations int
			StoredKey  []byte `json:"stored_key"`
			ServerKey  []byte `json:"server_key"`
		}
	}
	if err := json.Unmarshal([]byte(r.body), &in); err != nil {
		t.Fatal(err)
	}
	c := in.Credentials
	if in.SetupToken != "tok123" || in.Username != "alice" || c.Iterations != 600000 || len(c.Salt) != 16 || len(c.StoredKey) != 32 || len(c.ServerKey) != 32 {
		t.Fatalf("%+v", in)
	}
	// the keys are the ones the password gives
	stored, server := scram.Keys(scram.SaltedPassword("my very long password", c.Salt, c.Iterations))
	if !bytes.Equal(stored, c.StoredKey) || !bytes.Equal(server, c.ServerKey) {
		t.Fatal("credentials do not match the password")
	}
	// refusals before anything is sent
	for _, c := range [][]string{{"--user", "alice"}, {"--setup-token", "t"}, {"--setup-token", "t", "--user", "Bad_Name"}} {
		if code, _, _ := runWith(t, "my very long password\n", append([]string{"setup", "--password-stdin"}, c...)...); code == 0 {
			t.Errorf("%v accepted", c)
		}
	}
	if code, _, errs := runWith(t, "short\n", "setup", "--setup-token", "t", "--user", "alice", "--password-stdin"); code != 1 || !strings.Contains(errs, "12") {
		t.Errorf("short password: %d %q", code, errs)
	}
}

func TestLoginBeforeSetupSaysSetupNeeded(t *testing.T) {
	ca := newTestCA(t)
	srv, log := fakeServer(t, ca)
	cliEnv(t, srv.URL)
	log.setupNeeded.Store(true)
	code, _, errs := runWith(t, testPass+"\n", "login", "--user", testUser, "--password-stdin", "--yes")
	if code != 1 || !strings.Contains(errs, "fridge setup") || !strings.Contains(errs, "no accounts") {
		t.Fatalf("%d %q", code, errs)
	}
	if _, ok := log.last("/api/auth/scram/start"); ok {
		t.Fatal("a SCRAM round was started against a server with no accounts")
	}
}

func TestCAOfADatabase(t *testing.T) {
	ca := newTestCA(t)
	srv, log := fakeServer(t, ca)
	cliEnv(t, srv.URL)
	if code, _, errs := runWith(t, testPass+"\n", "login", "--user", testUser, "--password-stdin", "--yes"); code != 0 {
		t.Fatalf("login: %q", errs)
	}
	if code, out, errs := runWith(t, "", "ca"); code != 0 || out != string(ca.CertPEM()) {
		t.Fatalf("instance CA: %d %q %q", code, out, errs)
	}
	code, out, errs := runWith(t, "", "ca", "--db", "alice_shop")
	if code != 0 || out != "TENANT-CA\n" {
		t.Fatalf("database CA: %d %q %q", code, out, errs)
	}
	if r, ok := log.last("/api/databases/alice_shop/ca"); !ok || r.auth != "Bearer SESSION-1" {
		t.Fatalf("the request: %+v %v", r, ok)
	}
}
