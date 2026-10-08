// readme_numbers renders every evidence-derived number in the README from the
// published results, between markers of the form
//
//	<!-- evidence:NAME -->
//	...
//	<!-- /evidence:NAME -->
//
// With -check it changes nothing and fails if the README differs from what it
// would write, which is how CI keeps the prose honest: a number in the README
// that the evidence does not say cannot survive a push. Text outside the
// markers carries no measured number.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/rajeev-chaurasia/benchgrid/internal/evidence"
)

type env struct {
	GitCommit   string  `json:"git_commit"`
	CPU         string  `json:"cpu"`
	Postgres    string  `json:"postgres"`
	IdleCPUBusy float64 `json:"idle_cpu_busy_fraction"`
}

func main() {
	check := flag.Bool("check", false, "fail if README.md is out of date instead of rewriting it")
	readme := flag.String("readme", "README.md", "file to render into")
	flag.Parse()
	dir, err := latest("evidence/results", false)
	if err != nil {
		die(err)
	}
	blocks, err := render(dir)
	if err != nil {
		die(err)
	}
	// Results from the GCP fleet live in their own directory, named with a
	// -gcp suffix, and add their own blocks when there are any.
	if gdirs := allDirs("evidence/results", true); len(gdirs) > 0 {
		g, err := renderGCP(gdirs)
		if err != nil {
			die(err)
		}
		for k, v := range g {
			blocks[k] = v
		}
	}
	old, err := os.ReadFile(*readme)
	if err != nil {
		die(err)
	}
	updated, err := splice(old, blocks)
	if err != nil {
		die(err)
	}
	if *check {
		if !bytes.Equal(old, updated) {
			die(fmt.Errorf("%s does not match %s; run go run ./script/readme_numbers", *readme, dir))
		}
		fmt.Println("readme numbers ok")
		return
	}
	if err := os.WriteFile(*readme, updated, 0o644); err != nil {
		die(err)
	}
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "readme_numbers:", err)
	os.Exit(1)
}

func allDirs(root string, gcp bool) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), "-gcp") == gcp {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	sort.Strings(dirs)
	return dirs
}

func latest(root string, gcp bool) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), "-gcp") == gcp {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 0 {
		return "", fmt.Errorf("no results under %s", root)
	}
	sort.Strings(dirs)
	return filepath.Join(root, dirs[len(dirs)-1]), nil
}

var marker = regexp.MustCompile(`(?s)(<!-- evidence:([a-z_]+) -->\n).*?(<!-- /evidence:([a-z_]+) -->)`)

// splice replaces the body of every marked block, and fails on a block the
// renderer does not know or a known block the README lacks, so a renamed
// marker cannot silently stop being checked.
func splice(doc []byte, blocks map[string]string) ([]byte, error) {
	seen := map[string]bool{}
	var bad error
	out := marker.ReplaceAllFunc(doc, func(m []byte) []byte {
		sub := marker.FindSubmatch(m)
		name := string(sub[2])
		if name != string(sub[4]) {
			bad = fmt.Errorf("marker %s closed as %s", name, sub[4])
			return m
		}
		body, ok := blocks[name]
		if !ok {
			bad = fmt.Errorf("README has a block %q the renderer does not produce", name)
			return m
		}
		seen[name] = true
		return []byte(string(sub[1]) + body + string(sub[3]))
	})
	if bad != nil {
		return nil, bad
	}
	for name := range blocks {
		if !seen[name] {
			return nil, fmt.Errorf("README is missing the %q block", name)
		}
	}
	return out, nil
}

func pct(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", *v*100)
}

func ms(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f ms", *v/1e6)
}

