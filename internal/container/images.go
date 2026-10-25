package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/errdefs"
)

// ErrImageNotFound: the image is not on this Docker host.
var ErrImageNotFound = errors.New("image not found")

type ImageInfo struct {
	Ref, ID     string
	RepoDigests []string
	Size        int64
	Created     time.Time
	Env         []string
}

// ContainerUse is a database container (running or stopped) and the image it was made from.
type ContainerUse struct {
	Name, Image, ImageID, DBID string
	Created, StartedAt         time.Time
}

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

// ImagePull pulls ref and waits for the stream to end: the pull is only done then.
func (c *Client) ImagePull(ctx context.Context, ref string) error {
	rc, err := c.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer rc.Close()
	if _, err := io.Copy(io.Discard, rc); err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	return nil
}

// ImageRemove deletes the image's tag (and the image once nothing else refers to it). Not forced:
// an image a container still uses is refused by Docker.
func (c *Client) ImageRemove(ctx context.Context, ref string) error {
	_, err := c.cli.ImageRemove(ctx, ref, image.RemoveOptions{PruneChildren: true})
	if errdefs.IsNotFound(err) {
		return ErrImageNotFound
	}
	return err
}

// DBContainers lists every container Fridge made for a database, stopped ones included.
func (c *Client) DBContainers(ctx context.Context) ([]ContainerUse, error) {
	list, err := c.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", "fridge.db_id"))})
	if err != nil {
		return nil, err
	}
	out := make([]ContainerUse, 0, len(list))
	for _, s := range list {
		u := ContainerUse{Image: s.Image, ImageID: s.ImageID, DBID: s.Labels["fridge.db_id"], Created: time.Unix(s.Created, 0)}
		if len(s.Names) > 0 {
			u.Name = strings.TrimPrefix(s.Names[0], "/")
		}
		if in, err := c.cli.ContainerInspect(ctx, s.ID); err == nil && in.ContainerJSONBase != nil && in.State != nil {
			u.StartedAt, _ = time.Parse(time.RFC3339Nano, in.State.StartedAt) // the zero time ("0001-…") for never started
			if u.StartedAt.Year() <= 1 {
				u.StartedAt = time.Time{}
			}
		}
		out = append(out, u)
	}
	return out, nil
}

// RemoteDigest asks the registry for the digest ref points at now.
func (c *Client) RemoteDigest(ctx context.Context, ref string) (string, error) {
	r, err := c.cli.DistributionInspect(ctx, ref, "")
	if err != nil {
		return "", err
	}
	return r.Descriptor.Digest.String(), nil
}
