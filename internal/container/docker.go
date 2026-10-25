// Package container wraps the Docker Engine SDK for running database containers.
package container

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
)

type RunSpec struct {
	Name, Image string
	Env         []string
	Volume      string
	VolumePath  string
	Healthcheck []string
	Labels      map[string]string
	Network     string   // "" = the client's network; NoNetwork = isolated. Alias = Name on user-defined networks.
	Cmd         []string // overrides the image's command
	Ephemeral   bool     // throwaway: no restart policy, Docker removes it when it exits
}

// NoNetwork runs a container with no network at all (Docker's "none").
const NoNetwork = "none"

type Client struct {
	cli     *client.Client
	network string
}

func New(network string) (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Client{cli: cli, network: network}, nil
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.cli.Ping(ctx)
	return err
}

func (c *Client) EnsureNetwork(ctx context.Context) error {
	list, err := c.cli.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("name", c.network))})
	if err != nil {
		return err
	}
	for _, n := range list { // name filter is a substring match
		if n.Name == c.network {
			return nil
		}
	}
	_, err = c.cli.NetworkCreate(ctx, c.network, network.CreateOptions{Driver: "bridge"})
	if err != nil && errdefs.IsConflict(err) {
		return nil
	}
	return err
}

func (c *Client) Run(ctx context.Context, s RunSpec) (string, error) {
	if _, err := c.cli.ImageInspect(ctx, s.Image); err != nil {
		if !errdefs.IsNotFound(err) {
			return "", err
		}
		rc, err := c.cli.ImagePull(ctx, s.Image, image.PullOptions{})
		if err != nil {
			return "", fmt.Errorf("pull %s: %w", s.Image, err)
		}
		_, err = io.Copy(io.Discard, rc) // pull completes only when the stream is drained
		rc.Close()
		if err != nil {
			return "", fmt.Errorf("pull %s: %w", s.Image, err)
		}
	}
	cfg := &container.Config{Image: s.Image, Env: s.Env, Labels: s.Labels, Cmd: s.Cmd}
	if len(s.Healthcheck) > 0 {
		cfg.Healthcheck = &container.HealthConfig{
			Test: s.Healthcheck, Interval: 2 * time.Second, Timeout: 5 * time.Second,
			Retries: 15, StartPeriod: 2 * time.Second,
		}
	}
	netName := s.Network
	if netName == "" {
		netName = c.network
	}
	host := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
		NetworkMode:   container.NetworkMode(netName),
	}
	if s.Ephemeral {
		host.RestartPolicy = container.RestartPolicy{}
		host.AutoRemove = true
	}
	if s.Volume != "" {
		if _, err := c.cli.VolumeCreate(ctx, volume.CreateOptions{Name: s.Volume, Labels: s.Labels}); err != nil {
			return "", err
		}
		host.Binds = []string{s.Volume + ":" + s.VolumePath}
	}
	var nets *network.NetworkingConfig
	if netName != NoNetwork { // Docker rejects aliases on "none"
		nets = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			netName: {Aliases: []string{s.Name}},
		}}
	}
	resp, err := c.cli.ContainerCreate(ctx, cfg, host, nets, nil, s.Name)
	if err != nil {
		return "", err
	}
	if err := c.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return resp.ID, err
	}
	return resp.ID, nil
}

func (c *Client) WaitHealthy(ctx context.Context, id string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		info, err := c.cli.ContainerInspect(ctx, id)
		if err != nil {
			return err
		}
		if h := info.State.Health; h != nil {
			switch h.Status {
			case "healthy":
				return nil
			case "unhealthy":
				return fmt.Errorf("container %s unhealthy", id)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("container %s not healthy within %s: %w", id, timeout, ctx.Err())
		case <-t.C:
		}
	}
}

func (c *Client) Exec(ctx context.Context, id string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	ex, err := c.cli.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd: cmd, AttachStdout: true, AttachStderr: true, AttachStdin: stdin != nil,
	})
	if err != nil {
		return -1, err
	}
	hj, err := c.cli.ContainerExecAttach(ctx, ex.ID, container.ExecAttachOptions{})
	if err != nil {
		return -1, err
	}
	defer hj.Close()
	// The hijacked conn ignores ctx; closing it is the only way to unblock StdCopy and the stdin copy.
	// ponytail: the exec'd process keeps running in the container (Docker has no exec-kill API).
	stop := context.AfterFunc(ctx, func() { hj.Close() })
	defer stop()
	inErr := make(chan error, 1)
	if stdin != nil {
		go func() {
			if _, err := io.Copy(hj.Conn, stdin); err != nil {
				inErr <- err
				hj.Close() // abort: never signal a clean EOF for truncated input
				return
			}
			hj.CloseWrite()
		}()
	}
	_, outErr := stdcopy.StdCopy(stdout, stderr, hj.Reader)
	if err := ctx.Err(); err != nil {
		return -1, fmt.Errorf("exec: %w", err)
	}
	select {
	case err := <-inErr:
		return -1, fmt.Errorf("exec stdin: %w", err)
	default:
	}
	if outErr != nil {
		return -1, outErr
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		in, err := c.cli.ContainerExecInspect(ctx, ex.ID)
		if err != nil {
			return -1, err
		}
		if !in.Running {
			return in.ExitCode, nil
		}
		if time.Now().After(deadline) {
			return -1, fmt.Errorf("exec %s still running after output closed", ex.ID)
		}
		select {
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (c *Client) Remove(ctx context.Context, id, vol string) error {
	if id != "" {
		err := c.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
		if err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}
	if vol != "" {
		err := c.cli.VolumeRemove(ctx, vol, true)
		if err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// Subnets returns the IPAM subnets of the named networks (default: the client's network).
func (c *Client) Subnets(ctx context.Context, names ...string) ([]netip.Prefix, error) {
	if len(names) == 0 {
		names = []string{c.network}
	}
	var out []netip.Prefix
	for _, n := range names {
		ins, err := c.cli.NetworkInspect(ctx, n, network.InspectOptions{})
		if err != nil {
			return nil, err
		}
		for _, cfg := range ins.IPAM.Config {
			p, err := netip.ParsePrefix(cfg.Subnet)
			if err != nil {
				return nil, fmt.Errorf("network %s: subnet %q: %w", n, cfg.Subnet, err)
			}
			out = append(out, p)
		}
	}
	return out, nil
}
