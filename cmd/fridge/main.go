package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // distroless has no zoneinfo; lets TZ pick the schedules' time zone

	"fridge/internal/alert"
	"fridge/internal/api"
	"fridge/internal/app"
	"fridge/internal/auth"
	"fridge/internal/container"
	"fridge/internal/pki"
	"fridge/internal/proxy"
	"fridge/internal/secret"
	"fridge/internal/store"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "server":
			if err := runServer(); err != nil {
				log.Fatal(err)
			}
			return
		case "healthcheck":
			os.Exit(healthcheck())
		}
	}
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func runServer() error {
	var missing []string
	for _, k := range []string{"FRIDGE_DATABASE_URL", "FRIDGE_SECRET_KEY"} {
		if os.Getenv(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required env: %s", strings.Join(missing, ", "))
	}
	box, err := secret.NewBox(os.Getenv("FRIDGE_SECRET_KEY"))
	if err != nil {
		return fmt.Errorf("FRIDGE_SECRET_KEY: %w", err)
	}
	listen, dataDir, traefikDir := env("FRIDGE_LISTEN", ":8080"), env("FRIDGE_DATA", "/data"), env("FRIDGE_TRAEFIK_DIR", "/traefik")
	network, apiBackend := env("FRIDGE_NETWORK", "fridge-net"), env("FRIDGE_API_BACKEND", "http://fridge:8080")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := openStore(ctx, os.Getenv("FRIDGE_DATABASE_URL"), 30*time.Second)
	if err != nil {
		return fmt.Errorf("metadata db: %w", err)
	}
	defer st.Close()
	if n, err := st.FailRunning(ctx, "interrupted by restart"); err != nil {
		return err
	} else if n > 0 {
		log.Printf("marked %d interrupted job(s) failed", n)
	}
	rt, err := container.New(network)
	if err != nil {
		return err
	}
	if err := rt.EnsureNetwork(ctx); err != nil {
		return fmt.Errorf("docker network %s: %w", network, err)
	}
	ca, err := pki.LoadOrCreate(filepath.Join(dataDir, "ca"))
	if err != nil {
		return fmt.Errorf("CA: %w", err)
	}
	for _, d := range []string{"dynamic", "certs"} {
		if err := os.MkdirAll(filepath.Join(traefikDir, d), 0o755); err != nil {
			return err
		}
	}

	a := &app.App{
		Store: st, Box: box, RT: rt, CA: ca, TraefikDir: traefikDir, APIBackend: apiBackend,
		HealthTimeout: 120 * time.Second, Alert: alert.Webhook(os.Getenv("FRIDGE_ALERT_WEBHOOK")),
	}
	if err := a.RecoverInterrupted(ctx); err != nil { // after FailRunning, before the scheduler loads
		return fmt.Errorf("recover interrupted nodes: %w", err)
	}
	allow, err := a.Allowlist(ctx)
	if err != nil {
		return err
	}
	if err := proxy.WriteBase(traefikDir, allow, apiBackend); err != nil {
		return err
	}
	a.Sched = app.NewScheduler(a)
	if err := a.Sched.Reload(ctx); err != nil {
		return err
	}
	key, _ := base64.StdEncoding.DecodeString(os.Getenv("FRIDGE_SECRET_KEY")) // validated by NewBox above
	au, err := auth.New(st, key, os.Getenv("FRIDGE_SETUP_TOKEN"))
	if err != nil {
		return err
	}
	if need, err := au.SetupNeeded(ctx); err != nil {
		return err
	} else if need && !au.TokenConfigured() {
		log.Printf("WARNING: no users yet and FRIDGE_SETUP_TOKEN is not set — setup is impossible until it is")
	}
	prefixes, err := trustedProxies(ctx, rt, network)
	if err != nil {
		return err
	}
	if err := a.Sched.AddFixed("@hourly", func() {
		if _, err := au.PurgeExpired(ctx); err != nil {
			log.Printf("purge expired sessions: %v", err)
		}
	}); err != nil {
		log.Printf("purge job: %v", err)
	}
	a.Sched.Start()

	srv := &http.Server{Addr: listen, Handler: api.New(a, au, api.Options{TrustedProxy: api.TrustCIDRs(prefixes)}), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Printf("fridge listening on %s", listen)

	select {
	case err = <-errc: // listen failed; still drain below
	case <-ctx.Done():
		log.Print("shutting down")
	}
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if serr := srv.Shutdown(sctx); serr != nil {
		log.Printf("http shutdown: %v", serr)
	}
	a.Sched.Stop()
	done := make(chan struct{})
	go func() { a.Wait(); close(done) }()
	select {
	case <-done:
	case <-sctx.Done():
		log.Print("background jobs still running; they will be marked failed on next start")
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// openStore retries until the metadata DB accepts connections (compose may start us first).
func openStore(ctx context.Context, url string, wait time.Duration) (*store.Store, error) {
	deadline := time.Now().Add(wait)
	for {
		st, err := store.Open(ctx, url)
		if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			return st, err
		}
		log.Printf("metadata db not ready: %v", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// healthcheck probes the local /readyz; the distroless image has no curl.
func healthcheck() int {
	_, port, err := net.SplitHostPort(env("FRIDGE_LISTEN", ":8080"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/readyz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, resp.Status)
		return 1
	}
	return 0
}

// subnetser is the part of the Docker client trustedProxies needs.
type subnetser interface {
	Subnets(ctx context.Context, names ...string) ([]netip.Prefix, error)
}

// trustedProxies: FRIDGE_TRUSTED_PROXY (CIDR list) wins; otherwise the fridge network's
// subnets. If those cannot be read, only loopback is trusted (nil prefixes).
func trustedProxies(ctx context.Context, rt subnetser, network string) ([]netip.Prefix, error) {
	if v := os.Getenv("FRIDGE_TRUSTED_PROXY"); v != "" {
		p, err := api.ParseCIDRs(v)
		if err != nil {
			return nil, fmt.Errorf("FRIDGE_TRUSTED_PROXY: %w", err)
		}
		return p, nil
	}
	p, err := rt.Subnets(ctx, network)
	if err != nil {
		log.Printf("trusted proxy: cannot read subnets of %s, trusting loopback only: %v", network, err)
		return nil, nil
	}
	return p, nil
}
