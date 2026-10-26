// Package swarm runs Fridge's database nodes and one-shot jobs as Docker Swarm services.
// A database is a pinned single-replica service with a local volume plus a tiny "edge" sidecar that sits on
// both the shared edge network and the database's private overlay, so nothing else ever joins that overlay.
// Everything here goes through the manager's Docker API: no agent runs on the other nodes.
package swarm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	LabelRole = "fridge.role"     // "db" | "edge" | "job"
	LabelKey  = "fridge.node_key" // ties a database's services and network together
	LabelDB   = "fridge.db_id"
	NodeLabel = "fridge.db" // swarm node label: "true" = databases may be placed here
	EdgePort  = 5432
)

// Names derived from a node key. Docker DNS resolves them inside the overlays.
func DBService(key string) string   { return "fridge-db-" + key }
func EdgeService(key string) string { return "fridge-edge-" + key }

type Config struct {
	EdgeNetwork   string // shared by the proxy, Fridge and every sidecar
	DBNetwork     string // internal: every database, its sidecar and the jobs that work on it; databases accept only their own certificate
	EgressNetwork string // non-internal and otherwise empty: gives jobs internet without exposing anything
	ServerImage   string // the Fridge image: runs the sidecars and the small maintenance jobs
}

type Client struct {
	cli *client.Client
	cfg Config
}

func New(cfg Config) (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &Client{cli: cli, cfg: cfg}, nil
}

func (c *Client) Close() error { return c.cli.Close() }

// Ping succeeds on a swarm manager, the only place services can be managed from.
func (c *Client) Ping(ctx context.Context) error {
	info, err := c.cli.Info(ctx)
	if err != nil {
		return err
	}
	if !info.Swarm.ControlAvailable {
		return errors.New("this Docker daemon is not a swarm manager")
	}
	return nil
}

// EnsureNetworks creates the shared overlays when they are missing (the stack normally does).
func (c *Client) EnsureNetworks(ctx context.Context) error {
	for _, n := range []string{c.cfg.EdgeNetwork, c.cfg.EgressNetwork, c.cfg.DBNetwork} {
		if ok, err := c.networkExists(ctx, n); err != nil {
			return err
		} else if ok {
			continue
		}
		_, err := c.cli.NetworkCreate(ctx, n, network.CreateOptions{Driver: "overlay", Attachable: true, Internal: n == c.cfg.DBNetwork, Options: map[string]string{"encrypted": ""}})
		if err != nil && !errdefs.IsConflict(err) {
			return fmt.Errorf("create network %s: %w", n, err)
		}
	}
	return nil
}

