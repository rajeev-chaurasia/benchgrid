// validate_evidence recomputes every published number from the raw files
// beside it and fails if any summary disagrees, if any file differs from the
// manifest, or if the claim the README makes does not hold in the data. It
// also fails if either negative control stopped failing, because a clean
// result from a harness that can no longer produce a dirty one is not
// evidence.
package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rajeev-chaurasia/benchgrid/internal/agent"
	"github.com/rajeev-chaurasia/benchgrid/internal/artifact"
	"github.com/rajeev-chaurasia/benchgrid/internal/evidence"
)

type check struct {
	dir      string
	failures []string
}

func (c *check) fail(f string, a ...any) {
	c.failures = append(c.failures, fmt.Sprintf(f, a...))
}

func (c *check) must(err error) bool {
	if err != nil {
		c.fail("%v", err)
		return false
	}
	return true
}

// same compares through JSON, which is the form a reader sees, so a field the
// summary carries but the recomputation does not is a disagreement too.
func (c *check) same(label string, published, recomputed any) {
	a, _ := json.Marshal(published)
	b, _ := json.Marshal(recomputed)
	if string(a) != string(b) {
		c.fail("%s: published\n  %s\nrecomputed\n  %s", label, a, b)
	}
}

func main() {
	root := "evidence/results"
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	sort.Strings(dirs)
	if len(dirs) == 0 {
		fmt.Fprintln(os.Stderr, "no results to validate")
		os.Exit(1)
	}
	bad := 0
	for _, d := range dirs {
		c := &check{dir: d}
		c.run()
		if len(c.failures) == 0 {
			fmt.Printf("ok    %s\n", d)
			continue
		}
		bad++
		fmt.Printf("FAIL  %s\n", d)
		for _, f := range c.failures {
			fmt.Printf("  %s\n", f)
		}
	}
	if bad > 0 {
		os.Exit(1)
	}
}

func (c *check) exists(rel string) bool {
	_, err := os.Stat(filepath.Join(c.dir, rel))
	return err == nil
}

func (c *check) run() {
	if !c.must(evidence.VerifyManifest(c.dir)) {
		return
	}
	if c.exists("lease_race") {
		c.leaseRace()
	}
	if c.exists("fence") {
		c.fence("fence")
	}
	if c.exists("linux") {
		c.fence("linux")
	}
	if c.exists("chaos") {
		c.chaos()
	}
	if c.exists("noise") {
		c.noise()
	}
	c.everyRunVerifies()
}

func (c *check) leaseRace() {
	var published []evidence.RaceSummary
	if !c.must(evidence.ReadJSON(filepath.Join(c.dir, "lease_race", "summary.json"), &published)) {
		return
	}
	for _, p := range published {
		grants, err := evidence.ReadJSONLGz[evidence.Grant](filepath.Join(c.dir, "lease_race", p.Mode+".grants.jsonl.gz"))
		if !c.must(err) {
			continue
		}
		ops, err := evidence.ReadJSONLGz[evidence.Op](filepath.Join(c.dir, "lease_race", p.Mode+".ops.jsonl.gz"))
		if !c.must(err) {
			continue
		}
		r := evidence.SummarizeRace(p.Mode, grants, ops)
		r.Rigs, r.Workers, r.Errors = p.Rigs, p.Workers, p.Errors
		c.same("lease_race "+p.Mode, p, r)
		if r.PeakInFlight < 2 {
			c.fail("lease_race %s: peak in flight %d, the race did not race", p.Mode, r.PeakInFlight)
		}
		switch p.Mode {
		case "conditional":
			if r.DoubleBookings != 0 {
				c.fail("CLAIM: conditional lease double booked %d times", r.DoubleBookings)
			}
		case "naive":
			if r.DoubleBookings == 0 {
				c.fail("CONTROL: naive lease produced no double booking, so zero above proves nothing")
			}
		}
	}
}

func (c *check) fence(run string) {
	var published []evidence.FenceSummary
	if !c.must(evidence.ReadJSON(filepath.Join(c.dir, run, "summary.json"), &published)) {
		return
	}
	for _, p := range published {
		dir := filepath.Join(c.dir, run, p.Mode)
		intervals, err := evidence.ReadJSONLGz[agent.Interval](filepath.Join(dir, "intervals.jsonl.gz"))
		if !c.must(err) {
			continue
		}
		freezes, err := evidence.ReadJSONLGz[evidence.Freeze](filepath.Join(dir, "freezes.jsonl.gz"))
		if !c.must(err) {
			continue
		}
		base := evidence.FenceSummary{Mode: p.Mode, Replicas: p.Replicas, Rigs: p.Rigs, Experiments: p.Experiments, States: p.States}
		r := evidence.SummarizeFence(base, intervals, freezes)
		c.same(run+" "+p.Mode, p, r)
		if p.States["QUEUED"]+p.States["RUNNING"] > 0 {
			c.fail("%s %s: experiments left open", run, p.Mode)
		}
		switch p.Mode {
		case "fenced":
			if r.ProcessOverlaps != 0 || r.SessionOverlaps != 0 {
				c.fail("CLAIM: %s fenced rigs ran overlapping work: %d process pairs, %d session pairs", run, r.ProcessOverlaps, r.SessionOverlaps)
			}
			if r.StaleRefused == 0 {
				c.fail("%s fenced: no stale fence ever reached a rig, so the freezes never landed", run)
			}
		case "unfenced":
			if r.ProcessOverlaps == 0 {
				c.fail("CONTROL: %s unfenced rigs never overlapped, so zero above proves nothing", run)
			}
		}
	}
}

