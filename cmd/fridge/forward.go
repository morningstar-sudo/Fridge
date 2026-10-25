package main

import (
	"context"
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
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *to == "" {
		fmt.Fprintln(stderr, "forward: --to is required")
		return 2
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(stderr, "forward:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("forward %s -> %s", ln.Addr(), *to)
	if err := serveForward(ctx, ln, *to); err != nil {
		fmt.Fprintln(stderr, "forward:", err)
		return 1
	}
	return 0
}

// serveForward relays until ctx ends; open connections are cut then.
func serveForward(ctx context.Context, ln net.Listener, to string) error {
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
			relay(c, to)
		}()
	}
}

func relay(c net.Conn, to string) {
	defer c.Close()
	up, err := net.DialTimeout("tcp", to, 5*time.Second)
	if err != nil {
		log.Printf("forward: dial %s: %v", to, err) // the database may still be starting; the client just sees a reset
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
