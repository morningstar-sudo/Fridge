//go:build integration

package container

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Real Docker. Run: go test -tags integration ./internal/container/ -run Images -v
func TestImagesAgainstDocker(t *testing.T) {
	ctx := context.Background()
	c, err := New("fridge-it-img-net")
	if err != nil {
		t.Fatal(err)
	}
	const ref = "postgres:17-alpine"

	if _, err := c.ImageInspect(ctx, "fridge-no-such-image:1"); !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("missing image: %v", err)
	}
	if err := c.ImagePull(ctx, ref); err != nil {
		t.Fatal(err)
	}
	info, err := c.ImageInspect(ctx, ref)
	if err != nil || info.ID == "" || info.Size <= 0 || info.Created.IsZero() || len(info.RepoDigests) == 0 {
		t.Fatalf("%+v %v", info, err)
	}
	var version bool
	for _, e := range info.Env {
		version = version || len(e) > 11 && e[:11] == "PG_VERSION="
	}
	if !version {
		t.Fatalf("no PG_VERSION in %v", info.Env)
	}
	if d, err := c.RemoteDigest(ctx, ref); err != nil || d == "" {
		t.Logf("remote digest unavailable (offline?): %q %v", d, err)
	}

	if err := c.EnsureNetwork(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.cli.NetworkRemove(ctx, "fridge-it-img-net")
	const name = "fridge-it-imgtest"
	_ = c.Remove(ctx, name, name)
	id, err := c.Run(ctx, RunSpec{Name: name, Image: ref, Env: []string{"POSTGRES_PASSWORD=x"}, Volume: name,
		VolumePath: "/var/lib/postgresql/data", Labels: map[string]string{"fridge.db_id": "4242"}})
	defer c.Remove(ctx, name, name)
	if err != nil {
		t.Fatal(err)
	}
	_ = id
	uses, err := c.DBContainers(ctx)
	var found *ContainerUse
	for i := range uses {
		if uses[i].Name == name {
			found = &uses[i]
		}
	}
	if err != nil || found == nil || found.DBID != "4242" || found.Image != ref || found.ImageID != info.ID || found.StartedAt.IsZero() || time.Since(found.StartedAt) > time.Hour {
		t.Fatalf("%+v %v", found, err)
	}
	// Docker refuses to remove an image a container uses: Prune relies on that as a second guard.
	if err := c.ImageRemove(ctx, ref); err == nil {
		t.Fatal("removed an image in use")
	}
}
