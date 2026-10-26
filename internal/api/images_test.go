package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"fridge/internal/swarm"
)

type fakeImgs struct {
	mu     sync.Mutex
	gate   chan struct{}
	images map[string]swarm.ImageInfo
}

func (f *fakeImgs) ImageInspect(_ context.Context, ref string) (swarm.ImageInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i, ok := f.images[ref]; ok {
		return i, nil
	}
	return swarm.ImageInfo{}, swarm.ErrImageNotFound
}

func (f *fakeImgs) ImagePull(_ context.Context, ref string) error {
	if f.gate != nil {
		<-f.gate
	}
	return nil
}

func (f *fakeImgs) RemoteDigest(context.Context, string) (string, error) { return "sha256:new", nil }

func withImages(e *tenants) *fakeImgs {
	f := &fakeImgs{images: map[string]swarm.ImageInfo{
		"postgres:16": {Ref: "postgres:16", ID: "sha256:aaaaaaaaaaaaaaaa", Size: 400, Env: []string{"PG_VERSION=16.4"}, RepoDigests: []string{"postgres@sha256:old"}},
	}}
	e.a.Img = f
	return f
}

func TestImagesAdminOnly(t *testing.T) {
	e := newTenants(t)
	withImages(e)
	for _, c := range [][3]string{{"GET", "/api/images", ""}, {"POST", "/api/images/pull", `{"refs":["postgres:15"]}`}, {"GET", "/api/images/pull/x", ""},
		{"POST", "/api/images/check-updates", ""}} {
		e.want(t, e.alice, c[0], c[1], c[2], 403)
	}

	var rows []struct {
		Ref     string
		Present bool
		Version string
		Size    int64
		UsedBy  []string `json:"used_by"`
	}
	json.Unmarshal(e.want(t, e.root, "GET", "/api/images", "", 200).Body.Bytes(), &rows)
	var pg16 *struct {
		Ref     string
		Present bool
		Version string
		Size    int64
		UsedBy  []string `json:"used_by"`
	}
	for i := range rows {
		if rows[i].Ref == "postgres:16" {
			pg16 = &rows[i]
		}
	}
	if len(rows) != 19 || pg16 == nil || !pg16.Present || pg16.Version != "16.4" || pg16.UsedBy == nil { // every version of the five engines
		t.Fatalf("%+v", rows)
	}

	// pull: validated, audited
	e.want(t, e.root, "POST", "/api/images/pull", `{"refs":["nginx:latest"]}`, 400)
	var job struct{ Job string }
	json.Unmarshal(e.want(t, e.root, "POST", "/api/images/pull", `{"refs":["postgres:15"]}`, 202).Body.Bytes(), &job)
	e.a.Wait()
	var st struct{ Status map[string]string }
	json.Unmarshal(e.want(t, e.root, "GET", "/api/images/pull/"+job.Job, "", 200).Body.Bytes(), &st)
	if st.Status["postgres:15"] != "done" {
		t.Fatalf("%v", st)
	}
	e.want(t, e.root, "GET", "/api/images/pull/nope", "", 404)

	e.want(t, e.root, "POST", "/api/images/check-updates", "", 200)

	es := e.audit(t, e.root, "?action=image.")
	var pulled bool
	for _, x := range es {
		switch {
		case x.Action == "image.pull" && x.Success && strings.Contains(jb(x.Detail), "postgres:15"):
			pulled = true
		}
	}
	if !pulled {
		t.Fatalf("audit pull=%v: %+v", pulled, es)
	}
	// the refusals are in the log too
	if x := find(es, "image.pull", "alice"); x == nil || x.Success {
		t.Fatalf("alice's refused pull: %+v", x)
	}
}

func TestActivityIncludesPulls(t *testing.T) {
	e := newTenants(t)
	f := withImages(e)
	f.gate = make(chan struct{})
	e.want(t, e.root, "POST", "/api/images/pull", `{"refs":["postgres:15"]}`, 202)

	var act struct {
		Jobs []struct {
			Kind, Database string
			StartedBy      string `json:"started_by"`
		}
	}
	json.Unmarshal(e.want(t, e.root, "GET", "/api/activity", "", 200).Body.Bytes(), &act)
	if len(act.Jobs) != 1 || act.Jobs[0].Kind != "image_pull" || act.Jobs[0].Database != "postgres:15" || act.Jobs[0].StartedBy != "root1" {
		t.Fatalf("%+v", act.Jobs)
	}
	close(f.gate)
	e.a.Wait()
	json.Unmarshal(e.want(t, e.root, "GET", "/api/activity", "", 200).Body.Bytes(), &act)
	if len(act.Jobs) != 0 {
		t.Fatalf("%+v", act.Jobs)
	}
}
