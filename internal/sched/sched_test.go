package sched

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rajeev-chaurasia/benchgrid/internal/capability"
	"github.com/rajeev-chaurasia/benchgrid/internal/spec"
	"github.com/rajeev-chaurasia/benchgrid/internal/testdb"
	"github.com/rajeev-chaurasia/benchgrid/internal/wire"
)

var ctx = context.Background()

// fakeAgent answers dispatches with a fixed reply and counts them.
type fakeAgent struct {
	srv   *httptest.Server
	reply wire.DispatchReply
	code  int
	got   atomic.Int32
}

func newFakeAgent(t *testing.T, accept bool, reason string) *fakeAgent {
	f := &fakeAgent{reply: wire.DispatchReply{Accepted: accept, Reason: reason}, code: http.StatusAccepted}
	if !accept {
		f.code = http.StatusConflict
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.got.Add(1)
		w.WriteHeader(f.code)
		json.NewEncoder(w).Encode(f.reply)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func addRig(t *testing.T, db *pgxpool.Pool, r capability.Rig) {
	t.Helper()
	b, _ := json.Marshal(r)
	if _, err := db.Exec(ctx, `INSERT INTO rigs (id, descriptor) VALUES ($1, $2)`, r.RigID, b); err != nil {
		t.Fatal(err)
	}
}

func submit(t *testing.T, db *pgxpool.Pool, id string, s spec.Spec, maxAttempts int) {
	t.Helper()
	b, _ := json.Marshal(s)
	h, _ := s.SHA256()
	if _, err := db.Exec(ctx, `INSERT INTO experiments (id, spec, spec_sha256, max_attempts) VALUES ($1, $2, $3, $4)`, id, b, h, maxAttempts); err != nil {
		t.Fatal(err)
	}
}

func baseSpec() spec.Spec {
	return spec.Spec{
		Benchmark: "b", Revision: strings.Repeat("a", 40), Command: []string{"{binary}"},
		Repetitions: 1, TimeoutSeconds: 10,
		Requirements: spec.Requirements{AllowEmulated: true},
		Metrics:      []spec.Metric{{Name: "iteration_latency", Unit: "ns", Direction: "lower_is_better"}},
		Artifacts:    spec.Artifacts{BinarySHA256: strings.Repeat("b", 64)},
	}
}

type expRow struct {
	state, reason string
	attempt       int
	rig           *string
}

func load(t *testing.T, db *pgxpool.Pool, id string) expRow {
	t.Helper()
	var e expRow
	if err := db.QueryRow(ctx, `SELECT state, status_reason, attempt, rig_id FROM experiments WHERE id = $1`, id).Scan(&e.state, &e.reason, &e.attempt, &e.rig); err != nil {
		t.Fatal(err)
	}
	return e
}

func holder(t *testing.T, db *pgxpool.Pool, rig string) *string {
	var h *string
	db.QueryRow(ctx, `SELECT holder FROM rigs WHERE id = $1`, rig).Scan(&h)
	return h
}

func TestPlacementFiltersBeforeRanking(t *testing.T) {
	db := testdb.Open(t, "sched")
	a := newFakeAgent(t, true, "")
	// cpu-old has been idle longest, so ranking alone would choose it. The
	// filter has to remove it first.
	addRig(t, db, capability.Rig{RigID: "cpu-old", Arch: "x86_64", Emulated: true, Endpoint: a.srv.URL})
	addRig(t, db, capability.Rig{RigID: "gpu-1", Arch: "x86_64", GPUVendor: "nvidia", DriverVersion: "550.54", Emulated: true, Endpoint: a.srv.URL})
	db.Exec(ctx, `UPDATE rigs SET last_released_at = clock_timestamp() WHERE id = 'gpu-1'`)

	s := baseSpec()
	s.Requirements.GPUVendor, s.Requirements.Driver = "nvidia", ">=550"
	submit(t, db, "exp_gpu", s, 3)

	sc := New(Config{ID: "t"}, db, nil)
	placed, err := sc.Place(ctx)
	if err != nil || len(placed) != 1 || placed[0].Rig.RigID != "gpu-1" || placed[0].Attempt != 1 || placed[0].Grant.Fence != 1 {
		t.Fatalf("%+v %v", placed, err)
	}
	if e := load(t, db, "exp_gpu"); e.state != "RUNNING" || e.attempt != 1 {
		t.Errorf("%+v", e)
	}
}

func TestUnplaceablePassConsumesNoAttempt(t *testing.T) {
	db := testdb.Open(t, "sched")
	addRig(t, db, capability.Rig{RigID: "cpu", Emulated: true})
	s := baseSpec()
	s.Requirements.GPUVendor = "nvidia"
	submit(t, db, "exp_wait", s, 3)
	sc := New(Config{ID: "t"}, db, nil)
	for i := 0; i < 3; i++ {
		if p, err := sc.Place(ctx); err != nil || len(p) != 0 {
			t.Fatal(p, err)
		}
	}
	if e := load(t, db, "exp_wait"); e.state != "QUEUED" || e.attempt != 0 {
		t.Errorf("%+v", e)
	}
}

func TestEmulatedRigsAreOptIn(t *testing.T) {
	db := testdb.Open(t, "sched")
	addRig(t, db, capability.Rig{RigID: "emu", Emulated: true})
	s := baseSpec()
	s.Requirements.AllowEmulated = false
	submit(t, db, "exp_real", s, 3)
	if p, _ := New(Config{ID: "t"}, db, nil).Place(ctx); len(p) != 0 {
		t.Error("placed a real-hardware experiment on an emulated rig")
	}
}

func TestRefusedDispatchFreesRigAndRequeues(t *testing.T) {
	db := testdb.Open(t, "sched")
	a := newFakeAgent(t, false, wire.RejectStaleFence)
	addRig(t, db, capability.Rig{RigID: "r", Emulated: true, Endpoint: a.srv.URL})
	submit(t, db, "exp_x", baseSpec(), 3)
	sc := New(Config{ID: "t"}, db, nil)
	placed, _ := sc.Place(ctx)
	sc.dispatch(ctx, placed[0])
	if e := load(t, db, "exp_x"); e.state != "QUEUED" || e.attempt != 1 || !strings.Contains(e.reason, "stale_fence") {
		t.Errorf("%+v", e)
	}
	if h := holder(t, db, "r"); h != nil {
		t.Error("rig still held after a refusal")
	}
	placed, _ = sc.Place(ctx)
	if len(placed) != 1 || placed[0].Attempt != 2 || placed[0].Grant.Fence != 2 {
		t.Errorf("retry: %+v", placed)
	}
}

// An unreachable agent may still have accepted, so the scheduler does not
// guess. Nothing changes until the lease lapses unrenewed, and then the reaper
// requeues.
func TestUnreachableDispatchIsReapedAfterLeaseLapses(t *testing.T) {
	db := testdb.Open(t, "sched")
	addRig(t, db, capability.Rig{RigID: "r", Emulated: true, Endpoint: "http://127.0.0.1:1"})
	submit(t, db, "exp_lost", baseSpec(), 3)
	sc := New(Config{ID: "t", LeaseTTL: 150 * time.Millisecond, HeartbeatStale: 100 * time.Millisecond, Settle: time.Nanosecond}, db, nil)
	placed, _ := sc.Place(ctx)
	sc.dispatch(ctx, placed[0])
	if e := load(t, db, "exp_lost"); e.state != "RUNNING" {
		t.Fatalf("changed state on a network error: %+v", e)
	}
	sc.Reap(ctx)
	if e := load(t, db, "exp_lost"); e.state != "RUNNING" {
		t.Fatalf("reaped a live lease: %+v", e)
	}
	time.Sleep(250 * time.Millisecond)
	sc.Reap(ctx)
	if e := load(t, db, "exp_lost"); e.state != "QUEUED" || e.reason != "rig_silent" {
		t.Errorf("%+v", e)
	}
	var st string
	db.QueryRow(ctx, `SELECT status FROM attempts WHERE experiment_id = 'exp_lost' AND attempt = 1`).Scan(&st)
	if st != "ABANDONED" {
		t.Errorf("attempt status %s", st)
	}
}

func placeOne(t *testing.T, db *pgxpool.Pool, id string, maxAttempts int) placement {
	t.Helper()
	addRig(t, db, capability.Rig{RigID: "r-" + id, Emulated: true})
	submit(t, db, id, baseSpec(), maxAttempts)
	placed, err := New(Config{ID: "t"}, db, nil).Place(ctx)
	if err != nil || len(placed) != 1 {
		t.Fatal(placed, err)
	}
	return placed[0]
}

func TestCompletionIsIdempotentAndFenced(t *testing.T) {
	db := testdb.Open(t, "sched")
	p := placeOne(t, db, "exp_c", 3)
	ok := wire.Completion{RigID: p.Rig.RigID, Fence: p.Grant.Fence, Status: "SUCCEEDED"}

	wrong := ok
	wrong.Fence++
	if err := Complete(ctx, db, "exp_c", 1, wrong); !errors.Is(err, ErrUnknownAttempt) {
		t.Errorf("accepted a completion under the wrong fence: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := Complete(ctx, db, "exp_c", 1, ok); err != nil {
			t.Fatalf("completion %d: %v", i, err)
		}
	}
	other := ok
	other.Status, other.StatusReason = "FAILED", "exit:1"
	if err := Complete(ctx, db, "exp_c", 1, other); !errors.Is(err, ErrConflict) {
		t.Errorf("a contradicting completion was accepted: %v", err)
	}
	if e := load(t, db, "exp_c"); e.state != "SUCCEEDED" {
		t.Errorf("%+v", e)
	}
	if h := holder(t, db, p.Rig.RigID); h != nil {
		t.Error("rig not released on completion")
	}
}

func TestRetryPolicy(t *testing.T) {
	cases := []struct {
		status, reason string
		max            int
		state          string
		reasonPrefix   string
	}{
		{"INVALID", "preflight:load1", 3, "QUEUED", "INVALID:preflight:load1"},
		{"FAILED", "artifact:fetch", 3, "QUEUED", "FAILED:artifact"},
		{"FAILED", "exit:3", 3, "FAILED", "exit:3"},
		{"FAILED", "timeout", 3, "FAILED", "timeout"},
		{"INVALID", "preempted:fence", 1, "FAILED", "attempts_exhausted"},
	}
	for _, c := range cases {
		t.Run(c.status+" "+c.reason, func(t *testing.T) {
			db := testdb.Open(t, "sched")
			p := placeOne(t, db, "exp_r", c.max)
			if err := Complete(ctx, db, "exp_r", 1, wire.Completion{RigID: p.Rig.RigID, Fence: p.Grant.Fence, Status: c.status, StatusReason: c.reason}); err != nil {
				t.Fatal(err)
			}
			if e := load(t, db, "exp_r"); e.state != c.state || !strings.HasPrefix(e.reason, c.reasonPrefix) {
				t.Errorf("%+v", e)
			}
		})
	}
}

// After a control plane outage every lease has lapsed while the work is still
// running. A lapsed lease on a rig that has not reported since is not reaped
// until the rig has had its staleness window to speak, and a heartbeat that
// renews the fence ends the question.
func TestLapsedLeaseIsNotReapedWhileTheRigMightStillVouch(t *testing.T) {
	db := testdb.Open(t, "sched")
	p := placeOne(t, db, "exp_outage", 3)
	sc := New(Config{ID: "t", HeartbeatStale: time.Hour, Settle: time.Nanosecond}, db, nil)
	db.Exec(ctx, `UPDATE rigs SET expires_at = clock_timestamp() - interval '1 minute', last_heartbeat = clock_timestamp() - interval '2 minutes'`)
	sc.Reap(ctx)
	if e := load(t, db, "exp_outage"); e.state != "RUNNING" {
		t.Fatalf("reaped on a lapsed lease alone: %+v", e)
	}
	db.Exec(ctx, `UPDATE rigs SET last_heartbeat = clock_timestamp()`)
	sc.Reap(ctx)
	if e := load(t, db, "exp_outage"); e.state != "QUEUED" || e.reason != "lease_not_renewed" {
		t.Errorf("a heartbeat that did not renew should have proved the attempt dead: %+v", e)
	}
	_ = p
}

func TestReaperSettlesAfterAGap(t *testing.T) {
	db := testdb.Open(t, "sched")
	placeOne(t, db, "exp_gap", 3)
	db.Exec(ctx, `UPDATE rigs SET expires_at = clock_timestamp() - interval '1 minute', last_heartbeat = clock_timestamp()`)
	sc := New(Config{ID: "t", Settle: time.Hour}, db, nil)
	sc.Reap(ctx)
	if e := load(t, db, "exp_gap"); e.state != "RUNNING" {
		t.Errorf("a freshly started reaper acted before settling: %+v", e)
	}
}

// A run that outlived its lease is still a measurement. If no newer attempt
// has started, its result closes the experiment; once one has, it does not.
func TestLateCompletionAfterReap(t *testing.T) {
	db := testdb.Open(t, "sched")
	p := placeOne(t, db, "exp_late", 3)
	sc := New(Config{ID: "t"}, db, nil)
	if err := sc.abandon(ctx, "exp_late", 1, p.Rig.RigID, p.Grant.Fence, "lease_expired"); err != nil {
		t.Fatal(err)
	}
	c := wire.Completion{RigID: p.Rig.RigID, Fence: p.Grant.Fence, Status: "SUCCEEDED"}
	if err := Complete(ctx, db, "exp_late", 1, c); err != nil {
		t.Fatal(err)
	}
	if e := load(t, db, "exp_late"); e.state != "SUCCEEDED" || e.attempt != 1 {
		t.Errorf("%+v", e)
	}

	q := placeOne(t, db, "exp_late2", 3)
	sc.abandon(ctx, "exp_late2", 1, q.Rig.RigID, q.Grant.Fence, "lease_expired")
	placed, _ := sc.Place(ctx)
	if len(placed) != 1 || placed[0].Attempt != 2 {
		t.Fatalf("%+v", placed)
	}
	Complete(ctx, db, "exp_late2", 1, wire.Completion{RigID: q.Rig.RigID, Fence: q.Grant.Fence, Status: "SUCCEEDED"})
	if e := load(t, db, "exp_late2"); e.state != "RUNNING" || e.attempt != 2 {
		t.Errorf("a superseded attempt closed the experiment: %+v", e)
	}
}

func TestConcurrentSchedulersNeverDoubleBook(t *testing.T) {
	db := testdb.Open(t, "sched")
	a := newFakeAgent(t, true, "")
	for i := 0; i < 4; i++ {
		addRig(t, db, capability.Rig{RigID: "r" + string(rune('a'+i)), Emulated: true, Endpoint: a.srv.URL})
	}
	for i := 0; i < 40; i++ {
		submit(t, db, "exp_"+string(rune('A'+i)), baseSpec(), 3)
	}
	results := make(chan []placement, 8)
	for i := 0; i < 8; i++ {
		go func(i int) {
			p, err := New(Config{ID: "s" + string(rune('0'+i))}, db, nil).Place(ctx)
			if err != nil {
				t.Error(err)
			}
			results <- p
		}(i)
	}
	rigs := map[string]int{}
	total := 0
	for i := 0; i < 8; i++ {
		for _, p := range <-results {
			rigs[p.Rig.RigID]++
			total++
		}
	}
	if total != 4 {
		t.Errorf("%d placements for 4 rigs", total)
	}
	for r, n := range rigs {
		if n != 1 {
			t.Errorf("rig %s placed %d times", r, n)
		}
	}
}

// A scheduler frozen between sending a dispatch and recording it can wake
// after the reaper abandoned the attempt. Its late write must not undo that.
func TestLateDispatchRecordDoesNotResurrect(t *testing.T) {
	db := testdb.Open(t, "sched")
	a := newFakeAgent(t, true, "")
	addRig(t, db, capability.Rig{RigID: "r", Emulated: true, Endpoint: a.srv.URL})
	submit(t, db, "exp_z", baseSpec(), 3)
	sc := New(Config{ID: "t"}, db, nil)
	placed, _ := sc.Place(ctx)
	sc.abandon(ctx, "exp_z", 1, "r", placed[0].Grant.Fence, "lease_taken")
	sc.dispatch(ctx, placed[0])
	var st string
	db.QueryRow(ctx, `SELECT status FROM attempts WHERE experiment_id = 'exp_z' AND attempt = 1`).Scan(&st)
	if st != "ABANDONED" {
		t.Errorf("abandoned attempt rewritten to %s", st)
	}
}
