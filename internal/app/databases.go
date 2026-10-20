package app

import (
	"context"
	"crypto/rand"
	"fmt"
	"hash/fnv"
	"log"
	"regexp"
	"strings"
	"time"

	"fridge/internal/container"
	"fridge/internal/engine"
	"fridge/internal/proxy"
	"fridge/internal/store"
)

type CreateRequest struct{ Engine, Name, Domain, Version, TLSMode string }

var (
	nameRe  = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
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
	switch req.TLSMode {
	case "", "internal":
		req.TLSMode = "internal"
	case "acme":
		return store.Database{}, invalid("acme lands in phase 3")
	default:
		return store.Database{}, invalid("tls_mode must be internal")
	}
	if !nameRe.MatchString(req.Name) {
		return store.Database{}, invalid("name must match %s", nameRe)
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

	pw := rand.Text()
	enc, err := a.Box.Encrypt(pw)
	if err != nil {
		return store.Database{}, err
	}
	d := store.Database{
		Name: req.Name, Engine: req.Engine, Version: version, Domain: req.Domain, TLSMode: req.TLSMode,
		User: req.Name + "_app", PasswordEnc: enc,
		Volume: fmt.Sprintf("fridge-%s-%s", req.Engine, req.Name), Status: "creating",
	}
	if err := a.Store.CreateDatabase(ctx, &d); err != nil {
		return store.Database{}, err
	}

	if err := a.provision(ctx, &d, eng, info, pw); err != nil {
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

func (a *App) provision(ctx context.Context, d *store.Database, eng engine.Engine, info engine.Info, pw string) error {
	spec := eng.Spec(engine.Params{Name: d.Name, User: d.User, Password: pw, Version: d.Version})
	id, err := a.RT.Run(ctx, container.RunSpec{
		Name: d.Volume, Image: spec.Image, Env: spec.Env, Volume: d.Volume, VolumePath: spec.VolumePath,
		Healthcheck: spec.Healthcheck, Labels: map[string]string{"fridge.db_id": fmt.Sprint(d.ID)},
	})
	if err != nil {
		return fmt.Errorf("run container: %w", err)
	}
	d.ContainerID = id
	if err := a.RT.WaitHealthy(ctx, id, a.HealthTimeout); err != nil {
		return fmt.Errorf("wait healthy: %w", err)
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
	dests, err := a.Store.ListDestinations(ctx)
	if err != nil || len(dests) == 0 {
		return err
	}
	h := fnv.New32a()
	h.Write([]byte(d.Name))
	sc := store.Schedule{
		DatabaseID: d.ID, DestinationID: dests[0].ID, // ListDestinations is ordered by id
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

func (a *App) ConnString(d store.Database) (string, error) {
	eng, _, err := engine.Get(d.Engine)
	if err != nil {
		return "", err
	}
	pw, err := a.Box.Decrypt(d.PasswordEnc)
	if err != nil {
		return "", err
	}
	return eng.ConnString(engine.Params{Name: d.Name, User: d.User, Password: pw, Version: d.Version}, d.Domain), nil
}

// removeNode tears down container, volume, route and cert, then marks the row "error". Failures are only logged.
func (a *App) removeNode(ctx context.Context, d store.Database) {
	id := d.ContainerID
	if id == "" {
		id = d.Volume // container name == volume name
	}
	if err := a.RT.Remove(ctx, id, d.Volume); err != nil {
		log.Printf("removeNode %s: remove container: %v", d.Name, err)
	}
	if err := proxy.RemoveRoute(a.TraefikDir, d.ID); err != nil {
		log.Printf("removeNode %s: remove route: %v", d.Name, err)
	}
	if err := proxy.RemoveCert(a.TraefikDir, d.ID); err != nil {
		log.Printf("removeNode %s: remove cert: %v", d.Name, err)
	}
	if err := a.Store.SetDatabaseState(ctx, d.ID, "error", ""); err != nil {
		log.Printf("removeNode %s: set state: %v", d.Name, err)
	}
}
