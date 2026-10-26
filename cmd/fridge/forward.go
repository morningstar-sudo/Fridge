package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// runForward is the per-database edge sidecar: it accepts TCP on --listen and relays every connection to --to.
// It sits on the database's private network and on the shared edge network, so the proxy and Fridge reach the
// database without ever joining its network. It knows nothing about the protocol (TLS passes through untouched).
func runForward(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("forward", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", ":5432", "address to listen on")
	to := fs.String("to", "", "host:port to relay to (required)")
	mode := fs.String("upstream", "plain", "how to speak to the target: plain, pg (PostgreSQL's SSLRequest, then TLS) or tls (TLS at once); the TLS modes present the client certificate in $FRIDGE_TLS_CERT/$FRIDGE_TLS_KEY and trust $FRIDGE_TLS_CA")
	name := fs.String("server-name", "", "with a TLS upstream: the name the target's certificate must carry (default: the host of --to)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *to == "" {
		fmt.Fprintln(stderr, "forward: --to is required")
		return 2
	}
	up := plainUpstream(*to)
	if *mode != "plain" && *mode != "pg" && *mode != "tls" {
		fmt.Fprintf(stderr, "forward: --upstream %q is not plain, pg or tls\n", *mode)
		return 2
	}
	if *mode != "plain" {
		host, _, err := net.SplitHostPort(*to)
		if err != nil {
			fmt.Fprintln(stderr, "forward: --to:", err)
			return 2
		}
		if *name == "" {
			*name = host
		}
		cfg, err := clientTLS(os.Getenv("FRIDGE_TLS_CERT"), os.Getenv("FRIDGE_TLS_KEY"), os.Getenv("FRIDGE_TLS_CA"), *name)
		if err != nil {
			fmt.Fprintln(stderr, "forward:", err)
			return 2
		}
		if *mode == "pg" {
			up = pgTLSUpstream(*to, cfg)
		} else {
			up = tlsUpstream(*to, cfg)
		}
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(stderr, "forward:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("forward %s -> %s", ln.Addr(), *to)
	if err := serveForward(ctx, ln, up); err != nil {
		fmt.Fprintln(stderr, "forward:", err)
		return 1
	}
	return 0
}

// serveForward relays until ctx ends; open connections are cut then.
func serveForward(ctx context.Context, ln net.Listener, up upstream) error {
	var wg sync.WaitGroup
	conns := map[net.Conn]struct{}{}
	var mu sync.Mutex
	go func() {
		<-ctx.Done()
		ln.Close()
		mu.Lock()
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
	}()
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		mu.Lock()
		conns[c] = struct{}{}
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				mu.Lock()
				delete(conns, c)
				mu.Unlock()
			}()
			relay(c, up)
		}()
	}
}

// upstream opens the connection to the database for one client.
type upstream func() (net.Conn, error)

func plainUpstream(to string) upstream {
	return func() (net.Conn, error) { return net.DialTimeout("tcp", to, 5*time.Second) }
}

// tlsUpstream connects with TLS from the first byte (Redis, MongoDB): plain TCP in, TLS with the client certificate out.
func tlsUpstream(to string, cfg *tls.Config) upstream {
	return func() (net.Conn, error) {
		c, err := (&net.Dialer{Timeout: 5 * time.Second}).Dial("tcp", to)
		if err != nil {
			return nil, err
		}
		tc := tls.Client(c, cfg)
		c.SetDeadline(time.Now().Add(10 * time.Second))
		if err := tc.Handshake(); err != nil {
			c.Close()
			return nil, err
		}
		c.SetDeadline(time.Time{})
		return tc, nil
	}
}

// pgSSLRequest is the 8-byte message that asks a PostgreSQL server to switch to TLS.
var pgSSLRequest = []byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}

// pgTLSUpstream connects the way libpq does for sslmode=verify-full with a client certificate. The client's own
// bytes (already out of TLS: the proxy terminated it) are relayed through the encrypted connection afterwards.
func pgTLSUpstream(to string, cfg *tls.Config) upstream {
	return func() (net.Conn, error) {
		c, err := net.DialTimeout("tcp", to, 5*time.Second)
		if err != nil {
			return nil, err
		}
		c.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := c.Write(pgSSLRequest); err != nil {
			c.Close()
			return nil, err
		}
		var b [1]byte
		if _, err := io.ReadFull(c, b[:]); err != nil || b[0] != 'S' {
			c.Close()
			return nil, fmt.Errorf("the database refused TLS (answer %q, %v)", b[0], err)
		}
		tc := tls.Client(c, cfg)
		if err := tc.Handshake(); err != nil {
			c.Close()
			return nil, err
		}
		c.SetDeadline(time.Time{})
		return tc, nil
	}
}

func clientTLS(certPEM, keyPEM, caPEM, serverName string) (*tls.Config, error) {
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("client certificate: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, errors.New("FRIDGE_TLS_CA holds no certificate")
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, RootCAs: roots, ServerName: serverName, MinVersion: tls.VersionTLS12}, nil
}

func relay(c net.Conn, open upstream) {
	defer c.Close()
	up, err := open()
	if err != nil {
		log.Printf("forward: upstream: %v", err) // the database may still be starting; the client just sees a reset
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite() // pass the end of one direction on without cutting the other
		}
		done <- struct{}{}
	}
	go pipe(up, c)
	go pipe(c, up)
	<-done
	<-done
}
