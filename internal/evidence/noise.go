package evidence

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/rajeev-chaurasia/benchgrid/internal/artifact"
	"github.com/rajeev-chaurasia/benchgrid/internal/stats"
)

type NoiseRun struct {
	ID       string   `json:"id"`
	Status   string   `json:"status"`
	Reason   string   `json:"reason"`
	CV       *float64 `json:"cv"`
	MedianNS *float64 `json:"median_ns"`
}

type NoiseCondition struct {
	Condition       string     `json:"condition"`
	Runs            int        `json:"runs"`
	Succeeded       int        `json:"succeeded"`
	Invalid         int        `json:"invalid"`
	MedianCV        *float64   `json:"median_cv_of_succeeded"`
	MaxCV           *float64   `json:"max_cv_of_succeeded"`
	MedianOfMedians *float64   `json:"median_of_median_latency_ns"`
	PerRun          []NoiseRun `json:"per_run"`
}

// SummarizeNoise reads each run's final sealed attempt and summarizes the
// iteration latency CV by condition, over succeeded runs only. An INVALID run
// is counted, never averaged in: it is the gate declining to produce a number,
// which is the outcome being measured, not a gap in the data.
func SummarizeNoise(runs map[string][]string, store string) ([]NoiseCondition, error) {
	var names []string
	for n := range runs {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []NoiseCondition
	for _, name := range names {
		c := NoiseCondition{Condition: name, PerRun: []NoiseRun{}}
		var cvs, medians []float64
		for _, id := range runs[name] {
			dir, err := finalAttemptDir(store, id)
			if err != nil {
				return nil, err
			}
			run, _, err := artifact.Verify(dir)
			if err != nil {
				return nil, err
			}
			sum := run.Summary["iteration_latency"]
			r := NoiseRun{ID: id, Status: run.Status, Reason: run.StatusReason}
			c.Runs++
			switch run.Status {
			case artifact.Succeeded:
				c.Succeeded++
				r.CV, r.MedianNS = sum.CV, sum.Median
				if sum.CV != nil {
					cvs = append(cvs, *sum.CV)
					medians = append(medians, *sum.Median)
				}
			case artifact.Invalid:
				c.Invalid++
			}
			c.PerRun = append(c.PerRun, r)
		}
		if len(cvs) > 0 {
			sort.Float64s(cvs)
			sort.Float64s(medians)
			m := stats.Percentile(cvs, 50)
			mx := cvs[len(cvs)-1]
			mm := stats.Percentile(medians, 50)
			c.MedianCV, c.MaxCV, c.MedianOfMedians = &m, &mx, &mm
		}
		out = append(out, c)
	}
	return out, nil
}

// finalAttemptDir is the sealed attempt with the highest number, which is the
// one a consumer would ingest.
func finalAttemptDir(store, id string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(store, "runs", id))
	if err != nil {
		return "", err
	}
	best := 0
	for _, e := range entries {
		n, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "attempt-"))
		if err == nil && n > best {
			if _, err := os.Stat(filepath.Join(store, "runs", id, e.Name(), artifact.ManifestFile)); err == nil {
				best = n
			}
		}
	}
	return artifact.AttemptDir(store, id, best), nil
}
