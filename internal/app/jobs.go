package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"fridge/internal/engine"
	"fridge/internal/job"
	"fridge/internal/store"
	"fridge/internal/swarm"
)

// s3Job is destination id as a job needs it: credentials in the clear and, for a non-admin's destination,
// the guard the job's own S3 client must apply (the job runs where Fridge's guard cannot see it).
func (a *App) s3Job(ctx context.Context, destID int64) (job.S3, *job.Guard, error) {
	d, err := a.Store.GetDestination(ctx, destID)
	if err != nil {
		return job.S3{}, nil, err
	}
	if d.DeletedAt != nil {
		return job.S3{}, nil, invalid("destination deleted")
	}
	sk, err := a.Box.Decrypt(d.SecretKeyEnc)
	if err != nil {
		return job.S3{}, nil, err
	}
	g, err := a.guardFor(ctx, d)
	if err != nil {
		return job.S3{}, nil, err
	}
	s3 := job.S3{Endpoint: d.Endpoint, Region: d.Region, Bucket: d.Bucket, AccessKey: d.AccessKey, SecretKey: sk, PathStyle: d.PathStyle}
	if g == nil {
		return s3, nil, nil
	}
	jg := &job.Guard{Allow: g.Allow}
	for _, p := range g.Blocked {
		jg.Blocked = append(jg.Blocked, p.String())
	}
	return s3, jg, nil
}

// jobParams are the engine parameters for a job working on node d: its superuser, and the database's own
// address on the node's private network.
func (a *App) jobParams(ctx context.Context, d store.Database) (engine.Params, error) {
	if d.NodeKey == "" {
		return engine.Params{}, invalid("database %s predates swarm mode and has no node key", d.Name)
	}
	eng, _, err := engine.Get(d.Engine)
	if err != nil {
		return engine.Params{}, err
	}
	_, pw, err := a.RevealSuperuser(ctx, d)
	if err != nil {
		return engine.Params{}, err
	}
	p := nodeParams(d)
	p.AdminPassword = pw
	p.Host = swarm.DBService(d.NodeKey)
	p.Port = eng.Spec(p).Port
	t, err := a.nodeTLS(ctx, d)
	if err != nil {
		return engine.Params{}, err
	}
	p.TLS = t
	return p, nil
}

// runJob runs spec as a swarm job and returns its parsed result and the job's stderr tail.
// A non-zero exit becomes an error carrying that tail.
func (a *App) runJob(ctx context.Context, name string, spec job.Spec, networks []string, timeout time.Duration) (map[string]any, string, error) {
	return a.runJobOn(ctx, name, spec, jobPlace{Networks: networks}, timeout)
}

// jobPlace is where and how a job runs, beyond its spec: its networks, the node it is pinned to, a data volume of that
// node and the user it runs as.
type jobPlace struct {
	Networks []string
	NodeID   string
	Volumes  []string
	User     string
}

func (a *App) runJobOn(ctx context.Context, name string, spec job.Spec, pl jobPlace, timeout time.Duration) (map[string]any, string, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, "", err
	}
	res, err := a.RT.RunJob(ctx, swarm.JobSpec{
		Name: name, Image: a.JobImage, Entrypoint: []string{"/fridge"}, Args: []string{"job", "run"},
		Env: []string{job.EnvSpec + "=" + string(raw)}, Networks: pl.Networks, NodeID: pl.NodeID, Volumes: pl.Volumes, User: pl.User, Timeout: timeout,
	})
	out := parseResult(res.Stdout)
	if err != nil {
		return out, res.Stderr, err
	}
	if res.ExitCode != 0 {
		return out, res.Stderr, fmt.Errorf("%s exited %d", name, res.ExitCode)
	}
	return out, res.Stderr, nil
}

// parseResult returns the JSON of the last FRIDGE_RESULT line of a job's stdout (nil when absent).
func parseResult(stdout string) map[string]any {
	var out map[string]any
	sc := bufio.NewScanner(strings.NewReader(stdout))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), job.ResultPrefix); ok {
			var m map[string]any
			if json.Unmarshal([]byte(rest), &m) == nil {
				out = m
			}
		}
	}
	return out
}

func resultInt(m map[string]any, k string) (int64, error) {
	f, ok := m[k].(float64)
	if !ok {
		return 0, errors.New("the job reported no " + k)
	}
	return int64(f), nil
}

// preloadFile puts a backup file into the node's data volume before the node exists: a job pinned to the node's host
// mounts the volume (Docker creates it there) and writes the file where the engine reads it at start.
func (a *App) preloadFile(ctx context.Context, d store.Database, eng engine.Engine, pre *Preload) error {
	s3, guard, err := a.s3Job(ctx, pre.DestinationID)
	if err != nil {
		return err
	}
	spec := job.Spec{Op: "preload", Engine: d.Engine, S3: s3, Guard: guard, Key: pre.Key}
	_, stderr, err := a.runJobOn(ctx, "preload", spec, jobPlace{Networks: []string{a.EgressNetwork}, NodeID: d.HostID, Volumes: []string{d.Volume + ":/data"}, User: "0"}, backupTimeout)
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr))
	}
	return nil
}
