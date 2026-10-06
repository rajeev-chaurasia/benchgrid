package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rajeev-chaurasia/benchgrid/internal/artifact"
	"github.com/rajeev-chaurasia/benchgrid/internal/capability"
	"github.com/rajeev-chaurasia/benchgrid/internal/lease"
	"github.com/rajeev-chaurasia/benchgrid/internal/testdb"
	"github.com/rajeev-chaurasia/benchgrid/internal/wire"
)

const specJSON = `{"benchmark":"b","revision":"` + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `","command":["{binary}"],
 "warmups":0,"repetitions":1,"timeout_seconds":10,"requirements":{"allow_emulated":true},"environment":{},
 "metrics":[{"name":"iteration_latency","unit":"ns","direction":"lower_is_better"}],
 "artifacts":{"binary_sha256":"` + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" + `"}}`

func newServer(t *testing.T) (*Server, *httptest.Server) {
	db := testdb.Open(t, "server")
	s := &Server{DB: db, Store: &artifact.FSStore{Root: t.TempDir()}, LeaseTTL: 200 * time.Millisecond}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func post(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSubmitIsIdempotentPerSpec(t *testing.T) {
	_, ts := newServer(t)
	sub := map[string]any{"spec": json.RawMessage(specJSON), "idempotency_key": "ci-123"}
	var a, b Experiment
	r1 := post(t, ts.URL+"/v1/experiments", sub)
	json.NewDecoder(r1.Body).Decode(&a)
	r2 := post(t, ts.URL+"/v1/experiments", sub)
	json.NewDecoder(r2.Body).Decode(&b)
	if r1.StatusCode != 201 || r2.StatusCode != 200 || a.ID != b.ID {
		t.Errorf("%d %d %s %s", r1.StatusCode, r2.StatusCode, a.ID, b.ID)
	}
	other := map[string]any{"spec": json.RawMessage(strings.Replace(specJSON, `"repetitions":1`, `"repetitions":2`, 1)), "idempotency_key": "ci-123"}
	if r := post(t, ts.URL+"/v1/experiments", other); r.StatusCode != http.StatusConflict {
		t.Errorf("same key, different spec: %d", r.StatusCode)
	}
	bad := map[string]any{"spec": json.RawMessage(strings.Replace(specJSON, `"unit":"ns"`, `"unit":"ms"`, 1))}
	if r := post(t, ts.URL+"/v1/experiments", bad); r.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("invalid spec: %d", r.StatusCode)
	}
}

// The rig, not the scheduler, keeps a lease alive. A heartbeat naming the
// current fence renews it; one naming an older fence does not.
func TestHeartbeatRenewsOnlyTheCurrentFence(t *testing.T) {
	s, ts := newServer(t)
	ctx := context.Background()
	hb := wire.Heartbeat{Descriptor: capability.Rig{RigID: "r"}, AgentState: "READY"}
	if r := post(t, ts.URL+"/v1/rigs/r/heartbeat", hb); r.StatusCode != http.StatusNoContent {
		t.Fatalf("register: %d", r.StatusCode)
	}
	g, ok, _ := lease.Acquire(ctx, s.DB, "r", "t", "e", 1, 200*time.Millisecond)
	if !ok {
		t.Fatal("no lease")
	}
	hb.Active = []wire.ActiveRun{{ExperimentID: "e", Attempt: 1, Fence: g.Fence}}
	for i := 0; i < 4; i++ {
		time.Sleep(100 * time.Millisecond)
		post(t, ts.URL+"/v1/rigs/r/heartbeat", hb)
	}
	if _, ok, _ := lease.Acquire(ctx, s.DB, "r", "x", "e2", 1, time.Second); ok {
		t.Fatal("a lease renewed by heartbeats was taken over")
	}
	hb.Active[0].Fence = g.Fence - 1
	time.Sleep(300 * time.Millisecond)
	post(t, ts.URL+"/v1/rigs/r/heartbeat", hb)
	if _, ok, _ := lease.Acquire(ctx, s.DB, "r", "x", "e2", 1, time.Second); !ok {
		t.Error("a heartbeat for an old fence kept the lease alive")
	}
}

func TestHeartbeatRejectsMismatchedRig(t *testing.T) {
	_, ts := newServer(t)
	hb := wire.Heartbeat{Descriptor: capability.Rig{RigID: "other"}, AgentState: "READY"}
	if r := post(t, ts.URL+"/v1/rigs/r/heartbeat", hb); r.StatusCode != http.StatusBadRequest {
		t.Errorf("%d", r.StatusCode)
	}
}

func TestArtifactPathsAreStrict(t *testing.T) {
	_, ts := newServer(t)
	for _, path := range []string{"/v1/artifacts/runs/e/attempt-01/run.json", "/v1/artifacts/runs/e/attempt-0/run.json", "/v1/artifacts/runs/e/1/run.json"} {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+path, strings.NewReader("{}"))
		req.Header.Set(wire.SHA256Header, strings.Repeat("0", 64))
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode/100 == 2 {
			t.Errorf("%s accepted", path)
		}
	}
}
