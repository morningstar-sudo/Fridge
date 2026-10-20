//go:build integration

package container

import (
	"context"
	"fmt"
	"io"
	gonet "net"
	"net/netip"
	"slices"
	"sync/atomic"
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
		for _, n := range []string{netA, netB, ctl, "fridge-it-node-leaky"} {
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
		if err != nil || !ins.Internal || ins.Labels["fridge.node"] != "1" || ins.Options[inhibitIPv4] != "true" {
			t.Fatalf("%s: internal=%v labels=%v options=%v err=%v", n, ins.Internal, ins.Labels, ins.Options, err)
		}
	}
	// A pre-existing network that is internal but still has a host address on its bridge is refused.
	const leaky = "fridge-it-node-leaky"
	if _, err := c.cli.NetworkCreate(ctx, leaky, network.CreateOptions{Driver: "bridge", Internal: true}); err != nil {
		t.Fatal(err)
	}
	err = c.CreateNetwork(ctx, leaky)
	_ = c.RemoveNetwork(ctx, leaky)
	if err == nil {
		t.Fatal("CreateNetwork accepted a node network without inhibit_ipv4")
	}
	if ok, err := c.NetworkExists(ctx, netA); err != nil || !ok {
		t.Fatalf("NetworkExists(%s) = %v, %v", netA, ok, err)
	}
	if ok, err := c.NetworkExists(ctx, "fridge-it-node-none"); err != nil || ok {
		t.Fatalf("NetworkExists(missing) = %v, %v", ok, err)
	}

	// A host listener on all interfaces, to prove node A cannot reach the host either.
	ln, err := gonet.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	hostPort := fmt.Sprint(ln.Addr().(*gonet.TCPAddr).Port)
	var hits atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			conn.Close()
		}
	}()
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
	// The host: through node A's own gateway address and the control network's gateway.
	var gws []string
	for _, n := range []string{netA, ctl} {
		ins, err := c.cli.NetworkInspect(ctx, n, network.InspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, cfg := range ins.IPAM.Config {
			if cfg.Gateway != "" {
				gws = append(gws, cfg.Gateway)
			}
		}
	}
	if len(gws) == 0 {
		t.Fatal("no gateway address to probe")
	}
	for _, gw := range gws {
		_, _ = c.Exec(ctx, ids["fridge-it-pga"], []string{"pg_isready", "-h", gw, "-p", hostPort, "-t", "3"}, nil, io.Discard, io.Discard)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("node A reached the host listener (%d connection(s)) via %v", n, gws)
	}
	// Positive control: the proxy on the control network does reach the host listener.
	_, _ = c.Exec(ctx, ids["fridge-it-proxy"], []string{"pg_isready", "-h", gws[len(gws)-1], "-p", hostPort, "-t", "3"}, nil, io.Discard, io.Discard)
	if hits.Load() == 0 {
		t.Fatal("control probe never reached the host listener; the negative check proves nothing")
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
