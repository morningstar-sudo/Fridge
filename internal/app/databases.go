package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"fridge/internal/engine"
	"fridge/internal/store"
	"fridge/internal/swarm"
)

// Preload is a backup file to put in the data volume of a node before it starts (see engine.Engine.Preload).
type Preload struct {
	DestinationID int64
	Key           string
}

type CreateRequest struct {
	Engine, Name, Domain, Version string
	OwnerID                       int64    // required, > 0
	DBUser, DBPassword            string   // user-chosen suffix / password; empty = derived / random
	Preload                       *Preload // restore into a new node: a file the node must start with (Redis)
}

var (
	labelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

func validDomain(d string) bool {
	if len(d) > 253 || !strings.Contains(d, ".") {
		return false
	}
	for _, l := range strings.Split(d, ".") {
		if !labelRe.MatchString(l) {
			return false
		}
	}
	return true
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func (a *App) CreateDatabase(ctx context.Context, req CreateRequest) (store.Database, error) {
	req.Domain = strings.ToLower(strings.TrimSpace(req.Domain))
	if req.OwnerID <= 0 {
		return store.Database{}, invalid("owner is required")
	}
	if !validDomain(req.Domain) {
		return store.Database{}, invalid("domain must be a hostname")
	}
	eng, info, err := engine.Get(req.Engine)
	if err != nil {
		return store.Database{}, invalid("%v", err)
	}
	version, err := engine.ResolveVersion(req.Engine, req.Version)
	if err != nil {
		return store.Database{}, invalid("%v", err)
	}

	name, user, domainID, err := a.resolveNames(ctx, req)
	if err != nil {
		return store.Database{}, err
	}
	pw := req.DBPassword
	if pw == "" {
		pw = rand.Text()
	}
	enc, err := a.Box.Encrypt(pw)
	if err != nil {
		return store.Database{}, err
	}
	// name is "<owner>_<suffix>", so this hashes owner + "_" + suffix + "_" + nanos.
	sum := sha256.Sum256([]byte(name + "_" + strconv.FormatInt(a.clock().UnixNano(), 10)))
	nodeKey := hex.EncodeToString(sum[:])[:16]
	adminPW := rand.Text()
	adminEnc, err := a.Box.Encrypt(adminPW)
	if err != nil {
		return store.Database{}, err
	}
	host, err := a.RT.PickHost(ctx) // the database and its volume stay on this node for good
	if err != nil {
		return store.Database{}, err
	}
	d := store.Database{
		HostID: host.ID,
		Name:   name, Engine: req.Engine, Version: version, Domain: req.Domain,
		User: user, PasswordEnc: enc, DomainID: &domainID,
		Volume: fmt.Sprintf("fridge-%s-%s", req.Engine, nodeKey), Status: "creating", OwnerID: &req.OwnerID,
		NodeKey: nodeKey, AdminUser: "su_" + nodeKey, AdminPasswordEnc: adminEnc,
	}
	if err := a.Store.CreateDatabase(ctx, &d); err != nil {
		return store.Database{}, err
	}

	if err := a.provision(ctx, &d, eng, info, pw, adminPW, req.Preload); err != nil {
		if logs := a.RT.NodeLogs(context.WithoutCancel(ctx), d.NodeKey); strings.TrimSpace(logs) != "" { // gone with the node: keep what it said
			log.Printf("database %s failed to start: %v\n%s", d.Name, err, logs)
		}
		a.removeNode(ctx, d)
		return store.Database{}, err
	}
	if err := a.Store.SetDatabaseState(ctx, d.ID, "running", d.ContainerID); err != nil {
		a.removeNode(ctx, d)
		return store.Database{}, err
	}
	d.Status = "running"

	if err := a.defaultSchedule(ctx, d); err != nil {
		log.Printf("default schedule for %s: %v", d.Name, err)
	}
	return d, nil
}

// provision starts the node (its overlay network, the pinned database service and the edge sidecar), then creates the
// non-superuser app user owning the database through the sidecar, (its TLS material is issued first; Traefik learns the route by polling ProxyConfig).
// Passwords travel in the service environment (container init) and on the SQL connection, never in argv.
func (a *App) provision(ctx context.Context, d *store.Database, eng engine.Engine, info engine.Info, pw, adminPW string, pre *Preload) error {
	p := nodeParams(*d)
	p.Password, p.AdminPassword = pw, adminPW
	if err := a.issueNodeTLS(ctx, *d); err != nil {
		return fmt.Errorf("issue node certificate: %w", err)
	}
	tlsm, err := a.nodeTLS(ctx, *d)
	if err != nil {
		return err
	}
	p.TLS = tlsm
	spec := eng.Spec(p)
	if pre != nil {
		if err := a.preloadFile(ctx, *d, eng, pre); err != nil {
			return fmt.Errorf("load the backup into the volume: %w", err)
		}
	}
	err = a.RT.CreateNode(ctx, swarm.NodeSpec{
		Key: d.NodeKey, DBID: d.ID, HostID: d.HostID, Image: spec.Image, Env: spec.Env,
		VolumeName: d.Volume, VolumePath: spec.VolumePath, Port: spec.Port, Healthcheck: spec.Healthcheck,
		Command: spec.Command, EdgeEnv: edgeEnv(tlsm), Upstream: eng.Upstream(),
	})
	if err != nil {
		return fmt.Errorf("create node: %w", err)
	}
	d.ContainerID = swarm.DBService(d.NodeKey) // the swarm service name; the column keeps its old name
	if err := a.RT.NodeReady(ctx, d.NodeKey, a.HealthTimeout); err != nil {
		return fmt.Errorf("wait healthy: %w", err)
	}
	actx, cancel := context.WithTimeout(ctx, a.HealthTimeout)
	defer cancel()
	// Errors are not wrapped with the SQL: it carries the app user's password.
	if err := a.execAsAdmin(actx, *d, eng, adminPW, eng.AppUserSQL(d.User, pw, d.Name)); err != nil {
		return fmt.Errorf("create app user: %w", err)
	}
	return nil
}

func (a *App) defaultSchedule(ctx context.Context, d store.Database) error {
	dest, ok, err := a.ownerDestination(ctx, d)
	if err != nil || !ok {
		return err
	}
	h := fnv.New32a()
	h.Write([]byte(d.Name))
	sc := store.Schedule{
		DatabaseID: d.ID, DestinationID: dest.ID,
		Cron: fmt.Sprintf("%d 2 * * *", h.Sum32()%60), Prefix: "fridge", KeepLatest: 7, Enabled: true,
	}
	if err := a.Store.CreateSchedule(ctx, &sc); err != nil {
		return err
	}
	if a.Sched != nil {
		return a.Sched.Reload(ctx)
	}
	return nil
}

// ConnString is the app user's full connection string, password included.
func (a *App) ConnString(d store.Database) (string, error) {
	pw, err := a.Box.Decrypt(d.PasswordEnc)
	if err != nil {
		return "", err
	}
	return connAs(d, d.User, pw)
}

// MaskedConnString is the app user's connection string with the password shown as "****".
func MaskedConnString(d store.Database) (string, error) {
	const mark = "FRIDGEMASKEDPASSWORD" // survives URL escaping unchanged
	cs, err := connAs(d, d.User, mark)
	return strings.Replace(cs, mark, "****", 1), err
}

// SuperuserConnString connects as the node superuser.
func (a *App) SuperuserConnString(ctx context.Context, d store.Database) (string, error) {
	u, pw, err := a.RevealSuperuser(ctx, d)
	if err != nil {
		return "", err
	}
	return connAs(d, u, pw)
}

func connAs(d store.Database, user, pw string) (string, error) {
	eng, _, err := engine.Get(d.Engine)
	if err != nil {
		return "", err
	}
	p := nodeParams(d)
	p.User, p.Password = user, pw
	return eng.ConnString(p, d.Domain), nil
}

// nodeParams is d without secrets.
func nodeParams(d store.Database) engine.Params {
	return engine.Params{Name: d.Name, User: d.User, Version: d.Version, AdminUser: d.AdminUser}
}

// removeNode tears down the node's services, network and volume, its route and cert, then marks the row "error". Failures are only logged.
func (a *App) removeNode(ctx context.Context, d store.Database) {
	// Rollback must survive a cancelled request context.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	if d.NodeKey != "" {
		if err := a.RT.RemoveNode(ctx, swarm.NodeRef{Key: d.NodeKey, HostID: d.HostID, Volume: d.Volume}); err != nil {
			log.Printf("removeNode %s: %v", d.Name, err)
		}
	}
	if err := a.Store.SetDatabaseState(ctx, d.ID, "error", ""); err != nil {
		log.Printf("removeNode %s: set state: %v", d.Name, err)
	}
	// A dead node must not keep a schedule: it would alert nightly and pin its destination.
	if err := a.Store.DeleteSchedulesForDatabase(ctx, d.ID); err != nil {
		log.Printf("removeNode %s: delete schedules: %v", d.Name, err)
	} else if a.Sched != nil {
		if err := a.Sched.Reload(ctx); err != nil {
			log.Printf("removeNode %s: reload scheduler: %v", d.Name, err)
		}
	}
}

// RecoverInterrupted tears down nodes left half-built by a restart: databases still "creating"
// and the targets of restores that failed (FailRunning marks interrupted ones failed). Run it
// after FailRunning, before serving.
func (a *App) RecoverInterrupted(ctx context.Context) error {
	dbs, err := a.Store.ListDatabases(ctx)
	if err != nil {
		return err
	}
	var victims []store.Database
	for _, d := range dbs {
		if d.Status == "creating" {
			victims = append(victims, d)
		}
	}
	targets, err := a.Store.OrphanedRestoreTargets(ctx)
	if err != nil {
		return err
	}
	victims = append(victims, targets...)
	if len(victims) == 0 {
		return nil
	}
	names := make([]string, len(victims))
	for i, d := range victims {
		a.removeNode(ctx, d)
		names[i] = d.Name
	}
	a.alertf(ctx, "removed %d node(s) interrupted by restart: %s", len(victims), strings.Join(names, ", "))
	return nil
}
