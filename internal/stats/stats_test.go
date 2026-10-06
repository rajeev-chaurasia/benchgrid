package stats

import (
	"testing"
)

// Expected values are numpy 2.x output (np.percentile defaults,
// np.std(ddof=1), np.median(abs(x - np.median(x)))), pasted as printed, so
// this pins agreement with what TraceLab will compute rather than with
// arithmetic done by hand.
var numpyCases = []struct {
	name string
	x    []float64
	want [7]float64 // mean median p90 p95 p99 stddev mad
}{
	{
		name: "periodic loop with one late tick",
		x:    []float64{19.8, 20.1, 20.0, 21.0, 19.9, 32.4},
		want: [7]float64{22.200000000000003, 20.05, 26.7, 29.549999999999997, 31.830000000000002, 5.015575739633486, 0.20000000000000107},
	},
	{
		name: "nanosecond latencies",
		x:    []float64{1834221, 1790012, 1801555, 1799999, 2100450, 1795000, 1802001},
		want: [7]float64{1846176.857142857, 1801555.0, 1940712.6, 2020581.2999999998, 2084476.2599999998, 113023.85188007672, 6555.0},
	},
}

func TestSummarizeMatchesNumpy(t *testing.T) {
	for _, c := range numpyCases {
		s := Summarize("ns", c.x)
		got := [7]*float64{s.Mean, s.Median, s.P90, s.P95, s.P99, s.Stddev, s.MAD}
		names := [7]string{"mean", "median", "p90", "p95", "p99", "stddev", "mad"}
		for i := range got {
			if got[i] == nil || !Agree(*got[i], c.want[i]) {
				t.Errorf("%s: %s = %v, want %v", c.name, names[i], deref(got[i]), c.want[i])
			}
		}
		if s.CV == nil || *s.CV != *s.Stddev / *s.Mean {
			t.Errorf("%s: cv is not stddev/mean", c.name)
		}
	}
}

func TestEmptyIsAllNull(t *testing.T) {
	s := Summarize("ns", nil)
	if s.N != 0 || s.Mean != nil || s.Median != nil || s.P99 != nil || s.Stddev != nil || s.MAD != nil || s.CV != nil {
		t.Errorf("empty summary must be n 0 and every field null: %+v", s)
	}
}

func TestSingleSampleHasNoSpread(t *testing.T) {
	s := Summarize("ns", []float64{7})
	if *s.Median != 7 || *s.P99 != 7 || *s.MAD != 0 {
		t.Errorf("single sample location wrong: %+v", s)
	}
	if s.Stddev != nil || s.CV != nil {
		t.Error("stddev and cv must be null for one sample, not zero")
	}
}

func TestZeroMeanHasNoCV(t *testing.T) {
	s := Summarize("ratio", []float64{0, 0})
	if s.CV != nil || s.Stddev == nil || *s.Stddev != 0 {
		t.Errorf("zero mean: %+v", s)
	}
}

func TestInputNotReordered(t *testing.T) {
	x := []float64{3, 1, 2}
	Summarize("ns", x)
	if x[0] != 3 || x[1] != 1 || x[2] != 2 {
		t.Errorf("input mutated: %v", x)
	}
}

func TestAgreeAtZero(t *testing.T) {
	if !Agree(0, 0) || !Agree(0, 1e-13) || Agree(0, 1e-6) {
		t.Error("absolute floor wrong")
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
