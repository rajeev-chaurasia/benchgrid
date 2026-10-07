package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/rajeev-chaurasia/benchgrid/internal/spec"
)

// perfEvents maps each metric the perf collector can produce to the hardware
// or software event it counts.
var perfEvents = map[string]string{
	"cycles":           "cycles",
	"instructions":     "instructions",
	"cache_misses":     "cache-misses",
	"context_switches": "context-switches",
}

// perfWrap returns the command line that runs argv under perf stat, counting
// only the events the spec declares metrics for, and writing machine
// readable output to out. The wrapped benchmark is perf's child, so it is
// still in the iteration's process group and still reaped with it.
func perfWrap(perf string, argv []string, sp spec.Spec, out string) []string {
	var events []string
	for _, m := range sp.Metrics {
		if e, ok := perfEvents[m.Name]; ok {
			events = append(events, e)
		}
	}
	if needsIPC(sp) {
		events = appendMissing(events, "cycles", "instructions")
	}
	cmd := []string{perf, "stat", "-x", ",", "-o", out, "-e", strings.Join(events, ","), "--"}
	return append(cmd, argv...)
}

func needsIPC(sp spec.Spec) bool {
	_, ok := sp.Metric("ipc")
	return ok
}

func appendMissing(list []string, items ...string) []string {
	for _, it := range items {
		found := false
		for _, l := range list {
			if l == it {
				found = true
			}
		}
		if !found {
			list = append(list, it)
		}
	}
	return list
}

// parsePerf reads perf stat's -x output: one line per event, the count first
// and the event name third, with comment lines starting with #. An event perf
// could not count is reported as <not supported> or <not counted>, which is
// an error here: a run that asked for cycles and silently published none
// would look like a run that used no cycles.
func parsePerf(path string, sp spec.Spec) (map[string]float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no perf output: %w", err)
	}
	counts := map[string]float64{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 3 {
			continue
		}
		event := f[2]
		if i := strings.IndexByte(event, ':'); i >= 0 {
			event = event[:i]
		}
		if strings.HasPrefix(f[0], "<") {
			return nil, fmt.Errorf("%s %s", event, strings.Trim(f[0], "<>"))
		}
		v, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			return nil, fmt.Errorf("%s: unreadable count %q", event, f[0])
		}
		counts[event] = v
	}
	out := map[string]float64{}
	for metric, event := range perfEvents {
		if _, declared := sp.Metric(metric); !declared {
			continue
		}
		v, ok := counts[event]
		if !ok {
			return nil, fmt.Errorf("%s not reported", event)
		}
		out[metric] = v
	}
	if needsIPC(sp) {
		c, i := counts["cycles"], counts["instructions"]
		if c <= 0 {
			return nil, fmt.Errorf("ipc needs cycles, and none were counted")
		}
		out["ipc"] = i / c
	}
	return out, nil
}
