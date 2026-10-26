package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"fridge/internal/pki"
)

func TestForwardRelaysBothWays(t *testing.T) {
	// an upstream that answers first (like a database greeting) and then echoes upper-cased input until EOF
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.Write([]byte("hello\n"))
				b, _ := io.ReadAll(c)
				c.Write([]byte(strings.ToUpper(string(b))))
			}()
		}
	}()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveForward(ctx, ln, plainUpstream(up.Addr().String())) }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	greeting := make([]byte, 6)
	if _, err := io.ReadFull(c, greeting); err != nil || string(greeting) != "hello\n" { // server speaks first: nothing is sniffed
		t.Fatalf("greeting %q %v", greeting, err)
	}
	payload := bytes.Repeat([]byte("abc"), 100000) // bigger than any buffer
	go func() { c.Write(payload); c.(*net.TCPConn).CloseWrite() }()
	got, err := io.ReadAll(c)
	if err != nil || string(got) != strings.ToUpper(string(payload)) {
		t.Fatalf("echo: %d bytes, %v", len(got), err)
	}

	// an unreachable upstream just resets the client; the forwarder keeps serving
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := dead.Addr().String()
	dead.Close()
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- serveForward(ctx2, ln2, plainUpstream(addr)) }()
	c2, err := net.Dial("tcp", ln2.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c2.SetDeadline(time.Now().Add(5 * time.Second))
	if n, err := c2.Read(make([]byte, 1)); err == nil || n != 0 {
		t.Fatalf("read from a dead upstream: %d %v", n, err)
	}

	cancel()
	cancel2()
	for _, d := range []chan error{done, done2} {
		select {
		case err := <-d:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("serveForward did not stop")
		}
	}
}

func TestForwardNeedsTarget(t *testing.T) {
	var b bytes.Buffer
	if code := runForward([]string{"--listen", "127.0.0.1:0"}, &b); code != 2 || !strings.Contains(b.String(), "--to") {
		t.Fatalf("%d %q", code, b.String())
	}
}

// fakePG answers the SSLRequest, then speaks TLS and requires a client certificate signed by (here: equal to) clientCA.
func fakePG(t *testing.T, serverCert tls.Certificate, clientCA *x509.CertPool) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	cfg := &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientCAs: clientCA, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				req := make([]byte, 8)
				if _, err := io.ReadFull(c, req); err != nil || !bytes.Equal(req, pgSSLRequest) {
					return
				}
				c.Write([]byte("S"))
				tc := tls.Server(c, cfg)
				if tc.Handshake() != nil {
					return
				}
				b, _ := io.ReadAll(tc)
				tc.Write([]byte("db:" + string(b)))
				tc.CloseWrite()
			}()
		}
	}()
	return ln
}

func TestForwardTLSPresentsTheNodeCertificate(t *testing.T) {
	ca, err := pki.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srvCert, srvKey, _ := ca.IssueNames([]string{"fridge-db-k1"}, time.Hour)
	srvPair, err := tls.X509KeyPair(srvCert, srvKey)
	if err != nil {
		t.Fatal(err)
	}
	cliCert, cliKey, _ := pki.SelfSignedClient("fridge-edge-k1", time.Hour)
	otherCert, otherKey, _ := pki.SelfSignedClient("fridge-edge-k2", time.Hour)
	trusted := x509.NewCertPool()
	trusted.AppendCertsFromPEM(cliCert)
	db := fakePG(t, srvPair, trusted)

	run := func(cert, key []byte, serverName string) (string, error) {
		cfg, err := clientTLS(string(cert), string(key), string(ca.CertPEM()), serverName)
		if err != nil {
			t.Fatal(err)
		}
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go serveForward(ctx, ln, pgTLSUpstream(db.Addr().String(), cfg))
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		c.Write([]byte("startup"))
		c.(*net.TCPConn).CloseWrite()
		b, err := io.ReadAll(c)
		return string(b), err
	}
	if got, err := run(cliCert, cliKey, "fridge-db-k1"); err != nil || got != "db:startup" {
		t.Fatalf("the node's own certificate: %q %v", got, err)
	}
	if got, _ := run(otherCert, otherKey, "fridge-db-k1"); got != "" {
		t.Fatalf("another node's certificate was served: %q", got)
	}
	if got, _ := run(cliCert, cliKey, "fridge-db-other"); got != "" {
		t.Fatalf("a server certificate for another name was trusted: %q", got)
	}
}

func TestForwardTLSNeedsMaterial(t *testing.T) {
	t.Setenv("FRIDGE_TLS_CERT", "")
	t.Setenv("FRIDGE_TLS_KEY", "")
	t.Setenv("FRIDGE_TLS_CA", "")
	var b bytes.Buffer
	if code := runForward([]string{"--listen", "127.0.0.1:0", "--to", "db:5432", "--tls"}, &b); code != 2 || !strings.Contains(b.String(), "certificate") {
		t.Fatalf("%d %q", code, b.String())
	}
}
