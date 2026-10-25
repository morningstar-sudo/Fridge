package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"

	"fridge/internal/auth"
	"fridge/internal/objstore"
	"fridge/internal/store"
)

func TestCheckEndpointRoles(t *testing.T) {
	a, _, _ := newTestApp(t)
	ctx := context.Background()
	admin := auth.Principal{UserID: 1, Username: "root1", Role: "admin"}
	alice := auth.Principal{UserID: 2, Username: "alice", Role: "user"}

	if err := a.CheckEndpoint(ctx, admin, "http://127.0.0.1:9000"); err != nil {
		t.Fatalf("admin: %v", err)
	}
	err := a.CheckEndpoint(ctx, alice, "http://127.0.0.1:9000")
	if !errors.Is(err, ErrInvalid) || !errors.Is(err, objstore.ErrEndpointBlocked) || err.Error() != "endpoint không được phép" {
		t.Fatalf("alice loopback: %v", err)
	}
	for _, bad := range []string{"http://fridge-meta:9000", "http://169.254.169.254/latest", "http://[::1]:9000", "http://", "::::"} {
		if err := a.CheckEndpoint(ctx, alice, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if err := a.CheckEndpoint(ctx, alice, "http://192.168.1.50:9000"); err != nil {
		t.Fatalf("private range: %v", err)
	}
	if err := a.SetEndpointAllowlist(ctx, []string{"127.0.0.1", "127.0.0.1", " MinIO.lan "}); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.EndpointAllowlist(ctx); len(got) != 2 || got[1] != "minio.lan" {
		t.Fatalf("allowlist %v", got)
	}
	if err := a.CheckEndpoint(ctx, alice, "http://127.0.0.1:9000"); err != nil {
		t.Fatalf("allowlisted: %v", err)
	}
	for _, bad := range [][]string{{""}, {"http://x"}, {"a b"}, {"host:9000"}, {"a..b"}} {
		if err := a.SetEndpointAllowlist(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// the Docker networks of Fridge are blocked too
	a.RT.(*fakeRuntime).subnets = []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16")}
	if err := a.CheckEndpoint(ctx, alice, "http://172.18.0.4:9000"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("fridge subnet: %v", err)
	}
}

func TestObjectsUsesGuardForNonAdminOwner(t *testing.T) {
	a, rt, objs := newTestApp(t)
	ctx := context.Background()
	rt.subnets = []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16")}
	var got *objstore.Guard
	var calls int
	a.OpenObjects = func(d store.Destination, sk string, g *objstore.Guard) (ObjectStore, error) {
		got, calls = g, calls+1
		return objs, nil
	}
	admin, alice := own(t, a), mkUser(t, a, "alice", "user")
	n := 0
	mk := func(owner int64) int64 {
		n++
		d, err := a.CreateDestination(ctx, store.Destination{Name: fmt.Sprint("d", n), Endpoint: "http://s3.example.com", Bucket: "b", AccessKey: "ak", OwnerID: &owner}, "sk")
		if err != nil {
			t.Fatal(err)
		}
		return d.ID
	}
	if _, _, err := a.objects(ctx, mk(alice)); err != nil || got == nil || len(got.Blocked) != 1 || got.Blocked[0] != rt.subnets[0] {
		t.Fatalf("alice's destination: guard %+v, %v", got, err)
	}
	if _, _, err := a.objects(ctx, mk(admin)); err != nil || got != nil || calls != 2 {
		t.Fatalf("admin's destination: guard %+v, %v", got, err)
	}
	// demoting the owner puts their destination under the guard
	root2 := mkUser(t, a, "root2", "admin")
	id := mk(root2)
	if _, _, err := a.objects(ctx, id); err != nil || got != nil {
		t.Fatalf("admin-owned destination was guarded: %+v, %v", got, err)
	}
	if err := a.Store.SetUserRole(ctx, root2, "user"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.objects(ctx, id); err != nil || got == nil {
		t.Fatalf("demoted owner's destination is not guarded: %+v, %v", got, err)
	}
}
