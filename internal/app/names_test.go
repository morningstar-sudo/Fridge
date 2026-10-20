package app

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"fridge/internal/store"
)

type nameEnv struct {
	a          *App
	rt         *fakeRuntime
	alice, bob int64
	aliceDom   string
	bobDom     string
}

func newNameEnv(t *testing.T) nameEnv {
	a, rt, _ := newTestApp(t)
	e := nameEnv{a: a, rt: rt, alice: mkUser(t, a, "alice", "user"), bob: mkUser(t, a, "bob", "user"), aliceDom: "alice.lan", bobDom: "bob.lan"}
	regDomain(a, e.alice, e.aliceDom)
	regDomain(a, e.bob, e.bobDom)
	return e
}

func (e nameEnv) create(owner int64, dom, name, user, pw string) (store.Database, error) {
	return e.a.CreateDatabase(context.Background(), CreateRequest{Engine: "postgres", Name: name, Domain: dom, OwnerID: owner, DBUser: user, DBPassword: pw})
}

func envOf(rt *fakeRuntime, key string) string {
	for _, e := range rt.runs[len(rt.runs)-1].Env {
		if v, ok := strings.CutPrefix(e, key+"="); ok {
			return v
		}
	}
	return ""
}

func TestDBNamePrefix(t *testing.T) {
	e := newNameEnv(t)
	d, err := e.create(e.alice, e.aliceDom, "shop", "", "")
	if err != nil || d.Name != "alice_shop" || envOf(e.rt, "POSTGRES_DB") != "alice_shop" {
		t.Fatalf("d=%+v err=%v", d, err)
	}
	if d.DomainID == nil {
		t.Fatal("domain id not set")
	}
	if _, err := e.create(e.alice, e.aliceDom, "alice_shop", "u2", ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup prefixed err = %v", err)
	}
	for _, n := range []string{"bob_x", "Shop", "-x", strings.Repeat("a", 31), ""} {
		if _, err := e.create(e.alice, e.aliceDom, n, "", ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: err = %v", n, err)
		}
	}
	if d, err := e.create(e.bob, e.bobDom, "shop", "", ""); err != nil || d.Name != "bob_shop" {
		t.Fatalf("bob: %+v %v", d, err)
	}
	for _, dom := range []string{"unreg.lan", "bob.lan"} {
		if _, err := e.create(e.alice, dom, "other", "", ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("domain %s: err = %v", dom, err)
		}
	}
}

func TestDBUserPrefix(t *testing.T) {
	e := newNameEnv(t)
	for _, c := range []struct{ in, name, want string }{
		{"shop", "a1", "alice_shop"}, {"alice_shop", "a2", "alice_shop"}, {"bob_x", "a3", "alice_bob_x"}, {"", "my-db", "alice_my_db"},
	} {
		x := newNameEnv(t)
		d, err := x.create(x.alice, x.aliceDom, c.name, c.in, "")
		if err != nil || d.User != c.want {
			t.Errorf("%q: user=%q err=%v want %q", c.in, d.User, err, c.want)
		}
	}
	e = newNameEnv(t)
	for _, u := range []string{strings.Repeat("a", 16), "a-b"} {
		if _, err := e.create(e.alice, e.aliceDom, "x", u, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: err = %v", u, err)
		}
	}
	if _, err := e.create(e.alice, e.aliceDom, "one", "shop", ""); err != nil {
		t.Fatal(err)
	}
	if d, err := e.create(e.bob, e.bobDom, "one", "shop", ""); err != nil || d.User != "bob_shop" {
		t.Fatalf("bob: %+v %v", d, err)
	}
	if _, err := e.create(e.alice, e.dom(e.alice, "two"), "two", "shop", ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("dup user err = %v", err)
	}
}

func TestCustomDBCredentials(t *testing.T) {
	e := newNameEnv(t)
	pw := "S3cure-pass.~x"
	d, err := e.create(e.alice, e.aliceDom, "shop", "shop_rw", pw)
	if err != nil {
		t.Fatal(err)
	}
	if envOf(e.rt, "POSTGRES_USER") != "alice_shop_rw" || envOf(e.rt, "POSTGRES_PASSWORD") != pw {
		t.Fatalf("env = %v", e.rt.runs[0].Env)
	}
	cs, err := e.a.ConnString(d)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(cs)
	if err != nil || u.User.Username() != "alice_shop_rw" {
		t.Fatalf("cs=%q err=%v", cs, err)
	}
	if got, _ := u.User.Password(); got != pw {
		t.Fatalf("password = %q", got)
	}
	for name, bad := range map[string]string{"short": "elevenchars", "space": "has space 1234", "non-ascii": "pässwörd-12345"} {
		if _, err := e.create(e.alice, e.dom(e.alice, "bad"), "bad", "bad", bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := e.create(e.alice, e.dom(e.alice, "rnd"), "rnd", "rnd", ""); err != nil || len(envOf(e.rt, "POSTGRES_PASSWORD")) != 26 {
		t.Fatalf("random pw %q err=%v", envOf(e.rt, "POSTGRES_PASSWORD"), err)
	}
}

func TestRestoreNamePrefix(t *testing.T) {
	a, _, objs := newTestApp(t)
	alice := mkUser(t, a, "alice", "user")
	regDomain(a, alice, "copy.lan")
	key := "fridge/postgres-17/abc/abc-1.dump"
	dest := restoreFixture(t, a, objs, key, []byte("D"), nil)
	r, err := a.StartRestore(context.Background(), RestoreRequest{OwnerID: alice, DestinationID: dest.ID, Key: key, New: &CreateRequest{Name: "copy", Domain: "copy.lan"}})
	if err != nil {
		t.Fatal(err)
	}
	a.Wait()
	got, _ := a.Store.GetRestore(context.Background(), r.ID)
	if got.TargetDatabaseID == nil {
		t.Fatalf("restore %+v", got)
	}
	if db, _ := a.Store.GetDatabase(context.Background(), *got.TargetDatabaseID); db.Name != "alice_copy" {
		t.Fatalf("name = %q", db.Name)
	}
}

// dom registers a fresh domain for owner (one active DB per domain).
func (e nameEnv) dom(owner int64, n string) string {
	d := n + ".x.lan"
	regDomain(e.a, owner, d)
	return d
}
