package app

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"fridge/internal/container"
	"fridge/internal/engine"
	"fridge/internal/store"
)

// Images is the Docker image surface the Images page needs; *container.Client satisfies it.
type Images interface {
	ImageInspect(ctx context.Context, ref string) (container.ImageInfo, error)
	ImagePull(ctx context.Context, ref string) error
	ImageRemove(ctx context.Context, ref string) error
	DBContainers(ctx context.Context) ([]container.ContainerUse, error)
	RemoteDigest(ctx context.Context, ref string) (string, error)
}

var _ Images = (*container.Client)(nil)

// Registry reads an image's environment from the remote registry; *registry.Client satisfies it.
type Registry interface {
	RemoteEnv(ctx context.Context, ref, key string) (string, error)
}

type ImageRow struct {
	Ref, ShortDigest, Version string
	Size                      int64
	Created                   time.Time
	Present                   bool
	UsedBy                    []string // database containers made from it, running or not
	LastUsed                  *time.Time
	Update                    *store.ImageCheck
}

const versionEnv = "PG_VERSION"

// catalogRefs are the images Fridge can create nodes from, in catalog order.
func catalogRefs() []string {
	var out []string
	for _, info := range engine.List() {
		eng, _, err := engine.Get(info.Name)
		if err != nil {
			continue
		}
		for _, v := range info.Versions {
			out = append(out, eng.Spec(engine.Params{Version: v}).Image)
		}
	}
	return out
}

func envValue(env []string, key string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, key+"="); ok {
			return v
		}
	}
	return ""
}

func (a *App) needImages() error {
	if a.Img == nil {
		return errors.New("image management is not available")
	}
	return nil
}

