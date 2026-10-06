package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/rajeev-chaurasia/benchgrid/internal/artifact"
	"github.com/rajeev-chaurasia/benchgrid/internal/probe"
	"github.com/rajeev-chaurasia/benchgrid/internal/spec"
	"github.com/rajeev-chaurasia/benchgrid/internal/wire"
)

var benchload, benchloadSHA string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "benchload")
	if err != nil {
		panic(err)
	}
	benchload = filepath.Join(dir, "benchload")
	if out, err := exec.Command("go", "build", "-o", benchload, "../../cmd/benchload").CombinedOutput(); err != nil {
		panic(string(out))
	}
	b, _ := os.ReadFile(benchload)
	sum := sha256.Sum256(b)
	benchloadSHA = hex.EncodeToString(sum[:])
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func newAgent(t *testing.T, fenced bool, profile *probe.Profile) (*Agent, string) {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "blobs"), 0o755)
	b, _ := os.ReadFile(benchload)
	os.WriteFile(filepath.Join(dir, "blobs", benchloadSHA), b, 0o755)
	log := filepath.Join(dir, "intervals.jsonl")
	a, err := New(Config{RigID: "rig-t", StateDir: dir, Fenced: fenced, IntervalLog: log,
		Prober: &probe.Prober{Profile: profile, CPUWindow: 10 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	return a, log
}

func testSpec(args ...string) spec.Spec {
	return spec.Spec{
		Benchmark: "cpu_hash", Revision: "8f3c2aa0b6d1e4f7a9c3b5d7e9f1a3c5b7d9e1f3",
		Command: append([]string{spec.BinaryPlaceholder}, args...),
		Warmups: 1, Repetitions: 3, TimeoutSeconds: 30,
		Requirements: spec.Requirements{AllowEmulated: true},
		Metrics: []spec.Metric{
			{Name: "iteration_latency", Unit: "ns", Direction: "lower_is_better"},
			{Name: "max_rss", Unit: "bytes", Direction: "lower_is_better"},
			{Name: "throughput", Unit: "ops_per_s", Direction: "higher_is_better"},
		},
		Artifacts: spec.Artifacts{BinarySHA256: benchloadSHA},
	}
}

func dispatch(exp string, attempt int, fence int64, s spec.Spec) wire.Dispatch {
	return wire.Dispatch{ExperimentID: exp, Attempt: attempt, Fence: fence,
		LeaseAcquired: time.Now().UTC().Format(Timestamp), Spec: s}
}

func waitDone(t *testing.T, a *Agent, exp string, attempt int) wire.RunStatus {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := a.Status(exp, attempt); ok && st.Phase == PhaseDone {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s/%d never finished", exp, attempt)
	return wire.RunStatus{}
}

func readIntervals(t *testing.T, path string) []Interval {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []Interval
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var iv Interval
		json.Unmarshal(sc.Bytes(), &iv)
		out = append(out, iv)
	}
	return out
}

// overlaps counts pairs of intervals of one kind that ran at the same time
// under different fences, the quantity the fence evidence publishes.
func overlaps(ivs []Interval, kind string) int {
	n := 0
	for i := range ivs {
		for j := i + 1; j < len(ivs); j++ {
			x, y := ivs[i], ivs[j]
			if x.Kind == kind && y.Kind == kind && x.Fence != y.Fence && x.StartNS < y.EndNS && y.StartNS < x.EndNS {
				n++
			}
		}
	}
	return n
}

func TestRunProducesVerifiableArtifact(t *testing.T) {
	a, _ := newAgent(t, true, &probe.Profile{})
	if r := a.Accept(dispatch("exp_ok", 1, 1, testSpec("-rounds", "2000"))); !r.Accepted {
		t.Fatalf("rejected: %+v", r)
	}
	st := waitDone(t, a, "exp_ok", 1)
	if st.Status != artifact.Succeeded {
		t.Fatalf("%+v", st)
	}
	run, samples, err := artifact.Verify(artifact.AttemptDir(filepath.Join(a.cfg.StateDir, "runs"), "exp_ok", 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 12 || run.Summary["throughput"].N != 3 || !run.Rig.Emulated {
		t.Errorf("samples %d summary %+v emulated %v", len(samples), run.Summary["throughput"], run.Rig.Emulated)
	}
	if r := a.Accept(dispatch("exp_ok", 1, 1, testSpec())); !r.Duplicate {
		t.Errorf("repeat dispatch was not recognised as a duplicate: %+v", r)
	}
}

func TestHigherFencePreemptsAndLowerIsRejected(t *testing.T) {
	a, log := newAgent(t, true, &probe.Profile{})
	slow := testSpec("-rounds", "100", "-sleep", "300ms")
	if r := a.Accept(dispatch("exp_old", 1, 5, slow)); !r.Accepted {
		t.Fatal(r)
	}
	time.Sleep(150 * time.Millisecond)
	if r := a.Accept(dispatch("exp_new", 1, 6, testSpec("-rounds", "100"))); !r.Accepted {
		t.Fatal(r)
	}
	if st := waitDone(t, a, "exp_old", 1); st.Status != artifact.Invalid || st.StatusReason != "preempted:fence" {
		t.Errorf("old session: %+v", st)
	}
	if st := waitDone(t, a, "exp_new", 1); st.Status != artifact.Succeeded {
		t.Errorf("new session: %+v", st)
	}
	if r := a.Accept(dispatch("exp_stale", 1, 5, testSpec())); r.Accepted || r.Reason != wire.RejectStaleFence {
		t.Errorf("stale fence accepted: %+v", r)
	}
	ivs := readIntervals(t, log)
	if n := overlaps(ivs, "session") + overlaps(ivs, "process"); n != 0 {
		t.Errorf("%d overlapping intervals under fencing", n)
	}
}

// The control: the same dispatches against an agent that does not check the
// fence must overlap, or the test above proves nothing.
func TestUnfencedAgentOverlaps(t *testing.T) {
	a, log := newAgent(t, false, &probe.Profile{})
	slow := testSpec("-rounds", "100", "-sleep", "300ms")
	a.Accept(dispatch("exp_old", 1, 5, slow))
	time.Sleep(150 * time.Millisecond)
	a.Accept(dispatch("exp_new", 1, 6, slow))
	waitDone(t, a, "exp_old", 1)
	waitDone(t, a, "exp_new", 1)
	if n := overlaps(readIntervals(t, log), "process"); n == 0 {
		t.Error("unfenced agent produced no overlap, so the fenced test is not evidence")
	}
}

func TestTimeoutKillsTheWholeGroup(t *testing.T) {
	old := grace
	grace = 200 * time.Millisecond
	t.Cleanup(func() { grace = old })
	a, _ := newAgent(t, true, &probe.Profile{})
	sp := testSpec("-rounds", "10", "-hang", "-orphan")
	sp.TimeoutSeconds = 1
	a.Accept(dispatch("exp_hang", 1, 1, sp))
	st := waitDone(t, a, "exp_hang", 1)
	if st.Status != artifact.Failed || st.StatusReason != "timeout" {
		t.Errorf("%+v", st)
	}
	out, _ := exec.Command("pgrep", "-f", benchload+" -hang").Output()
	if len(out) > 0 {
		exec.Command("pkill", "-9", "-f", benchload).Run()
		t.Errorf("benchmark processes survived the timeout: %s", out)
	}
	if a.quarantined() {
		t.Error("a clean timeout quarantined the rig")
	}
}

func TestRepeatedCrashesQuarantine(t *testing.T) {
	a, _ := newAgent(t, true, &probe.Profile{})
	for i := 1; i <= 3; i++ {
		exp := fmt.Sprintf("exp_crash_%d", i)
		a.Accept(dispatch(exp, 1, int64(i), testSpec("-rounds", "10", "-crash")))
		if st := waitDone(t, a, exp, 1); st.StatusReason != "crash" {
			t.Fatalf("%+v", st)
		}
	}
	if r := a.Accept(dispatch("exp_after", 1, 4, testSpec())); r.Accepted || r.Reason != wire.RejectQuarantined {
		t.Errorf("quarantined rig accepted work: %+v", r)
	}
	a.Unquarantine()
	if r := a.Accept(dispatch("exp_after", 1, 4, testSpec("-rounds", "10"))); !r.Accepted {
		t.Errorf("unquarantined rig refused work: %+v", r)
	}
	waitDone(t, a, "exp_after", 1)
}

func TestGateHoldsAndFailsOnInjectedGPULoad(t *testing.T) {
	gpu := filepath.Join(t.TempDir(), "gpu")
	os.WriteFile(gpu, []byte("0.9"), 0o644)
	a, _ := newAgent(t, true, &probe.Profile{GPUUtilFile: gpu})
	sp := testSpec("-rounds", "10")
	limit := 0.05
	sp.Environment = spec.Environment{MaxGPUUtil: &limit, PreflightTimeoutSeconds: 1}
	a.Accept(dispatch("exp_busy", 1, 1, sp))
	if st := waitDone(t, a, "exp_busy", 1); st.Status != artifact.Invalid || st.StatusReason != "preflight:gpu_util" {
		t.Errorf("%+v", st)
	}

	// Cool-down: the gate waits, and passes once the load drops.
	sp.Environment.PreflightTimeoutSeconds = 10
	a.Accept(dispatch("exp_wait", 1, 2, sp))
	time.Sleep(700 * time.Millisecond)
	os.WriteFile(gpu, []byte("0.01"), 0o644)
	if st := waitDone(t, a, "exp_wait", 1); st.Status != artifact.Succeeded {
		t.Errorf("%+v", st)
	}
}

func TestIdentityMismatchQuarantines(t *testing.T) {
	a, _ := newAgent(t, true, &probe.Profile{})
	sp := testSpec()
	sp.Requirements.GPUVendor = "nvidia"
	a.Accept(dispatch("exp_gpu", 1, 1, sp))
	if st := waitDone(t, a, "exp_gpu", 1); st.StatusReason != "preflight:identity" {
		t.Errorf("%+v", st)
	}
	if !a.quarantined() {
		t.Error("a rig that is not what the spec matched was left in service")
	}
}

func TestCorruptBinaryIsNotRun(t *testing.T) {
	a, _ := newAgent(t, true, &probe.Profile{})
	os.WriteFile(filepath.Join(a.cfg.StateDir, "blobs", benchloadSHA), []byte("not the binary"), 0o755)
	a.Accept(dispatch("exp_corrupt", 1, 1, testSpec()))
	st := waitDone(t, a, "exp_corrupt", 1)
	if st.Status != artifact.Failed {
		t.Errorf("%+v", st)
	}
}

func TestGateTreatsUnreadableAsFailing(t *testing.T) {
	limit := 50.0
	if _, ok := Gate(spec.Environment{MaxTempC: &limit}, probe.Readings{}); ok {
		t.Error("a temperature limit passed on a rig that cannot read temperature")
	}
	if _, ok := Gate(spec.Environment{}, probe.Readings{}); !ok {
		t.Error("no thresholds must mean no gate")
	}
}

var _ = context.Background
