package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"fridge/internal/mysqlproxy"
)

// runMySQLRouter is the front door of every MySQL/MariaDB database: it accepts clients on --listen and sends each to the
// database its user belongs to (see internal/mysqlproxy). The routes come from Fridge (--config-url, polled with the
// bearer token in $FRIDGE_PROVIDER_TOKEN, the same one Traefik uses) or, for tests, from --config-file.
func runMySQLRouter(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("mysql-router", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", ":3306", "address to listen on")
	url := fs.String("config-url", "", "Fridge's configuration endpoint (polled)")
	file := fs.String("config-file", "", "read the configuration from this file once instead")
	poll := fs.Duration("poll", 5*time.Second, "how often to poll --config-url")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if (*url == "") == (*file == "") {
		fmt.Fprintln(stderr, "mysql-router: give exactly one of --config-url and --config-file")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := &mysqlproxy.Router{}
	if *file != "" {
		b, err := os.ReadFile(*file)
		if err == nil {
			var cfg *mysqlproxy.Config
			if cfg, err = mysqlproxy.Parse(b); err == nil {
				r.Set(cfg)
			}
		}
		if err != nil {
			fmt.Fprintln(stderr, "mysql-router:", err)
			return 1
		}
	} else {
		go pollConfig(ctx, r, *url, os.Getenv("FRIDGE_PROVIDER_TOKEN"), *poll)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(stderr, "mysql-router:", err)
		return 1
	}
	log.Printf("mysql router on %s", ln.Addr())
	if err := r.Serve(ctx, ln); err != nil {
		fmt.Fprintln(stderr, "mysql-router:", err)
		return 1
	}
	return 0
}

// pollConfig keeps the router's configuration current; a failed poll leaves the last good one in place.
func pollConfig(ctx context.Context, r *mysqlproxy.Router, url, token string, every time.Duration) {
	cl := &http.Client{Timeout: 10 * time.Second}
	var last string
	for {
		if err := fetchOnce(ctx, cl, r, url, token, &last); err != nil {
			log.Printf("mysql router: config: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func fetchOnce(ctx context.Context, cl *http.Client, r *mysqlproxy.Router, url, token string, last *string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if string(b) == *last {
		return nil
	}
	cfg, err := mysqlproxy.Parse(b)
	if err != nil {
		return err
	}
	r.Set(cfg)
	*last = string(b)
	return nil
}
