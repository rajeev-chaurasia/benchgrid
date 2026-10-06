package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rajeev-chaurasia/benchgrid/internal/artifact"
	"github.com/rajeev-chaurasia/benchgrid/internal/capability"
	"github.com/rajeev-chaurasia/benchgrid/internal/probe"
	"github.com/rajeev-chaurasia/benchgrid/internal/spec"
	"github.com/rajeev-chaurasia/benchgrid/internal/wire"
)

// Timestamp is the contract's format: UTC, nine fraction digits, always.
const Timestamp = "2006-01-02T15:04:05.000000000Z"

type session struct {
	d     wire.Dispatch
	fence int64

	ctx    context.Context
	cancel context.CancelCauseFunc
	// execDone closes when nothing launched by this session can still be
	// running on the rig. Preemption waits on it, so it must not close before
	// the process group is reaped.
	execDone chan struct{}

	mu     sync.Mutex
	phase  string
	result string
	reason string
	pgid   int
}

var errPreempted = errors.New("preempted:fence")

func (a *Agent) newSession(d wire.Dispatch) *session {
	ctx, cancel := context.WithCancelCause(context.Background())
	return &session{d: d, fence: d.Fence, ctx: ctx, cancel: cancel,
		execDone: make(chan struct{}), phase: PhaseLeased}
}

func (s *session) preempt() { s.cancel(errPreempted) }

func (s *session) setPhase(p string) {
	s.mu.Lock()
	s.phase = p
	s.mu.Unlock()
}

func (s *session) status() wire.RunStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return wire.RunStatus{ExperimentID: s.d.ExperimentID, Attempt: s.d.Attempt, Fence: s.fence,
		Phase: s.phase, Status: s.result, StatusReason: s.reason}
}

func (s *session) finalStatus() string  { s.mu.Lock(); defer s.mu.Unlock(); return s.result }
func (s *session) statusReason() string { s.mu.Lock(); defer s.mu.Unlock(); return s.reason }

// outcome carries what the run decided, so the collection step that follows
// does not need to know which branch produced it.
type outcome struct {
	status, reason string
	samples        []artifact.Sample
	before, after  probe.Readings
	governor       string
	started        time.Time
}

func (a *Agent) run(s *session) {
	sessionStart := a.clock()
	out := a.execute(s)
	if cause := context.Cause(s.ctx); errors.Is(cause, errPreempted) {
		out.status, out.reason = artifact.Invalid, errPreempted.Error()
	}

	s.setPhase(PhaseCleanup)
	if err := a.cleanup(s); err != nil {
		a.quarantine("cleanup:" + err.Error())
		if out.status == artifact.Succeeded {
			out.status, out.reason = artifact.Invalid, "cleanup_failed"
		}
	}
	end := a.clock()
	close(s.execDone)
	a.intervals.Record(Interval{RigID: a.cfg.RigID, Kind: "session", Fence: s.fence,
		ExperimentID: s.d.ExperimentID, Attempt: s.d.Attempt, StartNS: sessionStart, EndNS: end,
		Outcome: out.status + " " + out.reason})

	s.mu.Lock()
	s.result, s.reason = out.status, out.reason
	s.mu.Unlock()
	a.finished(s)

	s.setPhase(PhaseCollecting)
	if err := a.collect(s, out); err != nil {
		a.log.Error("collect failed", "experiment", s.d.ExperimentID, "err", err)
	}
	s.setPhase(PhaseDone)
}

