package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"fridge/internal/store"
)

func TestCreateDatabaseHappyPath(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx := context.Background()
	d, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "abc", Domain: "ABC.com "})
	if err != nil {
		t.Fatal(err)
	}
	if d.Domain != "abc.com" || d.Version != "17" || d.Status != "running" || d.User != "root1_abc" {
		t.Fatalf("db = %+v", d)
	}
	got, _ := a.Store.GetDatabase(ctx, d.ID)
	if got.Status != "running" || got.ContainerID == "" {
		t.Fatalf("stored = %+v", got)
	}
	if len(rt.runs) != 1 || rt.runs[0].Name != d.Volume || !nodeNameRe.MatchString(d.Volume) || rt.runs[0].Image != "postgres:17" {
		t.Fatalf("runs = %+v", rt.runs)
	}
	pw := ""
	for _, e := range rt.runs[0].Env {
		if v, ok := strings.CutPrefix(e, "POSTGRES_PASSWORD="); ok {
			pw = v
		}
	}
	if pw == "" || strings.Contains(d.PasswordEnc, pw) {
		t.Fatalf("password handling wrong: pw=%q enc=%q", pw, d.PasswordEnc)
	}
	if _, err := os.Stat(filepath.Join(a.TraefikDir, "dynamic", fmt.Sprintf("db-%d.yml", d.ID))); err != nil {
		t.Fatal(err)
	}
	cs, err := a.ConnString(d)
	if err != nil || !strings.Contains(cs, "@abc.com:5432/root1_abc?sslmode=require") {
		t.Fatalf("conn = %q, %v", cs, err)
	}
}

func TestCreateDatabaseValidation(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ok := CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "abc", Domain: "abc.com"}
	cases := map[string]func(*CreateRequest){
		"version":    func(r *CreateRequest) { r.Version = "99" },
		"name case":  func(r *CreateRequest) { r.Name = "Abc" },
		"name inj":   func(r *CreateRequest) { r.Name = "a;rm" },
		"domain":     func(r *CreateRequest) { r.Domain = "localhost" },
		"domain inj": func(r *CreateRequest) { r.Domain = "a.com`)||x" },
		"domain dot": func(r *CreateRequest) { r.Domain = "a..com" },
		"acme":       func(r *CreateRequest) { r.TLSMode = "acme" },
		"tls":        func(r *CreateRequest) { r.TLSMode = "weird" },
		"engine":     func(r *CreateRequest) { r.Engine = "mysql" },
	}
	for name, mut := range cases {
		r := ok
		mut(&r)
		if _, err := createDB(a, context.Background(), r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if len(rt.runs) != 0 {
		t.Fatalf("runs = %v", rt.runs)
	}
}

func TestCreateDatabaseRollbackOnUnhealthy(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx := context.Background()
	rt.healthErr = errors.New("unhealthy")
	req := CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "abc", Domain: "abc.com"}
	if _, err := createDB(a, ctx, req); err == nil {
		t.Fatal("want error")
	}
	dbs, _ := a.Store.ListDatabases(ctx)
	if len(dbs) != 1 || dbs[0].Status != "error" {
		t.Fatalf("dbs = %+v", dbs)
	}
	id := dbs[0].ID
	if v := dbs[0].Volume; len(rt.removes) != 1 || rt.removes[0] != (removeCall{"ctr-" + v, v}) || !nodeNameRe.MatchString(v) {
		t.Fatalf("removes = %+v", rt.removes)
	}
	for _, p := range []string{
		filepath.Join(a.TraefikDir, "dynamic", fmt.Sprintf("db-%d.yml", id)),
		filepath.Join(a.TraefikDir, "certs", fmt.Sprintf("db-%d.crt", id)),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists (%v)", p, err)
		}
	}
	rt.healthErr = nil
	if _, err := createDB(a, ctx, req); err != nil {
		t.Fatal(err)
	}
}

func TestCreateDatabaseDefaultSchedule(t *testing.T) {
	a, _, _ := newTestApp(t)
	ctx := context.Background()
	d1, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "one", Domain: "one.com"})
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := a.Store.ListSchedules(ctx, d1.ID); len(s) != 0 {
		t.Fatalf("schedules = %+v", s)
	}
	var first int64
	for _, n := range []string{"d1", "d2"} {
		x := store.Destination{Name: n, Provider: "s3", Shared: true}
		if err := a.Store.CreateDestination(ctx, &x); err != nil {
			t.Fatal(err)
		}
		if first == 0 {
			first = x.ID
		}
	}
	d2, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "two", Domain: "two.com"})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := a.Store.ListSchedules(ctx, d2.ID)
	if len(s) != 1 || s[0].DestinationID != first || s[0].KeepLatest != 7 || s[0].Prefix != "fridge" || !s[0].Enabled ||
		!regexp.MustCompile(`^\d{1,2} 2 \* \* \*$`).MatchString(s[0].Cron) {
		t.Fatalf("schedules = %+v", s)
	}
}

