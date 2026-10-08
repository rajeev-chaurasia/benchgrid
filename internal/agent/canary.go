package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/rajeev-chaurasia/benchgrid/internal/wire"
)

// The canary is a fixed calibration workload the agent runs on its own bench
// CPUs while the rig is idle. Every rig runs the same work, so its spread is
// comparable across rigs in a way no real benchmark's is: a rig whose canary
// varies much more than its peers' is noisy, whatever the cause, and on cloud
// VMs the cause is often the physical host, which nothing inside the VM can
// fix. The isolation study found one such VM four times noisier than the
// others of the same type.

const canaryIterations = 30

// CanaryMain is `rigagent canary`: it runs the calibration and prints one
// JSON result line. It runs as a separate process so it can be placed through
// the pin shim exactly where a benchmark would be.
func CanaryMain() error {
	block := make([]byte, 4096)
	var sum [32]byte
	ns := make([]float64, canaryIterations)
	for i := range ns {
		start := time.Now()
		for k := 0; k < 2000; k++ {
			sum = sha256.Sum256(block)
			block[0] = sum[0]
		}
		ns[i] = float64(time.Since(start).Nanoseconds())
	}
	mean, sd := meanSD(ns[5:])
	sort.Float64s(ns)
	r := wire.Canary{CV: sd / mean, MedianNS: ns[len(ns)/2], Iterations: canaryIterations - 5}
	return json.NewEncoder(os.Stdout).Encode(r)
}

func meanSD(v []float64) (float64, float64) {
	var s float64
	for _, x := range v {
		s += x
	}
	m := s / float64(len(v))
	var ss float64
	for _, x := range v {
		ss += (x - m) * (x - m)
	}
	return m, math.Sqrt(ss / float64(len(v)-1))
}

// runCanary holds the admission lock for the whole run, so no benchmark can
// be accepted onto the rig while the canary measures it, and skips if one is
// already running. A dispatch that arrives meanwhile waits a second or two,
// which is well inside the scheduler's request timeout.
func (a *Agent) runCanary(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.active) > 0 || a.quarantined() {
		return
	}
	argv := []string{a.bin, "canary"}
	if a.self != "" {
		pin := []string{a.self, "pin", "--cpus", FormatCPUs(a.cfg.BenchCPUs)}
		if a.benchCgroup != "" {
			pin = append(pin, "--cgroup", a.benchCgroup)
		}
		argv = append(append(pin, "--"), a.bin, "canary")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).Output()
	if err != nil {
		a.log.Warn("canary failed", "err", err)
		return
	}
	var c wire.Canary
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &c); err != nil {
		a.log.Warn("canary output unreadable", "err", err)
		return
	}
	c.At = time.Now().UTC().Format(Timestamp)
	a.smu.Lock()
	a.canary = &c
	a.smu.Unlock()
	a.log.Info("canary", "cv", c.CV, "median_ns", c.MedianNS)
}

func (a *Agent) canaryLoop(ctx context.Context) {
	if a.cfg.CanaryEvery <= 0 {
		return
	}
	// The first canary runs right away, so a rig is measured before it has
	// had the chance to be given any work.
	a.runCanary(ctx)
	t := time.NewTicker(a.cfg.CanaryEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.runCanary(ctx)
		}
	}
}
