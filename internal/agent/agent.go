// Package agent is the host process that owns one rig. It is the last line of
// exclusivity: whatever the schedulers and the lease table believe, the agent
// decides what actually runs, and it decides by fence.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rajeev-chaurasia/benchgrid/internal/capability"
	"github.com/rajeev-chaurasia/benchgrid/internal/probe"
	"github.com/rajeev-chaurasia/benchgrid/internal/wire"
	"golang.org/x/sys/unix"
)

const (
	StateReady       = "READY"
	StateQuarantined = "QUARANTINED"

	PhaseLeased     = "LEASED"
	PhasePreflight  = "PREFLIGHT"
	PhaseRunning    = "RUNNING"
	PhaseCollecting = "COLLECTING"
	PhaseCleanup    = "CLEANUP"
	PhaseDone       = "DONE"
)

type Config struct {
	RigID    string
	StateDir string
	// ControlURLs are control plane replicas, tried in turn. An agent pinned to
	// one replica goes dark when that replica stalls, and its leases expire
	// under work that is still running.
	ControlURLs []string
	Endpoint    string
	// Fenced is false only for the negative control, which exists to show what
	// this agent prevents. Nothing outside the evidence harness turns it off.
	Fenced         bool
	Prober         *probe.Prober
	HeartbeatEvery time.Duration
	// RequestTimeout bounds every call to the control plane. A frozen replica
	// accepts the connection and never answers, so this, not the TCP stack, is
	// what decides how quickly the agent moves on to another replica, and it
	// has to be well inside the lease TTL.
	RequestTimeout time.Duration
	IntervalLog    string
	// CrashLimit consecutive crashed attempts quarantine the rig. A benchmark
	// that crashes once is the benchmark's problem; one that crashes on every
	// attempt on one rig is more likely the rig's.
	CrashLimit int
	Logger     *slog.Logger
	// BenchCPUs, on Linux, pins every benchmark to these CPUs through the pin
	// shim, and Cgroups also places them in a cgroup that owns those CPUs.
	BenchCPUs []int
	Cgroups   bool
	// HostRoots are /proc and /sys, replaceable for tests.
	HostRoots probe.HostRoots
}

type key struct {
	experiment string
	attempt    int
}

type Agent struct {
	cfg       Config
	fence     *FenceStore
	intervals *IntervalLog
	client    *http.Client
	log       *slog.Logger
	epoch     time.Time
	next      atomic.Int32

	// mu is the admission lock. Accept holds it while preempted sessions wind
	// down, so nothing a session does on its way out may take it, or
	// preemption deadlocks. Everything a session touches lives under smu.
	mu       sync.Mutex
	sessions map[key]*session
	active   map[*session]bool

	smu        sync.Mutex
	state      string
	reason     string
	pgroups    map[int]*session
	crashes    int
	descriptor capability.Rig

	self        string
	benchCgroup string
}

func New(cfg Config) (*Agent, error) {
	if cfg.HeartbeatEvery == 0 {
		cfg.HeartbeatEvery = time.Second
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 2 * time.Second
	}
	if cfg.CrashLimit == 0 {
		cfg.CrashLimit = 3
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Prober == nil {
		cfg.Prober = &probe.Prober{}
	}
	fs, err := OpenFenceStore(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	iv, err := OpenIntervalLog(cfg.IntervalLog)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{"spool", "work", "blobs", "runs", "pgroups"} {
		if err := os.MkdirAll(filepath.Join(cfg.StateDir, d), 0o755); err != nil {
			return nil, err
		}
	}
	a := &Agent{
		cfg:       cfg,
		fence:     fs,
		intervals: iv,
		client:    &http.Client{Timeout: cfg.RequestTimeout},
		log:       cfg.Logger.With("rig", cfg.RigID),
		epoch:     time.Now(),
		state:     StateReady,
		sessions:  map[key]*session{},
		active:    map[*session]bool{},
		pgroups:   map[int]*session{},
	}
	if len(cfg.BenchCPUs) > 0 {
		if !pinSupported() {
			return nil, fmt.Errorf("bench CPUs: pinning is only supported on Linux")
		}
		if a.self, err = os.Executable(); err != nil {
			return nil, err
		}
		if cfg.Cgroups {
			// A rig without a delegated cgroup still pins, and says so in
			// host.json, rather than refusing to start.
			if a.benchCgroup, err = setupCgroups(cfg.BenchCPUs); err != nil {
				a.log.Warn("no bench cgroup, pinning by affinity only", "err", err)
			}
		}
	}
	a.descriptor = a.describe(context.Background())
	if reason, err := a.reapPrevious(); err != nil {
		a.state, a.reason = StateQuarantined, reason
		a.log.Error("quarantined at start", "err", err)
	}
	return a, nil
}

// clock is the host's monotonic clock, not the process's. Every interval on
// one rig is measured against it, so overlap is decided without any
// cross-machine clock agreement, and intervals recorded before and after an
// agent restart remain comparable, which a per-process epoch would not be.
func (a *Agent) clock() int64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return time.Since(a.epoch).Nanoseconds()
	}
	return ts.Nano()
}

