package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rajeev-chaurasia/benchgrid/internal/evidence"
)

// renderGlance is the table at the top of the README: one row per study, each
// number read from the same summaries the full sections below are rendered
// from, so the short version cannot drift from the long one.
func renderGlance(local string, gdirs []string) (string, error) {
	var race []evidence.RaceSummary
	var fence, linux []evidence.FenceSummary
	var chaos evidence.ChaosSummary
	for _, f := range []struct {
		path string
		v    any
	}{
		{"lease_race/summary.json", &race}, {"fence/summary.json", &fence},
		{"linux/summary.json", &linux}, {"chaos/summary.json", &chaos},
	} {
		if err := evidence.ReadJSON(filepath.Join(local, f.path), f.v); err != nil {
			return "", err
		}
	}
	var t strings.Builder
	t.WriteString("| study | with benchgrid | without, or the control | where |\n| --- | --- | --- | --- |\n")

	var dbProduct, dbControl int
	for _, r := range race {
		if r.Mode == "conditional" {
			dbProduct += r.DoubleBookings
		} else {
			dbControl += r.DoubleBookings
		}
	}
	fmt.Fprintf(&t, "| [lease race](#the-lease), double bookings | **%s** | %s with a read then write | laptop |\n", comma(dbProduct), comma(dbControl))

	var ovFenced, ovControl int
	for _, set := range [][]evidence.FenceSummary{fence, linux} {
		for _, x := range set {
			if x.Mode == "fenced" {
				ovFenced += x.ProcessOverlaps
			} else {
				ovControl += x.ProcessOverlaps
			}
		}
	}
	fmt.Fprintf(&t, "| [frozen schedulers](#the-fence), overlapping runs on one rig | **%d** | %d with the fence check removed | laptop, Linux containers |\n", ovFenced, ovControl)
	fmt.Fprintf(&t, "| [chaos](#chaos), experiments ending as they should | **%d of %d** | n/a | laptop |\n", chaos.AsExpected, chaos.Experiments)

	if len(gdirs) == 0 {
		return t.String(), nil
	}
	find := func(rel string) string {
		for i := len(gdirs) - 1; i >= 0; i-- {
			if _, err := os.Stat(filepath.Join(gdirs[i], rel)); err == nil {
				return filepath.Join(gdirs[i], rel)
			}
		}
		return filepath.Join(gdirs[len(gdirs)-1], rel)
	}

	var first gateSummary
	if evidence.ReadJSON(find("gate/summary.json"), &first) == nil {
		fmt.Fprintf(&t, "| [CI gate, first evaluation](#the-gate-on-every-avbench-profile), pairs mostly split across rigs | %d of %d false, %d of %d caught | n/a | GCP |\n",
			first.FalseAlarms, first.NullComparisons, first.InjectedCaught, first.InjectedComparisons)
	}
	var soft, strict gateSummary
	if evidence.ReadJSON(find("gate_soft/summary.json"), &soft) == nil &&
		evidence.ReadJSON(find("gate_strict/summary.json"), &strict) == nil {
		fmt.Fprintf(&t, "| [CI gate, pairs on one rig](#pairing-on-one-rig-and-leaving-noisy-rigs-out), noisy rigs excluded | **%d of %d** false, **%d of %d** caught | %d reruns for noise without the rig noise limit, %d with | GCP |\n",
			strict.FalseAlarms, strict.NullComparisons, strict.InjectedCaught, strict.InjectedComparisons, soft.Reruns, strict.Reruns)
	}
	var gp gpuSummary
	if evidence.ReadJSON(find("gpu/summary.json"), &gp) == nil {
		var parts []string
		for _, g := range gp.Gates {
			v := g.Verdict
			if g.Ratio != nil {
				v = fmt.Sprintf("%s %+.1f%%", g.Verdict, (*g.Ratio-1)*100)
			}
			parts = append(parts, fmt.Sprintf("%s **%s**", g.Label, v))
		}
		fmt.Fprintf(&t, "| [GPU gate](#the-gpu) | %s | n/a | NVIDIA L4 |\n", strings.Join(parts, ", "))
	}
	var tun isoSummary
	if evidence.ReadJSON(find("tuning/summary.json"), &tun) == nil {
		fmt.Fprintf(&t, "| [tuned against stock nodes](#against-a-stock-machine), median CV under noise | **%s** | %s on stock nodes | GCP |\n",
			pct(tun.Cells["n2d-isolated/noise"].MedianCV), pct(tun.Cells["n2d-stock/noise"].MedianCV))
	}
	var hil hilSummary
	if evidence.ReadJSON(find("hil/summary.json"), &hil) == nil {
		misses, cycles := map[string]int{}, map[string]int{}
		for k, c := range hil.Cells {
			cls, _, _ := strings.Cut(k, "/")
			misses[cls] += c.Misses
			cycles[cls] += c.TotalCycles
		}
		fmt.Fprintf(&t, "| [%.0f Hz sensor loop](#a-sensor-in-the-loop-simulated), deadline misses | **%d of %s** | %d of %s on stock nodes | GCP |\n",
			hil.HZ, misses["n2d-isolated"], comma(cycles["n2d-isolated"]), misses["n2d-stock"], comma(cycles["n2d-stock"]))
	}
	var cn canarySummary
	if evidence.ReadJSON(find("canary/summary.json"), &cn) == nil {
		var limited, unlimited int
		for r, c := range cn.Canary {
			if c.Median > cn.Limit {
				limited += cn.Placements["limited"][r]
				unlimited += cn.Placements["unlimited"][r]
			}
		}
		fmt.Fprintf(&t, "| [rig noise canary](#noisy-rigs-benched), jobs placed on a noisy rig | **%d** | %d with no limit | GCP |\n", limited, unlimited)
	}
	var sc scaleSummary
	if evidence.ReadJSON(find("scale/summary.json"), &sc) == nil && sc.JobsPerMinute != nil {
		fmt.Fprintf(&t, "| [scale](#scale), jobs succeeded | **%s of %s** at %.0f jobs/minute | n/a | GCP |\n",
			comma(sc.States["SUCCEEDED"]), comma(sc.Jobs), *sc.JobsPerMinute)
	}
	return t.String(), nil
}
