package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/rajeev-chaurasia/benchgrid/internal/agent"
	"github.com/rajeev-chaurasia/benchgrid/internal/evidence"
)

const (
	fenceReplicas    = 3
	fenceRigs        = 8
	fenceExperiments = 300
	fenceFreezeProb  = "0.1"
	fenceTTL         = 3 * time.Second
)

func (h *harness) fence(ctx context.Context) error {
	dir := filepath.Join(h.out, "fence")
	var summaries []evidence.FenceSummary
	for _, mode := range []string{"fenced", "unfenced"} {
		s, err := h.fenceMode(ctx, dir, mode, "")
		if err != nil {
			return fmt.Errorf("%s: %w", mode, err)
		}
		summaries = append(summaries, s)
	}
	return evidence.WriteJSON(filepath.Join(dir, "summary.json"), summaries)
}

// fenceMode runs one mode of the fence experiment. With a linux image, every
// agent is a Linux container and every experiment requires a Linux rig.
func (h *harness) fenceMode(ctx context.Context, dir, mode, linuxImage string) (evidence.FenceSummary, error) {
	out := filepath.Join(dir, mode)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return evidence.FenceSummary{}, err
	}
	name, rigs := "fence_"+mode, fenceRigs
	if linuxImage != "" {
		name, rigs = "linux_"+mode, linuxRigs
	}
	c, err := h.newCluster(ctx, name, clusterConfig{
		linuxImage: linuxImage,
		replicas:   fenceReplicas,
		serverArgs: []string{"-lease-ttl", fenceTTL.String(), "-tick", "100ms",
			"-fault-freeze-before-dispatch", fenceFreezeProb},
		rigs: rigs,
		agentArgs: func(int) []string {
			if mode == "unfenced" {
				return []string{"-unfenced-negative-control"}
			}
			return nil
		},
	})
	if err != nil {
		return evidence.FenceSummary{}, err
	}
	defer c.close()

	epoch := time.Now()
	stop := make(chan struct{})
	var freezes []evidence.Freeze
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		freezes = c.thawFrozen(stop, epoch, func() time.Duration {
			// Long enough for the lease to lapse, the reaper to requeue, and
			// another replica to lease the rig again, so that the frozen
			// scheduler wakes up holding a fence the rig has moved past.
			return fenceTTL + time.Second + time.Duration(rand.IntN(2000))*time.Millisecond
		})
	}()

	var partitions []evidence.Fault
	if linuxImage != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			partitions = c.partition(stop, epoch)
		}()
	}

	for i := 0; i < fenceExperiments; i++ {
		s := benchSpec(fmt.Sprintf("%040x", i), "-rounds", "2000", "-sleep", "200ms")
		s.Requirements.AllowEmulated = true
		if linuxImage != "" {
			s.Requirements.OS = "linux"
		}
		if _, err := c.submit(s, fmt.Sprintf("%s-%d", name, i)); err != nil {
			close(stop)
			return evidence.FenceSummary{}, err
		}
	}
	drainErr := c.drain(ctx, 15*time.Minute)
	close(stop)
	wg.Wait()

	base := evidence.FenceSummary{Mode: mode, Replicas: fenceReplicas, Rigs: rigs, States: map[string]int{}}
	rows, err := c.db.Query(ctx, `SELECT state, count(*) FROM experiments GROUP BY state`)
	if err != nil {
		return base, err
	}
	for rows.Next() {
		var st string
		var n int
		rows.Scan(&st, &n)
		base.States[st] = n
		base.Experiments += n
	}
	rows.Close()
	if drainErr != nil {
		return base, drainErr
	}

	intervals, err := c.intervals()
	if err != nil {
		return base, err
	}
	if err := evidence.WriteJSONLGz(filepath.Join(out, "intervals.jsonl.gz"), intervals); err != nil {
		return base, err
	}
	if err := evidence.WriteJSONLGz(filepath.Join(out, "freezes.jsonl.gz"), freezes); err != nil {
		return base, err
	}
	if linuxImage != "" {
		if err := evidence.WriteJSONLGz(filepath.Join(out, "partitions.jsonl.gz"), partitions); err != nil {
			return base, err
		}
	}
	if linuxImage != "" {
		// The Linux runs publish their run artifacts too, so the validator can
		// check from the runs themselves that they ran on Linux and that the
		// memory figures are in bytes.
		if err := copyDir(filepath.Join(c.store, "runs"), filepath.Join(out, "store", "runs")); err != nil {
			return base, err
		}
	}
	s := evidence.SummarizeFence(base, intervals, freezes, partitions)
	return s, evidence.WriteJSON(filepath.Join(out, "summary.json"), s)
}

// thawFrozen watches the replicas for a SIGSTOP they sent themselves, records
// it, and resumes each one after hold(). It returns every freeze once stop
// closes, after resuming anything still frozen.
func (c *cluster) thawFrozen(stop <-chan struct{}, epoch time.Time, hold func() time.Duration) []evidence.Freeze {
	var out []evidence.Freeze
	frozenAt := map[*proc]time.Time{}
	until := map[*proc]time.Time{}
	t := time.NewTicker(25 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			for p, at := range frozenAt {
				p.signal(syscall.SIGCONT)
				out = append(out, evidence.Freeze{Server: p.name, Kind: "self", DetectedNS: at.Sub(epoch).Nanoseconds(), ResumedNS: time.Since(epoch).Nanoseconds()})
			}
			sort.Slice(out, func(i, j int) bool { return out[i].DetectedNS < out[j].DetectedNS })
			return out
		case <-t.C:
		}
		for _, p := range c.servers {
			if at, ok := frozenAt[p]; ok {
				if time.Now().After(until[p]) {
					p.signal(syscall.SIGCONT)
					out = append(out, evidence.Freeze{Server: p.name, Kind: "self", DetectedNS: at.Sub(epoch).Nanoseconds(), ResumedNS: time.Since(epoch).Nanoseconds()})
					delete(frozenAt, p)
				}
				continue
			}
			if p.stopped() {
				frozenAt[p] = time.Now()
				until[p] = time.Now().Add(hold())
			}
		}
	}
}

func (c *cluster) intervals() ([]agent.Interval, error) {
	var all []agent.Interval
	for _, a := range c.agents {
		rows, err := evidence.ReadJSONL[agent.Interval](filepath.Join(c.dir, a.name+".intervals.jsonl"))
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
	}
	return all, nil
}

// partition cuts one rig container at a time off the network for longer than
// its lease, then reconnects it, until stop closes. The agent keeps running
// through it: its heartbeats fail, its lease lapses, its attempt is requeued
// elsewhere, and when the network returns it reports a result the control
// plane has moved past. That is the failure a partition produces and a crash
// does not.
func (c *cluster) partition(stop <-chan struct{}, epoch time.Time) []evidence.Fault {
	rng := rand.New(rand.NewPCG(11, 11))
	var out []evidence.Fault
	for {
		select {
		case <-stop:
			return out
		case <-time.After(time.Duration(2000+rng.IntN(3000)) * time.Millisecond):
		}
		a := c.agents[rng.IntN(len(c.agents))]
		f := evidence.Fault{Kind: "partition", Target: a.name, AtNS: time.Since(epoch).Nanoseconds()}
		if exec.Command("docker", "network", "disconnect", "bridge", a.container).Run() != nil {
			continue
		}
		time.Sleep(fenceTTL + time.Duration(1000+rng.IntN(4000))*time.Millisecond)
		exec.Command("docker", "network", "connect", "bridge", a.container).Run()
		f.UntilNS = time.Since(epoch).Nanoseconds()
		out = append(out, f)
	}
}
