package app

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"fridge/internal/store"
)

func TestCreateDatabaseHappyPath(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx := context.Background()
	d, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "abc", Domain: "ABC.com "})
	if err != nil {
		t.Fatal(err)
	}
	if d.Domain != "abc.com" || d.Version != "17" || d.Status != "running" || d.User != "root1_abc" {
		t.Fatalf("db = %+v", d)
	}
	got, _ := a.Store.GetDatabase(ctx, d.ID)
	if got.Status != "running" || got.ContainerID == "" {
		t.Fatalf("stored = %+v", got)
	}
	if len(rt.runs) != 1 || rt.runs[0].Name != d.Volume || !nodeNameRe.MatchString(d.Volume) || rt.runs[0].Image != "postgres:17" {
		t.Fatalf("runs = %+v", rt.runs)
	}
	pw := ""
	for _, e := range rt.runs[0].Env {
		if v, ok := strings.CutPrefix(e, "POSTGRES_PASSWORD="); ok {
			pw = v
		}
	}
	if pw == "" || strings.Contains(d.PasswordEnc, pw) {
		t.Fatalf("password handling wrong: pw=%q enc=%q", pw, d.PasswordEnc)
	}
	cfg, err := a.ProxyConfig(ctx)
	if err != nil || len(cfg.DBs) != 1 {
		t.Fatalf("proxy config: %+v, %v", cfg, err)
	}
	if r := cfg.DBs[0]; r.Domain != "abc.com" || r.Backend != "fridge-edge-"+d.NodeKey+":5432" || r.EntryPoint != "pg" || !strings.Contains(r.CertPEM, "CERTIFICATE") || !strings.Contains(r.KeyPEM, "PRIVATE KEY") {
		t.Fatalf("route = %+v", r)
	}
	if enc, _ := a.Store.ListRoutes(ctx); len(enc) != 1 || strings.Contains(enc[0].KeyEnc, "PRIVATE KEY") {
		t.Fatalf("the route key must be stored encrypted: %+v", enc)
	}
	cs, err := a.ConnString(d)
	if err != nil || !strings.Contains(cs, "@abc.com:5432/root1_abc?sslmode=require") {
		t.Fatalf("conn = %q, %v", cs, err)
	}
}

func TestCreateDatabaseValidation(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ok := CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "abc", Domain: "abc.com"}
	cases := map[string]func(*CreateRequest){
		"version":    func(r *CreateRequest) { r.Version = "99" },
		"name case":  func(r *CreateRequest) { r.Name = "Abc" },
		"name inj":   func(r *CreateRequest) { r.Name = "a;rm" },
		"domain":     func(r *CreateRequest) { r.Domain = "localhost" },
		"domain inj": func(r *CreateRequest) { r.Domain = "a.com`)||x" },
		"domain dot": func(r *CreateRequest) { r.Domain = "a..com" },
		"acme":       func(r *CreateRequest) { r.TLSMode = "acme" },
		"tls":        func(r *CreateRequest) { r.TLSMode = "weird" },
		"engine":     func(r *CreateRequest) { r.Engine = "mysql" },
	}
	for name, mut := range cases {
		r := ok
		mut(&r)
		if _, err := createDB(a, context.Background(), r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if len(rt.runs) != 0 {
		t.Fatalf("runs = %v", rt.runs)
	}
}

func TestCreateDatabaseRollbackOnUnhealthy(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx := context.Background()
	rt.healthErr = errors.New("unhealthy")
	req := CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "abc", Domain: "abc.com"}
	if _, err := createDB(a, ctx, req); err == nil {
		t.Fatal("want error")
	}
	dbs, _ := a.Store.ListDatabases(ctx)
	if len(dbs) != 1 || dbs[0].Status != "error" {
		t.Fatalf("dbs = %+v", dbs)
	}
	if v := dbs[0].Volume; len(rt.removes) != 1 || rt.removes[0] != (removeCall{"ctr-" + v, v}) || !nodeNameRe.MatchString(v) {
		t.Fatalf("removes = %+v", rt.removes)
	}
	if cfg, err := a.ProxyConfig(ctx); err != nil || len(cfg.DBs) != 0 {
		t.Errorf("a failed node is still routed: %+v %v", cfg.DBs, err)
	}
	rt.healthErr = nil
	if _, err := createDB(a, ctx, req); err != nil {
		t.Fatal(err)
	}
}

