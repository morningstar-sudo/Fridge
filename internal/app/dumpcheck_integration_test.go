//go:build integration

package app

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"fridge/internal/container"
)

// Real Docker: the deep check runs a real pg_restore in a throwaway container with no network.
// Run: bash scripts/test-db.sh && go test -tags integration -timeout 10m ./internal/app/ -run DumpCheckReal -v
func TestDumpCheckRealPgRestore(t *testing.T) {
	a, _, objs := newTestApp(t)
	rt, err := container.New("fridge-it-check-net")
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.EnsureNetwork(context.Background()); err != nil { // FridgeSubnets (SSRF guard) reads it
		t.Fatal(err)
	}
	t.Cleanup(func() { exec.Command("docker", "network", "rm", "fridge-it-check-net").Run() })
	a.RT = rt
	a.backupTimeout = 5 * time.Minute
	ctx := context.Background()

	whole := dumpFile(t, "pg14.dump")
	dest := restoreFixture(t, a, objs, "up/whole.dump", whole, nil)
	objs.objs["up/half.dump"] = fakeObj{whole[:len(whole)/2], nil}

	c, err := a.StartDumpCheck(ctx, dest.ID, "up/whole.dump", own(t, a))
	if err != nil {
		t.Fatal(err)
	}
	got := waitCheck(t, a, c.ID)
	if got.Status != "ok" || got.Result["toc_entries"].(float64) < 5 || got.Result["bytes"] != float64(len(whole)) {
		t.Fatalf("whole dump: %+v", got)
	}
	c, _ = a.StartDumpCheck(ctx, dest.ID, "up/half.dump", own(t, a))
	if got = waitCheck(t, a, c.ID); got.Status != "failed" {
		t.Fatalf("half a dump passed: %+v", got)
	}
	t.Logf("half dump: error=%q log=%q", got.Error, got.Log)

	out, err := exec.Command("docker", "ps", "-a", "--filter", "name=fridge-check-", "--format", "{{.Names}}").Output()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("leftover check containers: %q %v", out, err)
	}
}
