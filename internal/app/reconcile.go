package app

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"fridge/internal/engine"
	"fridge/internal/swarm"
)

// reconciler remembers which problems were already reported, so a missing service alerts once, not every run.
type reconciler struct {
	mu       sync.Mutex
	reported map[string]bool
}

// Reconcile compares the databases that should be running with what the swarm has and repairs what is safe to
// repair without losing data:
//   - a running database whose edge sidecar is gone gets it back (CreateNode is idempotent);
//   - the services, network and volume of a failed or removed database are cleaned up;
//   - a missing database service is only reported: recreating it could start an empty database over a lost volume;
//   - swarm resources that match no database row are only reported: Fridge cannot tell what their volume holds.
func (a *App) Reconcile(ctx context.Context) error {
	dbs, err := a.Store.ListDatabases(ctx)
	if err != nil {
		return err
	}
	states, err := a.RT.ListNodes(ctx)
	if err != nil {
		return err
	}
	byKey := map[string]swarm.NodeState{}
	for _, s := range states {
		byKey[s.Key] = s
	}
	problems := map[string]bool{}
	for _, d := range dbs {
		if d.NodeKey == "" {
			continue
		}
		st, has := byKey[d.NodeKey]
		delete(byKey, d.NodeKey)
		if d.Status == "running" && has {
			if t, err := a.Store.GetDBTLS(ctx, d.ID); err == nil && time.Until(t.NotAfter) < 60*24*time.Hour {
				problems[fmt.Sprintf("database %s: its node certificate expires on %s; recreate the database or restore it into a new one before then", d.Name, t.NotAfter.Format("2006-01-02"))] = true
			}
		}
		switch d.Status {
		case "running":
			switch {
			case !has || !st.DB:
				problems[fmt.Sprintf("database %s has no service in the swarm", d.Name)] = true
			case !st.Edge && engineHasSidecar(d.Engine):
				eng, _, err := engine.Get(d.Engine)
				if err != nil {
					continue
				}
				p := nodeParams(d)
				tlsm, err := a.nodeTLS(ctx, d)
				if err != nil {
					problems[fmt.Sprintf("database %s has no edge sidecar and cannot get one: %v", d.Name, err)] = true
					continue
				}
				err = a.RT.CreateNode(ctx, swarm.NodeSpec{Key: d.NodeKey, DBID: d.ID, HostID: d.HostID, Port: eng.Spec(p).Port, EdgeEnv: edgeEnv(tlsm), Upstream: eng.Upstream()})
				if err != nil {
					problems[fmt.Sprintf("database %s: repairing its edge sidecar failed: %v", d.Name, err)] = true
					continue
				}
				log.Printf("reconcile: restored the edge sidecar of %s", d.Name)
				a.alertf(ctx, "database %s: its edge sidecar was missing and has been recreated", d.Name)
			}
		case "error", "deleted":
			if has {
				if err := a.RT.RemoveNode(ctx, swarm.NodeRef{Key: d.NodeKey, HostID: d.HostID, Volume: d.Volume}); err != nil {
					log.Printf("reconcile: clean up %s: %v", d.Name, err)
				} else {
					log.Printf("reconcile: removed the leftovers of %s", d.Name)
				}
			}
		}
	}
	for _, st := range byKey {
		problems[fmt.Sprintf("swarm resources %s (db id %s) belong to no database; left alone", st.Key, st.DBID)] = true
	}

	a.rec.mu.Lock()
	if a.rec.reported == nil {
		a.rec.reported = map[string]bool{}
	}
	var fresh []string
	for p := range problems {
		if !a.rec.reported[p] {
			fresh = append(fresh, p)
		}
	}
	for p := range a.rec.reported { // resolved problems may be reported again if they come back
		if !problems[p] {
			delete(a.rec.reported, p)
		}
	}
	for _, p := range fresh {
		a.rec.reported[p] = true
	}
	a.rec.mu.Unlock()
	sort.Strings(fresh)
	for _, p := range fresh {
		log.Printf("reconcile: %s", p)
		a.alertf(ctx, "%s", p)
	}
	return nil
}

// engineHasSidecar: every engine but the ones with their own router (MySQL, MariaDB) is reached through an edge sidecar.
func engineHasSidecar(name string) bool {
	eng, _, err := engine.Get(name)
	return err == nil && eng.Upstream() != "none"
}
