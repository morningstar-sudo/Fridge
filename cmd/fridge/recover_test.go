package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fridge/internal/auth"
	"fridge/internal/scram"
	"fridge/internal/store"
	"fridge/internal/store/storetest"
)

// recoverEnv is a real metadata database, a Traefik directory and the server environment `recover` reads.
func recoverEnv(t *testing.T) (*store.Store, string) {
	t.Helper()
	st := storetest.New(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "certs"), 0o755)
	t.Setenv("FRIDGE_DATABASE_URL", storetest.URL(t))
	t.Setenv("FRIDGE_SECRET_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("FRIDGE_TRAEFIK_DIR", dir)
	t.Setenv("FRIDGE_API_BACKEND", "http://fridge:8080")
	t.Setenv("FRIDGE_PUBLIC_HOSTS", "fridge.lan,10.0.0.5")
	return st, dir
}

func mkUser(t *testing.T, st *store.Store, name, role string) store.User {
	t.Helper()
	salt := make([]byte, 16)
	stored, server := scram.Keys(scram.SaltedPassword("x", salt, auth.MinIterations))
	u := store.User{Username: name, Role: role, Salt: salt, Iterations: auth.MinIterations, StoredKey: stored, ServerKey: server}
	if err := st.CreateUser(context.Background(), &u); err != nil {
		t.Fatal(err)
	}
	return u
}

func audits(t *testing.T, st *store.Store, prefix string) []store.AuditEntry {
	t.Helper()
	es, err := st.ListAudit(context.Background(), store.AuditFilter{Action: prefix})
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func TestRecoverAllowlist(t *testing.T) {
	st, dir := recoverEnv(t)
	code, out, errs := runWith(t, "", "recover", "allowlist", "10.0.0.0/8", "203.0.113.0/24")
	if code != 0 || !strings.Contains(out, "10.0.0.0/8") {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	got, ok, err := st.GetSetting(context.Background(), "allowlist")
	if err != nil || !ok || !strings.Contains(got, "203.0.113.0/24") {
		t.Fatalf("setting %q %v %v", got, ok, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "dynamic", "_base.yml"))
	if err != nil {
		t.Fatal(err)
	}
	base := string(b)
	abs, _ := filepath.Abs(dir)
	if !strings.Contains(base, "203.0.113.0/24") || !strings.Contains(base, "defaultCertificate") || !strings.Contains(base, filepath.Join(abs, "certs", "web.crt")) {
		t.Fatalf("base file lost the web certificate or the CIDRs:\n%s", base)
	}
	es := audits(t, st, "recover.")
	if len(es) != 1 || es[0].Action != "recover.allowlist" || es[0].Actor != "recover" || es[0].ActorID != nil || !es[0].Success {
		t.Fatalf("%+v", es)
	}
	// a bad CIDR changes nothing but is on record
	if code, _, errs := runWith(t, "", "recover", "allowlist", "not-a-cidr"); code != 1 || errs == "" {
		t.Fatalf("bad CIDR: %d", code)
	}
	if es = audits(t, st, "recover."); len(es) != 2 || es[0].Success {
		t.Fatalf("%+v", es)
	}
	if got2, _, _ := st.GetSetting(context.Background(), "allowlist"); got2 != got {
		t.Fatal("a bad CIDR changed the setting")
	}
}

func TestRecoverResetAndPromote(t *testing.T) {
	st, _ := recoverEnv(t)
	mkUser(t, st, "alice", "admin")
	bob := mkUser(t, st, "bob", "user")

	code, out, errs := runWith(t, "", "recover", "reset-password", "bob")
	if code != 0 || !strings.HasPrefix(out, "https://fridge.lan:8443/#/invite/") || len(strings.TrimSpace(out)) < len("https://fridge.lan:8443/#/invite/")+20 {
		t.Fatalf("reset: %d %q %q", code, out, errs)
	}
	// the token in the link is a real, single-use reset invite for bob
	tok := strings.TrimSpace(strings.TrimPrefix(out, "https://fridge.lan:8443/#/invite/"))
	au, err := auth.New(st, make([]byte, 32), "")
	if err != nil {
		t.Fatal(err)
	}
	if name, purpose, err := au.InviteInfo(context.Background(), tok); err != nil || name != "bob" || purpose != "reset_password" {
		t.Fatalf("invite %q %q %v", name, purpose, err)
	}

	if code, _, errs := runWith(t, "", "recover", "promote", "bob"); code != 0 {
		t.Fatalf("promote: %q", errs)
	}
	if u, _ := st.GetUser(context.Background(), bob.ID); u.Role != "admin" {
		t.Fatalf("role %q", u.Role)
	}
	es := audits(t, st, "recover.")
	seen := map[string]bool{}
	for _, e := range es {
		if e.Actor != "recover" || e.ActorID != nil || e.TargetID != "2" || !e.Success {
			t.Fatalf("%+v", e)
		}
		seen[e.Action] = true
		if strings.Contains(string(mustJSON(e.Detail)), tok) {
			t.Fatal("the invite token is in the audit log")
		}
	}
	if !seen["recover.reset_password"] || !seen["recover.promote"] || len(es) != 2 {
		t.Fatalf("%+v", es)
	}

	// no host configured: localhost
	t.Setenv("FRIDGE_PUBLIC_HOSTS", "")
	if _, out, _ := runWith(t, "", "recover", "reset-password", "bob"); !strings.HasPrefix(out, "https://localhost:8443/#/invite/") {
		t.Fatalf("default host: %q", out)
	}
	// unknown user, missing environment, bad usage
	if code, _, errs := runWith(t, "", "recover", "promote", "nobody"); code != 1 || !strings.Contains(errs, "no account") {
		t.Fatalf("unknown user: %d %q", code, errs)
	}
	t.Setenv("FRIDGE_DATABASE_URL", "")
	if code, _, errs := runWith(t, "", "recover", "promote", "bob"); code != 1 || !strings.Contains(errs, "inside the server container") {
		t.Fatalf("no env: %d %q", code, errs)
	}
	for _, args := range [][]string{{"recover"}, {"recover", "frobnicate"}, {"recover", "promote"}, {"recover", "allowlist"}} {
		if code, _, _ := runWith(t, "", args...); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
