package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rajeev-chaurasia/benchgrid/internal/artifact"
	"github.com/rajeev-chaurasia/benchgrid/internal/probe"
	"github.com/rajeev-chaurasia/benchgrid/internal/spec"
)

// Hand written to the documented `perf stat -x,` format, not captured from a
// machine: neither macOS nor Docker's Linux VM exposes hardware counters, so
// these fixtures and a stand-in perf are the only things exercising this
// collector, which docs/known-misses.md says.
const perfOK = `# started on Tue Oct  7 10:00:00 2026

1032845,,cycles:u,1032845,100.00,,
2114632,,instructions:u,1032845,100.00,2.05,insn per cycle
1204,,cache-misses:u,1032845,100.00,,
3,,context-switches:u,1032845,100.00,0.003,K/sec
`

const perfUnsupported = `<not supported>,,cycles:u,0,100.00,,
2114632,,instructions:u,1032845,100.00,,
`

// fakePerf puts a perf on PATH that runs the command after -- and then writes
// the given fixture where -o asked, which is everything the agent relies on
// perf to do.
func fakePerf(t *testing.T, fixture string) {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "fixture.csv")
	os.WriteFile(data, []byte(fixture), 0o644)
	script := "#!/bin/sh\nout=\"\"\nwhile [ \"$1\" != \"--\" ]; do if [ \"$1\" = \"-o\" ]; then shift; out=\"$1\"; fi; shift; done\nshift\n\"$@\"\ncode=$?\ncat '" + data + "' > \"$out\"\nexit $code\n"
	os.WriteFile(filepath.Join(dir, "perf"), []byte(script), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func perfSpec() spec.Spec {
	sp := testSpec("-rounds", "10")
	sp.Warmups, sp.Repetitions = 0, 2
	sp.Collectors = []string{"perf"}
	sp.Metrics = append(sp.Metrics,
		spec.Metric{Name: "cycles", Unit: "count", Direction: "lower_is_better"},
		spec.Metric{Name: "ipc", Unit: "unitless", Direction: "higher_is_better"},
	)
	return sp
}

func TestPerfCountersBecomeMetrics(t *testing.T) {
	fakePerf(t, perfOK)
	a, _ := newAgent(t, true, &probe.Profile{Profilers: []string{"perf"}})
	a.Accept(dispatch("exp_perf", 1, 1, perfSpec()))
	if st := waitDone(t, a, "exp_perf", 1); st.Status != artifact.Succeeded {
		t.Fatalf("%+v", st)
	}
	run, _, err := artifact.Verify(artifact.AttemptDir(filepath.Join(a.cfg.StateDir, "runs"), "exp_perf", 1))
	if err != nil {
		t.Fatal(err)
	}
	if c := run.Summary["cycles"]; c.N != 2 || *c.Median != 1032845 {
		t.Errorf("cycles %+v", c)
	}
	if ipc := run.Summary["ipc"]; ipc.Median == nil || *ipc.Median < 2.04 || *ipc.Median > 2.05 {
		t.Errorf("ipc %+v", ipc)
	}
}

// Profiler failure: an event perf could not count must not turn into a run
// that silently has no cycles.
func TestPerfThatCannotCountInvalidatesTheRun(t *testing.T) {
	fakePerf(t, perfUnsupported)
	a, _ := newAgent(t, true, &probe.Profile{Profilers: []string{"perf"}})
	a.Accept(dispatch("exp_perf_bad", 1, 1, perfSpec()))
	if st := waitDone(t, a, "exp_perf_bad", 1); st.Status != artifact.Invalid || st.StatusReason != "collector:perf" {
		t.Errorf("%+v", st)
	}
	b, _ := os.ReadFile(filepath.Join(artifact.AttemptDir(filepath.Join(a.cfg.StateDir, "runs"), "exp_perf_bad", 1), artifact.DiagnosticsFile))
	if len(b) == 0 {
		t.Error("no diagnostics for a profiler failure")
	}
}

func TestPerfMetricWithoutCollectorIsRejected(t *testing.T) {
	sp := perfSpec()
	sp.Collectors = nil
	if sp.Validate() == nil {
		t.Error("a spec declared cycles without asking for perf")
	}
}
