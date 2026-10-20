package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"fridge/internal/store"
)

func newDB(t *testing.T, a *App, name string) store.Database {
	t.Helper()
	d, err := createDB(a, context.Background(), CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: name, Domain: name + ".com"})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestProvisionCreatesNodeNetwork(t *testing.T) {
	a, rt, _ := newTestApp(t)
	d := newDB(t, a, "abc")
	net := "fridge-node-" + d.NodeKey
	if NodeNetwork(d) != net {
		t.Fatalf("NodeNetwork = %q", NodeNetwork(d))
	}
	want := []string{"create " + net, "connect " + net + " fridge-traefik ", "run " + d.Volume + " " + net}
	if len(rt.ops) != 3 || !slices.Equal(rt.ops, want) {
		t.Fatalf("ops = %q, want %q", rt.ops, want)
	}
	if rt.runs[0].Network != net {
		t.Fatalf("run network = %q", rt.runs[0].Network)
	}
	for _, op := range rt.ops {
		if strings.Contains(op, "fridge-net") {
			t.Fatalf("node touched control network: %q", op)
		}
	}
	b, err := os.ReadFile(filepath.Join(a.TraefikDir, "dynamic", fmt.Sprintf("db-%d.yml", d.ID)))
	if err != nil || !strings.Contains(string(b), d.Volume+":5432") {
		t.Fatalf("route = %s, %v", b, err)
	}
}

func TestProvisionRollbackRemovesNetwork(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx := context.Background()
	rt.healthErr = errors.New("unhealthy")
	if _, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "abc", Domain: "abc.com"}); err == nil {
		t.Fatal("want error")
	}
	dbs, _ := a.Store.ListDatabases(ctx)
	net, name := NodeNetwork(dbs[0]), dbs[0].Volume
	want := []string{"run " + name + " " + net, "remove ctr-" + name, "disconnect " + net + " fridge-traefik", "rmnet " + net}
	if got := rt.ops[2:]; !slices.Equal(got, want) {
		t.Fatalf("ops = %q, want tail %q", rt.ops, want)
	}
	if _, ok := rt.nets[net]; ok {
		t.Fatalf("network %s left behind", net)
	}

	rt.healthErr, rt.ops = nil, nil
	rt.connectErr = errors.New("no such container")
	if _, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "def", Domain: "def.com"}); err == nil {
		t.Fatal("want error")
	}
	if !slices.ContainsFunc(rt.ops, func(s string) bool { return strings.HasPrefix(s, "rmnet fridge-node-") }) ||
		slices.ContainsFunc(rt.ops, func(s string) bool { return strings.HasPrefix(s, "run ") }) {
		t.Fatalf("ops = %q", rt.ops)
	}
}

func TestRemoveNodeRemovesNetwork(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx := context.Background()
	d := newDB(t, a, "abc")
	d, _ = a.Store.GetDatabase(ctx, d.ID)
	rt.ops = nil
	a.removeNode(ctx, d)
	net := NodeNetwork(d)
	want := []string{"remove " + d.ContainerID, "disconnect " + net + " fridge-traefik", "rmnet " + net}
	if !slices.Equal(rt.ops, want) {
		t.Fatalf("ops = %q, want %q", rt.ops, want)
	}
	if _, ok := rt.nets[net]; ok {
		t.Fatalf("network %s left behind", net)
	}
}

