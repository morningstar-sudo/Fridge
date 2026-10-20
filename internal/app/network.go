package app

import (
	"context"
	"fmt"
	"log"
	"slices"

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
	for _, d := range dbs {
		if d.Status != "running" && d.Status != "stopped" {
			continue
		}
		if err := a.reconcileNode(ctx, d); err != nil {
			log.Printf("reconcile network %s: %v", d.Name, err)
			a.alertf(ctx, "network isolation for %s failed: %v", d.Name, err)
		}
	}
	return nil
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
