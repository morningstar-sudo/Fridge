package app

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"

	"fridge/internal/container"
	"fridge/internal/store"
)

// NodeNetwork is the node's private internal network, shared only with Traefik.
func NodeNetwork(d store.Database) string {
	if d.NodeKey == "" { // phase-1 node
		return fmt.Sprintf("%slegacy-%d", container.NodeNetworkPrefix, d.ID)
	}
	return container.NodeNetworkPrefix + d.NodeKey
}

// ReconcileNetworks makes sure every live node sits on its own network with Traefik, and moves
// phase-1 nodes off the control network (attach to the node network first, so routing never breaks).
// Route files need no change: the container name stays the alias. Per-node failures are alerted, not fatal.
func (a *App) ReconcileNetworks(ctx context.Context) error {
	dbs, err := a.Store.ListDatabases(ctx)
	if err != nil {
		return err
	}
	a.netMu.Lock()
	defer a.netMu.Unlock()
	for _, d := range dbs {
		if !live(d) {
			continue
		}
		if err := a.reconcileNode(ctx, d); err != nil {
			log.Printf("reconcile network %s: %v", d.Name, err)
			a.alertf(ctx, "network isolation for %s failed: %v", d.Name, err)
		}
	}
	return nil
}

func live(d store.Database) bool { return d.Status == "running" || d.Status == "stopped" }

// HealNetworks re-attaches Traefik and node containers to node networks that already exist
// (a recreated Traefik container comes back on the control network only). It never creates a
// network, so it cannot resurrect one removeNode is tearing down. It alerts once per run listing
// repaired DBs, and once per DB when it goes from healthy to failing.
func (a *App) HealNetworks(ctx context.Context) (repaired int, err error) {
	dbs, err := a.Store.ListDatabases(ctx)
	if err != nil {
		return 0, err
	}
	a.netMu.Lock()
	defer a.netMu.Unlock()
	if a.healFailed == nil {
		a.healFailed = map[int64]bool{}
	}
	proxyNets, proxyErr := a.RT.ContainerNetworks(ctx, a.TraefikContainer)
	var fixed []string
	for _, d := range dbs {
		if !live(d) {
			continue
		}
		did, err := a.healNode(ctx, d, proxyNets, proxyErr)
		switch {
		case err != nil:
			if !a.healFailed[d.ID] {
				a.healFailed[d.ID] = true
				a.alertf(ctx, "network heal for %s failed: %v", d.Name, err)
			}
			continue
		case did:
			fixed = append(fixed, d.Name)
		}
		delete(a.healFailed, d.ID)
	}
	if len(fixed) > 0 {
		a.alertf(ctx, "re-attached node network(s) for: %s", strings.Join(fixed, ", "))
	}
	return len(fixed), nil
}

func (a *App) healNode(ctx context.Context, d store.Database, proxyNets []string, proxyErr error) (bool, error) {
	net, ctr := NodeNetwork(d), d.Volume
	if ok, err := a.RT.NetworkExists(ctx, net); err != nil || !ok {
		return false, err // absent: not reconciled yet or being removed; never create here
	}
	if proxyErr != nil {
		return false, fmt.Errorf("inspect %s: %w", a.TraefikContainer, proxyErr)
	}
	did := false
	if !slices.Contains(proxyNets, net) {
		if err := a.RT.ConnectNetwork(ctx, net, a.TraefikContainer, nil); err != nil {
			return false, fmt.Errorf("connect %s: %w", a.TraefikContainer, err)
		}
		did = true
	}
	nets, err := a.RT.ContainerNetworks(ctx, ctr)
	if err != nil {
		return did, err
	}
	if !slices.Contains(nets, net) {
		if err := a.RT.ConnectNetwork(ctx, net, ctr, []string{ctr}); err != nil {
			return did, err
		}
		did = true
	}
	return did, nil
}

func (a *App) reconcileNode(ctx context.Context, d store.Database) error {
	net, ctr := NodeNetwork(d), d.Volume // container name == volume name
	if err := a.RT.CreateNetwork(ctx, net); err != nil {
		return err
	}
	if err := a.RT.ConnectNetwork(ctx, net, a.TraefikContainer, nil); err != nil {
		return fmt.Errorf("connect %s: %w", a.TraefikContainer, err)
	}
	nets, err := a.RT.ContainerNetworks(ctx, ctr)
	if err != nil {
		return err
	}
	if !slices.Contains(nets, net) {
		if err := a.RT.ConnectNetwork(ctx, net, ctr, []string{ctr}); err != nil {
			return err
		}
	}
	if slices.Contains(nets, a.ControlNetwork) {
		return a.RT.DisconnectNetwork(ctx, a.ControlNetwork, ctr)
	}
	return nil
}
