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
	if gdir, err := latest("evidence/results", true); err == nil {
		g, err := renderGCP(gdir)
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
func renderGCP(dir string) (map[string]string, error) {
	b := map[string]string{}
	var e gcpEnv
	if err := evidence.ReadJSON(filepath.Join(dir, "env.json"), &e); err != nil {
		return nil, err
	}
	b["gcp_source"] = fmt.Sprintf("From `%s/`, at commit `%s`: %d tuned and %d default `%s` bench nodes\n"+
		"(%s, kernel %s) in %s, with the control plane on %s.\n",
		filepath.ToSlash(dir), e.GitCommit[:7], e.TunedRigs, e.DefaultRigs, e.RigMachine, e.CPUModel, e.Kernel, e.Zone, e.ControlPlane)

	var g gateSummary
	if err := evidence.ReadJSON(filepath.Join(dir, "gate", "summary.json"), &g); err == nil {
		var t strings.Builder
		fmt.Fprintf(&t, "%d comparisons over the 30 avbench profiles on tuned nodes: %d null, where\n"+
			"baseline and candidate are the same binary, and %d with an injected slowdown.\n"+
			"%d runs, %d of them reruns of a run noisier than 5%%; %d of %d pairs ran on\n"+
			"one rig.\n\n", g.Comparisons, g.NullComparisons, g.InjectedComparisons, g.Runs, g.Reruns, g.PairsOnOneRig, g.Pairs)
		t.WriteString("| comparison | runs | REGRESSION | PASS | INCONCLUSIVE | other |\n| --- | ---: | ---: | ---: | ---: | ---: |\n")
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
		b["gcp_gate"] = t.String()
	}

	var iso isoSummary
	if err := evidence.ReadJSON(filepath.Join(dir, "isolation", "summary.json"), &iso); err == nil {
		var t strings.Builder
		t.WriteString("| nodes | noise on the system core | runs | not succeeded | median of per-profile median CV |\n| --- | --- | ---: | ---: | ---: |\n")
		for _, cls := range []string{"n2d-isolated", "n2d-default"} {
			for _, cond := range []string{"quiet", "noise"} {
				c := iso.Cells[cls+"/"+cond]
				label := map[string]string{"n2d-isolated": "tuned", "n2d-default": "default"}[cls]
				fmt.Fprintf(&t, "| %s | %s | %d | %d | %s |\n", label, map[string]string{"quiet": "off", "noise": "on"}[cond], c.Runs, c.NotSucceeded, pct(c.MedianCV))
			}
		}
		b["gcp_isolation"] = t.String()
	}

	var sc scaleSummary
	if err := evidence.ReadJSON(filepath.Join(dir, "scale", "summary.json"), &sc); err == nil {
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
		fmt.Fprintf(&t, "| | |\n| --- | ---: |\n| jobs | %s |\n| outcomes | %s |\n| succeeded | %s |\n| throughput | %s jobs/minute |\n| queue wait, median and p95 | %s and %s |\n| wall clock | %s |\n",
			comma(sc.Jobs), strings.Join(states, ", "), rate, perMin, secs(sc.QueueWait.P50), secs(sc.QueueWait.P95), secs(sc.WallSeconds))
		b["gcp_scale"] = t.String()
	}
	return b, nil
}
