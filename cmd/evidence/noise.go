package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/rajeev-chaurasia/benchgrid/internal/evidence"
	"github.com/rajeev-chaurasia/benchgrid/internal/spec"
)

const noiseRuns = 12

// Two factors, two levels each. quiet against loaded_ungated shows what the
// noise does; loaded_ungated against loaded_gated shows what the gate does
// about it; quiet against quiet_gated shows what the gate costs when there is
// nothing to catch, which is the false alarm rate that decides whether anyone
// would leave it switched on.
var noiseConditions = []struct {
	name   string
	stress bool
	gated  bool
}{
	{"quiet", false, false},
	{"quiet_gated", false, true},
	{"loaded_ungated", true, false},
	{"loaded_gated", true, true},
}

func (h *harness) noise(ctx context.Context) error {
	out := filepath.Join(h.out, "noise")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	c, err := h.newCluster(ctx, "noise", clusterConfig{replicas: 1, serverArgs: []string{"-lease-ttl", "10s"}, rigs: 1})
	if err != nil {
		return err
	}
	defer c.close()

	self, err := os.Executable()
	if err != nil {
		return err
	}
	runs := map[string][]string{}
	for ci, cond := range noiseConditions {
		var load *exec.Cmd
		if cond.stress {
			load = exec.Command(self, "stress", "-seed", fmt.Sprint(ci+1))
			if err := load.Start(); err != nil {
				return err
			}
		}
		for i := 0; i < noiseRuns; i++ {
			s := spec.Spec{
				Benchmark: "cpu_hash", Revision: fmt.Sprintf("%040x", 0xa0000+ci*100+i),
				Command: []string{spec.BinaryPlaceholder, "-rounds", "100000"},
				Warmups: 5, Repetitions: 30, TimeoutSeconds: 600,
				Metrics: []spec.Metric{{Name: "iteration_latency", Unit: "ns", Direction: "lower_is_better"}},
			}
			if cond.gated {
				limit := 0.3
				s.Environment = spec.Environment{MaxCPUUtil: &limit, GateEachIteration: true, PreflightTimeoutSeconds: 20}
			}
			id, err := c.submitOnce(s, fmt.Sprintf("noise-%s-%d", cond.name, i))
			if err != nil {
				return err
			}
			runs[cond.name] = append(runs[cond.name], id)
			// One at a time, so no run is measured next to another.
			if err := c.drain(ctx, 10*time.Minute); err != nil {
				return err
			}
		}
		if load != nil {
			load.Process.Kill()
			load.Wait()
		}
		time.Sleep(2 * time.Second)
	}
	if err := copyDir(filepath.Join(c.store, "runs"), filepath.Join(out, "store", "runs")); err != nil {
		return err
	}
	if err := evidence.WriteJSON(filepath.Join(out, "runs.json"), runs); err != nil {
		return err
	}
	s, err := evidence.SummarizeNoise(runs, filepath.Join(out, "store"))
	if err != nil {
		return err
	}
	return evidence.WriteJSON(filepath.Join(out, "summary.json"), s)
}
