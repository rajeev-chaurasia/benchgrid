package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/rajeev-chaurasia/benchgrid/internal/evidence"
	"github.com/rajeev-chaurasia/benchgrid/internal/spec"
)

const (
	chaosReplicas    = 3
	chaosRigs        = 8
	chaosExperiments = 300
	chaosFaultWindow = 4 * time.Minute
	chaosTTL         = 3 * time.Second
)

func (h *harness) chaos(ctx context.Context) error {
	out := filepath.Join(h.out, "chaos")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	c, err := h.newCluster(ctx, "chaos", clusterConfig{
		replicas:   chaosReplicas,
		serverArgs: []string{"-lease-ttl", chaosTTL.String(), "-tick", "100ms", "-fault-artifact-error-rate", "0.2"},
		rigs:       chaosRigs,
	})
	if err != nil {
		return err
	}
	defer c.close()

	// Most experiments should succeed whatever is done to the system around
	// them. A few are built to fail, so the run also shows that recovery does
	// not turn a real failure into a success by retrying it until it passes.
	kinds := make([]string, chaosExperiments)
	ids := make([]string, chaosExperiments)
	epoch := time.Now()
	stop := make(chan struct{})
	var faults []evidence.Fault
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		faults = c.injectFaults(stop, epoch)
	}()

	for i := 0; i < chaosExperiments; i++ {
		args := []string{"-rounds", "2000", "-sleep", "300ms"}
		kinds[i] = "normal"
		switch i % 25 {
		case 0:
			kinds[i] = "crash"
			args = append(args, "-crash")
		case 1:
			kinds[i] = "exit"
			args = append(args, "-exit", "3")
		}
		s := benchSpec(fmt.Sprintf("%040x", 0xc0000+i), args...)
		s.Requirements.AllowEmulated = true
		id, err := c.submitRetrying(s, fmt.Sprintf("chaos-%d", i))
		if err != nil {
			close(stop)
			wg.Wait()
			return err
		}
		ids[i] = id
	}

	faultDeadline := time.After(chaosFaultWindow)
	for open := true; open; {
		select {
		case <-faultDeadline:
			open = false
		case <-time.After(time.Second):
			var n int
			c.db.QueryRow(ctx, `SELECT count(*) FROM experiments WHERE state IN ('QUEUED', 'RUNNING')`).Scan(&n)
			open = n > 0
		}
	}
	close(stop)
	wg.Wait()
	if err := c.drain(ctx, 10*time.Minute); err != nil {
		return err
	}
	// Let agents flush their spools for attempts that finished after the
	// experiment was already closed, so the store holds everything.
	time.Sleep(5 * time.Second)

	var exps []evidence.ChaosExperiment
	for i, id := range ids {
		e := evidence.ChaosExperiment{ID: id, Kind: kinds[i], Expected: evidence.ExpectedFor(kinds[i])}
		if err := c.db.QueryRow(ctx, `SELECT state, status_reason, attempt FROM experiments WHERE id = $1`, id).Scan(&e.State, &e.Reason, &e.Attempts); err != nil {
			return err
		}
		exps = append(exps, e)
	}
	var end evidence.ChaosEnd
	c.db.QueryRow(ctx, `SELECT count(*) FROM rigs WHERE holder IS NOT NULL`).Scan(&end.RigsStillLeased)
	rows, err := c.db.Query(ctx, `SELECT id, agent_reason FROM rigs WHERE agent_state = 'QUARANTINED' ORDER BY id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var q evidence.Quarantine
		rows.Scan(&q.Rig, &q.Reason)
		end.Quarantined = append(end.Quarantined, q)
	}
	rows.Close()

	intervals, err := c.intervals()
	if err != nil {
		return err
	}
	if err := copyDir(filepath.Join(c.store, "runs"), filepath.Join(out, "store", "runs")); err != nil {
		return err
	}
	if err := evidence.WriteJSONLGz(filepath.Join(out, "experiments.jsonl.gz"), exps); err != nil {
		return err
	}
	if err := evidence.WriteJSONLGz(filepath.Join(out, "faults.jsonl.gz"), faults); err != nil {
		return err
	}
	if err := evidence.WriteJSONLGz(filepath.Join(out, "intervals.jsonl.gz"), intervals); err != nil {
		return err
	}
	if err := evidence.WriteJSON(filepath.Join(out, "end_state.json"), end); err != nil {
		return err
	}
	s, err := evidence.SummarizeChaos(exps, faults, intervals, end, filepath.Join(out, "store"))
	if err != nil {
		return err
	}
	s.Replicas, s.Rigs = chaosReplicas, chaosRigs
	return evidence.WriteJSON(filepath.Join(out, "summary.json"), s)
}

// submitRetrying keeps trying through replica faults, which are already
// being injected while experiments are submitted.
func (c *cluster) submitRetrying(s spec.Spec, key string) (string, error) {
	var lastErr error
	for i := 0; i < 40; i++ {
		id, err := c.submit(s, key)
		if err == nil {
			return id, nil
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	return "", lastErr
}

// injectFaults applies one fault every one to three seconds until stop closes,
// then restores every process. Each fault is one a real deployment has: a rig
// host losing its agent, a control plane replica crashing or stalling, and the
// whole control plane disappearing while rigs keep working.
func (c *cluster) injectFaults(stop <-chan struct{}, epoch time.Time) []evidence.Fault {
	rng := rand.New(rand.NewPCG(7, 7))
	var mu sync.Mutex
	var faults []evidence.Fault
	var pending sync.WaitGroup
	record := func(f evidence.Fault) {
		mu.Lock()
		faults = append(faults, f)
		mu.Unlock()
	}
	now := func() int64 { return time.Since(epoch).Nanoseconds() }
	later := func(d time.Duration, fn func()) {
		pending.Add(1)
		go func() { defer pending.Done(); time.Sleep(d); fn() }()
	}
	busy := map[*proc]bool{}
	var busyMu sync.Mutex
	claim := func(p *proc) bool {
		busyMu.Lock()
		defer busyMu.Unlock()
		if busy[p] {
			return false
		}
		busy[p] = true
		return true
	}
	free := func(ps ...*proc) {
		busyMu.Lock()
		defer busyMu.Unlock()
		for _, p := range ps {
			delete(busy, p)
		}
	}

	for {
		select {
		case <-stop:
			pending.Wait()
			return faults
		case <-time.After(time.Duration(1000+rng.IntN(2000)) * time.Millisecond):
		}
		switch k := rng.IntN(10); {
		case k < 4:
			a := c.agents[rng.IntN(len(c.agents))]
			if !claim(a) {
				continue
			}
			d := time.Duration(1000+rng.IntN(2000)) * time.Millisecond
			f := evidence.Fault{Kind: "agent_kill", Target: a.name, AtNS: now()}
			a.kill()
			later(d, func() { a.start(); f.UntilNS = now(); record(f); free(a) })
		case k < 6:
			s := c.servers[rng.IntN(len(c.servers))]
			if !claim(s) {
				continue
			}
			d := time.Duration(1000+rng.IntN(1000)) * time.Millisecond
			f := evidence.Fault{Kind: "replica_kill", Target: s.name, AtNS: now()}
			s.kill()
			later(d, func() { s.start(); f.UntilNS = now(); record(f); free(s) })
		case k < 9:
			s := c.servers[rng.IntN(len(c.servers))]
			if !claim(s) {
				continue
			}
			d := time.Duration(2000+rng.IntN(3000)) * time.Millisecond
			f := evidence.Fault{Kind: "replica_freeze", Target: s.name, AtNS: now()}
			s.signal(syscall.SIGSTOP)
			later(d, func() { s.signal(syscall.SIGCONT); f.UntilNS = now(); record(f); free(s) })
		default:
			for _, s := range c.servers {
				if !claim(s) {
					free(c.servers...)
					goto next
				}
			}
			{
				d := time.Duration(4000+rng.IntN(4000)) * time.Millisecond
				f := evidence.Fault{Kind: "control_plane_outage", Target: "all", AtNS: now()}
				for _, s := range c.servers {
					s.signal(syscall.SIGSTOP)
				}
				later(d, func() {
					for _, s := range c.servers {
						s.signal(syscall.SIGCONT)
					}
					f.UntilNS = now()
					record(f)
					free(c.servers...)
				})
			}
		next:
		}
	}
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return exec.Command("cp", "-R", src, dst).Run()
}
