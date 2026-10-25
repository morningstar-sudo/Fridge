package api

import (
	"strings"
	"testing"
)

func TestDestinationSSRFAPI(t *testing.T) {
	e := newTenants(t)
	body := func(endpoint string) string {
		return jb(map[string]string{"name": "n" + strings.NewReplacer(":", "", "/", "", ".", "", "[", "", "]", "").Replace(endpoint), "endpoint": endpoint, "bucket": "bkt", "access_key": "ak", "secret_key": "sk"})
	}
	for _, ep := range []string{"http://fridge-meta:9000", "http://127.0.0.1:9000", "http://169.254.169.254", "http://localhost:9000", "http://[::1]:9000"} {
		w := e.want(t, e.bob, "POST", "/api/destinations", body(ep), 400)
		if strings.TrimSpace(w.Body.String()) != `{"error":"endpoint không được phép"}` {
			t.Fatal(ep, w.Body.String())
		}
	}
	e.want(t, e.bob, "POST", "/api/destinations", body("http://192.168.1.50:9000"), 201)
	e.want(t, e.root, "POST", "/api/destinations", body("http://127.0.0.1:9000"), 201)

	// editing an allowed destination into a forbidden one is refused as well
	ok := id(t, e.want(t, e.bob, "POST", "/api/destinations", body("http://10.1.2.3:9000"), 201))
	w := e.want(t, e.bob, "PUT", "/api/destinations/"+ok, body("http://127.0.0.1:9000"), 400)
	if !strings.Contains(w.Body.String(), "endpoint không được phép") {
		t.Fatal(w.Body.String())
	}
	e.want(t, e.bob, "PUT", "/api/destinations/"+ok, body("http://10.1.2.4:9000"), 200)

	// the exemption list is admin-only, validated, and effective
	e.want(t, e.bob, "GET", "/api/settings/endpoint-allowlist", "", 403)
	e.want(t, e.bob, "PUT", "/api/settings/endpoint-allowlist", `{"hosts":["127.0.0.1"]}`, 403)
	e.want(t, e.root, "PUT", "/api/settings/endpoint-allowlist", `{"hosts":["not a host"]}`, 400)
	e.want(t, e.root, "PUT", "/api/settings/endpoint-allowlist", `{"hosts":["127.0.0.1"]}`, 200)
	if b := e.want(t, e.root, "GET", "/api/settings/endpoint-allowlist", "", 200).Body.String(); !strings.Contains(b, `"127.0.0.1"`) {
		t.Fatal(b)
	}
	e.want(t, e.bob, "POST", "/api/destinations", body("http://127.0.0.1:9001"), 201)

	// refusals and the allowlist change are in the audit trail
	es := e.audit(t, e.root, "?limit=500")
	if x := find(es, "dest.create", "bob"); x == nil {
		t.Fatal("no dest.create by bob")
	}
	failed := 0
	for _, x := range es {
		if x.Actor == "bob" && x.Action == "dest.create" && !x.Success {
			failed++
		}
	}
	if failed != 5 {
		t.Fatalf("%d failed dest.create entries", failed)
	}
	if x := find(es, "settings.endpoint_allowlist", "root1"); x == nil || !x.Success {
		t.Fatalf("%+v", x)
	}
}
