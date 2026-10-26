package mysqlproxy

import (
	"bytes"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"fridge/internal/pki"
)

// nativeProof is mysql_native_password: SHA1(pw) XOR SHA1(scramble + SHA1(SHA1(pw))).
func nativeProof(pw string, scr []byte) []byte {
	h1 := sha1.Sum([]byte(pw))
	h2 := sha1.Sum(h1[:])
	h := sha1.New()
	h.Write(scr)
	h.Write(h2[:])
	sum := h.Sum(nil)
	out := make([]byte, 20)
	for i := range out {
		out[i] = h1[i] ^ sum[i]
	}
	return out
}

type fakeDB struct {
	ln       net.Listener
	password string
	user     string
	gotDB    chan string
	plugin   string // the default authentication plugin the greeting announces
}

// startDB is a MySQL server in miniature: greeting, SSLRequest, TLS that demands the node's client certificate, a
// handshake response checked against password, OK/ERR, then an echo "command phase".
func startDB(t *testing.T, ca *pki.CA, clientCert []byte, host, user, password string, plugin ...string) *fakeDB {
	t.Helper()
	srvCert, srvKey, _ := ca.IssueNames([]string{host}, time.Hour)
	pair, err := tls.X509KeyPair(srvCert, srvKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(clientCert)
	cfg := &tls.Config{Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	db := &fakeDB{ln: ln, password: password, user: user, gotDB: make(chan string, 4), plugin: "caching_sha2_password"}
	if len(plugin) > 0 {
		db.plugin = plugin[0]
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go db.serve(c, cfg)
		}
	}()
	return db
}

func (d *fakeDB) serve(c net.Conn, cfg *tls.Config) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	scr := scramble()
	caps := uint32(ourCaps)
	writePacket(c, 0, greeting("8.4.0", 7, caps, scr, d.plugin))
	_, p, err := readPacket(c)
	if err != nil || len(p) != 32 {
		return
	}
	tc := tls.Server(c, cfg)
	if tc.Handshake() != nil {
		return
	}
	_, p, err = readPacket(tc)
	if err != nil {
		return
	}
	r, err := parseResponse(p)
	if err != nil {
		return
	}
	d.gotDB <- r.db
	proofOver, ack := scr, byte(3)
	if r.plugin != nativePlugin { // MySQL 8 style: the account uses native, so the database asks again, with a fresh scramble
		scr2 := scramble()
		writePacket(tc, 3, append(append(append([]byte{0xfe}, nativePlugin...), 0), append(scr2, 0)...))
		s, proof, err := readPacket(tc)
		if err != nil || s != 4 {
			return
		}
		r.auth, r.plugin, proofOver, ack = proof, nativePlugin, scr2, 5
	}
	if r.user != d.user || r.plugin != nativePlugin || !bytes.Equal(r.auth, nativeProof(d.password, proofOver)) || r.caps&capLenencAuth != 0 || r.caps&capConnectAttrs != 0 {
		writePacket(tc, ack, errPacket(1045, fmt.Sprintf("denied user=%q plugin=%q lenenc=%v attrs=%v proofok=%v", r.user, r.plugin, r.caps&capLenencAuth != 0, r.caps&capConnectAttrs != 0, bytes.Equal(r.auth, nativeProof(d.password, proofOver)))))
		return
	}
	writePacket(tc, ack, []byte{0, 0, 0, 2, 0, 0, 0})
	c.SetDeadline(time.Time{})
	buf := make([]byte, 64)
	for {
		n, err := tc.Read(buf)
		if n > 0 {
			tc.Write(append([]byte("echo:"), buf[:n]...))
		}
		if err != nil {
			return
		}
	}
}

type env struct {
	ca       *pki.CA
	router   *Router
	addr     string
	frontend *x509.CertPool
}