func comma(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func render(dir string) (map[string]string, error) {
	var e env
	var race []evidence.RaceSummary
	var fence, linux []evidence.FenceSummary
	var chaos evidence.ChaosSummary
	var noise []evidence.NoiseCondition
	for _, f := range []struct {
		path string
		v    any
	}{
		{"env.json", &e}, {"lease_race/summary.json", &race}, {"fence/summary.json", &fence},
		{"linux/summary.json", &linux}, {"chaos/summary.json", &chaos}, {"noise/summary.json", &noise},
	} {
		if err := evidence.ReadJSON(filepath.Join(dir, f.path), f.v); err != nil {
			return nil, err
		}
	}
	b := map[string]string{}

	b["source"] = fmt.Sprintf("From `%s/`, at commit `%s`, on Postgres %s and an\n%s whose own "+
		"background load kept %.0f%% of its CPU busy before any run started.\n",
		filepath.ToSlash(dir), e.GitCommit[:7], e.Postgres, e.CPU, e.IdleCPUBusy*100)

	var r strings.Builder
	if len(race) > 0 {
		fmt.Fprintf(&r, "%s workers, %d rigs, %s acquisition attempts per mode, TTLs of 5 to 20 ms,\none grant in ten left to expire.\n\n",
			comma(race[0].Workers), race[0].Rigs, comma(race[0].Attempts))
	}
	r.WriteString("| acquire | grants | double bookings | peak attempts in flight |\n| --- | ---: | ---: | ---: |\n")
	for _, x := range race {
		label := map[string]string{"conditional": "one conditional `UPDATE` (the product)", "naive": "read, then unconditional write (control)"}[x.Mode]
		db := comma(x.DoubleBookings)
		if x.Mode == "conditional" {
			db = "**" + db + "**"
		}
		fmt.Fprintf(&r, "| %s | %s | %s | %d |\n", label, comma(x.Grants), db, x.PeakInFlight)
	}
	b["lease"] = r.String()

	var f strings.Builder
	f.WriteString("| rigs | agent | experiments | freezes | partitions | stale dispatches that reached a rig | refused | overlapping process pairs | overlapping session pairs |\n")
	f.WriteString("| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, set := range []struct {
		label string
		rows  []evidence.FenceSummary
	}{{"host processes", fence}, {"Linux containers", linux}} {
		for _, x := range set.rows {
			agent := map[string]string{"fenced": "checks the fence", "unfenced": "does not (control)"}[x.Mode]
			po, so := fmt.Sprint(x.ProcessOverlaps), fmt.Sprint(x.SessionOverlaps)
			if x.Mode == "fenced" {
				po, so = "**"+po+"**", "**"+so+"**"
			}
			parts := "n/a"
			if set.label == "Linux containers" {
				parts = fmt.Sprint(x.Partitions)
			}
			fmt.Fprintf(&f, "| %d %s | %s | %d | %d | %s | %d | %d | %s | %s |\n",
				x.Rigs, set.label, agent, x.Experiments, x.Freezes, parts, x.StaleArrivals, x.StaleRefused, po, so)
		}
	}
	b["fence"] = f.String()

	var c strings.Builder
	fmt.Fprintf(&c, "%d replicas, %d rigs in four emulated hardware classes, %d experiments,\n"+
		"%d of them built to fail. During the run: %d agents killed and restarted, %d\n"+
		"replicas killed and restarted, %d replicas frozen, %d outages of the whole\n"+
		"control plane at once, and one artifact write in five refused. ",
		chaos.Replicas, chaos.Rigs, chaos.Experiments, chaos.ByKind["crash"]+chaos.ByKind["exit"],
		chaos.Faults["agent_kill"], chaos.Faults["replica_kill"], chaos.Faults["replica_freeze"],
		chaos.Faults["control_plane_outage"])
	if chaos.StaleRefused == 0 {
		c.WriteString("Those random\nfaults, with nothing aimed, produced no stale dispatch at all, which is why\nthe fence run has to aim its freezes to test the fence.\n\n")
	} else {
		fmt.Fprintf(&c, "Those random\nfaults, with nothing aimed, produced %d stale dispatch%s, every one refused\nat the rig.\n\n", chaos.StaleRefused, map[bool]string{true: "", false: "es"}[chaos.StaleRefused == 1])
	}
	c.WriteString("| | |\n| --- | ---: |\n")
	fmt.Fprintf(&c, "| experiments ending as they should (sound ones succeed, broken ones fail) | %d of %d |\n", chaos.AsExpected, chaos.Experiments)
	fmt.Fprintf(&c, "| final attempts with a sealed artifact that verifies and agrees with the control plane | %d of %d |\n", chaos.ArtifactsSealed, chaos.Experiments)
	fmt.Fprintf(&c, "| runs placed on a rig their spec did not allow | %d |\n", chaos.PlacementViolations)
	fmt.Fprintf(&c, "| overlapping process pairs | %d |\n", chaos.ProcessOverlaps)
	fmt.Fprintf(&c, "| rigs still leased afterwards | %d |\n", chaos.RigsLeasedAtEnd)
	var retried []string
	var counts []string
	for k := range chaos.AttemptsHist {
		if k != "1" {
			counts = append(counts, k)
		}
	}
	sort.Slice(counts, func(i, j int) bool {
		return len(counts[i]) < len(counts[j]) || (len(counts[i]) == len(counts[j]) && counts[i] < counts[j])
	})
	for _, k := range counts {
		retried = append(retried, fmt.Sprintf("%d with %s", chaos.AttemptsHist[k], k))
	}
	if len(retried) == 0 {
		retried = []string{"none"}
	}
	fmt.Fprintf(&c, "| experiments needing more than one attempt | %s |\n", strings.Join(retried, ", "))
	b["chaos"] = c.String()

	by := map[string]evidence.NoiseCondition{}
	for _, n := range noise {
		by[n.Condition] = n
	}
	lg, lu, q, qg := by["loaded_gated"], by["loaded_ungated"], by["quiet"], by["quiet_gated"]
	b["noise_claim"] = fmt.Sprintf("> With bursty load injected on its host, the measurement gate declined to\n"+
		"> publish %d of %d runs that an ungated agent published with a median\n"+
		"> coefficient of variation of %s, against %s with no injected load.\n",
		lg.Invalid, lg.Runs, pct(lu.MedianCV), pct(q.MedianCV))

	var n strings.Builder
	if lg.Succeeded == 0 {
		n.WriteString("No loaded run got through the gate. ")
	} else {
		fmt.Fprintf(&n, "%d loaded run%s got through the gate, with a median CV of %s. ", lg.Succeeded, plural(lg.Succeeded), pct(lg.MedianCV))
	}
	fmt.Fprintf(&n, "The gate also declined %d of\n%d runs with no injected load, because the host's own background load,\n"+
		"%.0f%% of its CPU before the runs began, crossed the limit during them. A\n"+
		"gate that refuses that often on an idle machine is not one anybody would\nleave switched on here.\n\n",
		qg.Invalid, qg.Runs, e.IdleCPUBusy*100)
	n.WriteString("| condition | published | declined | median CV of published | median latency |\n| --- | ---: | ---: | ---: | ---: |\n")
	for _, row := range []struct {
		label string
		c     evidence.NoiseCondition
	}{{"no load", q}, {"no load, gated", qg}, {"bursty load", lu}, {"bursty load, gated", lg}} {
		fmt.Fprintf(&n, "| %s | %d | %d | %s | %s |\n", row.label, row.c.Succeeded, row.c.Invalid, pct(row.c.MedianCV), ms(row.c.MedianOfMedians))
	}
	b["noise"] = n.String()
	return b, nil
}

type canarySummary struct {
	Limit      float64                   `json:"limit"`
	Placements map[string]map[string]int `json:"placements"`
	States     map[string]map[string]int `json:"states"`
	Canary     map[string]struct {
		Observations int     `json:"observations"`
		Min          float64 `json:"min"`
		Median       float64 `json:"median"`
		Max          float64 `json:"max"`
	} `json:"canary"`
}

type gpuSummary struct {
	GPU           string    `json:"gpu"`
	PreflightTemp []float64 `json:"preflight_temp_c"`
	PreflightUtil []float64 `json:"preflight_gpu_util"`
	Sizes         map[string]struct {
		Runs         int      `json:"runs"`
		NotSucceeded int      `json:"not_succeeded"`
		MedianMS     *float64 `json:"median_ms_per_pass"`
		MedianFPS    *float64 `json:"median_frames_per_s"`
		MedianCV     *float64 `json:"median_cv"`
	} `json:"sizes"`
	Gates []struct {
		Label   string   `json:"label"`
		Verdict string   `json:"verdict"`
		Ratio   *float64 `json:"ratio"`
		Low     *float64 `json:"low"`
		High    *float64 `json:"high"`
		Pairs   int      `json:"pairs"`
	} `json:"gates"`
}

// rangeOf renders a [min, max] pair, scaled, or says it was not read.
func rangeOf(v []float64, format string, scale ...float64) string {
	if len(v) != 2 {
		return "not read"
	}
	k := 1.0
	if len(scale) > 0 {
		k = scale[0]
	}
	lo, hi := fmt.Sprintf(format, v[0]*k), fmt.Sprintf(format, v[1]*k)
	if lo == hi {
		return lo
	}
	return lo + " to " + hi
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

type gateSummary struct {
	Comparisons         int                       `json:"comparisons"`
	NullComparisons     int                       `json:"null_comparisons"`
	FalseAlarms         int                       `json:"false_alarms"`
	InjectedComparisons int                       `json:"injected_comparisons"`
	InjectedCaught      int                       `json:"injected_caught"`
	BySize              map[string]map[string]int `json:"by_size"`
	Runs                int                       `json:"runs"`
	Reruns              int                       `json:"reruns_for_noise"`
	PairsOnOneRig       int                       `json:"pairs_on_one_rig"`
	Pairs               int                       `json:"pairs"`
}

type isoCell struct {
	Runs         int      `json:"runs"`
	NotSucceeded int      `json:"not_succeeded"`
	Profiles     int      `json:"profiles"`
	MedianCV     *float64 `json:"median_cv"`
}

type isoSummary struct {
	Metric string             `json:"metric"`
	Cells  map[string]isoCell `json:"cells"`
	PerRig map[string]struct {
		Class string `json:"class"`
		Quiet struct {
			Runs     int      `json:"runs"`
			MedianCV *float64 `json:"median_cv"`
			P90CV    *float64 `json:"p90_cv"`
		} `json:"quiet"`
		Noise struct {
			Runs     int      `json:"runs"`
			MedianCV *float64 `json:"median_cv"`
			P90CV    *float64 `json:"p90_cv"`
		} `json:"noise"`
	} `json:"per_rig"`
}

type scaleSummary struct {
	Jobs          int            `json:"jobs"`
	States        map[string]int `json:"states"`
	SuccessRate   *float64       `json:"success_rate"`
	WallSeconds   *float64       `json:"wall_seconds"`
	JobsPerMinute *float64       `json:"jobs_per_minute"`
	QueueWait     struct {
		P50 *float64 `json:"p50"`
		P95 *float64 `json:"p95"`
		Max *float64 `json:"max"`
	} `json:"queue_wait_seconds"`
	LeaseToFinish struct {
		P50 *float64 `json:"p50"`
		P95 *float64 `json:"p95"`
	} `json:"lease_to_finish_seconds"`
}

type gcpEnv struct {
	GitCommit    string   `json:"git_commit"`
	Zone         string   `json:"zone"`
	RigMachine   string   `json:"rig_machine"`
	CPUModel     string   `json:"cpu_model"`
	Kernel       string   `json:"kernel"`
	TunedRigs    int      `json:"tuned_rigs"`
	DefaultRigs  int      `json:"default_rigs"`
	GPU          string   `json:"gpu"`
	ControlPlane string   `json:"control_plane"`
	Notes        []string `json:"notes"`
	// Fleet describes the bench nodes in words when the tuned and default
	// counts do not, as for a fleet of tuned and stock nodes in two regions.
	Fleet string `json:"fleet"`
}

func secs(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f s", *v)
}

// renderGCP renders whichever GCP studies the directory holds. A study that
// has not been run yet has no file and no block, and the README carries no
// block for it either, which splice enforces.
func renderGCP(dirs []string) (map[string]string, error) {
	b := map[string]string{}
	// Each study is read from the newest results directory that has it, so a
	// later directory adds studies without hiding the earlier ones.
	find := func(rel string) string {
		for i := len(dirs) - 1; i >= 0; i-- {
			if _, err := os.Stat(filepath.Join(dirs[i], rel)); err == nil {
				return filepath.Join(dirs[i], rel)
			}
		}
		return filepath.Join(dirs[len(dirs)-1], rel)
	}
	var src strings.Builder
	for _, dir := range dirs {
		var e gcpEnv
		if err := evidence.ReadJSON(filepath.Join(dir, "env.json"), &e); err != nil {
			return nil, err
		}
		fleet := fmt.Sprintf("%d tuned and %d default", e.TunedRigs, e.DefaultRigs)
		if e.Fleet != "" {
			fleet = e.Fleet
		}
		fmt.Fprintf(&src, "- `%s/`, published at commit `%s`: %s bench nodes (%s, %s,\n  kernel %s), control plane on %s.\n",
			filepath.ToSlash(dir), e.GitCommit[:7], fleet, e.RigMachine, e.CPUModel, e.Kernel, e.ControlPlane)
	}
	b["gcp_source"] = src.String()

	var g gateSummary
	if err := evidence.ReadJSON(find("gate/summary.json"), &g); err == nil {
		b["gcp_gate"] = renderGate(g)
	}
	var iso isoSummary
	if err := evidence.ReadJSON(find("isolation/summary.json"), &iso); err == nil {
		b["gcp_isolation"] = renderIsolation(iso)
	}
	var tun isoSummary
	if err := evidence.ReadJSON(find("tuning/summary.json"), &tun); err == nil {
		b["gcp_tuning"] = renderIsolation(tun)
	}
	var hil hilSummary
	if err := evidence.ReadJSON(find("hil/summary.json"), &hil); err == nil {
		b["gcp_hil"] = renderHIL(hil)
	}
	var gs gateSummary
	if err := evidence.ReadJSON(find("gate_strict/summary.json"), &gs); err == nil {
		b["gcp_gate_strict"] = renderGate(gs)
	}

	var sc scaleSummary
	if err := evidence.ReadJSON(find("scale/summary.json"), &sc); err == nil {
		var t strings.Builder
		states := make([]string, 0, len(sc.States))
		for k, v := range sc.States {
			states = append(states, fmt.Sprintf("%s %s", comma(v), k))
		}
		sort.Strings(states)
		rate := "n/a"
		if sc.SuccessRate != nil {
			rate = fmt.Sprintf("%.2f%%", *sc.SuccessRate*100)
		}
		perMin := "n/a"
		if sc.JobsPerMinute != nil {
			perMin = fmt.Sprintf("%.0f", *sc.JobsPerMinute)
		}
		fmt.Fprintf(&t, "| | |\n| --- | ---: |\n| jobs | %s |\n| outcomes | %s |\n| succeeded | %s |\n| throughput | %s jobs/minute |\n| lease to finished, median and p95 | %s and %s |\n| wall clock | %s |\n",
			comma(sc.Jobs), strings.Join(states, ", "), rate, perMin, secs(sc.LeaseToFinish.P50), secs(sc.LeaseToFinish.P95), secs(sc.WallSeconds))
		fmt.Fprintf(&t, "\nEvery job was submitted at once, so queue wait measures the backlog draining\n(median %s), not the scheduler: a job's own time from lease to finished,\nincluding preflight, the run, and sealing its results in Cloud Storage, is\nthe row above.\n", secs(sc.QueueWait.P50))
		b["gcp_scale"] = t.String()
	}
	var cn canarySummary
	if err := evidence.ReadJSON(find("canary/summary.json"), &cn); err == nil {
		var t strings.Builder
		fmt.Fprintf(&t, "| rig | canary CV, median and range | jobs placed with `max_rig_noise_cv` %.0f%% | jobs placed with no limit |\n| --- | --- | ---: | ---: |\n", cn.Limit*100)
		rigs := map[string]bool{}
		for r := range cn.Canary {
			rigs[r] = true
		}
		for _, m := range cn.Placements {
			for r := range m {
				rigs[r] = true
			}
		}
		names := make([]string, 0, len(rigs))
		for r := range rigs {
			names = append(names, r)
		}
		sort.Strings(names)
		for _, r := range names {
			c := cn.Canary[r]
			fmt.Fprintf(&t, "| %s | %.2f%% (%.2f%% to %.2f%%, %d readings) | %d | %d |\n", r, c.Median*100, c.Min*100, c.Max*100, c.Observations, cn.Placements["limited"][r], cn.Placements["unlimited"][r])
		}
		total := func(m map[string]int) (n int) {
			for _, v := range m {
				n += v
			}
			return
		}
		fmt.Fprintf(&t, "\n%d of %d limited jobs and %d of %d unlimited ones succeeded.\n",
			cn.States["limited"]["SUCCEEDED"], total(cn.States["limited"]), cn.States["unlimited"]["SUCCEEDED"], total(cn.States["unlimited"]))
		b["gcp_canary"] = t.String()
	}

	var gp gpuSummary
	if err := evidence.ReadJSON(find("gpu/summary.json"), &gp); err == nil {
		var t strings.Builder
		fmt.Fprintf(&t, "On a real %s, not emulated. Before each run the agent read the GPU's\n"+
			"temperature at %s and its utilization at %s through nvidia-smi, and would\n"+
			"have waited or refused above 85 C or 10%%.\n\n", gp.GPU, rangeOf(gp.PreflightTemp, "%.0f C"), rangeOf(gp.PreflightUtil, "%.0f%%", 100))
		t.WriteString("| batch of frames | runs | not succeeded | GPU time per forward pass | frames per second | median CV |\n| --- | ---: | ---: | ---: | ---: | ---: |\n")
		for _, size := range []string{"small", "medium", "large"} {
			c := gp.Sizes[size]
			label := map[string]string{"small": "4 at 192x192", "medium": "8 at 256x256", "large": "16 at 320x320"}[size]
			ms, fps := "n/a", "n/a"
			if c.MedianMS != nil {
				ms = fmt.Sprintf("%.2f ms", *c.MedianMS)
			}
			if c.MedianFPS != nil {
				fps = fmt.Sprintf("%.0f", *c.MedianFPS)
			}
			fmt.Fprintf(&t, "| %s | %d | %d | %s | %s | %s |\n", label, c.Runs, c.NotSucceeded, ms, fps, pct(c.MedianCV))
		}
		t.WriteString("\n| gate comparison on the GPU | verdict | change | 95% interval | pairs |\n| --- | --- | ---: | --- | ---: |\n")
		for _, g := range gp.Gates {
			ch, iv := "n/a", "n/a"
			if g.Ratio != nil {
				ch = fmt.Sprintf("%+.1f%%", (*g.Ratio-1)*100)
				iv = fmt.Sprintf("%+.1f%% to %+.1f%%", (*g.Low-1)*100, (*g.High-1)*100)
			}
			fmt.Fprintf(&t, "| %s | %s | %s | %s | %d |\n", g.Label, g.Verdict, ch, iv, g.Pairs)
		}
		b["gcp_gpu"] = t.String()
	}
	var totals []map[string]string
	var report struct {
		LoadedRuns    int      `json:"loaded_runs"`
		LoadedSamples int      `json:"loaded_samples"`
		Already       int      `json:"already_present"`
		Corrupt       []string `json:"corrupt"`
	}
	if evidence.ReadJSON(find("bigquery/totals.json"), &totals) == nil && len(totals) == 1 &&
		evidence.ReadJSON(find("bigquery/export_report.json"), &report) == nil {
		t := totals[0]
		b["gcp_bigquery"] = fmt.Sprintf("The tables hold %s runs, %s of them succeeded, from %s rigs across %s\n"+
			"benchmarks. The last export verified every attempt against its manifest\n"+
			"before loading it and found %d corrupt; the queries and what they returned\n"+
			"are in `bigquery/`.\n", t["runs"], t["succeeded"], t["rigs"], t["benchmarks"], len(report.Corrupt))
	}
	return b, nil
}

func renderGate(g gateSummary) string {
	var t strings.Builder
	fmt.Fprintf(&t, "%d comparisons over the 30 avbench profiles on tuned nodes: %d null, where\n"+
		"baseline and candidate are the same binary, and %d with an injected slowdown.\n"+
		"%s runs, %d of them reruns of a run noisier than 5%%; %d of %d pairs ran on\n"+
		"one rig.\n\n", g.Comparisons, g.NullComparisons, g.InjectedComparisons, comma(g.Runs), g.Reruns, g.PairsOnOneRig, g.Pairs)
	t.WriteString("| comparison | comparisons | REGRESSION | PASS | INCONCLUSIVE | other |\n| --- | ---: | ---: | ---: | ---: | ---: |\n")
	keys := make([]string, 0, len(g.BySize))
	for k := range g.BySize {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i] == "null" || keys[j] == "null" {
			return keys[i] == "null"
		}
		var a, c float64
		fmt.Sscanf(keys[i], "%f", &a)
		fmt.Sscanf(keys[j], "%f", &c)
		return a < c
	})
	for _, k := range keys {
		v := g.BySize[k]
		total := 0
		for _, n := range v {
			total += n
		}
		other := total - v["REGRESSION"] - v["PASS"] - v["INCONCLUSIVE"]
		label := "injected " + k
		if k == "null" {
			label = "null (no change)"
		}
		fmt.Fprintf(&t, "| %s | %d | %d | %d | %d | %d |\n", label, total, v["REGRESSION"], v["PASS"], v["INCONCLUSIVE"], other)
	}
	fmt.Fprintf(&t, "\nOn the null comparisons, %d of %d were called a regression: a false alarm rate\nof %.1f%%. Of the injected ones, %d of %d were caught.\n",
		g.FalseAlarms, g.NullComparisons, 100*float64(g.FalseAlarms)/float64(max(1, g.NullComparisons)), g.InjectedCaught, g.InjectedComparisons)
	return t.String()
}

