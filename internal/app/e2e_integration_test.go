//go:build integration

package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"fridge/internal/container"
	"fridge/internal/objstore"
	"fridge/internal/pki"
	"fridge/internal/secret"
	"fridge/internal/store"
	"fridge/internal/store/storetest"
)

// Real Docker + MinIO: backup a Postgres 14 node to S3, restore it into a new Postgres 17 node.
// Run: bash scripts/test-db.sh && FRIDGE_IT_S3_*=... go test -tags integration -timeout 20m ./internal/app/ -run E2E -v
func TestE2EBackupRestoreAcrossVersions(t *testing.T) {
	ep, ak, sk, bucket := os.Getenv("FRIDGE_IT_S3_ENDPOINT"), os.Getenv("FRIDGE_IT_S3_ACCESS"), os.Getenv("FRIDGE_IT_S3_SECRET"), os.Getenv("FRIDGE_IT_S3_BUCKET")
	if ep == "" || ak == "" || sk == "" || bucket == "" {
		t.Skip("FRIDGE_IT_S3_* not set")
	}
	ctx := context.Background()
	const network = "fridge-it-net"

	rt, err := container.New(network)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exec.Command("docker", "network", "rm", network).Run() })

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
	a := &App{
		Store: storetest.New(t), Box: box, RT: rt, CA: ca,
		TraefikDir: t.TempDir(), HealthTimeout: 120 * time.Second,
		Alert: func(ctx context.Context, text string) { t.Logf("alert: %s", text) },
	}

	// Leftovers from an aborted run would make container/volume creation fail.
	names := []string{"it14", "it17", "it13"}
	for _, n := range names {
		rt.Remove(ctx, "fridge-postgres-"+n, "fridge-postgres-"+n)
	}
	t.Cleanup(func() {
		for _, n := range names {
			if d, err := a.Store.GetDatabaseByName(ctx, n); err == nil {
				a.removeNode(ctx, d)
			}
			rt.Remove(ctx, "fridge-postgres-"+n, "fridge-postgres-"+n) // also covers rows that never got written
		}
	})

	dest, err := a.CreateDestination(ctx, store.Destination{Name: "it", Endpoint: ep, Bucket: bucket, AccessKey: ak, PathStyle: true}, sk)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.TestDestination(ctx, dest.ID); err != nil {
		t.Fatal(err)
	}

	psql := func(d store.Database, sql string) string {
		t.Helper()
		var out, errb bytes.Buffer
		code, err := rt.Exec(ctx, d.ContainerID, []string{"psql", "-U", d.User, "-d", d.Name, "-v", "ON_ERROR_STOP=1", "-tAc", sql}, nil, &out, &errb)
		if err != nil || code != 0 {
			t.Fatalf("psql %q on %s: code %d err %v: %s", sql, d.Name, code, err, errb.String())
		}
		return strings.TrimSpace(out.String())
	}

	src, err := a.CreateDatabase(ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "it14", Domain: "it14.fridge.test", Version: "14"})
	if err != nil {
		t.Fatal(err)
	}
	psql(src, "create table t(x int); insert into t values (1),(2),(3)")

	b, err := a.StartBackup(ctx, src.ID, dest.ID, nil, "manual", nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if b, err = a.Store.GetBackup(ctx, b.ID); err != nil || b.Status != "ok" {
		t.Fatalf("backup: %+v %v", b, err)
	}
	if !strings.HasPrefix(b.S3Key, "fridge/postgres-14/it14/it14-") || !strings.HasSuffix(b.S3Key, ".dump") {
		t.Fatalf("key %q", b.S3Key)
	}
	objs, err := objstore.New(dest, sk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { objs.Delete(ctx, b.S3Key) })
	meta, err := objs.Meta(ctx, b.S3Key)
	if err != nil || meta["fridge-version"] != "14" {
		t.Fatalf("object meta: %v %v", meta, err)
	}

	r, err := a.StartRestore(ctx, RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: b.S3Key,
		New: &CreateRequest{OwnerID: own(t, a), Name: "it17", Domain: "it17.fridge.test", Version: "17"}})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if r, err = a.Store.GetRestore(ctx, r.ID); err != nil || r.Status != "ok" || r.TargetDatabaseID == nil {
		t.Fatalf("restore: %+v %v", r, err)
	}
	dst, err := a.Store.GetDatabase(ctx, *r.TargetDatabaseID)
	if err != nil {
		t.Fatal(err)
	}
	if dst.Version != "17" || dst.Status != "running" {
		t.Fatalf("restored node: %+v", dst)
	}
	if got := psql(dst, "select count(*) from t"); got != "3" {
		t.Fatalf("count on it17 = %q, want 3", got)
	}

	_, err = a.StartRestore(ctx, RestoreRequest{OwnerID: own(t, a), DestinationID: dest.ID, Key: b.S3Key,
		New: &CreateRequest{OwnerID: own(t, a), Name: "it13", Domain: "it13.fridge.test", Version: "13"}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("downgrade restore: got %v, want ErrInvalid", err)
	}
}
