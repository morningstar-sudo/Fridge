package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"fridge/internal/store"
	"fridge/internal/swarm"
)

type fakeImages struct {
	mu        sync.Mutex
	images    map[string]swarm.ImageInfo // on the manager
	pulled    []string
	gate      chan struct{}     // non-nil: pulls wait for it to be closed
	remote    map[string]string // ref -> registry digest
	remoteErr error
}

func (f *fakeImages) ImageInspect(_ context.Context, ref string) (swarm.ImageInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i, ok := f.images[ref]; ok {
		return i, nil
	}
	return swarm.ImageInfo{}, swarm.ErrImageNotFound
}

func (f *fakeImages) ImagePull(_ context.Context, ref string) error {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulled = append(f.pulled, ref)
	if ref == "postgres:14" {
		return errors.New("registry said no")
	}
	return nil
}

func (f *fakeImages) RemoteDigest(_ context.Context, ref string) (string, error) {
	return f.remote[ref], f.remoteErr
}

type fakeRegistry struct {
	env map[string]string
	err error
}

func (f fakeRegistry) RemoteEnv(_ context.Context, ref, key string) (string, error) {
	return f.env[ref], f.err
}

func img(ref, id string, size int64, version string, digests ...string) swarm.ImageInfo {
	return swarm.ImageInfo{Ref: ref, ID: "sha256:" + id, Size: size, Created: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		RepoDigests: digests, Env: []string{"PATH=/x", "PG_VERSION=" + version}}
}

func newImgApp(t *testing.T) (*App, *fakeImages) {
	a, _, _ := newTestApp(t)
	f := &fakeImages{images: map[string]swarm.ImageInfo{}, remote: map[string]string{}}
	a.Img = f
	return a, f
}

// The databases that exist say which images are in use; a failed node does not count.
func TestListImagesUsage(t *testing.T) {
	a, f := newImgApp(t)
	ctx := context.Background()
	t1 := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	f.images["postgres:16"] = img("postgres:16", "aaaaaaaaaaaabbbbbbbb", 400, "16.4")
	f.images["postgres:17"] = img("postgres:17", "cccccccccccc", 450, "17.2")
	var dbs []store.Database
	for _, n := range []string{"aa", "bb"} {
		d, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: n, Domain: n + ".com", Version: "16"})
		if err != nil {
			t.Fatal(err)
		}
		dbs = append(dbs, d)
	}
	a.RT.(*fakeRuntime).healthErr = errors.New("unhealthy")
	if _, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "cc", Domain: "cc.com", Version: "17"}); err == nil {
		t.Fatal("want a failed node")
	}
	if err := a.Store.UpsertImageCheck(ctx, store.ImageCheck{ImageRef: "postgres:17", RemoteVersion: "17.3", CheckedAt: t1}); err != nil {
		t.Fatal(err)
	}
	rows, err := a.ListImages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]ImageRow{}
	for _, r := range rows {
		by[r.Ref] = r
	}
	p16 := by["postgres:16"]
	if !p16.Present || len(p16.UsedBy) != 2 || p16.UsedBy[0] != dbs[0].Name || p16.UsedBy[1] != dbs[1].Name || p16.LastUsed == nil || p16.LastUsed.Before(dbs[1].CreatedAt) ||
		p16.ShortDigest != "aaaaaaaaaaaa" || p16.Version != "16.4" || p16.Size != 400 {
		t.Fatalf("postgres:16 %+v", p16)
	}
	if p17 := by["postgres:17"]; len(p17.UsedBy) != 0 || p17.Update == nil || p17.Update.RemoteVersion != "17.3" {
		t.Fatalf("postgres:17 (its only node failed) %+v", p17)
	}
	if p15 := by["postgres:15"]; p15.Present || len(p15.UsedBy) != 0 || p15.ShortDigest != "" {
		t.Fatalf("postgres:15 %+v", p15)
	}
	if len(rows) != len(catalogRefs()) {
		t.Fatalf("%d rows", len(rows))
	}
}

