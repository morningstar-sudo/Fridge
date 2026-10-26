package store_test

import (
	"context"
	"errors"
	"testing"

	"fridge/internal/store"
	"fridge/internal/store/storetest"
)

var ctx = context.Background()

func TestReopenKeepsDataAndSkipsMigrations(t *testing.T) {
	s := storetest.New(t)
	d := &store.Database{Name: "abc", Engine: "postgres", Version: "17", Domain: "abc.com", User: "u", Status: "running"}
	if err := s.CreateDatabase(ctx, d); err != nil {
		t.Fatal(err)
	}
	if d.ID == 0 || d.CreatedAt.IsZero() || d.HostID != "local" {
		t.Fatalf("not populated: %+v", d)
	}
	s.Close()

	s2, err := store.Open(ctx, storetest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err := s2.GetDatabaseByName(ctx, "abc")
	if err != nil || got.ID != d.ID {
		t.Fatalf("got %+v, %v", got, err)
	}
	var n int
	if err := s2.Pool().QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil || n != 7 {
		t.Fatalf("schema_migrations rows = %d, %v", n, err)
	}
}

func TestDatabaseUniqueness(t *testing.T) {
	s := storetest.New(t)
	mk := func(name, domain string) error {
		return s.CreateDatabase(ctx, &store.Database{Name: name, Engine: "postgres", Version: "17", Domain: domain, Status: "running"})
	}
	if err := mk("abc", "abc.com"); err != nil {
		t.Fatal(err)
	}
	if err := mk("abc", "other.com"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup name: %v", err)
	}
	if err := mk("xyz", "abc.com"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup domain: %v", err)
	}
	a, err := s.GetDatabaseByName(ctx, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDatabaseState(ctx, a.ID, "error", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDatabaseByName(ctx, "abc"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("error row visible: %v", err)
	}
	if err := mk("abc", "abc.com"); err != nil {
		t.Fatalf("recreate after error: %v", err)
	}
	list, err := s.ListDatabases(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %d, %v", len(list), err)
	}
}

func TestDestinationDeleteInUse(t *testing.T) {
	s := storetest.New(t)
	dest := &store.Destination{Name: "minio", Provider: "minio", Endpoint: "http://m:9000", Bucket: "b", AccessKey: "k", SecretKeyEnc: "x", PathStyle: true}
	if err := s.CreateDestination(ctx, dest); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateDestination(ctx, &store.Destination{Name: "minio", Provider: "s3"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup dest name: %v", err)
	}
	db := &store.Database{Name: "abc", Engine: "postgres", Version: "17", Domain: "abc.com", Status: "running"}
	if err := s.CreateDatabase(ctx, db); err != nil {
		t.Fatal(err)
	}
	var sid int64
	err := s.Pool().QueryRow(ctx, `INSERT INTO backup_schedules(database_id, destination_id, cron, prefix, keep_latest, enabled)
		VALUES($1,$2,'0 2 * * *','fridge',7,true) RETURNING id`, db.ID, dest.ID).Scan(&sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDestination(ctx, dest.ID); !errors.Is(err, store.ErrInUse) {
		t.Fatalf("want ErrInUse, got %v", err)
	}
	if _, err := s.Pool().Exec(ctx, "DELETE FROM backup_schedules WHERE id=$1", sid); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDestination(ctx, dest.ID); err != nil {
		t.Fatal(err)
	}
	if g, err := s.GetDestination(ctx, dest.ID); err != nil || g.DeletedAt == nil {
		t.Fatalf("want soft-deleted row, got %+v %v", g, err)
	}
}

func TestSettings(t *testing.T) {
	s := storetest.New(t)
	if _, ok, err := s.GetSetting(ctx, "k"); err != nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	for _, v := range []string{"a", "b"} {
		if err := s.SetSetting(ctx, "k", v); err != nil {
			t.Fatal(err)
		}
	}
	if v, ok, err := s.GetSetting(ctx, "k"); err != nil || !ok || v != "b" {
		t.Fatalf("v=%q ok=%v err=%v", v, ok, err)
	}
}
