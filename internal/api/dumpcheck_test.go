package api

import (
	"encoding/json"
	"testing"
)

func TestDumpCheckAPI(t *testing.T) {
	e := newTenants(t)
	dS := e.dest(t, e.root, "ds", true)
	var b struct {
		S3Key string `json:"s3_key"`
	}
	json.Unmarshal(e.want(t, e.alice, "POST", "/api/databases/alice_app/backups", `{"destination_id":`+dS+`}`, 202).Body.Bytes(), &b)
	e.a.Wait()
	body := jb(map[string]any{"destination_id": mustInt(t, dS), "key": b.S3Key})

	e.want(t, e.bob, "POST", "/api/dump-checks", body, 404) // alice's backup, bob has no right to it
	w := e.want(t, e.alice, "POST", "/api/dump-checks", body, 202)
	cid := id(t, w)
	e.a.Wait()
	var c struct {
		Status string
		Result map[string]any
	}
	json.Unmarshal(e.want(t, e.alice, "GET", "/api/dump-checks/"+cid, "", 200).Body.Bytes(), &c)
	if c.Status != "failed" && c.Status != "ok" { // the fake backup is not a real dump: either way the job finished
		t.Fatalf("%+v", c)
	}
	e.want(t, e.bob, "GET", "/api/dump-checks/"+cid, "", 404)
	e.want(t, e.root, "GET", "/api/dump-checks/"+cid, "", 200)
	e.want(t, e.alice, "GET", "/api/dump-checks/999", "", 404)
	e.want(t, e.alice, "GET", "/api/dump-checks/x", "", 404)
	e.want(t, e.bob, "POST", "/api/dump-checks", `{"destination_id":`+dS+`,"key":"nope.dump"}`, 404)

	es := e.audit(t, e.root, "?action=dumpcheck.")
	var okN, failN int
	for _, x := range es {
		switch {
		case x.Success:
			okN++
		default:
			failN++
		}
		if x.Action != "dumpcheck.start" {
			t.Fatal(x)
		}
	}
	if okN != 1 || failN != 2 {
		t.Fatalf("audit: %d ok, %d failed", okN, failN)
	}
}