func TestCreateDatabaseDefaultSchedule(t *testing.T) {
	a, _, _ := newTestApp(t)
	ctx := context.Background()
	d1, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "one", Domain: "one.com"})
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := a.Store.ListSchedules(ctx, d1.ID); len(s) != 0 {
		t.Fatalf("schedules = %+v", s)
	}
	var first int64
	for _, n := range []string{"d1", "d2"} {
		x := store.Destination{Name: n, Provider: "s3", Shared: true}
		if err := a.Store.CreateDestination(ctx, &x); err != nil {
			t.Fatal(err)
		}
		if first == 0 {
			first = x.ID
		}
	}
	d2, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "two", Domain: "two.com"})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := a.Store.ListSchedules(ctx, d2.ID)
	if len(s) != 1 || s[0].DestinationID != first || s[0].KeepLatest != 7 || s[0].Prefix != "fridge" || !s[0].Enabled ||
		!regexp.MustCompile(`^\d{1,2} 2 \* \* \*$`).MatchString(s[0].Cron) {
		t.Fatalf("schedules = %+v", s)
	}
}

func TestSetAllowlist(t *testing.T) {
	a, _, _ := newTestApp(t)
	ctx := context.Background()
	if got, _ := a.Allowlist(ctx); len(got) != 5 {
		t.Fatalf("default = %v", got)
	}
	if err := a.SetAllowlist(ctx, []string{"10.0.0.0/8", "bad"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	if err := a.SetAllowlist(ctx, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty err = %v", err)
	}
	if err := a.SetAllowlist(ctx, []string{"203.0.113.0/24"}); err != nil {
		t.Fatal(err)
	}
	if cfg, err := a.ProxyConfig(ctx); err != nil || len(cfg.Allowlist) != 1 || cfg.Allowlist[0] != "203.0.113.0/24" {
		t.Fatalf("proxy allowlist = %v, %v", cfg.Allowlist, err)
	}
	if got, _ := a.Allowlist(ctx); len(got) != 1 || got[0] != "203.0.113.0/24" {
		t.Fatalf("got = %v", got)
	}
}

func TestRemoveNodeCleansProvisionedDatabase(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx := context.Background()
	a.Sched = NewScheduler(a)
	if err := a.Store.CreateDestination(ctx, &store.Destination{Name: "s3", Provider: "s3", Shared: true}); err != nil {
		t.Fatal(err)
	}
	d, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "abc", Domain: "abc.com"})
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := a.Store.ListSchedules(ctx, d.ID); len(s) != 1 || len(a.Sched.c.Entries()) != 1 {
		t.Fatalf("precondition: schedules %+v entries %d", s, len(a.Sched.c.Entries()))
	}
	if cfg, err := a.ProxyConfig(ctx); err != nil || len(cfg.DBs) != 1 {
		t.Fatalf("precondition: %+v %v", cfg.DBs, err)
	}
	d, _ = a.Store.GetDatabase(ctx, d.ID)
	a.removeNode(ctx, d)
	if cfg, err := a.ProxyConfig(ctx); err != nil || len(cfg.DBs) != 0 {
		t.Errorf("a removed node is still routed: %+v %v", cfg.DBs, err)
	}
	if len(rt.removes) != 1 || rt.removes[0] != (removeCall{"ctr-" + d.Volume, d.Volume}) {
		t.Fatalf("removes = %+v", rt.removes)
	}
	if got, _ := a.Store.GetDatabase(ctx, d.ID); got.Status != "error" {
		t.Fatalf("status = %s", got.Status)
	}
	if s, _ := a.Store.ListSchedules(ctx, d.ID); len(s) != 0 || len(a.Sched.c.Entries()) != 0 {
		t.Fatalf("schedules %+v entries %d", s, len(a.Sched.c.Entries()))
	}
}

func TestCreateDatabaseRollbackSurvivesCancelledContext(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	rt.onWait = cancel
	rt.healthErr = context.Canceled
	if _, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "abc", Domain: "abc.com"}); err == nil {
		t.Fatal("want error")
	}
	if len(rt.removes) != 1 {
		t.Fatalf("removes = %+v", rt.removes)
	}
	dbs, _ := a.Store.ListDatabases(context.Background())
	if len(dbs) != 1 || dbs[0].Status != "error" {
		t.Fatalf("dbs = %+v", dbs)
	}
}