func (a *Agent) describe(ctx context.Context) capability.Rig {
	d := a.cfg.Prober.Describe(ctx, a.cfg.RigID)
	d.Endpoint = a.cfg.Endpoint
	d.BenchCPUs = append([]int{}, a.cfg.BenchCPUs...)
	if len(d.BenchCPUs) > 0 {
		isolated := map[int]bool{}
		for _, c := range probe.ReadHostTuning(ctx, a.cfg.HostRoots).IsolatedCPUs {
			isolated[c] = true
		}
		d.IsolatedBench = true
		for _, c := range d.BenchCPUs {
			d.IsolatedBench = d.IsolatedBench && isolated[c]
		}
	}
	return d
}

// Accept is the fence check. It holds the agent lock for its whole duration,
// including waiting for preempted work to stop, so that a new session's start
// is ordered strictly after every older session's end.
func (a *Agent) Accept(d wire.Dispatch) wire.DispatchReply {
	return a.AcceptContext(context.Background(), d)
}

// AcceptContext is Accept with the dispatch's trace context, so the session
// it starts joins the scheduler's trace.
func (a *Agent) AcceptContext(parent context.Context, d wire.Dispatch) wire.DispatchReply {
	a.mu.Lock()
	defer a.mu.Unlock()

	reply := func(ok bool, reason string) wire.DispatchReply {
		return wire.DispatchReply{Accepted: ok, Reason: reason, HighFence: a.fence.High()}
	}
	k := key{d.ExperimentID, d.Attempt}
	if s, ok := a.sessions[k]; ok && s.fence == d.Fence {
		r := reply(true, "")
		r.Duplicate = true
		return r
	}
	if a.quarantined() {
		return reply(false, wire.RejectQuarantined)
	}

	if a.cfg.Fenced {
		high := a.fence.High()
		switch {
		case d.Fence < high:
			now := a.clock()
			a.intervals.Record(Interval{RigID: a.cfg.RigID, Kind: "rejected", Fence: d.Fence,
				ExperimentID: d.ExperimentID, Attempt: d.Attempt, StartNS: now, EndNS: now})
			a.log.Warn("rejected stale fence", "fence", d.Fence, "high", high, "experiment", d.ExperimentID)
			return reply(false, wire.RejectStaleFence)
		case d.Fence == high:
			// The current fence already belongs to a different attempt. That
			// cannot happen with a correct lease table, so refuse loudly rather
			// than guess which one is real.
			return reply(false, wire.RejectConflict)
		}
		if err := a.fence.Advance(d.Fence); err != nil {
			a.log.Error("could not persist fence", "err", err)
			return reply(false, "fence_store:"+err.Error())
		}
		for s := range a.active {
			s.preempt()
		}
		for s := range a.active {
			<-s.execDone
		}
	}

	if !a.cfg.Fenced {
		// The control still keeps the mark, without acting on it, so the
		// evidence can count how many stale dispatches reached the rig in both
		// modes rather than only in the one that refuses them.
		if high := a.fence.High(); d.Fence < high {
			now := a.clock()
			a.intervals.Record(Interval{RigID: a.cfg.RigID, Kind: "stale_accepted", Fence: d.Fence,
				ExperimentID: d.ExperimentID, Attempt: d.Attempt, StartNS: now, EndNS: now})
		} else if d.Fence > high {
			a.fence.Advance(d.Fence)
		}
	}

	s := a.newSession(parent, d)
	a.sessions[k] = s
	a.active[s] = true
	go a.run(s)
	return reply(true, "")
}