var classLabel = map[string]string{
	"n2d-isolated": "tuned: SMT off, isolated core, pinned",
	"n2d-default":  "SMT off, nothing isolated",
	"n2d-stock":    "stock: SMT on, nothing isolated",
}

func renderIsolation(iso isoSummary) string {
	var t strings.Builder
	t.WriteString("| nodes | noise on the system core | runs | not succeeded | median of per-profile median CV |\n| --- | --- | ---: | ---: | ---: |\n")
	for _, k := range cellOrder(iso.Cells) {
		c := iso.Cells[k]
		cls, cond, _ := strings.Cut(k, "/")
		fmt.Fprintf(&t, "| %s | %s | %d | %d | %s |\n", classLabel[cls], map[string]string{"quiet": "off", "noise": "on"}[cond], c.Runs, c.NotSucceeded, pct(c.MedianCV))
	}
	t.WriteString("\n| rig | class | median CV, noise off | p90 CV, noise off | median CV, noise on | p90 CV, noise on |\n| --- | --- | ---: | ---: | ---: | ---: |\n")
	names := make([]string, 0, len(iso.PerRig))
	for n := range iso.PerRig {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r := iso.PerRig[n]
		fmt.Fprintf(&t, "| %s | %s | %s | %s | %s | %s |\n", n, strings.SplitN(classLabel[r.Class], ":", 2)[0], pct(r.Quiet.MedianCV), pct(r.Quiet.P90CV), pct(r.Noise.MedianCV), pct(r.Noise.P90CV))
	}
	return t.String()
}