func TestRecoverInterrupted(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx := context.Background()
	dest := store.Destination{Name: "s3", Provider: "s3", Shared: true}
	if err := a.Store.CreateDestination(ctx, &dest); err != nil {
		t.Fatal(err)
	}
	keep, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "keep", Domain: "keep.com"})
	if err != nil {
		t.Fatal(err)
	}
	half := store.Database{Name: "half", Engine: "postgres", Version: "17", Domain: "half.com", TLSMode: "internal",
		User: "half_app", Volume: "fridge-postgres-half", Status: "creating", NodeKey: "aaaaaaaaaaaaaaaa"}
	if err := a.Store.CreateDatabase(ctx, &half); err != nil {
		t.Fatal(err)
	}
	tgt, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "tgt", Domain: "tgt.com"})
	if err != nil {
		t.Fatal(err)
	}
	r := store.Restore{DestinationID: dest.ID, S3Key: "k.dump", SourceEngine: "postgres", Mode: "new"}
	if err := a.Store.StartRestore(ctx, &r); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.SetRestoreTarget(ctx, r.ID, tgt.ID); err != nil {
		t.Fatal(err)
	}
	a.Sched = NewScheduler(a)

	if _, err := a.Store.FailRunning(ctx, "interrupted by restart"); err != nil {
		t.Fatal(err)
	}
	if err := a.RecoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	want := []removeCall{{"ctr-fridge-postgres-half", "fridge-postgres-half"}, {"ctr-" + tgt.Volume, tgt.Volume}}
	if len(rt.removes) != 2 || rt.removes[0] != want[0] || rt.removes[1] != want[1] {
		t.Fatalf("removes = %+v", rt.removes)
	}
	for _, id := range []int64{half.ID, tgt.ID} {
		if got, _ := a.Store.GetDatabase(ctx, id); got.Status != "error" {
			t.Fatalf("db %d status = %s", id, got.Status)
		}
	}
	if s, _ := a.Store.ListSchedules(ctx, tgt.ID); len(s) != 0 {
		t.Fatalf("target schedules = %+v", s)
	}
	if got, _ := a.Store.GetDatabase(ctx, keep.ID); got.Status != "running" {
		t.Fatalf("keep status = %s", got.Status)
	}
	if s, _ := a.Store.ListSchedules(ctx, keep.ID); len(s) != 1 {
		t.Fatalf("keep schedules = %+v", s)
	}
	if len(rt.alerts) != 1 || !strings.Contains(rt.alerts[0], "half, root1_tgt") {
		t.Fatalf("alerts = %q", rt.alerts)
	}
	for _, n := range []string{"half", "tgt"} {
		if _, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: n, Domain: n + ".com"}); err != nil {
			t.Fatalf("reuse %s: %v", n, err)
		}
	}
	if err := a.RecoverInterrupted(ctx); err != nil || len(rt.removes) != 2 || len(rt.alerts) != 1 {
		t.Fatalf("second run: err=%v removes=%+v alerts=%q", err, rt.removes, rt.alerts)
	}
}

func TestProxyConfigServesWebCertificate(t *testing.T) {
	a, _, _ := newTestApp(t)
	cfg, err := a.ProxyConfig(context.Background())
	if err != nil || !strings.Contains(cfg.WebCertPEM, "CERTIFICATE") || !strings.Contains(cfg.WebKeyPEM, "PRIVATE KEY") || cfg.APIBackend != a.APIBackend {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
	a.WebCert = filepath.Join(t.TempDir(), "missing.crt")
	if _, err := a.ProxyConfig(context.Background()); err == nil {
		t.Fatal("served a config without a web certificate")
	}
}

// A node accepts only its own certificate: the database gets the server pair and the client certificate as its only
// trusted root, the sidecar gets the client pair, and the client key never reaches the database service.
func TestProvisionGivesEachNodeItsOwnCertificates(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx := context.Background()
	d1, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "one", Domain: "one.com"})
	if err != nil {
		t.Fatal(err)
	}
	d2, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "two", Domain: "two.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.runs) != 2 {
		t.Fatalf("runs = %d", len(rt.runs))
	}
	n1, n2 := rt.runs[0].Spec, rt.runs[1].Spec
	if !n1.TLS || len(n1.Command) != 3 || n1.Command[0] != "sh" {
		t.Fatalf("node 1 spec: %+v", n1)
	}
	envOf := func(env []string, k string) string {
		for _, e := range env {
			if v, ok := strings.CutPrefix(e, k+"="); ok {
				return v
			}
		}
		return ""
	}
	t1, err := a.nodeTLS(ctx, d1)
	if err != nil {
		t.Fatal(err)
	}
	t2, _ := a.nodeTLS(ctx, d2)
	if t1.ClientCert == t2.ClientCert || t1.ServerCert == t2.ServerCert || t1.ClientKey == t2.ClientKey {
		t.Fatal("two nodes share TLS material")
	}
	if got := envOf(n1.Env, "FRIDGE_TLS_CLIENT_CA"); got != t1.ClientCert || got == t2.ClientCert {
		t.Fatalf("the database trusts %q: want exactly its own client certificate", got)
	}
	if got := envOf(n1.Env, "FRIDGE_TLS_SERVER_KEY"); got != t1.ServerKey {
		t.Fatal("the database does not get its server key")
	}
	for _, e := range n1.Env {
		if strings.Contains(e, t1.ClientKey) {
			t.Fatalf("the client's private key is in the database service's environment: %s", e[:30])
		}
	}
	if envOf(n1.EdgeEnv, "FRIDGE_TLS_CERT") != t1.ClientCert || envOf(n1.EdgeEnv, "FRIDGE_TLS_KEY") != t1.ClientKey || envOf(n1.EdgeEnv, "FRIDGE_TLS_CA") != t1.CA || t1.CA == string(a.CA.CertPEM()) {
		t.Fatalf("sidecar env: %v", n1.EdgeEnv)
	}
	if n1.EdgeEnv[1] == n2.EdgeEnv[1] {
		t.Fatal("two sidecars share a client key")
	}
	st, err := a.Store.GetDBTLS(ctx, d1.ID)
	if err != nil || strings.Contains(st.ServerKeyEnc, "PRIVATE KEY") || strings.Contains(st.ClientKeyEnc, "PRIVATE KEY") {
		t.Fatalf("keys must be stored encrypted: %+v %v", st, err)
	}
}