// ListImages: the catalog images plus whatever the database containers actually run.
func (a *App) ListImages(ctx context.Context) ([]ImageRow, error) {
	if err := a.needImages(); err != nil {
		return nil, err
	}
	uses, err := a.Img.DBContainers(ctx)
	if err != nil {
		return nil, err
	}
	checks, err := a.Store.ListImageChecks(ctx)
	if err != nil {
		return nil, err
	}
	byRef := map[string]store.ImageCheck{}
	for _, c := range checks {
		byRef[c.ImageRef] = c
	}
	refs := catalogRefs()
	seen := map[string]bool{}
	for _, r := range refs {
		seen[r] = true
	}
	var extra []string
	for _, u := range uses {
		if !seen[u.Image] {
			seen[u.Image] = true
			extra = append(extra, u.Image)
		}
	}
	sort.Strings(extra)
	refs = append(refs, extra...)

	rows := make([]ImageRow, 0, len(refs))
	for _, ref := range refs {
		row := ImageRow{Ref: ref}
		info, err := a.Img.ImageInspect(ctx, ref)
		switch {
		case errors.Is(err, container.ErrImageNotFound):
		case err != nil:
			return nil, err
		default:
			row.Present, row.Size, row.Created = true, info.Size, info.Created
			row.ShortDigest = strings.TrimPrefix(info.ID, "sha256:")
			if len(row.ShortDigest) > 12 {
				row.ShortDigest = row.ShortDigest[:12]
			}
			row.Version = envValue(info.Env, versionEnv)
		}
		for _, u := range uses {
			if u.Image != ref && !(row.Present && u.ImageID != "" && u.ImageID == info.ID) {
				continue
			}
			row.UsedBy = append(row.UsedBy, u.Name)
			if !u.StartedAt.IsZero() && (row.LastUsed == nil || u.StartedAt.After(*row.LastUsed)) {
				t := u.StartedAt
				row.LastUsed = &t
			}
		}
		sort.Strings(row.UsedBy)
		if c, ok := byRef[ref]; ok {
			row.Update = &c
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// ---- pulling ----

type pullJob struct {
	startedBy string
	startedAt time.Time
	status    map[string]string // ref -> "pulling" | "done" | "error: …"
}

type pulls struct {
	mu   sync.Mutex
	jobs map[string]*pullJob
}

const keepPullJobs = 20

// PullImages pulls catalog images in the background; the job lives in memory only.
func (a *App) PullImages(refs []string, startedBy string) (string, error) {
	if err := a.needImages(); err != nil {
		return "", err
	}
	if len(refs) == 0 {
		return "", invalid("no images given")
	}
	inCatalog := map[string]bool{}
	for _, r := range catalogRefs() {
		inCatalog[r] = true
	}
	job := &pullJob{startedBy: startedBy, startedAt: a.clock(), status: map[string]string{}}
	for _, r := range refs {
		if !inCatalog[r] {
			return "", invalid("image %q is not in the catalog", r)
		}
		job.status[r] = "pulling"
	}
	id := rand.Text()
	a.pulls.mu.Lock()
	if a.pulls.jobs == nil {
		a.pulls.jobs = map[string]*pullJob{}
	}
	a.pulls.jobs[id] = job
	a.prunePullJobs()
	a.pulls.mu.Unlock()

	for ref := range job.status {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
			defer cancel()
			st := "done"
			if err := a.Img.ImagePull(ctx, ref); err != nil {
				log.Printf("image pull %s: %v", ref, err)
				st = "error: " + err.Error()
			}
			a.pulls.mu.Lock()
			job.status[ref] = st
			a.pulls.mu.Unlock()
		}()
	}
	return id, nil
}

// prunePullJobs forgets the oldest finished jobs beyond keepPullJobs. Caller holds the lock.
func (a *App) prunePullJobs() {
	if len(a.pulls.jobs) <= keepPullJobs {
		return
	}
	type fin struct {
		id string
		at time.Time
	}
	var done []fin
	for id, j := range a.pulls.jobs {
		running := false
		for _, st := range j.status {
			running = running || st == "pulling"
		}
		if !running {
			done = append(done, fin{id, j.startedAt})
		}
	}
	sort.Slice(done, func(i, j int) bool { return done[i].at.Before(done[j].at) })
	for _, f := range done {
		if len(a.pulls.jobs) <= keepPullJobs {
			return
		}
		delete(a.pulls.jobs, f.id)
	}
}

// PullStatus: ref -> "pulling" | "done" | "error: …".
func (a *App) PullStatus(jobID string) (map[string]string, error) {
	a.pulls.mu.Lock()
	defer a.pulls.mu.Unlock()
	j, ok := a.pulls.jobs[jobID]
	if !ok {
		return nil, store.ErrNotFound
	}
	out := make(map[string]string, len(j.status))
	for k, v := range j.status {
		out[k] = v
	}
	return out, nil
}

// RunningPulls are the pulls still in progress, one per image, for the activity view.
func (a *App) RunningPulls() []store.Job {
	a.pulls.mu.Lock()
	defer a.pulls.mu.Unlock()
	var out []store.Job
	for _, j := range a.pulls.jobs {
		for ref, st := range j.status {
			if st == "pulling" {
				out = append(out, store.Job{Kind: "image_pull", DBName: ref, StartedBy: j.startedBy, StartedAt: j.startedAt})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].DBName < out[j].DBName
	})
	return out
}

// ---- pruning ----

// PrunePreview lists images that are on disk and used by no database container, and their total size.
func (a *App) PrunePreview(ctx context.Context) ([]ImageRow, int64, error) {
	rows, err := a.ListImages(ctx)
	if err != nil {
		return nil, 0, err
	}
	var out []ImageRow
	var total int64
	for _, r := range rows {
		if r.Present && len(r.UsedBy) == 0 {
			out = append(out, r)
			total += r.Size
		}
	}
	return out, total, nil
}

// Prune removes the listed images that are, right now, unused. Anything else is skipped silently.
// It returns what it removed; a failing removal does not stop the rest.
func (a *App) Prune(ctx context.Context, refs []string) ([]string, error) {
	unused, _, err := a.PrunePreview(ctx) // checked again here, not trusted from an earlier preview
	if err != nil {
		return nil, err
	}
	ok := map[string]bool{}
	for _, r := range unused {
		ok[r.Ref] = true
	}
	var removed []string
	var errs []error
	for _, ref := range refs {
		if !ok[ref] {
			continue
		}
		delete(ok, ref)
		if err := a.Img.ImageRemove(ctx, ref); err != nil && !errors.Is(err, container.ErrImageNotFound) {
			errs = append(errs, fmt.Errorf("%s: %w", ref, err))
			continue
		}
		removed = append(removed, ref)
	}
	return removed, errors.Join(errs...)
}

// ---- update check ----

// CheckImageUpdates compares each catalog image on disk with the registry and records the result.
// A registry that cannot be reached is recorded per image, not returned.
func (a *App) CheckImageUpdates(ctx context.Context) error {
	if err := a.needImages(); err != nil {
		return err
	}
	for _, ref := range catalogRefs() {
		info, err := a.Img.ImageInspect(ctx, ref)
		if errors.Is(err, container.ErrImageNotFound) {
			continue
		} else if err != nil {
			return err
		}
		c := store.ImageCheck{ImageRef: ref, LocalVersion: envValue(info.Env, versionEnv), CheckedAt: a.clock()}
		if len(info.RepoDigests) > 0 {
			_, c.LocalDigest, _ = strings.Cut(info.RepoDigests[0], "@")
		}
		remote, err := a.Img.RemoteDigest(ctx, ref)
		switch {
		case err != nil:
			c.Error = err.Error()
		default:
			c.RemoteDigest = remote
			if hasDigest(info.RepoDigests, remote) {
				c.LocalDigest, c.RemoteVersion = remote, c.LocalVersion // up to date: nothing to ask the registry
			} else if a.Registry != nil {
				if c.RemoteVersion, err = a.Registry.RemoteEnv(ctx, ref, versionEnv); err != nil {
					c.Error = err.Error()
				}
			}
		}
		if err := a.Store.UpsertImageCheck(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

func hasDigest(repoDigests []string, digest string) bool {
	for _, d := range repoDigests {
		if _, dd, _ := strings.Cut(d, "@"); dd == digest {
			return true
		}
	}
	return false
}
