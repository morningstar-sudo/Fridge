package mysqlproxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Backend is where one user's connections go: the database's address on the shared database network and the TLS
// configuration that database accepts (its node's client certificate, the CA of its server certificate, its name).
type Backend struct {
	Addr string
	TLS  *tls.Config
}

// Config is the router's whole state. Users maps a MySQL user name to its backend; TLS is the certificate clients see
// (one for all, since there is no SNI to choose by).
type Config struct {
	Users map[string]Backend
	TLS   *tls.Config
}

type Router struct {
	cfg     atomic.Pointer[Config]
	Timeout time.Duration // the whole handshake; 0 = 15s
	connID  atomic.Uint32
}

// Set replaces the configuration; connections in progress keep the one they started with.
func (r *Router) Set(c *Config) { r.cfg.Store(c) }

// Serve accepts clients until ctx ends.
func (r *Router) Serve(ctx context.Context, ln net.Listener) error {
	var wg sync.WaitGroup
	go func() { <-ctx.Done(); ln.Close() }()
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			stop := context.AfterFunc(ctx, func() { c.Close() })
			defer stop()
			r.handle(c)
		}()
	}
}

func (r *Router) handle(c net.Conn) {
	defer c.Close()
	cfg := r.cfg.Load()
	timeout := r.Timeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	c.SetDeadline(time.Now().Add(timeout))
	var client net.Conn = c
	fail := func(seq byte, code uint16, msg string) {
		writePacket(client, seq, errPacket(code, msg))
	}

	scr := scramble()
	if err := writePacket(c, 0, greeting("8.0.0-fridge", r.connID.Add(1), ourCaps, scr, nativePlugin)); err != nil {
		return
	}
	seq, p, err := readPacket(c)
	if err != nil {
		return
	}
	if cfg == nil || cfg.TLS == nil {
		fail(seq+1, 1053, "Fridge is starting, try again in a moment")
		return
	}
	if len(p) == 32 { // SSLRequest: the client wants TLS, as it must
		tc := tls.Server(c, cfg.TLS)
		if err := tc.Handshake(); err != nil {
			return
		}
		client = tc
		if seq, p, err = readPacket(client); err != nil {
			return
		}
	} else {
		fail(seq+1, 3159, "Fridge requires a TLS connection (use --ssl-mode=REQUIRED or stricter)")
		return
	}
	resp, err := parseResponse(p)
	if err != nil {
		fail(seq+1, 1043, "bad handshake")
		return
	}
	denied := func() {
		fail(seq+1, 1045, fmt.Sprintf("Access denied for user '%s'", resp.user))
	}
	be, ok := cfg.Users[resp.user]
	if !ok {
		denied()
		return
	}

	// the database: its real greeting, then TLS with this node's client certificate
	up, err := net.DialTimeout("tcp", be.Addr, 5*time.Second)
	if err != nil {
		log.Printf("mysql router: dial %s: %v", be.Addr, err)
		fail(seq+1, 2003, "the database is not reachable")
		return
	}
	defer up.Close()
	up.SetDeadline(time.Now().Add(timeout))
	_, gp, err := readPacket(up)
	if err != nil {
		fail(seq+1, 2013, "the database did not answer")
		return
	}
	g, err := parseGreeting(gp)
	if err != nil || g.caps&capSSL == 0 || len(g.scramble) != 20 {
		log.Printf("mysql router: %s: %v (caps %#x)", be.Addr, err, g.caps)
		fail(seq+1, 2026, "the database does not offer TLS")
		return
	}
	if err := writePacket(up, 1, backendSSLRequest(resp, g.caps)); err != nil {
		return
	}
	ut := tls.Client(up, be.TLS)
	if err := ut.Handshake(); err != nil {
		log.Printf("mysql router: TLS to %s: %v", be.Addr, err)
		fail(seq+1, 2026, "could not secure the connection to the database")
		return
	}

	// The client proves its password against the database's own scramble. A database whose default plugin is not
	// native (MySQL 8) answers the first response with an AuthSwitchRequest carrying the scramble to prove against, so
	// then the first response is empty and the client is asked once; otherwise the greeting's scramble is the challenge.
	cseq, bseq := seq, byte(2) // the last packet number each side used
	challenge := func(scr []byte) ([]byte, bool) {
		req := append(append([]byte{0xfe}, nativePlugin...), 0)
		if err := writePacket(client, cseq+1, append(req, append(scr, 0)...)); err != nil {
			return nil, false
		}
		s, proof, err := readPacket(client)
		cseq = s
		return proof, err == nil
	}
	var first []byte
	plugin := nativePlugin
	if g.plugin == nativePlugin {
		var ok bool
		if first, ok = challenge(g.scramble); !ok {
			return
		}
	} else {
		plugin = g.plugin
	}
	if err := writePacket(ut, bseq, backendResponse(resp, g.caps, first, plugin)); err != nil {
		return
	}
	var ans []byte
	for {
		s, a, err := readPacket(ut)
		if err != nil {
			fail(cseq+1, 2013, "the database did not answer")
			return
		}
		bseq = s
		if len(a) > 0 && a[0] == 0xfe { // AuthSwitchRequest: plugin name, NUL, scramble (and a NUL)
			name, data, _ := strings.Cut(string(a[1:]), "\x00")
			if name != nativePlugin || len(data) < 20 {
				fail(cseq+1, 1045, fmt.Sprintf("Access denied for user '%s' (unsupported authentication plugin)", resp.user))
				return
			}
			proof, ok := challenge([]byte(data[:20]))
			if !ok {
				return
			}
			bseq++
			if err := writePacket(ut, bseq, proof); err != nil {
				return
			}
			continue
		}
		ans = a
		break
	}
	if len(ans) == 0 || (ans[0] != 0x00 && ans[0] != 0xff) {
		fail(cseq+1, 1045, fmt.Sprintf("Access denied for user '%s' (unsupported authentication exchange)", resp.user))
		return
	}
	if err := writePacket(client, cseq+1, ans); err != nil || ans[0] == 0xff {
		return
	}

	// command phase: sequence numbers restart with every command on both sides, so bytes just flow
	c.SetDeadline(time.Time{})
	up.SetDeadline(time.Time{})
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(ut, client)
	go pipe(client, ut)
	<-done
	<-done
}