// SCRAM channel binding makes the client compare the certificate it saw (Traefik's) with the database's: they must
// be the very same certificate, and it must name both the public domain and the node's service.
func TestRouteCertificateIsTheDatabasesOwn(t *testing.T) {
	a, _, _ := newTestApp(t)
	ctx := context.Background()
	d, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "abc", Domain: "abc.com"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := a.ProxyConfig(ctx)
	if err != nil || len(cfg.DBs) != 1 {
		t.Fatalf("%+v %v", cfg, err)
	}
	tlsm, _ := a.nodeTLS(ctx, d)
	if cfg.DBs[0].CertPEM != tlsm.ServerCert || cfg.DBs[0].KeyPEM != tlsm.ServerKey {
		t.Fatal("Traefik presents a different certificate from the database's")
	}
	b, _ := pem.Decode([]byte(tlsm.ServerCert))
	leaf, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname("abc.com"); err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname("fridge-db-" + d.NodeKey); err != nil {
		t.Fatal(err)
	}
}

// Each tenant has a CA of their own: it signs only their databases, the CA a user downloads vouches for those and for
// no one else's, and one tenant's databases share it.
func TestEachTenantHasItsOwnCA(t *testing.T) {
	a, _, _ := newTestApp(t)
	ctx := context.Background()
	alice, bob := mkUser(t, a, "alice", "user"), mkUser(t, a, "bob", "user")
	regDomain(a, alice, "a1.com")
	regDomain(a, alice, "a2.com")
	regDomain(a, bob, "b1.com")
	a1, err := createDB(a, ctx, CreateRequest{OwnerID: alice, Engine: "postgres", Name: "one", Domain: "a1.com"})
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := createDB(a, ctx, CreateRequest{OwnerID: alice, Engine: "postgres", Name: "two", Domain: "a2.com"})
	b1, err := createDB(a, ctx, CreateRequest{OwnerID: bob, Engine: "postgres", Name: "one", Domain: "b1.com"})
	if err != nil {
		t.Fatal(err)
	}
	caOf := func(d store.Database) string {
		pemCA, err := a.DatabaseCA(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		return pemCA
	}
	if caOf(a1) != caOf(a2) {
		t.Fatal("one tenant's databases are signed by different CAs")
	}
	if caOf(a1) == caOf(b1) || caOf(a1) == string(a.CA.CertPEM()) {
		t.Fatal("tenants share a CA with each other or with the instance")
	}
	verifies := func(d store.Database, caPEM string) error {
		tl, _ := a.nodeTLS(ctx, d)
		b, _ := pem.Decode([]byte(tl.ServerCert))
		leaf, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM([]byte(caPEM))
		_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: d.Domain})
		return err
	}
	if err := verifies(a1, caOf(a1)); err != nil {
		t.Fatalf("alice's CA does not vouch for her database: %v", err)
	}
	if err := verifies(a1, caOf(b1)); err == nil {
		t.Fatal("bob's CA vouches for alice's database")
	}
	if err := verifies(a1, string(a.CA.CertPEM())); err == nil {
		t.Fatal("the instance CA vouches for a tenant's database")
	}
	if tl, _ := a.nodeTLS(ctx, b1); tl.CA != caOf(b1) {
		t.Fatal("the sidecar and jobs of a node must trust the CA that signed it")
	}
	if n, _ := a.Store.GetTenantCA(ctx, alice); strings.Contains(n.KeyEnc, "PRIVATE KEY") {
		t.Fatal("the tenant CA key is stored in the clear")
	}
}
