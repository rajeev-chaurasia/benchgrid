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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rajeev-chaurasia/benchgrid/internal/artifact"
	"github.com/rajeev-chaurasia/benchgrid/internal/capability"
	"github.com/rajeev-chaurasia/benchgrid/internal/probe"
	"github.com/rajeev-chaurasia/benchgrid/internal/spec"
	"github.com/rajeev-chaurasia/benchgrid/internal/telemetry"
	"github.com/rajeev-chaurasia/benchgrid/internal/wire"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
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

	// span covers the whole session and phaseSpan the current phase. The
	// session's context carries only the span, never the dispatch's
	// cancellation, which belongs to an HTTP request long since answered.
	span      trace.Span
	phaseSpan trace.Span
	traceCtx  context.Context

	mu     sync.Mutex
	phase  string
	result string
	reason string
	pgid   int
}

var errPreempted = errors.New("preempted:fence")

func (a *Agent) newSession(parent context.Context, d wire.Dispatch) *session {
	linked := trace.ContextWithRemoteSpanContext(context.Background(), trace.SpanContextFromContext(parent))
	traceCtx, span := telemetry.Tracer().Start(linked, "session", trace.WithAttributes(
		attribute.String("benchgrid.rig", a.cfg.RigID),
		attribute.String("benchgrid.experiment", d.ExperimentID),
		attribute.Int("benchgrid.attempt", d.Attempt),
		attribute.Int64("benchgrid.fence", d.Fence),
	))
	ctx, cancel := context.WithCancelCause(context.Background())
	return &session{d: d, fence: d.Fence, ctx: ctx, cancel: cancel, span: span, traceCtx: traceCtx,
		execDone: make(chan struct{}), phase: PhaseLeased}
}

func (s *session) preempt() { s.cancel(errPreempted) }

