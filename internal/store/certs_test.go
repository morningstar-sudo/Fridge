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
