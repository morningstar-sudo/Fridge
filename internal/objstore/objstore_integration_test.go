//go:build integration

package objstore

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"fridge/internal/store"
)

func TestMinioRoundTrip(t *testing.T) {
	ep, ak, sk, bucket := os.Getenv("FRIDGE_IT_S3_ENDPOINT"), os.Getenv("FRIDGE_IT_S3_ACCESS"), os.Getenv("FRIDGE_IT_S3_SECRET"), os.Getenv("FRIDGE_IT_S3_BUCKET")
	if ep == "" || ak == "" || sk == "" || bucket == "" {
		t.Skip("FRIDGE_IT_S3_* not set")
	}
	ctx := context.Background()
	d := store.Destination{Endpoint: ep, AccessKey: ak, Bucket: bucket, PathStyle: true}
	s, err := New(d, sk)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Test(ctx); err != nil {
		t.Fatal(err)
	}
	key := "fridge-it/x/" + time.Now().Format("150405.000") + ".dump"
	if n, err := s.Put(ctx, key, strings.NewReader("hello"), map[string]string{"fridge-engine": "postgres"}); err != nil || n != 5 {
		t.Fatal(n, err)
	}
	if m, err := s.Meta(ctx, key); err != nil || m["fridge-engine"] != "postgres" {
		t.Fatal(m, err)
	}
	rc, err := s.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" {
		t.Fatalf("got %q", b)
	}
	objs, _, err := s.List(ctx, "fridge-it/", "", 10)
	found := false
	for _, o := range objs {
		found = found || o.Key == key
	}
	if err != nil || !found {
		t.Fatal(objs, err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	bad, _ := New(d, sk+"x")
	if err := bad.Test(ctx); err == nil {
		t.Fatal("wrong secret must fail Test")
	}
}
