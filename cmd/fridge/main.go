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
	"fridge/internal/audit"
	"fridge/internal/auth"
	"fridge/internal/pki"
	"fridge/internal/proxy"
	"fridge/internal/registry"
	"fridge/internal/secret"
	"fridge/internal/store"
	"fridge/internal/swarm"
	"fridge/internal/web"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "server":
			if err := runServer(); err != nil {
				log.Fatal(err)
			}
			return
		case "job":
			os.Exit(runJob(os.Args[2:], os.Stdout, os.Stderr))
		case "forward":
			os.Exit(runForward(os.Args[2:], os.Stderr))
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
	for _, k := range []string{"FRIDGE_DATABASE_URL", "FRIDGE_SECRET_KEY", "FRIDGE_SERVER_IMAGE", "FRIDGE_JOB_IMAGE", "FRIDGE_PROVIDER_TOKEN"} {
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
	listen, dataDir, providerListen := env("FRIDGE_LISTEN", ":8080"), env("FRIDGE_DATA", "/data"), env("FRIDGE_PROVIDER_LISTEN", ":8081")
	edgeNet, egressNet, apiBackend := env("FRIDGE_EDGE_NETWORK", "fridge_edge"), env("FRIDGE_EGRESS_NETWORK", "fridge_egress"), env("FRIDGE_API_BACKEND", "http://fridge:8080")

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
	rt, err := swarm.New(swarm.Config{EdgeNetwork: edgeNet, EgressNetwork: egressNet, ServerImage: os.Getenv("FRIDGE_SERVER_IMAGE")})
	if err != nil {
		return err
	}
	defer rt.Close()
	if err := rt.Ping(ctx); err != nil {
		return fmt.Errorf("swarm: %w", err)
	}
	if err := rt.EnsureNetworks(ctx); err != nil {
		return err
	}
	ca, err := pki.LoadOrCreate(filepath.Join(dataDir, "ca"))
	if err != nil {
		return fmt.Errorf("CA: %w", err)
	}

	a := &app.App{
		Store: st, Box: box, RT: rt, CA: ca, APIBackend: apiBackend,
		HealthTimeout: 120 * time.Second, Alert: alert.Webhook(os.Getenv("FRIDGE_ALERT_WEBHOOK")),
		JobImage: os.Getenv("FRIDGE_JOB_IMAGE"), EgressNetwork: egressNet, Img: rt,
		Audit: &audit.Logger{Store: st}, Registry: &registry.Client{},
	}
	if err := a.RecoverInterrupted(ctx); err != nil { // after FailRunning, before the scheduler loads
		return fmt.Errorf("recover interrupted nodes: %w", err)
	}
	a.WebCert, a.WebKey = filepath.Join(dataDir, "web", "web.crt"), filepath.Join(dataDir, "web", "web.key")
	webNames := webCertNames(os.Getenv("FRIDGE_PUBLIC_HOSTS"))
	if _, err := ensureWebCert(ca, a.WebCert, a.WebKey, webNames); err != nil {
		return fmt.Errorf("web certificate: %w", err)
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
	prefixes, err := trustedProxies(ctx, rt, edgeNet)
	if err != nil {
		return err
	}
	// Swarm drift: a sidecar that vanished, leftovers of failed nodes, a database service that is gone.
	reconcile := func() {
		cctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
		defer cancel()
		if err := a.Reconcile(cctx); err != nil {
			log.Printf("reconcile: %v", err)
		}
	}
	go reconcile()
	if err := a.Sched.AddFixed("*/5 * * * *", reconcile); err != nil {
		log.Printf("reconcile job: %v", err)
	}
	if err := a.Sched.AddFixed("@hourly", func() {
		if _, err := au.PurgeExpired(ctx); err != nil {
			log.Printf("purge expired sessions: %v", err)
		}
	}); err != nil {
		log.Printf("purge job: %v", err)
	}
	if err := a.Sched.AddFixed("23 3 * * *", func() { // the audit table refuses deleting anything younger than a year
		if n, err := st.PurgeAudit(ctx, time.Now().AddDate(0, 0, -365)); err != nil {
			log.Printf("purge audit log: %v", err)
		} else if n > 0 {
			log.Printf("purged %d audit entries older than a year", n)
		}
	}); err != nil {
		log.Printf("audit purge job: %v", err)
	}
	if err := a.Sched.AddFixed("17 4 * * *", func() {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		if err := a.CheckImageUpdates(cctx); err != nil {
			log.Printf("check image updates: %v", err)
		}
	}); err != nil {
		log.Printf("image update job: %v", err)
	}
	// Traefik polls ProxyConfig, which reads the web certificate files each time: a renewal needs nothing more.
	if err := a.Sched.AddFixed("41 3 * * *", func() {
		changed, err := ensureWebCert(ca, a.WebCert, a.WebKey, webNames)
		if err != nil {
			log.Printf("renew web certificate: %v", err)
			if a.Alert != nil {
				a.Alert(ctx, "renewing the web certificate failed: "+err.Error())
			}
		} else if changed {
			log.Print("web certificate renewed")
		}
	}); err != nil {
		log.Printf("web certificate job: %v", err)
	}
	a.Sched.Start()

	srv := &http.Server{Addr: listen, Handler: api.New(a, au, api.Options{TrustedProxy: api.TrustCIDRs(prefixes), Web: web.Handler()}), ReadHeaderTimeout: 10 * time.Second}
	// Traefik's configuration endpoint: only on the edge network, bearer token, carries the private keys of the routes.
	prov := &http.Server{Addr: providerListen, ReadHeaderTimeout: 10 * time.Second,
		Handler: proxy.Handler(os.Getenv("FRIDGE_PROVIDER_TOKEN"), func(r *http.Request) (proxy.Config, error) { return a.ProxyConfig(r.Context()) })}
	errc := make(chan error, 2)
	go func() { errc <- srv.ListenAndServe() }()
	go func() { errc <- prov.ListenAndServe() }()
	log.Printf("fridge listening on %s, traefik config on %s", listen, providerListen)

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
	prov.Shutdown(sctx)
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
	SubnetsOf(ctx context.Context, names ...string) ([]netip.Prefix, error)
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
	p, err := rt.SubnetsOf(ctx, network)
	if err != nil {
		log.Printf("trusted proxy: cannot read subnets of %s, trusting loopback only: %v", network, err)
		return nil, nil
	}
	return p, nil
}
