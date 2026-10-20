// Package storetest gives tests a throwaway Postgres database.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"

	"fridge/internal/store"

	"github.com/jackc/pgx/v5"
)

const defaultURL = "postgres://postgres:postgres@localhost:55432/postgres?sslmode=disable"

var (
	mu      sync.Mutex
	lastURL = map[*testing.T]string{}
)

func adminURL() string {
	if u := os.Getenv("FRIDGE_TEST_DATABASE_URL"); u != "" {
		return u
	}
	return defaultURL
}

// New creates a fresh database fridge_t_<hex>, opens a Store on it and drops it on cleanup.
func New(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	admin := adminURL()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("test postgres not reachable at %s — run scripts/test-db.sh: %v", admin, err)
	}
	defer conn.Close(ctx)

	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	name := "fridge_t_" + hex.EncodeToString(b)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dbURL := u.String()
	mu.Lock()
	lastURL[t] = dbURL
	mu.Unlock()

	s, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		s.Close()
		mu.Lock()
		delete(lastURL, t)
		mu.Unlock()
		c, err := pgx.Connect(ctx, admin)
		if err != nil {
			t.Logf("cleanup connect: %v", err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name)); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
	})
	return s
}

// URL returns the URL of the temp database created by New in this test.
func URL(t *testing.T) string {
	mu.Lock()
	defer mu.Unlock()
	return lastURL[t]
}