func (c *check) chaos() {
	dir := filepath.Join(c.dir, "chaos")
	var published evidence.ChaosSummary
	var end evidence.ChaosEnd
	if !c.must(evidence.ReadJSON(filepath.Join(dir, "summary.json"), &published)) ||
		!c.must(evidence.ReadJSON(filepath.Join(dir, "end_state.json"), &end)) {
		return
	}
	exps, err1 := evidence.ReadJSONLGz[evidence.ChaosExperiment](filepath.Join(dir, "experiments.jsonl.gz"))
	faults, err2 := evidence.ReadJSONLGz[evidence.Fault](filepath.Join(dir, "faults.jsonl.gz"))
	intervals, err3 := evidence.ReadJSONLGz[agent.Interval](filepath.Join(dir, "intervals.jsonl.gz"))
	if !c.must(err1) || !c.must(err2) || !c.must(err3) {
		return
	}
	r, err := evidence.SummarizeChaos(exps, faults, intervals, end, filepath.Join(dir, "store"))
	if !c.must(err) {
		return
	}
	r.Replicas, r.Rigs = published.Replicas, published.Rigs
	c.same("chaos", published, r)
	if r.ProcessOverlaps != 0 || r.SessionOverlaps != 0 {
		c.fail("CLAIM: overlapping work under chaos: %d process pairs, %d session pairs", r.ProcessOverlaps, r.SessionOverlaps)
	}
	if len(r.ArtifactErrors) != 0 {
		c.fail("chaos: %d final attempts without a matching verified artifact", len(r.ArtifactErrors))
	}
	if r.PlacementViolations != 0 {
		c.fail("CLAIM: %d runs landed on a rig their spec did not allow", r.PlacementViolations)
	}
	if r.RigsLeasedAtEnd != 0 {
		c.fail("chaos: %d rigs still leased after every experiment finished", r.RigsLeasedAtEnd)
	}
}

func (c *check) noise() {
	dir := filepath.Join(c.dir, "noise")
	var published []evidence.NoiseCondition
	var runs map[string][]string
	if !c.must(evidence.ReadJSON(filepath.Join(dir, "summary.json"), &published)) ||
		!c.must(evidence.ReadJSON(filepath.Join(dir, "runs.json"), &runs)) {
		return
	}
	r, err := evidence.SummarizeNoise(runs, filepath.Join(dir, "store"))
	if !c.must(err) {
		return
	}
	c.same("noise", published, r)
	by := map[string]evidence.NoiseCondition{}
	for _, n := range r {
		by[n.Condition] = n
	}
	if by["loaded_ungated"].Invalid != 0 {
		c.fail("noise: an ungated run was declared invalid, so the gate is not the only difference")
	}
	if by["loaded_gated"].Invalid == 0 {
		c.fail("CLAIM: the gate refused no run measured under injected load")
	}
}

// everyRunVerifies applies the run contract to every sealed attempt published
// anywhere in the results, not only the ones a summary happened to read. It
// also checks two things no summary covers: every run under linux/ says it
// ran on Linux, and every max_rss sample is a plausible number of bytes. The
// second is how a ru_maxrss read as bytes on Linux, where it is kilobytes,
// would show: a benchmark using a few kilobytes of memory.
func (c *check) everyRunVerifies() {
	n, linux := 0, 0
	filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != artifact.ManifestFile {
			return err
		}
		n++
		dir := filepath.Dir(path)
		run, samples, err := artifact.Verify(dir)
		if err != nil {
			c.fail("run artifact: %v", err)
			return nil
		}
		rel, _ := filepath.Rel(c.dir, dir)
		if strings.HasPrefix(rel, "linux"+string(filepath.Separator)) {
			linux++
			if run.Rig.OS != "linux" {
				c.fail("%s: published as a Linux run but its rig says %q", rel, run.Rig.OS)
			}
		}
		for _, s := range samples {
			if s.Metric == "max_rss" && (s.Value < 1<<20 || s.Value > 1<<32) {
				c.fail("%s: max_rss %v bytes is not a plausible size for this benchmark", rel, s.Value)
				break
			}
		}
		return nil
	})
	if c.exists("chaos") && n == 0 {
		c.fail("no run artifacts found under a results directory that has runs")
	}
	if c.exists("linux") && linux == 0 {
		c.fail("a linux run published no run artifacts")
	}
}
