package app

import (
	"context"
	"testing"

	"fridge/internal/store"
)

func TestFixedJobsSurviveReload(t *testing.T) {
	a, _, _ := newTestApp(t)
	ctx := context.Background()
	s := NewScheduler(a)
	if err := s.AddFixed("@hourly", func() {}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddFixed("bad spec", func() {}); err == nil {
		t.Fatal("bad spec accepted")
	}
	fixed := s.c.Entries()[0].ID
	if err := a.Store.CreateDestination(ctx, &store.Destination{Name: "s3", Provider: "s3"}); err != nil {
		t.Fatal(err)
	}
	d, err := a.CreateDatabase(ctx, CreateRequest{Engine: "postgres", Name: "abc", Domain: "abc.com"})
	if err != nil {
		t.Fatal(err)
	}
	scs, _ := a.Store.ListSchedules(ctx, d.ID) // CreateDatabase adds a default schedule
	if len(scs) != 1 {
		t.Fatal(scs)
	}
	sc := scs[0]
	for i := 0; i < 2; i++ {
		if err := s.Reload(ctx); err != nil {
			t.Fatal(err)
		}
	}
	has := func() bool {
		for _, e := range s.c.Entries() {
			if e.ID == fixed {
				return true
			}
		}
		return false
	}
	if n := len(s.c.Entries()); n != 2 || !has() {
		t.Fatalf("entries %d fixed kept %v", n, has())
	}
	if err := a.Store.DeleteSchedule(ctx, sc.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(s.c.Entries()); n != 1 || !has() {
		t.Fatalf("entries %d fixed kept %v", n, has())
	}
}
