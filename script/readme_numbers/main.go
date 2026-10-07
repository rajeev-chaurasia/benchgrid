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
	dir, err := latest("evidence/results")
	if err != nil {
		die(err)
	}
	blocks, err := render(dir)
	if err != nil {
		die(err)
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

func latest(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
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