func (a *Agent) execute(s *session) outcome {
	sp := s.d.Spec
	out := outcome{started: time.Now()}
	fail := func(status, reason string) outcome {
		out.status, out.reason = status, reason
		return out
	}

	deadline := time.Duration(sp.TimeoutSeconds) * time.Second
	ctx, cancel := context.WithTimeoutCause(s.ctx, deadline, errors.New("timeout"))
	defer cancel()

	s.setPhase(PhasePreflight)
	if why := capability.Match(sp.Requirements, sp.Environment, a.describe(ctx)); len(why) > 0 {
		// The rig no longer is what it advertised. Nothing it measures can be
		// trusted until an operator looks.
		a.quarantine("identity:" + why[0])
		return fail(artifact.Invalid, "preflight:identity")
	}
	if err := a.reapStale(); err != nil {
		a.quarantine("stale_process")
		return fail(artifact.Invalid, "preflight:stale_process")
	}
	binary, err := a.fetchBinary(ctx, sp.Artifacts.BinarySHA256)
	if err != nil {
		return fail(artifact.Failed, "artifact:"+err.Error())
	}
	out.governor = a.applyGovernor(sp.Environment)
	if sp.Environment.GovernorRequired && out.governor != sp.Environment.CPUGovernor {
		return fail(artifact.Invalid, "preflight:governor")
	}
	before, field, ok := a.waitForGate(ctx, sp.Environment)
	out.before = before
	if !ok {
		if ctx.Err() != nil {
			return fail(artifact.Invalid, cancelReason(ctx))
		}
		return fail(artifact.Invalid, "preflight:"+field)
	}

	s.setPhase(PhaseRunning)
	argv := make([]string, len(sp.Command))
	for i, x := range sp.Command {
		argv[i] = strings.ReplaceAll(x, spec.BinaryPlaceholder, binary)
	}
	total := sp.Warmups + sp.Repetitions
	var first int64 = -1
	for i := 0; i < total; i++ {
		if ctx.Err() != nil {
			return fail(artifact.Failed, cancelReason(ctx))
		}
		r, err := runIteration(ctx, argv, a.clock, func(pid int) {
			s.mu.Lock()
			s.pgid = pid
			s.mu.Unlock()
			a.trackGroup(pid, s)
		})
		a.intervals.Record(Interval{RigID: a.cfg.RigID, Kind: "process", Fence: s.fence,
			ExperimentID: s.d.ExperimentID, Attempt: s.d.Attempt, PID: r.PID,
			StartNS: r.StartNS, EndNS: r.EndNS})
		if r.PID > 0 && !groupAlive(r.PID) {
			a.trackGroup(r.PID, nil)
		}
		if first < 0 {
			first = r.StartNS
		}
		if r.Cancelled {
			return fail(artifact.Failed, cancelReason(ctx))
		}
		if err != nil {
			return fail(artifact.Failed, "exec:"+err.Error())
		}
		if r.Signal != "" {
			return fail(artifact.Failed, "crash")
		}
		if r.ExitCode != 0 {
			return fail(artifact.Failed, "exit:"+strconv.Itoa(r.ExitCode))
		}
		values := map[string]float64{
			"iteration_latency": float64(r.WallNS),
			"cpu_user":          float64(r.UserNS),
			"cpu_sys":           float64(r.SysNS),
			"max_rss":           float64(r.MaxRSS),
		}
		for k, v := range r.Reported {
			if _, builtin := spec.Builtin[k]; !builtin {
				values[k] = v
			}
		}
		for _, m := range sp.Metrics {
			v, ok := values[m.Name]
			if !ok {
				return fail(artifact.Failed, "metric_missing:"+m.Name)
			}
			out.samples = append(out.samples, artifact.Sample{Metric: m.Name, Iteration: i,
				Warmup: i < sp.Warmups, Value: v, Unit: m.Unit, TOffsetNS: r.StartNS - first})
		}
		if t := sp.Environment.MaxTempC; t != nil {
			// Load and utilization are expected to rise while the benchmark runs,
			// so only the thermal limit is rechecked between iterations: a rig
			// that throttles mid-run has measured its own heat, not the code.
			r := a.cfg.Prober.Read(ctx)
			if r.TempC != nil && *r.TempC > *t {
				out.after = r
				return fail(artifact.Invalid, "during:temp_c")
			}
		}
	}
	out.after = a.cfg.Prober.Read(context.Background())
	return fail(artifact.Succeeded, "")
}