func (c *Client) networkExists(ctx context.Context, name string) (bool, error) {
	list, err := c.cli.NetworkList(ctx, network.ListOptions{Filters: filters.NewArgs(filters.Arg("name", name))})
	if err != nil {
		return false, err
	}
	for _, n := range list { // the name filter matches substrings
		if n.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// ---- hosts ----

type Host struct{ ID, Hostname string }

// PickHost chooses the ready, active node labelled fridge.db=true that holds the fewest databases.
func (c *Client) PickHost(ctx context.Context) (Host, error) {
	nodes, err := c.cli.NodeList(ctx, swarm.NodeListOptions{})
	if err != nil {
		return Host{}, err
	}
	svcs, err := c.cli.ServiceList(ctx, swarm.ServiceListOptions{Filters: filters.NewArgs(filters.Arg("label", LabelRole+"=db"))})
	if err != nil {
		return Host{}, err
	}
	load := map[string]int{}
	for _, s := range svcs {
		if pl := s.Spec.TaskTemplate.Placement; pl != nil {
			for _, con := range pl.Constraints {
				if id, ok := strings.CutPrefix(con, "node.id == "); ok {
					load[id]++
				}
			}
		}
	}
	var best *swarm.Node
	for i := range nodes {
		n := &nodes[i]
		if n.Status.State != swarm.NodeStateReady || n.Spec.Availability != swarm.NodeAvailabilityActive || n.Spec.Labels[NodeLabel] != "true" {
			continue
		}
		if best == nil || load[n.ID] < load[best.ID] || (load[n.ID] == load[best.ID] && n.Description.Hostname < best.Description.Hostname) {
			best = n
		}
	}
	if best == nil {
		return Host{}, fmt.Errorf("no ready swarm node has the label %s=true (docker node update --label-add %s=true <node>)", NodeLabel, NodeLabel)
	}
	return Host{ID: best.ID, Hostname: best.Description.Hostname}, nil
}

// ---- database nodes ----

type NodeSpec struct {
	Key         string
	DBID        int64
	HostID      string // swarm node ID the database and its volume are pinned to
	Image       string
	Env         []string
	VolumeName  string
	VolumePath  string
	Port        int
	Healthcheck []string
	Command     []string // replaces the image's entrypoint (nil keeps it)
	EdgeEnv     []string // the sidecar's environment: its client certificate and the CA (with TLS)
	TLS         bool     // the sidecar speaks TLS with a client certificate to the database
}

type NodeRef struct {
	Key, HostID, Volume string
}

func one() *uint64 { v := uint64(1); return &v }

func labels(role, key string, dbID int64) map[string]string {
	return map[string]string{LabelRole: role, LabelKey: key, LabelDB: fmt.Sprint(dbID)}
}

func pin(hostID string) *swarm.Placement {
	return &swarm.Placement{Constraints: []string{"node.id == " + hostID}}
}

func (c *Client) serviceExists(ctx context.Context, name string) (bool, error) {
	_, _, err := c.cli.ServiceInspectWithRaw(ctx, name, swarm.ServiceInspectOptions{})
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// CreateNode builds the database service and its edge sidecar on the shared database network. Safe to repeat.
func (c *Client) CreateNode(ctx context.Context, n NodeSpec) error {
	db := DBService(n.Key)
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: db, Labels: labels("db", n.Key, n.DBID)},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: n.Image, Command: n.Command, Env: n.Env, Labels: labels("db", n.Key, n.DBID), StopGracePeriod: ptr(60 * time.Second),
				Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: n.VolumeName, Target: n.VolumePath, VolumeOptions: &mount.VolumeOptions{Labels: labels("db", n.Key, n.DBID)}}},
			},
			Placement:     pin(n.HostID),
			Networks:      []swarm.NetworkAttachmentConfig{{Target: c.cfg.DBNetwork, Aliases: []string{db}}},
			RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionAny, Delay: ptr(5 * time.Second)},
		},
		Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: one()}},
		EndpointSpec: &swarm.EndpointSpec{Mode: swarm.ResolutionModeDNSRR},   // a VIP's idle timeout would cut quiet connections
		UpdateConfig: &swarm.UpdateConfig{Order: swarm.UpdateOrderStopFirst}, // the volume cannot be shared by two tasks
	}
	if len(n.Healthcheck) > 0 {
		spec.TaskTemplate.ContainerSpec.Healthcheck = &container.HealthConfig{Test: n.Healthcheck, Interval: 2 * time.Second, Timeout: 5 * time.Second, Retries: 15, StartPeriod: 2 * time.Second}
	}
	if err := c.createService(ctx, spec); err != nil {
		return fmt.Errorf("create database service: %w", err)
	}
	edge := EdgeService(n.Key)
	espec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: edge, Labels: labels("edge", n.Key, n.DBID)},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: c.cfg.ServerImage, Command: []string{"/fridge"},
				Args:   edgeArgs(db, n),
				Env:    n.EdgeEnv,
				Labels: labels("edge", n.Key, n.DBID),
			},
			Placement: pin(n.HostID), // next to the database: the second leg never crosses hosts
			Networks: []swarm.NetworkAttachmentConfig{
				{Target: c.cfg.EdgeNetwork, Aliases: []string{edge}},
				{Target: c.cfg.DBNetwork},
			},
			RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionAny, Delay: ptr(2 * time.Second)},
		},
		Mode:         swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: one()}},
		EndpointSpec: &swarm.EndpointSpec{Mode: swarm.ResolutionModeDNSRR},
	}
	if err := c.createService(ctx, espec); err != nil {
		return fmt.Errorf("create edge service: %w", err)
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

func (c *Client) createService(ctx context.Context, spec swarm.ServiceSpec) error {
	if ok, err := c.serviceExists(ctx, spec.Name); err != nil || ok {
		return err
	}
	_, err := c.cli.ServiceCreate(ctx, spec, swarm.ServiceCreateOptions{})
	if errdefs.IsConflict(err) {
		return nil
	}
	return err
}

// NodeReady waits until the database's task is running (its healthcheck, if any, has passed).
func (c *Client) NodeReady(ctx context.Context, key string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last string
	for {
		tasks, err := c.cli.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(filters.Arg("service", DBService(key)), filters.Arg("desired-state", "running"))})
		if err == nil && len(tasks) > 0 {
			sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.After(tasks[j].CreatedAt) })
			t := tasks[0]
			if t.Status.State == swarm.TaskStateRunning {
				return nil
			}
			last = fmt.Sprintf("%s %s %s", t.Status.State, t.Status.Message, t.Status.Err)
		} else if err != nil && ctx.Err() == nil {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("database %s not running within %s: %s", key, timeout, strings.TrimSpace(last))
		case <-time.After(time.Second):
		}
	}
}

