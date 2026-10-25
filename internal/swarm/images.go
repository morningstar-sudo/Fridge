package swarm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/errdefs"
)

// ErrImageNotFound: the image is not on the manager's Docker host.
var ErrImageNotFound = errors.New("image not found")

type ImageInfo struct {
	Ref, ID     string
	RepoDigests []string
	Size        int64
	Created     time.Time
	Env         []string
}

// ImageInspect describes ref as the manager's daemon has it.
func (c *Client) ImageInspect(ctx context.Context, ref string) (ImageInfo, error) {
	r, err := c.cli.ImageInspect(ctx, ref)
	if errdefs.IsNotFound(err) {
		return ImageInfo{}, ErrImageNotFound
	}
	if err != nil {
		return ImageInfo{}, err
	}
	info := ImageInfo{Ref: ref, ID: r.ID, RepoDigests: r.RepoDigests, Size: r.Size}
	info.Created, _ = time.Parse(time.RFC3339Nano, r.Created)
	if r.Config != nil {
		info.Env = r.Config.Env
	}
	return info, nil
}

// RemoteDigest asks the registry for the digest ref points at now.
func (c *Client) RemoteDigest(ctx context.Context, ref string) (string, error) {
	r, err := c.cli.DistributionInspect(ctx, ref, "")
	if err != nil {
		return "", err
	}
	return r.Descriptor.Digest.String(), nil
}

// Nodes are the ready swarm nodes, whatever their labels.
func (c *Client) Nodes(ctx context.Context) ([]Host, error) {
	nodes, err := c.cli.NodeList(ctx, swarm.NodeListOptions{})
	if err != nil {
		return nil, err
	}
	var out []Host
	for _, n := range nodes {
		if n.Status.State == swarm.NodeStateReady {
			out = append(out, Host{ID: n.ID, Hostname: n.Description.Hostname})
		}
	}
	return out, nil
}

// ImagePull pulls ref on every ready node, so a database created later starts without waiting for the registry.
// The nodes pull in parallel through a job pinned to each; the Docker socket is mounted to reach the node's daemon.
func (c *Client) ImagePull(ctx context.Context, ref string) error {
	hosts, err := c.Nodes(ctx)
	if err != nil {
		return err
	}
	var mu sync.Mutex
	var errs []error
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := c.RunJob(ctx, JobSpec{Name: "pull", Image: c.cfg.ServerImage, Entrypoint: []string{"/fridge"}, Args: []string{"job", "pull", ref},
				NodeID: h.ID, Binds: []string{"/var/run/docker.sock:/var/run/docker.sock"}, Timeout: 30 * time.Minute})
			if err == nil && res.ExitCode != 0 {
				err = fmt.Errorf("exit %d: %s", res.ExitCode, res.Stderr)
			}
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", h.Hostname, err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}