func (a *Agent) Status(experiment string, attempt int) (wire.RunStatus, bool) {
	a.mu.Lock()
	s, ok := a.sessions[key{experiment, attempt}]
	a.mu.Unlock()
	if !ok {
		return wire.RunStatus{}, false
	}
	return s.status(), true
}

func (a *Agent) Snapshot() wire.Heartbeat {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.smu.Lock()
	hb := wire.Heartbeat{
		Descriptor:  a.descriptor,
		AgentState:  a.state,
		AgentReason: a.reason,
		HighFence:   a.fence.High(),
		Active:      []wire.ActiveRun{},
	}
	a.smu.Unlock()
	for s := range a.active {
		st := s.status()
		hb.Active = append(hb.Active, wire.ActiveRun{
			ExperimentID: st.ExperimentID, Attempt: st.Attempt, Fence: st.Fence, Phase: st.Phase,
		})
	}
	return hb
}

// Descriptor is what the rig advertises. Sessions read it through here rather
// than through Snapshot, which takes the admission lock.
func (a *Agent) Descriptor() capability.Rig {
	a.smu.Lock()
	defer a.smu.Unlock()
	return a.descriptor
}

func (a *Agent) quarantined() bool {
	a.smu.Lock()
	defer a.smu.Unlock()
	return a.state == StateQuarantined
}

func (a *Agent) quarantine(reason string) {
	a.smu.Lock()
	defer a.smu.Unlock()
	if a.state != StateQuarantined {
		a.log.Error("quarantined", "reason", reason)
	}
	a.state, a.reason = StateQuarantined, reason
}

// Unquarantine is an operator action. A quarantined rig is never returned to
// service automatically, because whatever quarantined it has not been shown
// to be gone.
func (a *Agent) Unquarantine() {
	a.smu.Lock()
	defer a.smu.Unlock()
	a.state, a.reason, a.crashes = StateReady, "", 0
	a.descriptor = a.describe(context.Background())
}

func (a *Agent) finished(s *session) {
	a.mu.Lock()
	delete(a.active, s)
	a.mu.Unlock()
	a.smu.Lock()
	defer a.smu.Unlock()
	if s.statusReason() == "crash" {
		a.crashes++
		if a.crashes >= a.cfg.CrashLimit && a.state != StateQuarantined {
			a.state, a.reason = StateQuarantined, "repeated_crashes"
		}
	} else if s.finalStatus() != "" {
		a.crashes = 0
	}
}

// reapStale kills every process group whose session has already finished
// executing and yet is still alive. Groups belonging to sessions still running
// are left alone: if two sessions run at once, that is for the fence to
// prevent, and a preflight that quietly killed the other session's work would
// hide exactly the overlap the negative control exists to show. It returns an
// error if any stale group survives SIGKILL, which quarantines the rig: a
// process that cannot be killed will contaminate every measurement after it.
func (a *Agent) reapStale() error {
	a.smu.Lock()
	var stale []int
	for pg, owner := range a.pgroups {
		select {
		case <-owner.execDone:
			stale = append(stale, pg)
		default:
		}
	}
	a.smu.Unlock()
	for _, pg := range stale {
		killGroup(pg, 9)
	}
	deadline := time.Now().Add(2 * time.Second)
	for _, pg := range stale {
		for groupAlive(pg) {
			if time.Now().After(deadline) {
				return fmt.Errorf("process group %d survived SIGKILL", pg)
			}
			time.Sleep(20 * time.Millisecond)
		}
		a.untrackGroup(pg)
	}
	return nil
}

func (a *Agent) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.heartbeatLoop(ctx) }()
	go func() { defer wg.Done(); a.spoolLoop(ctx) }()
	wg.Wait()
	return a.intervals.Close()
}

var errNoControl = errors.New("no control plane configured")