func cancelReason(ctx context.Context) string {
	if c := context.Cause(ctx); c != nil && c.Error() == "timeout" {
		return "timeout"
	}
	if errors.Is(context.Cause(ctx), errPreempted) {
		return errPreempted.Error()
	}
	return "cancelled"
}

// waitForGate polls the preflight readings until every threshold the spec
// sets is met or the preflight timeout passes. Waiting rather than failing at
// once is the cool-down: a rig that just finished a heavy run is usually fine
// a few seconds later.
func (a *Agent) waitForGate(ctx context.Context, env spec.Environment) (probe.Readings, string, bool) {
	limit := time.Duration(env.PreflightTimeoutSeconds) * time.Second
	if limit == 0 {
		limit = 30 * time.Second
	}
	deadline := time.Now().Add(limit)
	for {
		r := a.cfg.Prober.Read(ctx)
		field, ok := Gate(env, r)
		if ok || time.Now().After(deadline) || ctx.Err() != nil {
			return r, field, ok
		}
		select {
		case <-ctx.Done():
			return r, field, false
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Gate returns the first threshold the readings violate. A threshold the spec
// sets but the rig cannot read fails, because an unmeasured condition is not a
// met one.
func Gate(env spec.Environment, r probe.Readings) (string, bool) {
	check := func(limit *float64, v *float64) bool { return limit == nil || (v != nil && *v <= *limit) }
	switch {
	case !check(env.MaxLoad1, r.Load1):
		return "load1", false
	case !check(env.MaxCPUUtil, r.CPUUtil):
		return "cpu_util", false
	case !check(env.MaxGPUUtil, r.GPUUtil):
		return "gpu_util", false
	case !check(env.MaxTempC, r.TempC):
		return "temp_c", false
	case env.MinMemFreeBytes > 0 && (r.MemFree == nil || *r.MemFree < env.MinMemFreeBytes):
		return "mem_free", false
	}
	return "", true
}

// applyGovernor sets the requested governor on every CPU where the kernel
// allows it, and returns what is actually in force afterwards. It never
// reports the requested value on faith.
func (a *Agent) applyGovernor(env spec.Environment) string {
	if env.CPUGovernor != "" {
		paths, _ := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*/cpufreq/scaling_governor")
		for _, p := range paths {
			os.WriteFile(p, []byte(env.CPUGovernor), 0o644)
		}
	}
	return probe.CurrentGovernor()
}

func (a *Agent) fetchBinary(ctx context.Context, digest string) (string, error) {
	path := filepath.Join(a.cfg.StateDir, "blobs", digest)
	if ok, _ := fileHasDigest(path, digest); ok {
		return path, nil
	}
	if len(a.cfg.ControlURLs) == 0 {
		return "", errNoControl
	}
	var body []byte
	if err := a.call(ctx, http.MethodGet, "/v1/blobs/"+digest, nil, nil, &rawBody{&body}); err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".blob-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(body)
	tmp.Close()
	if werr != nil {
		return "", werr
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != digest {
		return "", fmt.Errorf("binary_sha256 mismatch: got %s", got[:12])
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	return path, os.Rename(tmp.Name(), path)
}

func fileHasDigest(path, digest string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == digest, nil
}

func (a *Agent) cleanup(s *session) error {
	s.mu.Lock()
	pg := s.pgid
	s.mu.Unlock()
	if pg > 0 {
		killGroup(pg, 9)
		deadline := time.Now().Add(2 * time.Second)
		for groupAlive(pg) {
			if time.Now().After(deadline) {
				return fmt.Errorf("process group %d survived SIGKILL", pg)
			}
			time.Sleep(10 * time.Millisecond)
		}
		a.trackGroup(pg, nil)
	}
	return os.RemoveAll(filepath.Join(a.cfg.StateDir, "work", s.d.ExperimentID+"-"+strconv.Itoa(s.d.Attempt)))
}

// rawBody lets call hand back an undecoded body for the one endpoint that does
// not speak JSON.
type rawBody struct{ dst *[]byte }