// setPhase also ends the previous phase's span and starts the next, so the
// trace shows where an attempt spent its time without any phase having to
// remember to close its own span.
func (s *session) setPhase(p string) {
	s.mu.Lock()
	s.phase = p
	if s.phaseSpan != nil {
		s.phaseSpan.End()
		s.phaseSpan = nil
	}
	if p != PhaseDone && s.traceCtx != nil {
		_, s.phaseSpan = telemetry.Tracer().Start(s.traceCtx, strings.ToLower(p))
	}
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
	// diagnostics is what a person needs to see why a run did not succeed,
	// published beside it as diagnostics.txt.
	diagnostics   string
	samples       []artifact.Sample
	before, after probe.Readings
	governor      string
	started       time.Time
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
	s.span.SetAttributes(attribute.String("benchgrid.status", out.status), attribute.String("benchgrid.reason", out.reason))
	s.span.End()
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

	// Every attempt runs in a workspace of its own, created empty and removed
	// afterwards, so nothing one benchmark leaves on disk is there for the
	// next to read, and the benchmark cannot depend on the agent's own working
	// directory.
	workspace := a.workspace(s)
	os.RemoveAll(workspace)
	if err := os.MkdirAll(filepath.Join(workspace, "tmp"), 0o755); err != nil {
		return fail(artifact.Invalid, "preflight:workspace")
	}

	s.setPhase(PhaseRunning)
	argv := make([]string, len(sp.Command))
	for i, x := range sp.Command {
		argv[i] = strings.ReplaceAll(x, spec.BinaryPlaceholder, binary)
	}
	total := sp.Warmups + sp.Repetitions
	var first int64 = -1
	var (
		measuredStart time.Time
		busyStart     float64
		haveBusy      bool
		ownNS         int64
	)
	for i := 0; i < total; i++ {
		if ctx.Err() != nil {
			return fail(artifact.Failed, cancelReason(ctx))
		}
		if i == sp.Warmups {
			measuredStart = time.Now()
			busyStart, haveBusy = probe.BusyCPUSeconds(ctx)
		}
		r, err := runIteration(ctx, iteration{
			argv: argv, dir: workspace, env: benchmarkEnv(workspace, i, i < sp.Warmups), clock: a.clock,
			beforeStart: func() { a.markLaunching(s) },
			onStart: func(pid int, startNS int64) {
				s.mu.Lock()
				s.pgid = pid
				s.mu.Unlock()
				a.trackGroup(pid, s, startNS)
			},
		})
		if r.PID == 0 {
			// Nothing started, so there is nothing for the marker to warn about.
			os.Remove(a.launchingPath(s))
		}
		a.intervals.Record(Interval{RigID: a.cfg.RigID, Kind: "process", Fence: s.fence,
			ExperimentID: s.d.ExperimentID, Attempt: s.d.Attempt, PID: r.PID,
			StartNS: r.StartNS, EndNS: r.EndNS})
		if r.PID > 0 && !groupAlive(r.PID) {
			a.untrackGroup(r.PID)
		}
		if first < 0 {
			first = r.StartNS
		}
		if r.Cancelled || err != nil || r.Signal != "" || r.ExitCode != 0 {
			out.diagnostics = diagnose(i, argv, r, err)
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
		if i >= sp.Warmups {
			ownNS += r.UserNS + r.SysNS
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
	if limit := sp.Environment.MaxCPUUtil; limit != nil && sp.Environment.GateDuringMeasurement {
		busyEnd, ok := probe.BusyCPUSeconds(context.Background())
		if !haveBusy || !ok {
			return fail(artifact.Invalid, "during:cpu_util_unreadable")
		}
		bg := BackgroundUtil(busyEnd-busyStart, ownNS, time.Since(measuredStart).Nanoseconds(), runtime.NumCPU())
		a.log.Info("background load during measurement", "experiment", s.d.ExperimentID, "background_util", bg)
		if bg > *limit {
			return fail(artifact.Invalid, "during:cpu_util")
		}
	}
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

// BackgroundUtil is the fraction of the machine's CPU capacity used by
// anything other than the benchmark while it ran. Clamped at zero, because
// the system counters and the process's own accounting tick at different
// resolutions and can disagree by a little in either direction.
func BackgroundUtil(busyDelta float64, ownNS, wallNS int64, cores int) float64 {
	if wallNS <= 0 || cores <= 0 {
		return 0
	}
	bg := (busyDelta - float64(ownNS)/1e9) / (float64(wallNS) / 1e9 * float64(cores))
	if bg < 0 {
		return 0
	}
	return bg
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

// applyGovernor sets the requested governor where the kernel allows it and
// returns what is actually in force afterwards.
func (a *Agent) applyGovernor(env spec.Environment) string {
	freq := a.cfg.Prober.CPUFreq
	if env.CPUGovernor == "" {
		return freq.Current()
	}
	return freq.Set(env.CPUGovernor)
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
		a.untrackGroup(pg)
	}
	return os.RemoveAll(a.workspace(s))
}

func (a *Agent) workspace(s *session) string {
	return filepath.Join(a.cfg.StateDir, "work", s.d.ExperimentID+"-"+strconv.Itoa(s.d.Attempt))
}

// benchmarkEnv is the whole environment a benchmark sees. It does not inherit
// the agent's, because a variable set on one rig's agent and not another's is
// an unrecorded difference between two runs of the same spec.
func benchmarkEnv(workspace string, iteration int, warmup bool) []string {
	w := "0"
	if warmup {
		w = "1"
	}
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + workspace,
		"TMPDIR=" + filepath.Join(workspace, "tmp"),
		"LANG=C",
		"BENCHGRID_ITERATION=" + strconv.Itoa(iteration),
		"BENCHGRID_WARMUP=" + w,
	}
}

func diagnose(i int, argv []string, r IterationResult, err error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "iteration: %d\ncommand: %s\npid: %d\nexit_code: %d\nsignal: %s\ncancelled: %v\nwall_ns: %d\n",
		i, strings.Join(argv, " "), r.PID, r.ExitCode, r.Signal, r.Cancelled, r.WallNS)
	if err != nil {
		fmt.Fprintf(&b, "error: %v\n", err)
	}
	fmt.Fprintf(&b, "stderr (first 16 KiB):\n%s", r.Stderr)
	return b.String()
}

// rawBody lets call hand back an undecoded body for the one endpoint that does
// not speak JSON.
type rawBody struct{ dst *[]byte }
