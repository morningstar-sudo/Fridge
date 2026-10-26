package store_test

import (
	"errors"
	"testing"
	"time"

	"fridge/internal/store"
	"fridge/internal/store/storetest"
)

func TestDBTLSRoundTrip(t *testing.T) {
	s := storetest.New(t)
	owner := mustUser(t, s, "alice", "user")
	d := mkDB(t, s, "a", owner, "running")
	if _, err := s.GetDBTLS(ctx, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no TLS yet: %v", err)
	}
	want := store.DBTLS{ServerCertPEM: "sc", ServerKeyEnc: "sk", ClientCertPEM: "cc", ClientKeyEnc: "ck", NotAfter: time.Now().Add(time.Hour).Truncate(time.Second)}
	if err := s.SetDBTLS(ctx, d.ID, want); err != nil {
		t.Fatal(err)
	}
	want.ServerCertPEM = "sc2" // replacing keeps one row
	if err := s.SetDBTLS(ctx, d.ID, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDBTLS(ctx, d.ID)
	if err != nil || got.ServerCertPEM != "sc2" || got.ClientKeyEnc != "ck" || !got.NotAfter.Equal(want.NotAfter) {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestTenantCAIsCreatedOnce(t *testing.T) {
	s := storetest.New(t)
	u := mustUser(t, s, "alice", "user")
	if _, err := s.GetTenantCA(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no CA yet: %v", err)
	}
	first, err := s.CreateTenantCA(ctx, store.TenantCA{UserID: u.ID, CertPEM: "c1", KeyEnc: "k1"})
	if err != nil || first.CertPEM != "c1" {
		t.Fatalf("%+v %v", first, err)
	}
	second, err := s.CreateTenantCA(ctx, store.TenantCA{UserID: u.ID, CertPEM: "c2", KeyEnc: "k2"}) // a racing request
	if err != nil || second.CertPEM != "c1" || second.KeyEnc != "k1" {
		t.Fatalf("the first CA must win: %+v %v", second, err)
	}
}

func TestDBTLSRemembersWhichCASigned(t *testing.T) {
	s := storetest.New(t)
	owner := mustUser(t, s, "bob", "user")
	d := mkDB(t, s, "b", owner, "running")
	if err := s.SetDBTLS(ctx, d.ID, store.DBTLS{ServerCertPEM: "s", ServerKeyEnc: "sk", ClientCertPEM: "c", ClientKeyEnc: "ck", CACertPEM: "THECA", NotAfter: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetDBTLS(ctx, d.ID); err != nil || got.CACertPEM != "THECA" {
		t.Fatalf("%+v %v", got, err)
	}
}
