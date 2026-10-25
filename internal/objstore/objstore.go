// Package objstore wraps an S3-compatible bucket (minio-go).
package objstore

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"fridge/internal/store"
)

type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

type Store struct {
	c      *minio.Client
	bucket string
}

// New builds a client for d. d.Endpoint is a full http(s) URL.
// A non-nil g vets every connection's real address (not just the name), unless the endpoint host is in g.Allow.
func New(d store.Destination, secretKey string, g *Guard) (*Store, error) {
	u, err := url.Parse(d.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("bad endpoint %q", d.Endpoint)
	}
	lookup := minio.BucketLookupAuto
	if d.PathStyle {
		lookup = minio.BucketLookupPath
	}
	opts := &minio.Options{
		Creds:        credentials.NewStaticV4(d.AccessKey, secretKey, ""),
		Secure:       u.Scheme == "https",
		Region:       d.Region,
		BucketLookup: lookup,
	}
	if g != nil && !g.allowed(u.Hostname()) {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Proxy = nil // a proxy would make the request for us, past the guard
		t.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: g.DialControl}).DialContext
		opts.Transport = t
	}
	c, err := minio.New(u.Host, opts)
	if err != nil {
		return nil, err
	}
	return &Store{c: c, bucket: d.Bucket}, nil
}

// Test puts, stats and removes a throwaway object.
func (s *Store) Test(ctx context.Context) error {
	key := "fridge-test-" + rand.Text()
	if _, err := s.Put(ctx, key, strings.NewReader("ok"), nil); err != nil {
		return err
	}
	if _, err := s.c.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err != nil {
		return err
	}
	return s.Delete(ctx, key)
}

// Put streams r (unknown length); returns bytes stored. A failing reader aborts the upload.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, meta map[string]string) (int64, error) {
	info, err := s.c.PutObject(ctx, s.bucket, key, r, -1, minio.PutObjectOptions{UserMetadata: meta, PartSize: 64 << 20}) // explicit: unknown length otherwise sizes buffers for 5 TiB
	if err != nil {
		return 0, err
	}
	return info.Size, nil
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	o, err := s.c.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	if _, err := o.Stat(); err != nil { // surfaces NoSuchKey now rather than on first Read
		o.Close()
		return nil, err
	}
	return o, nil
}

func (s *Store) Meta(ctx context.Context, key string) (map[string]string, error) {
	st, err := s.c.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range st.UserMetadata {
		out[strings.TrimPrefix(strings.ToLower(k), "x-amz-meta-")] = v
	}
	return out, nil
}

// List scans up to 1000 objects under prefix, then filters and sorts them.
func (s *Store) List(ctx context.Context, prefix, q string, limit int) ([]Object, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var objs []Object
	for o := range s.c.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			return nil, false, o.Err
		}
		objs = append(objs, Object{o.Key, o.Size, o.LastModified})
		if len(objs) >= 1000 {
			break
		}
	}
	out, trunc := FilterSort(objs, q, limit)
	return out, trunc, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	return s.c.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

// FilterSort keeps objects whose key contains q (case-insensitive), newest first, cut to limit.
func FilterSort(objs []Object, q string, limit int) ([]Object, bool) {
	q = strings.ToLower(q)
	var out []Object
	for _, o := range objs {
		if strings.Contains(strings.ToLower(o.Key), q) {
			out = append(out, o)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastModified.After(out[j].LastModified) })
	if limit > 0 && len(out) > limit {
		return out[:limit], true
	}
	return out, false
}
