//go:build integration

package container

import (
	"context"
	"io"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/docker/docker/api/types/network"
)

// A shell inside a node container must reach neither other nodes nor the control network;
// only the proxy (Traefik stand-in) reaches the node.
func TestNodeNetworkIsolation(t *testing.T) {
	ctx := context.Background()
	const ctl, netA, netB = "fridge-it-net", "fridge-it-node-a", "fridge-it-node-b"
	c, err := New(ctl)
	if err != nil {
		t.Fatal(err)
	}
	pg := func(name, net string) RunSpec {
		return RunSpec{Name: name, Image: "postgres:17-alpine", Env: []string{"POSTGRES_PASSWORD=x"}, Network: net,
			Volume: name, VolumePath: "/var/lib/postgresql/data", // named, so cleanup leaves no anonymous volume
			Healthcheck: []string{"CMD-SHELL", "pg_isready -U postgres"}, Labels: map[string]string{"fridge.test": "1"}}
	}
	specs := []RunSpec{pg("fridge-it-pga", netA), pg("fridge-it-pgb", netB), pg("fridge-it-meta", ""), pg("fridge-it-proxy", "")}
	cleanup := func() {
		for _, s := range specs {
			_ = c.Remove(ctx, s.Name, s.Volume)
		}
		for _, n := range []string{netA, netB, ctl} {
			_ = c.RemoveNetwork(ctx, n)
		}
	}
	cleanup() // leftovers from an aborted run
	t.Cleanup(cleanup)

	if err := c.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{netA, netB} {
		if err := c.CreateNetwork(ctx, n); err != nil {
			t.Fatal(err)
		}
		if err := c.CreateNetwork(ctx, n); err != nil { // idempotent
			t.Fatal(err)
		}
		ins, err := c.cli.NetworkInspect(ctx, n, network.InspectOptions{})
		if err != nil || !ins.Internal || ins.Labels["fridge.node"] != "1" {
			t.Fatalf("%s: internal=%v labels=%v err=%v", n, ins.Internal, ins.Labels, err)
		}
	}
	ids := map[string]string{}
	for _, s := range specs {
		id, err := c.Run(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		ids[s.Name] = id
	}
	for _, n := range []string{netA, netB} {
		for range 2 { // second connect is a no-op
			if err := c.ConnectNetwork(ctx, n, "fridge-it-proxy", nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, id := range ids {
		if err := c.WaitHealthy(ctx, id, 60*time.Second); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if got, err := c.ContainerNetworks(ctx, "fridge-it-proxy"); err != nil || !slices.Equal(got, []string{ctl, netA, netB}) {
		t.Fatalf("proxy networks = %q, %v", got, err)
	}
	if got, err := c.ContainerNetworks(ctx, "fridge-it-pga"); err != nil || !slices.Equal(got, []string{netA}) {
		t.Fatalf("pga networks = %q, %v", got, err)
	}

	isReady := func(from, host, timeout string) int {
		t.Helper()
		code, err := c.Exec(ctx, ids[from], []string{"pg_isready", "-h", host, "-t", timeout}, nil, io.Discard, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		return code
	}
	if code := isReady("fridge-it-pga", "fridge-it-pgb", "3"); code == 0 {
		t.Fatal("node A reached node B")
	}
	if code := isReady("fridge-it-pga", "fridge-it-meta", "3"); code == 0 {
		t.Fatal("node A reached the control network")
	}
	// By IP too: a failed name lookup alone would not prove there is no route.
	for name, net := range map[string]string{"fridge-it-pgb": netB, "fridge-it-meta": ctl} {
		info, err := c.cli.ContainerInspect(ctx, ids[name])
		if err != nil {
			t.Fatal(err)
		}
		if ip := info.NetworkSettings.Networks[net].IPAddress; ip == "" || isReady("fridge-it-pga", ip, "3") == 0 {
			t.Fatalf("node A reached %s at %q", name, ip)
		}
	}
	if code := isReady("fridge-it-proxy", "fridge-it-pga", "5"); code != 0 {
		t.Fatalf("proxy cannot reach node A: exit %d", code)
	}

	subs, err := c.Subnets(ctx, ctl)
	if err != nil || len(subs) != 1 {
		t.Fatalf("subnets = %v, %v", subs, err)
	}
	pin, err := c.cli.ContainerInspect(ctx, ids["fridge-it-proxy"])
	if err != nil {
		t.Fatal(err)
	}
	ip, err := netip.ParseAddr(pin.NetworkSettings.Networks[ctl].IPAddress)
	if err != nil || !subs[0].Contains(ip) {
		t.Fatalf("proxy ip %v not in %v (%v)", ip, subs[0], err)
	}
	fs, err := c.FridgeSubnets(ctx)
	if err != nil || !slices.Contains(fs, subs[0]) { // test node nets are fridge-it-node-*, not fridge-node-*

		t.Fatalf("FridgeSubnets = %v, %v", fs, err)
	}

	// Disconnect is idempotent; a network with no endpoints removes; missing ones are fine.
	for range 2 {
		if err := c.DisconnectNetwork(ctx, netA, "fridge-it-proxy"); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.DisconnectNetwork(ctx, netA, "fridge-it-no-such"); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(ctx, ids["fridge-it-pga"], "fridge-it-pga"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := c.RemoveNetwork(ctx, netA); err != nil {
			t.Fatal(err)
		}
	}
}
