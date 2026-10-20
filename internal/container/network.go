package container

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/errdefs"
)

// NodeNetworkPrefix names the per-node internal networks.
const NodeNetworkPrefix = "fridge-node-"

// inhibitIPv4 keeps the bridge itself address-less: without it, an Internal network still reaches
// the host through its gateway IP.
const inhibitIPv4 = "com.docker.network.bridge.inhibit_ipv4"

// CreateNetwork creates an isolated bridge network for one DB node: Internal (no route out) and
// with no host address on the bridge. An existing network is accepted only if it is isolated likewise.
func (c *Client) CreateNetwork(ctx context.Context, name string) error {
	ok, err := c.checkNodeNetwork(ctx, name)
	if err != nil || ok {
		return err
	}
	_, err = c.cli.NetworkCreate(ctx, name, network.CreateOptions{
		Driver: "bridge", Internal: true, Labels: map[string]string{"fridge.node": "1"},
		Options: map[string]string{inhibitIPv4: "true"},
	})
	if errdefs.IsConflict(err) { // lost a race to a concurrent create: re-check once
		if ok, err = c.checkNodeNetwork(ctx, name); err == nil && !ok {
			err = fmt.Errorf("network %s: create conflict but not found", name)
		}
	}
	return err
}

// checkNodeNetwork: (false, nil) = absent; (true, nil) = present and isolated; error = present but not isolated.
func (c *Client) checkNodeNetwork(ctx context.Context, name string) (bool, error) {
	ins, err := c.cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if ins.Name != name || !ins.Internal || ins.Options[inhibitIPv4] != "true" {
		return false, fmt.Errorf("network %s exists but is not an isolated node network", name)
	}
	return true, nil
}

// NetworkExists reports whether a network with exactly this name exists.
func (c *Client) NetworkExists(ctx context.Context, name string) (bool, error) {
	ins, err := c.cli.NetworkInspect(ctx, name, network.InspectOptions{})
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	return err == nil && ins.Name == name, err
}

// RemoveNetwork removes a network; a missing one is fine.
func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	if err := c.cli.NetworkRemove(ctx, name); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	return nil
}

// ConnectNetwork attaches a container to a network; already attached is fine.
func (c *Client) ConnectNetwork(ctx context.Context, netName, containerName string, aliases []string) error {
	nets, err := c.ContainerNetworks(ctx, containerName)
	if err != nil {
		return err
	}
	if slices.Contains(nets, netName) {
		return nil
	}
	err = c.cli.NetworkConnect(ctx, netName, containerName, &network.EndpointSettings{Aliases: aliases})
	if err != nil { // a concurrent connect (heal vs reconcile) wins the race: that's success too
		if nets, ierr := c.ContainerNetworks(ctx, containerName); ierr == nil && slices.Contains(nets, netName) {
			return nil
		}
	}
	return err
}

// DisconnectNetwork detaches a container; a missing container/network or no attachment is fine.
func (c *Client) DisconnectNetwork(ctx context.Context, netName, containerName string) error {
	nets, err := c.ContainerNetworks(ctx, containerName)
	if errdefs.IsNotFound(err) || err == nil && !slices.Contains(nets, netName) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := c.cli.NetworkDisconnect(ctx, netName, containerName, false); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	return nil
}

// ContainerNetworks lists the networks a container is attached to, sorted.
func (c *Client) ContainerNetworks(ctx context.Context, containerName string) ([]string, error) {
	info, err := c.cli.ContainerInspect(ctx, containerName)
	if err != nil {
		return nil, err
	}
	var out []string
	if info.NetworkSettings != nil {
		for n := range info.NetworkSettings.Networks {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

// FridgeSubnets returns the subnets of the control network and of every node network.
func (c *Client) FridgeSubnets(ctx context.Context) ([]netip.Prefix, error) {
	list, err := c.cli.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("name", NodeNetworkPrefix))})
	if err != nil {
		return nil, err
	}
	out, err := c.Subnets(ctx, c.network)
	if err != nil {
		return nil, err
	}
	for _, n := range list { // name filter is a substring match
		if !strings.HasPrefix(n.Name, NodeNetworkPrefix) {
			continue
		}
		for _, cfg := range n.IPAM.Config {
			p, err := netip.ParsePrefix(cfg.Subnet)
			if err != nil {
				return nil, fmt.Errorf("network %s: subnet %q: %w", n.Name, cfg.Subnet, err)
			}
			out = append(out, p)
		}
	}
	return out, nil
}