type hilSummary struct {
	HZ       float64 `json:"hz"`
	Cycles   int     `json:"cycles_per_session"`
	PeriodMS float64 `json:"period_ms"`
	Cells    map[string]struct {
		Runs        int      `json:"runs"`
		Failed      int      `json:"not_succeeded"`
		TotalCycles int      `json:"cycles"`
		Misses      int      `json:"deadline_misses"`
		MissRate    *float64 `json:"miss_rate"`
		CycleP50    *float64 `json:"median_cycle_p50_ms"`
		CycleP99    *float64 `json:"median_cycle_p99_ms"`
		JitterP99   *float64 `json:"median_jitter_p99_ms"`
		WorstJitter *float64 `json:"worst_jitter_ms"`
	} `json:"cells"`
}

func msOf(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.2f ms", *v)
}

func renderHIL(h hilSummary) string {
	var t strings.Builder
	fmt.Fprintf(&t, "A simulated sensor at %.0f Hz (a %.0f ms budget per cycle), %d cycles per session.\n\n", h.HZ, h.PeriodMS, h.Cycles)
	t.WriteString("| nodes | noise | cycles | deadline misses | cycle p50 | cycle p99 | wake-up jitter p99 | worst wake-up jitter |\n| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, k := range cellOrder(h.Cells) {
		c := h.Cells[k]
		cls, cond, _ := strings.Cut(k, "/")
		fmt.Fprintf(&t, "| %s | %s | %s | %d | %s | %s | %s | %s |\n", strings.SplitN(classLabel[cls], ":", 2)[0], map[string]string{"quiet": "off", "noise": "on"}[cond],
			comma(c.TotalCycles), c.Misses, msOf(c.CycleP50), msOf(c.CycleP99), msOf(c.JitterP99), msOf(c.WorstJitter))
	}
	return t.String()
}

// cellOrder lists "class/condition" keys by class, with the quiet condition
// before the noisy one, which is the order a reader compares them in.
func cellOrder[V any](cells map[string]V) []string {
	keys := make([]string, 0, len(cells))
	for k := range cells {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ci, ni, _ := strings.Cut(keys[i], "/")
		cj, nj, _ := strings.Cut(keys[j], "/")
		if ci != cj {
			return ci < cj
		}
		return ni == "quiet" && nj != "quiet"
	})
	return keys
}
