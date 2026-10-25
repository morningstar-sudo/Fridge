package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
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
	go func() { done <- serveForward(ctx, ln, up.Addr().String()) }()

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
	go func() { done2 <- serveForward(ctx2, ln2, addr) }()
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
