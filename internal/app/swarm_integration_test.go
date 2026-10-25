//go:build integration

package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"fridge/internal/engine"
	"fridge/internal/objstore"
	"fridge/internal/pki"
	"fridge/internal/secret"
	"fridge/internal/store"
	"fridge/internal/store/storetest"
	"fridge/internal/swarm"
)

// Real swarm, real jobs, real S3. It must run inside the swarm (on the edge and egress networks, with the Docker
// socket and FRIDGE_TEST_DATABASE_URL): scripts/swarm-test/gotest.sh builds this test into an image and runs it as a job.
func TestSwarmNodeLifecycle(t *testing.T) {
	serverImg, jobImg := os.Getenv("FRIDGE_SERVER_IMAGE"), os.Getenv("FRIDGE_JOB_IMAGE")
	ep, ak, sk, bucket := os.Getenv("FRIDGE_IT_S3_ENDPOINT"), os.Getenv("FRIDGE_IT_S3_ACCESS"), os.Getenv("FRIDGE_IT_S3_SECRET"), os.Getenv("FRIDGE_IT_S3_BUCKET")
	if serverImg == "" || jobImg == "" || ep == "" || bucket == "" {
		t.Skip("FRIDGE_SERVER_IMAGE, FRIDGE_JOB_IMAGE and FRIDGE_IT_S3_* are not set")
	}
	ctx := context.Background()
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	egress := env("FRIDGE_EGRESS_NETWORK", "fridge_egress")
	rt, err := swarm.New(swarm.Config{EdgeNetwork: env("FRIDGE_EDGE_NETWORK", "fridge_edge"), EgressNetwork: egress, ServerImage: serverImg})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if err := rt.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := rt.EnsureNetworks(ctx); err != nil {
		t.Fatal(err)
	}

	u, err := url.Parse(ep)
	if err != nil {
		t.Fatal(err)
	}
	mc, err := minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(ak, sk, ""), Secure: u.Scheme == "https"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := mc.BucketExists(ctx, bucket); err != nil {
		t.Fatal(err)
	} else if !ok {
		if err := mc.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	key := make([]byte, 32)
	rand.Read(key)
	box, err := secret.NewBox(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := pki.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &App{Store: storetest.New(t), Box: box, RT: rt, CA: ca, TraefikDir: t.TempDir(), HealthTimeout: 4 * time.Minute,
		JobImage: jobImg, EgressNetwork: egress, backupTimeout: 5 * time.Minute,
		Alert: func(ctx context.Context, text string) { t.Logf("alert: %s", text) }}
	t.Cleanup(func() {
		dbs, _ := a.Store.ListDatabases(ctx)
		for _, d := range dbs {
			a.removeNode(ctx, d)
		}
	})

	adminID := own(t, a)
	dest, err := a.CreateDestination(ctx, store.Destination{Name: "it", Endpoint: ep, Bucket: bucket, AccessKey: ak, PathStyle: true, OwnerID: &adminID}, sk)
	if err != nil {
		t.Fatal(err)
	}

	// connect runs queries as a role of node d through its edge sidecar, the way Fridge itself does.
	connect := func(d store.Database, user, pw string) (*pgx.Conn, error) {
		eng, _, _ := engine.Get(d.Engine)
		p := nodeParams(d)
		p.AdminUser, p.AdminPassword = user, pw
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		return pgx.Connect(cctx, eng.AdminDSN(p, swarm.EdgeService(d.NodeKey), swarm.EdgePort))
	}
	scalar := func(c *pgx.Conn, sql string) string {
		t.Helper()
		var s string
		if err := c.QueryRow(ctx, sql).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	isReady := func(from, target string) int {
		t.Helper()
		res, err := rt.RunJob(ctx, swarm.JobSpec{Name: "isready", Image: jobImg, Entrypoint: []string{"/usr/lib/postgresql/17/bin/pg_isready"},
			Args: []string{"-h", target, "-U", "x", "-t", "3"}, Networks: []string{from}, Timeout: 2 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("pg_isready %s from %s: exit %d %q %q", target, from, res.ExitCode, res.Stdout, res.Stderr)
		return res.ExitCode
	}

	// ---- create: services, network, host, the two users ----
	d1, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "it1", Domain: "it1.lan", Version: "17"})
	if err != nil {
		t.Fatal(err)
	}
	if d1.Status != "running" || d1.HostID == "" || d1.NodeKey == "" {
		t.Fatalf("node: %+v", d1)
	}
	if st := nodeState(t, rt, d1.NodeKey); !st.DB || !st.Edge || !st.HasNetwork || st.HostID != d1.HostID {
		t.Fatalf("swarm state %+v", st)
	}
	c, err := connect(d1, d1.User, appPassword(t, a, d1))
	if err != nil {
		t.Fatalf("app user cannot log in through the sidecar: %v", err)
	}
	if got := scalar(c, "select rolsuper::text from pg_roles where rolname = current_user"); got != "false" {
		t.Fatalf("app user rolsuper = %s", got)
	}
	if _, err := c.Exec(ctx, "create table t(x int); insert into t values (1),(2),(3)"); err != nil {
		t.Fatal(err)
	}
	c.Close(ctx)
	_, adminPW, err := a.RevealSuperuser(ctx, d1)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := connect(d1, d1.AdminUser, adminPW)
	if err != nil {
		t.Fatalf("superuser cannot log in: %v", err)
	}
	if got := scalar(sc, "select rolsuper::text from pg_roles where rolname = current_user"); got != "true" {
		t.Fatalf("superuser rolsuper = %s", got)
	}
	sc.Close(ctx)

	// ---- backup: a job on the node's network dumps straight into S3 ----
	b, err := a.StartBackup(ctx, d1.ID, dest.ID, nil, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if b, err = a.Store.GetBackup(ctx, b.ID); err != nil || b.Status != "ok" {
		t.Fatalf("backup: %+v %v", b, err)
	}
	objs, err := objstore.New(dest, sk, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { objs.Delete(ctx, b.S3Key) })
	if meta, err := objs.Meta(ctx, b.S3Key); err != nil || meta["fridge-version"] != "17" {
		t.Fatalf("object meta %v %v", meta, err)
	}

	// ---- deep check of that file, from a job that sees S3 and nothing else ----
	chk, err := a.StartDumpCheck(ctx, dest.ID, b.S3Key, own(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if chk = waitCheck(t, a, chk.ID); chk.Status != "ok" || chk.Result["toc_entries"].(float64) < 3 {
		t.Fatalf("check: %+v", chk)
	}

	// ---- restore into a new node: the data arrives, owned by that node's app user ----
	r, err := startRestore(a, ctx, RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: b.S3Key,
		New: &CreateRequest{OwnerID: own(t, a), Name: "it2", Domain: "it2.lan", Version: "17"}})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if r, err = a.Store.GetRestore(ctx, r.ID); err != nil || r.Status != "ok" || r.TargetDatabaseID == nil {
		t.Fatalf("restore: %+v %v", r, err)
	}
	d2, err := a.Store.GetDatabase(ctx, *r.TargetDatabaseID)
	if err != nil {
		t.Fatal(err)
	}
	if d2.HostID == "" || d2.NodeKey == d1.NodeKey {
		t.Fatalf("second node %+v", d2)
	}
	c2, err := connect(d2, d2.User, appPassword(t, a, d2))
	if err != nil {
		t.Fatal(err)
	}
	if got := scalar(c2, "select count(*)::text from t"); got != "3" {
		t.Fatalf("rows after restore: %s", got)
	}
	if got := scalar(c2, "select tableowner from pg_tables where tablename='t'"); got != d2.User {
		t.Fatalf("restored table owner %q, want %q", got, d2.User)
	}
	c2.Close(ctx)

	// ---- isolation ----
	if isReady(swarm.Network(d1.NodeKey), swarm.DBService(d1.NodeKey)) != 0 {
		t.Fatal("a job cannot reach its own node")
	}
	if isReady(swarm.Network(d1.NodeKey), swarm.DBService(d2.NodeKey)) == 0 {
		t.Fatal("node 1's network reached node 2")
	}
	if isReady(egress, swarm.EdgeService(d1.NodeKey)) == 0 {
		t.Fatal("the egress network reached a sidecar")
	}

	// ---- rotating the superuser ----
	if err := a.RotateSuperuser(ctx, d1); err != nil {
		t.Fatal(err)
	}
	if c, err := connect(d1, d1.AdminUser, adminPW); err == nil {
		c.Close(ctx)
		t.Fatal("the old superuser password still works")
	}
	d1, _ = a.Store.GetDatabase(ctx, d1.ID)
	_, newPW, err := a.RevealSuperuser(ctx, d1)
	if err != nil || newPW == adminPW {
		t.Fatalf("rotated password: %v", err)
	}
	if c, err := connect(d1, d1.AdminUser, newPW); err != nil {
		t.Fatalf("the new superuser password does not work: %v", err)
	} else {
		c.Close(ctx)
	}

	// ---- removing a node takes its services, network and volume; the other node is untouched ----
	if err := rt.RemoveNode(ctx, swarm.NodeRef{Key: d2.NodeKey, HostID: d2.HostID, Volume: d2.Volume}); err != nil {
		t.Fatal(err)
	}
	nodes, err := rt.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Key == d2.NodeKey {
			t.Fatalf("node 2 left behind: %+v", n)
		}
	}
	if st := nodeState(t, rt, d1.NodeKey); !st.DB || !st.Edge || !st.HasNetwork {
		t.Fatalf("node 1 was disturbed: %+v", st)
	}
}

func nodeState(t *testing.T, rt *swarm.Client, key string) swarm.NodeState {
	t.Helper()
	ns, err := rt.ListNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range ns {
		if n.Key == key {
			return n
		}
	}
	t.Fatalf("no swarm state for node %s", key)
	return swarm.NodeState{}
}
