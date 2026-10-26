package api

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"strconv"
	"strings"
	"testing"

	"fridge/internal/app"
	"fridge/internal/objstore"
	"fridge/internal/store"
)

const destBody = `{"name":"d","endpoint":"https://s3.example.com","bucket":"b","access_key":"AK","secret_key":"x"}`
const dbBody = `{"engine":"postgres","name":"abc","domain":"abc.com"}`

func TestInternalErrorHidden(t *testing.T) {
	h, _, a := newSrv(t)
	do(h, "POST", "/api/destinations", destBody, true)
	a.OpenObjects = func(store.Destination, string, *objstore.Guard) (app.ObjectStore, error) {
		return nil, errors.New("dial tcp 10.1.2.3: secret-host")
	}
	w := do(h, "GET", "/api/destinations/1/objects", "", true)
	if w.Code != 500 || strings.TrimSpace(w.Body.String()) != `{"error":"internal error"}` {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestDestinationLifecycle(t *testing.T) {
	h, _, a := newSrv(t)
	fo := &fakeObjs{list: []objstore.Object{{Key: "fridge/a.dump", Size: 3}}}
	a.OpenObjects = func(store.Destination, string, *objstore.Guard) (app.ObjectStore, error) { return fo, nil }
	do(h, "POST", "/api/destinations", destBody, true)
	before, _ := a.Store.GetDestination(context.Background(), 1)
	w := do(h, "PUT", "/api/destinations/1", `{"name":"d2","endpoint":"https://s3.example.com","bucket":"b","access_key":"AK"}`, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"d2"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	after, _ := a.Store.GetDestination(context.Background(), 1)
	if after.SecretKeyEnc != before.SecretKeyEnc {
		t.Fatal("empty secret_key must keep the old secret")
	}
	if w := do(h, "POST", "/api/destinations/1/test", "", true); w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	fo.testErr = errors.New("bad creds")
	if w := do(h, "POST", "/api/destinations/1/test", "", true); w.Code != 400 || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := do(h, "POST", "/api/destinations/99/test", "", true); w.Code != 404 {
		t.Fatal(w.Code)
	}
	w = do(h, "GET", "/api/destinations/1/objects?prefix=fridge", "", true)
	var o struct {
		Objects   []struct{ Key string } `json:"objects"`
		Truncated bool                   `json:"truncated"`
	}
	json.Unmarshal(w.Body.Bytes(), &o)
	if w.Code != 200 || len(o.Objects) != 1 || o.Objects[0].Key != "fridge/a.dump" || !o.Truncated {
		t.Fatal(w.Code, w.Body.String())
	}
	// the new database's default schedule references the destination
	do(h, "POST", "/api/databases", dbBody, true)
	if w := do(h, "DELETE", "/api/destinations/1", "", true); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestSchedules(t *testing.T) {
	h, _, _ := newSrv(t)
	do(h, "POST", "/api/destinations", destBody, true)
	do(h, "POST", "/api/databases", dbBody, true)
	if w := do(h, "POST", "/api/databases/alice_abc/schedules", `{"destination_id":1,"cron":"nope","keep_latest":3}`, true); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	w := do(h, "POST", "/api/databases/alice_abc/schedules", `{"destination_id":1,"cron":"0 3 * * *","keep_latest":3,"enabled":true}`, true)
	var sc struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &sc)
	if w.Code != 201 || sc.ID == 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/api/databases/alice_abc/schedules", "", true); w.Code != 200 || !strings.Contains(w.Body.String(), "0 3 * * *") {
		t.Fatal(w.Code, w.Body.String())
	}
	id := strconv.FormatInt(sc.ID, 10)
	w = do(h, "PUT", "/api/schedules/"+id, `{"destination_id":1,"cron":"0 4 * * *","keep_latest":5,"enabled":false}`, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "0 4 * * *") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := do(h, "PUT", "/api/schedules/"+id, `{"destination_id":1,"cron":"bad","keep_latest":5}`, true); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := do(h, "DELETE", "/api/schedules/"+id, "", true); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w := do(h, "DELETE", "/api/schedules/"+id, "", true); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestAllowlistAndCA(t *testing.T) {
	h, _, _ := newSrv(t)
	if w := do(h, "PUT", "/api/settings/allowlist", `{"cidrs":["nope"]}`, true); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := do(h, "PUT", "/api/settings/allowlist", `{"cidrs":["10.1.0.0/16"]}`, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/api/settings/allowlist", "", true); !strings.Contains(w.Body.String(), "10.1.0.0/16") {
		t.Fatal(w.Body.String())
	}
	w := do(h, "GET", "/api/ca", "", true)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") || !strings.Contains(w.Body.String(), "BEGIN CERTIFICATE") {
		t.Fatal(w.Code, w.Header(), w.Body.String())
	}
}

func TestBackupGetAndDownload(t *testing.T) {
	h, rt, a := newSrv(t)
	rt.dump = "PGDMP-bytes"
	fo := &fakeObjs{body: rt.dump}
	a.OpenObjects = func(store.Destination, string, *objstore.Guard) (app.ObjectStore, error) { return fo, nil }
	do(h, "POST", "/api/databases", dbBody, true)
	do(h, "POST", "/api/destinations", destBody, true)
	do(h, "POST", "/api/databases/alice_abc/backups", `{}`, true)
	a.Wait()
	w := do(h, "GET", "/api/backups/1", "", true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	var b struct {
		S3Key string `json:"s3_key"`
	}
	json.Unmarshal(w.Body.Bytes(), &b)
	w = do(h, "GET", "/api/backups/1/download", "", true)
	hd := w.Header()
	if w.Code != 200 || w.Body.String() != "PGDMP-bytes" || hd.Get("Content-Type") != "application/octet-stream" ||
		hd.Get("Content-Length") != "11" || hd.Get("Content-Disposition") != `attachment; filename="`+path.Base(b.S3Key)+`"` ||
		!strings.HasSuffix(b.S3Key, ".dump") || strings.Contains(w.Body.String(), "X-Amz-Credential") {
		t.Fatal(w.Code, hd, w.Body.String(), b.S3Key)
	}
	if w := do(h, "GET", "/api/backups/99", "", true); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestRestores(t *testing.T) {
	h, _, a := newSrv(t)
	fo := &fakeObjs{meta: map[string]string{"fridge-engine": "postgres", "fridge-version": "17"}, head: dumpFile(t, "pg17.dump")}
	a.OpenObjects = func(store.Destination, string, *objstore.Guard) (app.ObjectStore, error) { return fo, nil }
	do(h, "POST", "/api/destinations", destBody, true)
	if w := do(h, "POST", "/api/restores", `{"destination_id":1,"key":"k.dump"}`, true); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	w := do(h, "POST", "/api/restores", `{"destination_id":1,"key":"k.dump","new":{"name":"copy","domain":"copy.com"}}`, true)
	var out struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 202 || out.ID == 0 {
		t.Fatal(w.Code, w.Body.String())
	}
	a.Wait()
	if w := do(h, "GET", "/api/restores/"+strconv.FormatInt(out.ID, 10), "", true); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := do(h, "GET", "/api/restores/99", "", true); w.Code != 404 {
		t.Fatal(w.Code)
	}
}