// RemoveNode deletes both services, waits for their tasks to go, then the network and the volume
// (the volume lives on one host: a small job pinned there removes it). Missing pieces are fine.
func (c *Client) RemoveNode(ctx context.Context, ref NodeRef) error {
	var errs []error
	for _, s := range []string{EdgeService(ref.Key), DBService(ref.Key)} {
		if err := c.cli.ServiceRemove(ctx, s); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove %s: %w", s, err))
		}
	}
	c.waitNoTasks(ctx, ref.Key)
	if ref.Volume != "" && ref.HostID != "" {
		res, err := c.RunJob(ctx, JobSpec{Name: "rmvol", Image: c.cfg.ServerImage, Entrypoint: []string{"/fridge"}, Args: []string{"job", "rmvolume", ref.Volume},
			NodeID: ref.HostID, Binds: []string{"/var/run/docker.sock:/var/run/docker.sock"}, Timeout: time.Minute})
		if err == nil && res.ExitCode != 0 {
			err = fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("remove volume %s: %w", ref.Volume, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Client) waitNoTasks(ctx context.Context, key string) {
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		tasks, err := c.cli.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(filters.Arg("label", LabelKey+"="+key))})
		live := 0
		for _, t := range tasks {
			if t.Status.State == swarm.TaskStateRunning || t.Status.State == swarm.TaskStateStarting || t.Status.State == swarm.TaskStatePreparing || t.Status.State == swarm.TaskStateReady {
				live++
			}
		}
		if err != nil || live == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// NodeState is what exists in the swarm for one database key.
type NodeState struct {
	Key      string
	DBID     string
	HostID   string
	DB, Edge bool
}

// ListNodes reports the services that carry a node key.
func (c *Client) ListNodes(ctx context.Context) ([]NodeState, error) {
	svcs, err := c.cli.ServiceList(ctx, swarm.ServiceListOptions{Filters: filters.NewArgs(filters.Arg("label", LabelKey))})
	if err != nil {
		return nil, err
	}
	by := map[string]*NodeState{}
	get := func(key string) *NodeState {
		if by[key] == nil {
			by[key] = &NodeState{Key: key}
		}
		return by[key]
	}
	for _, s := range svcs {
		st := get(s.Spec.Labels[LabelKey])
		st.DBID = s.Spec.Labels[LabelDB]
		switch s.Spec.Labels[LabelRole] {
		case "db":
			st.DB = true
			if pl := s.Spec.TaskTemplate.Placement; pl != nil {
				for _, con := range pl.Constraints {
					if id, ok := strings.CutPrefix(con, "node.id == "); ok {
						st.HostID = id
					}
				}
			}
		case "edge":
			st.Edge = true
		}
	}
	out := make([]NodeState, 0, len(by))
	for _, st := range by {
		out = append(out, *st)
	}
	slices.SortFunc(out, func(a, b NodeState) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

// Subnets are the address ranges of every overlay (edge, egress, metadata, each database, ingress) and bridge: what a user-supplied
// S3 endpoint must never point into.
func (c *Client) Subnets(ctx context.Context) ([]netip.Prefix, error) {
	list, err := c.cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, n := range list {
		if n.Driver != "overlay" && n.Driver != "bridge" { // every network Fridge or its stack lives on, and the host's bridges
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

// ---- jobs ----

type JobSpec struct {
	Name       string // prefix of the service name
	Image      string
	Entrypoint []string
	Args       []string
	Env        []string
	Networks   []string
	NodeID     string   // "" = any node
	Binds      []string // "host:container"
	Labels     map[string]string
	Timeout    time.Duration
}

type JobResult struct {
	ExitCode       int
	Stdout, Stderr string // the last ~64 KiB of each
}

const logTail = 64 << 10

// RunJob runs a one-shot service (a swarm "replicated job") to completion and returns its exit code and output.
// The service is always removed. A task that cannot even start (image missing, no node fits) is an error.
func (c *Client) RunJob(ctx context.Context, j JobSpec) (JobResult, error) {
	var rnd [4]byte
	rand.Read(rnd[:])
	name := fmt.Sprintf("fridge-job-%s-%s", j.Name, hex.EncodeToString(rnd[:]))
	if j.Timeout == 0 {
		j.Timeout = 6 * time.Hour
	}
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: name, Labels: mergeLabels(j.Labels, map[string]string{LabelRole: "job"})},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{Image: j.Image, Command: j.Entrypoint, Args: j.Args, Env: j.Env, Labels: map[string]string{LabelRole: "job"}},
			RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionNone},
		},
		Mode: swarm.ServiceMode{ReplicatedJob: &swarm.ReplicatedJob{MaxConcurrent: one(), TotalCompletions: one()}},
	}
	for _, n := range j.Networks {
		spec.TaskTemplate.Networks = append(spec.TaskTemplate.Networks, swarm.NetworkAttachmentConfig{Target: n})
	}
	if j.NodeID != "" {
		spec.TaskTemplate.Placement = pin(j.NodeID)
	}
	for _, b := range j.Binds {
		src, dst, _ := strings.Cut(b, ":")
		spec.TaskTemplate.ContainerSpec.Mounts = append(spec.TaskTemplate.ContainerSpec.Mounts, mount.Mount{Type: mount.TypeBind, Source: src, Target: dst})
	}
	resp, err := c.cli.ServiceCreate(ctx, spec, swarm.ServiceCreateOptions{})
	if err != nil {
		return JobResult{}, fmt.Errorf("create job: %w", err)
	}
	defer func() { // the job must go even when the caller's context is gone
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		c.cli.ServiceRemove(rctx, resp.ID)
	}()

	deadline := time.NewTimer(j.Timeout)
	defer deadline.Stop()
	var task swarm.Task
	for {
		tasks, err := c.cli.TaskList(ctx, swarm.TaskListOptions{Filters: filters.NewArgs(filters.Arg("service", resp.ID))})
		if err != nil && ctx.Err() == nil {
			return JobResult{}, err
		}
		done := false
		for _, t := range tasks {
			switch t.Status.State {
			case swarm.TaskStateComplete, swarm.TaskStateFailed:
				task, done = t, true
			case swarm.TaskStateRejected:
				return JobResult{}, fmt.Errorf("job rejected: %s", t.Status.Err)
			}
		}
		if done {
			break
		}
		select {
		case <-ctx.Done():
			return JobResult{}, ctx.Err()
		case <-deadline.C:
			return JobResult{}, fmt.Errorf("job %s did not finish within %s", j.Name, j.Timeout)
		case <-time.After(500 * time.Millisecond):
		}
	}
	res := JobResult{}
	if task.Status.State == swarm.TaskStateFailed {
		res.ExitCode = 1
		if cs := task.Status.ContainerStatus; cs != nil && cs.ExitCode != 0 {
			res.ExitCode = cs.ExitCode
		}
	}
	rc, err := c.cli.ServiceLogs(ctx, resp.ID, container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return res, fmt.Errorf("job logs: %w", err)
	}
	defer rc.Close()
	var out, errb tail
	if _, err := stdcopy.StdCopy(&out, &errb, rc); err != nil {
		return res, fmt.Errorf("job logs: %w", err)
	}
	res.Stdout, res.Stderr = out.String(), errb.String()
	if res.ExitCode != 0 && strings.TrimSpace(res.Stderr) == "" {
		res.Stderr = task.Status.Err
	}
	return res, nil
}

func mergeLabels(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// tail keeps the last logTail bytes written to it.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2*logTail {
		t.b = append(t.b[:0], t.b[len(t.b)-logTail:]...)
	}
	return len(p), nil
}

func (t *tail) String() string {
	b := t.b
	if len(b) > logTail {
		b = b[len(b)-logTail:]
	}
	return strings.ToValidUTF8(string(b), "?")
}

// SubnetsOf is the address ranges of the named networks.
func (c *Client) SubnetsOf(ctx context.Context, names ...string) ([]netip.Prefix, error) {
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

// EdgeNetwork is the shared network's name (the proxy sits on it).
func (c *Client) EdgeNetwork() string { return c.cfg.EdgeNetwork }

func edgeArgs(db string, n NodeSpec) []string {
	args := []string{"forward", "--listen", fmt.Sprintf(":%d", EdgePort), "--to", fmt.Sprintf("%s:%d", db, n.Port)}
	if n.TLS {
		args = append(args, "--tls", "--server-name", db)
	}
	return args
}
