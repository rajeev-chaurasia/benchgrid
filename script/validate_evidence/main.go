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
		c.fence()
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

func (c *check) fence() {
	var published []evidence.FenceSummary
	if !c.must(evidence.ReadJSON(filepath.Join(c.dir, "fence", "summary.json"), &published)) {
		return
	}
	for _, p := range published {
		dir := filepath.Join(c.dir, "fence", p.Mode)
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
		c.same("fence "+p.Mode, p, r)
		if p.States["QUEUED"]+p.States["RUNNING"] > 0 {
			c.fail("fence %s: experiments left open", p.Mode)
		}
		switch p.Mode {
		case "fenced":
			if r.ProcessOverlaps != 0 || r.SessionOverlaps != 0 {
				c.fail("CLAIM: fenced rigs ran overlapping work: %d process pairs, %d session pairs", r.ProcessOverlaps, r.SessionOverlaps)
			}
			if r.StaleRefused == 0 {
				c.fail("fence fenced: no stale fence ever reached a rig, so the freezes never landed")
			}
		case "unfenced":
			if r.ProcessOverlaps == 0 {
				c.fail("CONTROL: unfenced rigs never overlapped, so zero above proves nothing")
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
}

// everyRunVerifies applies the run contract to every sealed attempt published
// anywhere in the results, not only the ones a summary happened to read.
func (c *check) everyRunVerifies() {
	n := 0
	filepath.WalkDir(c.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != artifact.ManifestFile {
			return err
		}
		n++
		if _, _, err := artifact.Verify(filepath.Dir(path)); err != nil {
			c.fail("run artifact: %v", err)
		}
		return nil
	})
	if c.exists("chaos") && n == 0 {
		c.fail("no run artifacts found under a results directory that has runs")
	}
}
