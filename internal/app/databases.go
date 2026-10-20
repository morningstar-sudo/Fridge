package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"fridge/internal/container"
	"fridge/internal/engine"
	"fridge/internal/proxy"
	"fridge/internal/store"
)

type CreateRequest struct {
	Engine, Name, Domain, Version, TLSMode string
	OwnerID                                int64  // required, > 0
	DBUser, DBPassword                     string // user-chosen suffix / password; empty = derived / random
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
	switch req.TLSMode {
	case "", "internal":
		req.TLSMode = "internal"
	case "acme":
		return store.Database{}, invalid("acme lands in phase 3")
	default:
		return store.Database{}, invalid("tls_mode must be internal")
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
	d := store.Database{
		Name: name, Engine: req.Engine, Version: version, Domain: req.Domain, TLSMode: req.TLSMode,
		User: user, PasswordEnc: enc, DomainID: &domainID,
		Volume: fmt.Sprintf("fridge-%s-%s", req.Engine, nodeKey), Status: "creating", OwnerID: &req.OwnerID,
		NodeKey: nodeKey, AdminUser: "su_" + nodeKey, AdminPasswordEnc: adminEnc,
	}
	if err := a.Store.CreateDatabase(ctx, &d); err != nil {
		return store.Database{}, err
	}

	if err := a.provision(ctx, &d, eng, info, pw, adminPW); err != nil {
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

// provision runs the node as its superuser, then creates the non-superuser app user owning the database.
// Passwords travel in env (container init) and on exec stdin, never in argv.
func (a *App) provision(ctx context.Context, d *store.Database, eng engine.Engine, info engine.Info, pw, adminPW string) error {
	p := nodeParams(*d)
	p.Password, p.AdminPassword = pw, adminPW
	spec := eng.Spec(p)
	// The node lives only on its own internal network with Traefik: no control plane, no peers, no internet.
	net := NodeNetwork(*d)
	if err := a.RT.CreateNetwork(ctx, net); err != nil {
		return fmt.Errorf("create network: %w", err)
	}
	if err := a.RT.ConnectNetwork(ctx, net, a.TraefikContainer, nil); err != nil {
		return fmt.Errorf("connect proxy: %w", err)
	}
	id, err := a.RT.Run(ctx, container.RunSpec{
		Name: d.Volume, Image: spec.Image, Env: spec.Env, Volume: d.Volume, VolumePath: spec.VolumePath,
		Healthcheck: spec.Healthcheck, Labels: map[string]string{"fridge.db_id": fmt.Sprint(d.ID)}, Network: net,
	})
	if err != nil {
		return fmt.Errorf("run container: %w", err)
	}
	d.ContainerID = id
	if err := a.RT.WaitHealthy(ctx, id, a.HealthTimeout); err != nil {
		return fmt.Errorf("wait healthy: %w", err)
	}
	code, err := a.RT.Exec(ctx, id, eng.AdminCmd(p), strings.NewReader(eng.AppUserSQL(d.User, pw, d.Name)), io.Discard, io.Discard)
	switch {
	case err != nil:
		return fmt.Errorf("create app user: %w", err)
	case code != 0: // stderr deliberately dropped: psql error context can echo the statement, password included
		return fmt.Errorf("create app user: exited %d", code)
	}
	certPEM, keyPEM, err := a.CA.Issue(d.Domain, 365*24*time.Hour)
	if err != nil {
		return fmt.Errorf("issue cert: %w", err)
	}
	certFile, keyFile, err := proxy.WriteCert(a.TraefikDir, d.ID, certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("write cert: %w", err)
	}
	if err := proxy.WriteRoute(a.TraefikDir, proxy.Route{
		ID: d.ID, EntryPoint: info.EntryPoint, Domain: d.Domain,
		Backend: fmt.Sprintf("%s:%d", d.Volume, spec.Port), CertFile: certFile, KeyFile: keyFile,
	}); err != nil {
		return fmt.Errorf("write route: %w", err)
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

// SuperuserConnString connects as the node superuser (a legacy node's db_user).
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

// nodeParams is d without secrets. Legacy (phase-1) nodes have no superuser: their db_user is the admin.
func nodeParams(d store.Database) engine.Params {
	p := engine.Params{Name: d.Name, User: d.User, Version: d.Version, AdminUser: d.AdminUser}
	if p.AdminUser == "" {
		p.AdminUser = d.User
	}
	return p
}

// removeNode tears down container, volume, node network, route and cert, then marks the row "error". Failures are only logged.
func (a *App) removeNode(ctx context.Context, d store.Database) {
	// Rollback must survive a cancelled request context.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	id := d.ContainerID
	if id == "" {
		id = d.Volume // container name == volume name
	}
	a.netMu.Lock() // a heal must not re-attach Traefik halfway through the teardown
	if err := a.RT.Remove(ctx, id, d.Volume); err != nil {
		log.Printf("removeNode %s: remove container: %v", d.Name, err)
	}
	net := NodeNetwork(d)
	if err := a.RT.DisconnectNetwork(ctx, net, a.TraefikContainer); err != nil {
		log.Printf("removeNode %s: disconnect proxy: %v", d.Name, err)
	}
	if err := a.RT.RemoveNetwork(ctx, net); err != nil {
		log.Printf("removeNode %s: remove network: %v", d.Name, err)
	}
	a.netMu.Unlock()
	if err := proxy.RemoveRoute(a.TraefikDir, d.ID); err != nil {
		log.Printf("removeNode %s: remove route: %v", d.Name, err)
	}
	if err := proxy.RemoveCert(a.TraefikDir, d.ID); err != nil {
		log.Printf("removeNode %s: remove cert: %v", d.Name, err)
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
