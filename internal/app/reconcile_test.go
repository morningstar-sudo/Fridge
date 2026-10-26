package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"fridge/internal/swarm"
)

func TestReconcileReportsAMissingServiceOnce(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx := context.Background()
	d, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "abc", Domain: "abc.com"})
	if err != nil {
		t.Fatal(err)
	}
	rt.alerts = nil
	for range 3 { // the swarm knows nothing of the node
		if err := a.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(rt.alerts) != 1 || !strings.Contains(rt.alerts[0], d.Name) {
		t.Fatalf("alerts = %q", rt.alerts)
	}
	if len(rt.runs) != 1 {
		t.Fatalf("reconcile recreated a database service: %d runs", len(rt.runs)) // only the original create
	}
	// it comes back, then goes again: that is news
	rt.nodes = []swarm.NodeState{{Key: d.NodeKey, HostID: d.HostID, DB: true, Edge: true}}
	a.Reconcile(ctx)
	rt.nodes = nil
	a.Reconcile(ctx)
	if len(rt.alerts) != 2 {
		t.Fatalf("alerts = %q", rt.alerts)
	}
}

func TestReconcileRestoresASidecar(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx := context.Background()
	d, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "abc", Domain: "abc.com"})
	if err != nil {
		t.Fatal(err)
	}
	rt.alerts, rt.runs = nil, nil
	rt.nodes = []swarm.NodeState{{Key: d.NodeKey, HostID: d.HostID, DB: true}} // no edge
	if err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rt.runs) != 1 || rt.runs[0].Spec.Key != d.NodeKey || rt.runs[0].Spec.HostID != d.HostID || rt.runs[0].Spec.Port != 5432 {
		t.Fatalf("runs = %+v", rt.runs)
	}
	if len(rt.alerts) != 1 || !strings.Contains(rt.alerts[0], "edge sidecar") {
		t.Fatalf("alerts = %q", rt.alerts)
	}
}

func TestReconcileCleansFailedNodesAndLeavesOrphansAlone(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx := context.Background()
	rt.healthErr = errors.New("unhealthy")
	if _, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "abc", Domain: "abc.com"}); err == nil {
		t.Fatal("want a failed node")
	}
	dbs, _ := a.Store.ListDatabases(ctx)
	failed := dbs[0]
	rt.removes, rt.alerts = nil, nil
	rt.nodes = []swarm.NodeState{
		{Key: failed.NodeKey, HostID: failed.HostID, DB: true},       // left behind by a failed create
		{Key: "feedfacefeedface", DBID: "999", DB: true, Edge: true}, // no row at all
	}
	if err := a.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rt.removes) != 1 || rt.removes[0].Volume != failed.Volume {
		t.Fatalf("removes = %+v", rt.removes)
	}
	if len(rt.alerts) != 1 || !strings.Contains(rt.alerts[0], "feedfacefeedface") {
		t.Fatalf("alerts = %q", rt.alerts)
	}
}

func TestReconcileWarnsBeforeANodeCertificateExpires(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx := context.Background()
	d, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "abc", Domain: "abc.com"})
	if err != nil {
		t.Fatal(err)
	}
	rt.nodes = []swarm.NodeState{{Key: d.NodeKey, HostID: d.HostID, DB: true, Edge: true}}
	rt.alerts = nil
	a.Reconcile(ctx)
	if len(rt.alerts) != 0 {
		t.Fatalf("a fresh certificate alerted: %q", rt.alerts)
	}
	tl, _ := a.Store.GetDBTLS(ctx, d.ID)
	tl.NotAfter = time.Now().Add(20 * 24 * time.Hour)
	if err := a.Store.SetDBTLS(ctx, d.ID, tl); err != nil {
		t.Fatal(err)
	}
	a.Reconcile(ctx)
	a.Reconcile(ctx)
	if len(rt.alerts) != 1 || !strings.Contains(rt.alerts[0], "certificate expires") {
		t.Fatalf("alerts = %q", rt.alerts)
	}
}