func TestReconcileNetworksLegacy(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx := context.Background()
	d := store.Database{Name: "old", Engine: "postgres", Version: "17", Domain: "old.com", TLSMode: "internal",
		User: "old", Volume: "fridge-postgres-old", Status: "running"}
	if err := a.Store.CreateDatabase(ctx, &d); err != nil {
		t.Fatal(err)
	}
	gone := store.Database{Name: "gone", Engine: "postgres", Version: "17", Domain: "gone.com", TLSMode: "internal",
		User: "gone", Volume: "fridge-postgres-gone", Status: "error"}
	if err := a.Store.CreateDatabase(ctx, &gone); err != nil {
		t.Fatal(err)
	}
	rt.nets = map[string]map[string]bool{"fridge-net": {d.Volume: true, "fridge-meta": true}}

	if err := a.ReconcileNetworks(ctx); err != nil {
		t.Fatal(err)
	}
	net := fmt.Sprintf("fridge-node-legacy-%d", d.ID)
	if NodeNetwork(d) != net {
		t.Fatalf("NodeNetwork = %q", NodeNetwork(d))
	}
	want := []string{"create " + net, "connect " + net + " fridge-traefik ", "connect " + net + " " + d.Volume + " " + d.Volume, "disconnect fridge-net " + d.Volume}
	if !slices.Equal(rt.ops, want) {
		t.Fatalf("ops = %q, want %q", rt.ops, want)
	}
	if got, _ := rt.ContainerNetworks(ctx, d.Volume); !slices.Equal(got, []string{net}) {
		t.Fatalf("container networks = %q", got)
	}
	if !rt.nets["fridge-net"]["fridge-meta"] {
		t.Fatal("control network members touched")
	}
}

func TestHealNetworks(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx := context.Background()
	d := newDB(t, a, "abc")
	gone := newDB(t, a, "gone")
	net, goneNet := NodeNetwork(d), NodeNetwork(gone)

	// Traefik recreated: it lost the node network. gone's network is being torn down (already removed).
	delete(rt.nets[net], "fridge-traefik")
	delete(rt.nets, goneNet)
	rt.ops = nil
	n, err := a.HealNetworks(ctx)
	if err != nil || n != 1 {
		t.Fatalf("repaired = %d, %v", n, err)
	}
	if !rt.nets[net]["fridge-traefik"] {
		t.Fatal("traefik not re-attached")
	}
	if _, ok := rt.nets[goneNet]; ok || slices.ContainsFunc(rt.ops, func(s string) bool { return strings.HasPrefix(s, "create") }) {
		t.Fatalf("heal created a network: %q", rt.ops)
	}
	if len(rt.alerts) != 1 || !strings.Contains(rt.alerts[0], d.Name) || strings.Contains(rt.alerts[0], gone.Name) {
		t.Fatalf("alerts = %q", rt.alerts)
	}

	if n, err := a.HealNetworks(ctx); err != nil || n != 0 || len(rt.alerts) != 1 {
		t.Fatalf("second run: repaired = %d, %v, alerts %q", n, err, rt.alerts)
	}

	// Failures alert once per OK→failed transition, and again only after a recovery.
	delete(rt.nets[net], "fridge-traefik")
	rt.connectErr = errors.New("no such container")
	for range 3 {
		if n, _ := a.HealNetworks(ctx); n != 0 {
			t.Fatalf("repaired %d while failing", n)
		}
	}
	if len(rt.alerts) != 2 || !strings.Contains(rt.alerts[1], "failed") {
		t.Fatalf("alerts = %q", rt.alerts)
	}
	rt.connectErr = nil
	if n, _ := a.HealNetworks(ctx); n != 1 || len(rt.alerts) != 3 {
		t.Fatalf("recovery: repaired %d alerts %q", n, rt.alerts)
	}
}

func TestReconcileNetworksIdempotent(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx := context.Background()
	d := newDB(t, a, "abc")
	net := NodeNetwork(d)
	for i := range 2 {
		rt.ops = nil
		if err := a.ReconcileNetworks(ctx); err != nil {
			t.Fatal(err)
		}
		for _, op := range rt.ops {
			if strings.HasPrefix(op, "disconnect") || strings.HasPrefix(op, "connect "+net+" "+d.Volume) {
				t.Fatalf("run %d: unexpected %q in %q", i, op, rt.ops)
			}
		}
	}
	if !rt.nets[net]["fridge-traefik"] || !rt.nets[net][d.Volume] || len(rt.nets[net]) != 2 {
		t.Fatalf("members of %s = %v", net, rt.nets[net])
	}
	if len(rt.alerts) != 0 {
		t.Fatalf("alerts = %q", rt.alerts)
	}
}
