package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"fridge/internal/auth"
	"fridge/internal/scram"
)

const defaultURL = "https://localhost:8443"

func baseURL() string {
	if v := os.Getenv("FRIDGE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultURL
}

// configDir holds the saved CA and session (~/.config/fridge).
func configDir() string {
	if d := os.Getenv("FRIDGE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "fridge")
}

// caPath is the CA the client trusts besides the system roots: FRIDGE_CA_FILE, else the one `fridge login` saved.
func caPath() string {
	if v := os.Getenv("FRIDGE_CA_FILE"); v != "" {
		return v
	}
	if d := configDir(); d != "" {
		return filepath.Join(d, "ca.pem")
	}
	return ""
}

type sessionFile struct {
	URL     string `json:"url"`
	Session string `json:"session"`
}

func sessionPath() string {
	if d := configDir(); d != "" {
		return filepath.Join(d, "session")
	}
	return ""
}

// loadSession: FRIDGE_SESSION wins; otherwise the saved one, but only for the server it was issued by.
func loadSession() string {
	if v := os.Getenv("FRIDGE_SESSION"); v != "" {
		return v
	}
	b, err := os.ReadFile(sessionPath())
	if err != nil {
		return ""
	}
	var s sessionFile
	if json.Unmarshal(b, &s) != nil || s.URL != baseURL() {
		return ""
	}
	return s.Session
}

func saveFile(path string, data []byte, perm os.FileMode) error {
	if path == "" {
		return errors.New("no config directory (set FRIDGE_CONFIG_DIR)")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, perm)
}

func saveSession(sid string) error {
	b, _ := json.Marshal(sessionFile{baseURL(), sid})
	return saveFile(sessionPath(), b, 0o600)
}

// newHTTPClient trusts the system roots plus the CA file, if any. There is no way to switch verification off.
func newHTTPClient() (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if p := caPath(); p != "" {
		pemBytes, err := os.ReadFile(p)
		switch {
		case err == nil:
			if !pool.AppendCertsFromPEM(pemBytes) {
				return nil, fmt.Errorf("%s holds no certificate", p)
			}
		case !errors.Is(err, os.ErrNotExist) || os.Getenv("FRIDGE_CA_FILE") != "":
			return nil, err
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	// ponytail: one 5-minute client timeout for everything (create pulls images); split if short calls must fail fast.
	return &http.Client{Timeout: 5 * time.Minute, Transport: tr}, nil
}

var errNotLoggedIn = errors.New("not logged in — run: fridge login")

// roundTrip performs a request with the given session ("" = none) and returns status and body.
func roundTrip(method, path string, in any, session string) (int, []byte, error) {
	cl, err := newHTTPClient()
	if err != nil {
		return 0, nil, err
	}
	var rd io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL()+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if session != "" {
		req.Header.Set("Authorization", "Bearer "+session)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// call performs a request as the logged-in user; non-2xx becomes an error carrying the API's message.
func call(method, path string, in any) ([]byte, error) {
	status, b, err := roundTrip(method, path, in, loadSession())
	if err != nil {
		return nil, err
	}
	return b, apiError(status, b)
}

func apiError(status int, b []byte) error {
	if status < 400 {
		return nil
	}
	var e struct{ Error string }
	json.Unmarshal(b, &e)
	switch {
	case status == 401 && e.Error == "setup required":
		return fmt.Errorf("the server has no accounts yet — open %s in a browser, or run: fridge setup --setup-token T --user NAME", baseURL())
	case status == 401 && e.Error == "unauthorized": // no or ended session; a failed login says "invalid credentials"
		return errNotLoggedIn
	case e.Error != "":
		return errors.New(e.Error)
	}
	return fmt.Errorf("HTTP %d", status)
}

// ---- password input ----

// readLine reads one line from the CLI's stdin.
func (c *cli) readLine() (string, error) {
	if c.rd == nil {
		c.rd = bufio.NewReader(c.in)
	}
	s, err := c.rd.ReadString('\n')
	if err != nil && (err != io.EOF || s == "") {
		return "", err
	}
	return strings.TrimRight(s, "\r\n"), nil
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// password asks for a password without echo (stty -echo); if that is not possible it warns and reads with echo.
func (c *cli) password(prompt string, fromStdin bool) (string, error) {
	if fromStdin {
		return c.readLine()
	}
	fmt.Fprint(c.errw, prompt)
	restore := func() {}
	if f, ok := c.in.(*os.File); ok && isTerminal(f) {
		off := exec.Command("stty", "-echo")
		off.Stdin = f
		if err := off.Run(); err != nil {
			fmt.Fprintln(c.errw, "\n(warning: cannot turn off echo; the password will be visible)")
		} else {
			restore = func() {
				on := exec.Command("stty", "echo")
				on.Stdin = f
				on.Run()
				fmt.Fprintln(c.errw)
			}
		}
	}
	defer restore()
	return c.readLine()
}

// ---- CA trust on first use ----

func fingerprint(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("the server's CA is not a PEM certificate")
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		return "", err
	}
	sum := sha256.Sum256(block.Bytes)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":"), nil
}

// needsCA reports whether the server's certificate is not trusted yet (an unknown authority).
func needsCA() (bool, error) {
	cl, err := newHTTPClient()
	if err != nil {
		return false, err
	}
	resp, err := cl.Get(baseURL() + "/api/setup/status")
	if err == nil {
		resp.Body.Close()
		return false, nil
	}
	var unknown x509.UnknownAuthorityError
	if errors.As(err, &unknown) {
		return true, nil
	}
	return false, err
}

// fetchCA downloads the server's CA. This is the only request that does not verify the server;
// the user vouches for the CA by its fingerprint before it is saved.
func fetchCA() ([]byte, error) {
	cl := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}} //nolint:gosec // see above
	resp, err := cl.Get(baseURL() + "/api/ca")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET /api/ca: HTTP %d", resp.StatusCode)
	}
	return b, nil
}

func (c *cli) trustCA(yes bool) error {
	if !strings.HasPrefix(baseURL(), "https://") {
		return nil
	}
	need, err := needsCA()
	if err != nil {
		return err
	}
	if !need {
		return nil
	}
	if os.Getenv("FRIDGE_CA_FILE") != "" {
		return fmt.Errorf("the server's certificate is not signed by the CA in FRIDGE_CA_FILE (%s)", os.Getenv("FRIDGE_CA_FILE"))
	}
	if _, err := os.Stat(caPath()); err == nil {
		return fmt.Errorf("the server's certificate is not signed by the CA saved in %s; if the server's CA really changed, delete that file and log in again", caPath())
	}
	pemBytes, err := fetchCA()
	if err != nil {
		return err
	}
	fp, err := fingerprint(pemBytes)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.errw, "The server %s uses a certificate from its own CA.\nSHA256 fingerprint: %s\n", baseURL(), fp)
	if !yes {
		fmt.Fprint(c.errw, "Trust this CA? [y/N] ")
		ans, err := c.readLine()
		if err != nil || !strings.EqualFold(strings.TrimSpace(ans), "y") && !strings.EqualFold(strings.TrimSpace(ans), "yes") {
			return errors.New("CA not trusted")
		}
	}
	return saveFile(caPath(), pemBytes, 0o644)
}

// ---- commands ----

func nonce() string {
	b := make([]byte, 18)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (c *cli) login(args []string) error {
	var user, caFile string
	var pwStdin, yes bool
	if _, err := c.need("login", args, 0, func(fs *flag.FlagSet) {
		fs.StringVar(&user, "user", "", "account name")
		fs.BoolVar(&pwStdin, "password-stdin", false, "read the password from stdin")
		fs.BoolVar(&yes, "yes", false, "trust the server's CA without asking")
		fs.StringVar(&caFile, "ca-file", "", "trust this CA certificate file (saved for later)")
	}); err != nil {
		return err
	}
	if caFile != "" {
		b, err := os.ReadFile(caFile)
		if err != nil {
			return err
		}
		if _, err := fingerprint(b); err != nil {
			return fmt.Errorf("%s: %w", caFile, err)
		}
		if err := saveFile(filepath.Join(configDir(), "ca.pem"), b, 0o644); err != nil {
			return err
		}
	}
	if err := c.trustCA(yes); err != nil {
		return err
	}
	if err := c.requireSetupDone(); err != nil {
		return err
	}
	if user == "" {
		fmt.Fprint(c.errw, "Username: ")
		var err error
		if user, err = c.readLine(); err != nil {
			return err
		}
	}
	pw, err := c.password("Password: ", pwStdin)
	if err != nil {
		return err
	}
	sc := scram.NewClient(user, pw, nonce())
	status, b, err := roundTrip("POST", "/api/auth/scram/start", map[string]string{"client_first": sc.First()}, "")
	if err == nil {
		err = apiError(status, b)
	}
	if err != nil {
		return err
	}
	var st struct {
		ServerFirst string `json:"server_first"`
		ScramID     string `json:"scram_id"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	final, err := sc.Final(st.ServerFirst)
	if err != nil {
		return err
	}
	status, b, err = roundTrip("POST", "/api/auth/scram/finish", map[string]any{"scram_id": st.ScramID, "client_final": final, "want_token": true}, "")
	if err == nil {
		err = apiError(status, b)
	}
	if err != nil {
		return err
	}
	var fin struct {
		ServerFinal string `json:"server_final"`
		SessionID   string `json:"session_id"`
		User        struct{ Username, Role string }
	}
	if err := json.Unmarshal(b, &fin); err != nil {
		return err
	}
	if err := sc.Verify(fin.ServerFinal); err != nil {
		return fmt.Errorf("the server did not prove it knows the password: %w", err)
	}
	if fin.SessionID == "" {
		return errors.New("the server returned no session")
	}
	if err := saveSession(fin.SessionID); err != nil {
		return err
	}
	fmt.Fprintf(c.out, "logged in as %s (%s)\n", fin.User.Username, fin.User.Role)
	return nil
}

func (c *cli) logout(args []string) error {
	if _, err := c.need("logout", args, 0, nil); err != nil {
		return err
	}
	if loadSession() != "" {
		if _, err := call("POST", "/api/auth/logout", nil); err != nil && !errors.Is(err, errNotLoggedIn) {
			fmt.Fprintln(c.errw, "warning: could not end the session on the server:", err)
		}
	}
	if err := os.Remove(sessionPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Fprintln(c.out, "logged out")
	return nil
}

func (c *cli) whoami(args []string) error {
	if _, err := c.need("whoami", args, 0, nil); err != nil {
		return err
	}
	b, err := call("GET", "/api/me", nil)
	if err != nil {
		return err
	}
	var v struct{ Username, Role string }
	return c.show(b, &v, func() { fmt.Fprintf(c.out, "%s (%s) on %s\n", v.Username, v.Role, baseURL()) })
}

// setup creates the first admin from the command line, for a server with no browser at hand.
// The password is turned into SCRAM credentials here and never sent.
func (c *cli) setup(args []string) error {
	var token, user string
	var pwStdin, yes bool
	if _, err := c.need("setup", args, 0, func(fs *flag.FlagSet) {
		fs.StringVar(&token, "setup-token", "", "FRIDGE_SETUP_TOKEN of the server (required)")
		fs.StringVar(&user, "user", "", "name of the admin account (required)")
		fs.BoolVar(&pwStdin, "password-stdin", false, "read the password from stdin")
		fs.BoolVar(&yes, "yes", false, "trust the server's CA without asking")
	}); err != nil {
		return err
	}
	if token == "" || user == "" {
		return usageErr("setup: --setup-token and --user are required")
	}
	if !auth.ValidUsername(user) {
		return fmt.Errorf("account names are 3-16 characters: lower-case letters and digits, starting with a letter")
	}
	if err := c.trustCA(yes); err != nil {
		return err
	}
	pw, err := c.password("New password (12+ characters): ", pwStdin)
	if err != nil {
		return err
	}
	if len(pw) < 12 {
		return errors.New("the password must have at least 12 characters")
	}
	if !pwStdin {
		again, err := c.password("Repeat: ", false)
		if err != nil {
			return err
		}
		if again != pw {
			return errors.New("the passwords differ")
		}
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	stored, server := scram.Keys(scram.SaltedPassword(pw, salt, auth.MinIterations))
	status, b, err := roundTrip("POST", "/api/setup", map[string]any{"setup_token": token, "username": user,
		"credentials": map[string]any{"salt": salt, "iterations": auth.MinIterations, "stored_key": stored, "server_key": server}}, "")
	if err == nil {
		err = apiError(status, b)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "admin %s created — now run: fridge login --user %s\n", user, user)
	return nil
}

// requireSetupDone stops a login attempt on a server that has no accounts yet: SCRAM would only say "bad credentials".
func (c *cli) requireSetupDone() error {
	status, b, err := roundTrip("GET", "/api/setup/status", nil, "")
	if err != nil {
		return err
	}
	if err := apiError(status, b); err != nil {
		return err
	}
	var st struct{ Needed bool }
	if json.Unmarshal(b, &st) == nil && st.Needed {
		return apiError(401, []byte(`{"error":"setup required"}`))
	}
	return nil
}
