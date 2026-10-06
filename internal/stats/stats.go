// Package stats computes the descriptive summary defined in
// docs/run-artifact.md. TraceLab recomputes every field from the raw samples
// and rejects a run that disagrees, so the definitions here are a contract and
// not a matter of taste.
package stats

import (
	"math"
	"sort"
)

// Every field but N is a pointer because the contract distinguishes a value
// that could not be computed from a value that happens to be zero. A single
// sample has no spread, and writing stddev 0 for it would read as a perfectly
// stable benchmark.
type Summary struct {
	Unit   string   `json:"unit"`
	N      int      `json:"n"`
	Mean   *float64 `json:"mean"`
	Median *float64 `json:"median"`
	P90    *float64 `json:"p90"`
	P95    *float64 `json:"p95"`
	P99    *float64 `json:"p99"`
	Stddev *float64 `json:"stddev"`
	MAD    *float64 `json:"mad"`
	CV     *float64 `json:"cv"`
}

func Summarize(unit string, values []float64) Summary {
	n := len(values)
	s := Summary{Unit: unit, N: n}
	if n == 0 {
		return s
	}
	x := append([]float64(nil), values...)
	sort.Float64s(x)

	var sum float64
	for _, v := range x {
		sum += v
	}
	mean := sum / float64(n)
	median := Percentile(x, 50)

	dev := make([]float64, n)
	for i, v := range x {
		dev[i] = math.Abs(v - median)
	}
	sort.Float64s(dev)

	s.Mean = ptr(mean)
	s.Median = ptr(median)
	s.P90 = ptr(Percentile(x, 90))
	s.P95 = ptr(Percentile(x, 95))
	s.P99 = ptr(Percentile(x, 99))
	s.MAD = ptr(Percentile(dev, 50))

	if n >= 2 {
		var ss float64
		for _, v := range x {
			d := v - mean
			ss += d * d
		}
		sd := math.Sqrt(ss / float64(n-1))
		s.Stddev = ptr(sd)
		if mean != 0 {
			s.CV = ptr(sd / mean)
		}
	}
	return s
}

// Percentile is Hyndman and Fan type 7, numpy's default. sorted must be
// ascending and non-empty.
func Percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	h := float64(n-1) * p / 100
	lo := int(math.Floor(h))
	if lo >= n-1 {
		return sorted[n-1]
	}
	return sorted[lo] + (h-float64(lo))*(sorted[lo+1]-sorted[lo])
}

// Agree is the tolerance from the contract. The absolute term exists because a
// purely relative tolerance can never be met when the true value is zero.
func Agree(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9*math.Max(math.Abs(a), math.Abs(b))+1e-12
}

func ptr(v float64) *float64 { return &v }