func TestSetAllowlist(t *testing.T) {
	a, _, _ := newTestApp(t)
	ctx := context.Background()
	if got, _ := a.Allowlist(ctx); len(got) != 3 {
		t.Fatalf("default = %v", got)
	}
	if err := a.SetAllowlist(ctx, []string{"10.0.0.0/8", "bad"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	if err := a.SetAllowlist(ctx, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty err = %v", err)
	}
	if err := a.SetAllowlist(ctx, []string{"203.0.113.0/24"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(a.TraefikDir, "dynamic", "_base.yml"))
	if err != nil || !strings.Contains(string(b), "203.0.113.0/24") {
		t.Fatalf("base = %s, %v", b, err)
	}
	if got, _ := a.Allowlist(ctx); len(got) != 1 || got[0] != "203.0.113.0/24" {
		t.Fatalf("got = %v", got)
	}
}

func TestRemoveNodeCleansProvisionedDatabase(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx := context.Background()
	a.Sched = NewScheduler(a)
	if err := a.Store.CreateDestination(ctx, &store.Destination{Name: "s3", Provider: "s3", Shared: true}); err != nil {
		t.Fatal(err)
	}
	d, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "abc", Domain: "abc.com"})
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := a.Store.ListSchedules(ctx, d.ID); len(s) != 1 || len(a.Sched.c.Entries()) != 1 {
		t.Fatalf("precondition: schedules %+v entries %d", s, len(a.Sched.c.Entries()))
	}
	files := []string{
		filepath.Join(a.TraefikDir, "dynamic", fmt.Sprintf("db-%d.yml", d.ID)),
		filepath.Join(a.TraefikDir, "certs", fmt.Sprintf("db-%d.crt", d.ID)),
		filepath.Join(a.TraefikDir, "certs", fmt.Sprintf("db-%d.key", d.ID)),
	}
	for _, p := range files {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("precondition: %v", err)
		}
	}
	d, _ = a.Store.GetDatabase(ctx, d.ID)
	a.removeNode(ctx, d)
	for _, p := range files {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists (%v)", p, err)
		}
	}
	if len(rt.removes) != 1 || rt.removes[0] != (removeCall{"ctr-" + d.Volume, d.Volume}) {
		t.Fatalf("removes = %+v", rt.removes)
	}
	if got, _ := a.Store.GetDatabase(ctx, d.ID); got.Status != "error" {
		t.Fatalf("status = %s", got.Status)
	}
	if s, _ := a.Store.ListSchedules(ctx, d.ID); len(s) != 0 || len(a.Sched.c.Entries()) != 0 {
		t.Fatalf("schedules %+v entries %d", s, len(a.Sched.c.Entries()))
	}
}

func TestCreateDatabaseRollbackSurvivesCancelledContext(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	rt.onWait = cancel
	rt.healthErr = context.Canceled
	if _, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "abc", Domain: "abc.com"}); err == nil {
		t.Fatal("want error")
	}
	if len(rt.removes) != 1 {
		t.Fatalf("removes = %+v", rt.removes)
	}
	dbs, _ := a.Store.ListDatabases(context.Background())
	if len(dbs) != 1 || dbs[0].Status != "error" {
		t.Fatalf("dbs = %+v", dbs)
	}
}

func TestRecoverInterrupted(t *testing.T) {
	a, rt, _ := newTestApp(t)
	ctx := context.Background()
	dest := store.Destination{Name: "s3", Provider: "s3", Shared: true}
	if err := a.Store.CreateDestination(ctx, &dest); err != nil {
		t.Fatal(err)
	}
	keep, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "keep", Domain: "keep.com"})
	if err != nil {
		t.Fatal(err)
	}
	half := store.Database{Name: "half", Engine: "postgres", Version: "17", Domain: "half.com", TLSMode: "internal",
		User: "half_app", Volume: "fridge-postgres-half", Status: "creating"}
	if err := a.Store.CreateDatabase(ctx, &half); err != nil {
		t.Fatal(err)
	}
	tgt, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: "tgt", Domain: "tgt.com"})
	if err != nil {
		t.Fatal(err)
	}
	r := store.Restore{DestinationID: dest.ID, S3Key: "k.dump", SourceEngine: "postgres", Mode: "new"}
	if err := a.Store.StartRestore(ctx, &r); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.SetRestoreTarget(ctx, r.ID, tgt.ID); err != nil {
		t.Fatal(err)
	}
	a.Sched = NewScheduler(a)

	if _, err := a.Store.FailRunning(ctx, "interrupted by restart"); err != nil {
		t.Fatal(err)
	}
	if err := a.RecoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	want := []removeCall{{"fridge-postgres-half", "fridge-postgres-half"}, {"ctr-" + tgt.Volume, tgt.Volume}}
	if len(rt.removes) != 2 || rt.removes[0] != want[0] || rt.removes[1] != want[1] {
		t.Fatalf("removes = %+v", rt.removes)
	}
	for _, id := range []int64{half.ID, tgt.ID} {
		if got, _ := a.Store.GetDatabase(ctx, id); got.Status != "error" {
			t.Fatalf("db %d status = %s", id, got.Status)
		}
	}
	if s, _ := a.Store.ListSchedules(ctx, tgt.ID); len(s) != 0 {
		t.Fatalf("target schedules = %+v", s)
	}
	if got, _ := a.Store.GetDatabase(ctx, keep.ID); got.Status != "running" {
		t.Fatalf("keep status = %s", got.Status)
	}
	if s, _ := a.Store.ListSchedules(ctx, keep.ID); len(s) != 1 {
		t.Fatalf("keep schedules = %+v", s)
	}
	if len(rt.alerts) != 1 || !strings.Contains(rt.alerts[0], "half, root1_tgt") {
		t.Fatalf("alerts = %q", rt.alerts)
	}
	for _, n := range []string{"half", "tgt"} {
		if _, err := createDB(a, ctx, CreateRequest{OwnerID: own(t, a), Engine: "postgres", Name: n, Domain: n + ".com"}); err != nil {
			t.Fatalf("reuse %s: %v", n, err)
		}
	}
	if err := a.RecoverInterrupted(ctx); err != nil || len(rt.removes) != 2 || len(rt.alerts) != 1 {
		t.Fatalf("second run: err=%v removes=%+v alerts=%q", err, rt.removes, rt.alerts)
	}
}