func TestPullValidation(t *testing.T) {
	a, f := newImgApp(t)
	for _, bad := range [][]string{{"nginx:latest"}, {"postgres:18", "evil/x:1"}, nil} {
		if _, err := a.PullImages(bad, "root1"); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v: %v", bad, err)
		}
	}
	f.gate = make(chan struct{})
	job, err := a.PullImages([]string{"postgres:15", "postgres:14"}, "root1")
	if err != nil {
		t.Fatal(err)
	}
	rp := a.RunningPulls()
	if len(rp) != 2 || rp[0].Kind != "image_pull" || rp[0].StartedBy != "root1" || rp[0].DBName != "postgres:14" || rp[0].StartedAt.IsZero() {
		t.Fatalf("running pulls %+v", rp)
	}
	if st, _ := a.PullStatus(job); st["postgres:15"] != "pulling" {
		t.Fatalf("status %v", st)
	}
	close(f.gate)
	a.Wait()
	st, err := a.PullStatus(job)
	if err != nil || st["postgres:15"] != "done" || st["postgres:14"] != "error: registry said no" {
		t.Fatalf("status %v %v", st, err)
	}
	if len(a.RunningPulls()) != 0 {
		t.Fatal("finished pulls still listed as running")
	}
	if _, err := a.PullStatus("nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestCheckImageUpdates(t *testing.T) {
	a, f := newImgApp(t)
	ctx := context.Background()
	a.Registry = fakeRegistry{env: map[string]string{"postgres:18": "18.2"}}
	f.images["postgres:18"] = img("postgres:18", "x18", 500, "18.1", "postgres@sha256:old")
	f.images["postgres:17"] = img("postgres:17", "x17", 450, "17.2", "postgres@sha256:same")
	f.images["postgres:16"] = img("postgres:16", "x16", 400, "16.4", "postgres@sha256:l16")
	f.remote["postgres:18"], f.remote["postgres:17"] = "sha256:new", "sha256:same"
	f.remote["postgres:16"] = "sha256:l16"
	if err := a.CheckImageUpdates(ctx); err != nil {
		t.Fatal(err)
	}
	got := map[string]store.ImageCheck{}
	cs, _ := a.Store.ListImageChecks(ctx)
	for _, c := range cs {
		got[c.ImageRef] = c
	}
	if c := got["postgres:18"]; c.LocalVersion != "18.1" || c.RemoteVersion != "18.2" || c.LocalDigest != "sha256:old" || c.RemoteDigest != "sha256:new" || c.Error != "" {
		t.Fatalf("outdated: %+v", c)
	}
	if c := got["postgres:17"]; c.LocalDigest != c.RemoteDigest || c.Error != "" {
		t.Fatalf("current: %+v", c)
	}
	if _, ok := got["postgres:15"]; ok {
		t.Fatal("an image that is not on disk was checked")
	}
	// a registry that cannot be reached is recorded, not returned
	f.remoteErr = errors.New("registry down")
	if err := a.CheckImageUpdates(ctx); err != nil {
		t.Fatalf("registry error escaped: %v", err)
	}
	cs, _ = a.Store.ListImageChecks(ctx)
	for _, c := range cs {
		if c.Error != "registry down" {
			t.Fatalf("%+v", c)
		}
	}
	// remote digest known but the version lookup fails: the digests are kept, the error says why
	f.remoteErr = nil
	a.Registry = fakeRegistry{err: errors.New("also down")}
	if err := a.CheckImageUpdates(ctx); err != nil {
		t.Fatal(err)
	}
	cs, _ = a.Store.ListImageChecks(ctx)
	for _, c := range cs {
		if c.ImageRef == "postgres:18" && (c.Error != "also down" || c.RemoteDigest != "sha256:new") {
			t.Fatalf("%+v", c)
		}
	}
}