func newEnv(t *testing.T, users map[string]Backend) *env {
	t.Helper()
	ca, err := pki.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fc, fk, _ := ca.IssueNames([]string{"127.0.0.1", "mysql.test"}, time.Hour)
	pair, _ := tls.X509KeyPair(fc, fk)
	r := &Router{Timeout: 3 * time.Second}
	r.Set(&Config{Users: users, TLS: &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	go r.Serve(ctx, ln)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	return &env{ca: ca, router: r, addr: ln.Addr().String(), frontend: pool}
}

// backend is a Backend for a fake database whose node client certificate is cert/key.
func (e *env) backend(t *testing.T, db *fakeDB, cert, key []byte, serverName string) Backend {
	t.Helper()
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(e.ca.CertPEM())
	return Backend{Addr: db.ln.Addr().String(), TLS: &tls.Config{Certificates: []tls.Certificate{pair}, RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12}}
}

// login plays a MySQL client: TLS, handshake response, the proof for whatever challenge the router sends. It returns
// the packet that answers the handshake and, when it was OK, the connection for the command phase.
func (e *env) login(t *testing.T, user, password, db string, useTLS bool) (answer []byte, conn net.Conn) {
	t.Helper()
	c, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	_, gp, err := readPacket(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseGreeting(gp); err != nil {
		t.Fatal(err)
	}
	caps := uint32(ourCaps)
	if db == "" {
		caps &^= capConnectWithDB
	}
	body := binary.LittleEndian.AppendUint32(nil, caps)
	body = binary.LittleEndian.AppendUint32(body, 1<<24)
	body = append(body, 45)
	body = append(body, make([]byte, 23)...)
	var rw io.ReadWriter = c
	seq := byte(1)
	if useTLS {
		writePacket(c, seq, body)
		tc := tls.Client(c, &tls.Config{RootCAs: e.frontend, ServerName: "mysql.test", MinVersion: tls.VersionTLS12})
		if err := tc.Handshake(); err != nil {
			t.Fatalf("client TLS: %v", err)
		}
		rw = tc
		conn = tc
		seq = 2
	}
	resp := append(body, user...)
	resp = append(resp, 0)
	resp = append(resp, 20)
	resp = append(resp, make([]byte, 20)...) // a proof for the router's made-up scramble: ignored
	if db != "" {
		resp = append(append(resp, db...), 0)
	}
	resp = append(append(resp, nativePlugin...), 0)
	resp = append(resp, 0) // connection attributes: none
	writePacket(rw, seq, resp)
	seq, p, err := readPacket(rw)
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}
	if p[0] == 0xfe { // AuthSwitchRequest: answer with the proof for the challenge it carries
		plugin, rest, _ := strings.Cut(string(p[1:]), "\x00")
		if plugin != nativePlugin || len(rest) != 21 {
			t.Fatalf("auth switch: %q %d", plugin, len(rest))
		}
		writePacket(rw, seq+1, nativeProof(password, []byte(rest[:20])))
		if _, p, err = readPacket(rw); err != nil {
			t.Fatalf("after the proof: %v", err)
		}
	}
	return p, conn
}

func TestRouterRoutesByUserAndRelays(t *testing.T) {
	e := newEnv(t, nil)
	cc1, ck1, _ := pki.SelfSignedClient("edge-1", time.Hour)
	cc2, ck2, _ := pki.SelfSignedClient("edge-2", time.Hour)
	db1 := startDB(t, e.ca, cc1, "fridge-db-1", "alice_app", "alice-secret")
	db2 := startDB(t, e.ca, cc2, "fridge-db-2", "bob_app", "bob-secret")
	e.router.Set(&Config{TLS: e.router.cfg.Load().TLS, Users: map[string]Backend{
		"alice_app": e.backend(t, db1, cc1, ck1, "fridge-db-1"),
		"bob_app":   e.backend(t, db2, cc2, ck2, "fridge-db-2"),
	}})

	for _, tc := range []struct {
		user, pw string
		db       *fakeDB
	}{{"alice_app", "alice-secret", db1}, {"bob_app", "bob-secret", db2}} {
		ans, conn := e.login(t, tc.user, tc.pw, "shop", true)
		if ans[0] != 0 {
			t.Fatalf("%s: not OK: %q", tc.user, ans)
		}
		if got := <-tc.db.gotDB; got != "shop" {
			t.Fatalf("%s: the database saw db %q", tc.user, got)
		}
		conn.Write([]byte("hello"))
		buf := make([]byte, 32)
		n, err := conn.Read(buf)
		if err != nil || string(buf[:n]) != "echo:hello" {
			t.Fatalf("%s: command phase: %q %v", tc.user, buf[:n], err)
		}
	}
}

func TestRouterRefusals(t *testing.T) {
	e := newEnv(t, nil)
	cc1, ck1, _ := pki.SelfSignedClient("edge-1", time.Hour)
	cc2, ck2, _ := pki.SelfSignedClient("edge-2", time.Hour)
	db1 := startDB(t, e.ca, cc1, "fridge-db-1", "alice_app", "alice-secret")
	e.router.Set(&Config{TLS: e.router.cfg.Load().TLS, Users: map[string]Backend{
		"alice_app": e.backend(t, db1, cc1, ck1, "fridge-db-1"),
		// carol's backend is alice's database, but with another node's certificate: that database must refuse the handshake
		"carol_app": e.backend(t, db1, cc2, ck2, "fridge-db-1"),
		// dave's backend presents the right certificate but expects another server name
		"dave_app": e.backend(t, db1, cc1, ck1, "someone-else"),
	}})
	deny := func(label string, p []byte, wantCode uint16) {
		t.Helper()
		if len(p) < 3 || p[0] != 0xff || binary.LittleEndian.Uint16(p[1:]) != wantCode {
			t.Fatalf("%s: want ERR %d, got %v", label, wantCode, p)
		}
	}
	p, _ := e.login(t, "alice_app", "wrong-password", "", true)
	deny("wrong password", p, 1045)
	p, _ = e.login(t, "nobody", "x", "", true)
	deny("unknown user", p, 1045)
	p, _ = e.login(t, "alice_app", "alice-secret", "", false)
	deny("no TLS", p, 3159)
	p, _ = e.login(t, "carol_app", "alice-secret", "", true)
	deny("another node's certificate (TLS 1.3 reports it on the first read)", p, 2013)
	p, _ = e.login(t, "dave_app", "alice-secret", "", true)
	deny("the database's name does not match", p, 2026)
	if p, _ := e.login(t, "alice_app", "alice-secret", "", true); p[0] != 0 {
		t.Fatalf("the right credentials still work after the refusals: %v", p)
	}
}

func TestRouterWithoutConfigSaysSo(t *testing.T) {
	r := &Router{}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go r.Serve(t.Context(), ln)
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	readPacket(c)
	writePacket(c, 1, make([]byte, 32))
	_, p, err := readPacket(c)
	if err != nil || p[0] != 0xff || !strings.Contains(string(p), "starting") {
		t.Fatalf("%v %q", err, p)
	}
}

func TestParseBuildsAWorkingConfig(t *testing.T) {
	ca, _ := pki.LoadOrCreate(t.TempDir())
	fc, fk, _ := ca.IssueNames([]string{"mysql.test"}, time.Hour)
	cc, ck, _ := pki.SelfSignedClient("edge", time.Hour)
	doc := `{"frontend":{"cert":` + q(fc) + `,"key":` + q(fk) + `},"users":{"alice_app":{"addr":"db:3306","cert":` + q(cc) + `,"key":` + q(ck) + `,"ca":` + q(ca.CertPEM()) + `,"server_name":"db"}}}`
	cfg, err := Parse([]byte(doc))
	if err != nil || cfg.Users["alice_app"].Addr != "db:3306" || cfg.Users["alice_app"].TLS.ServerName != "db" || len(cfg.TLS.Certificates) != 1 {
		t.Fatalf("%+v %v", cfg, err)
	}
	for _, bad := range []string{`{`, `{"frontend":{}}`, strings.Replace(doc, `"ca":`, `"ca2":`, 1)} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %.40s", bad)
		}
	}
}

func q(b []byte) string {
	out, _ := json.Marshal(string(b))
	return string(out)
}

// MariaDB announces mysql_native_password itself: the greeting's scramble is the one the client proves, with no switch.
func TestRouterNativeDefaultDatabase(t *testing.T) {
	e := newEnv(t, nil)
	cc, ck, _ := pki.SelfSignedClient("edge-1", time.Hour)
	db := startDB(t, e.ca, cc, "fridge-db-1", "alice_app", "alice-secret", nativePlugin)
	e.router.Set(&Config{TLS: e.router.cfg.Load().TLS, Users: map[string]Backend{"alice_app": e.backend(t, db, cc, ck, "fridge-db-1")}})
	if p, _ := e.login(t, "alice_app", "alice-secret", "", true); p[0] != 0 {
		t.Fatalf("not OK: %q", p)
	}
	if p, _ := e.login(t, "alice_app", "wrong", "", true); p[0] != 0xff {
		t.Fatalf("a wrong password was accepted: %q", p)
	}
}
