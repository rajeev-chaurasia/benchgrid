package spec

import (
	"strings"
	"testing"
)

const valid = `{
  "benchmark": "cpu_hash",
  "revision": "8f3c2aa0b6d1e4f7a9c3b5d7e9f1a3c5b7d9e1f3",
  "command": ["{binary}", "-iters", "200000"],
  "warmups": 5,
  "repetitions": 30,
  "timeout_seconds": 600,
  "requirements": {"arch": "arm64", "driver": ">=550", "allow_emulated": true},
  "environment": {"max_load1": 4.0, "max_cpu_util": 0.5},
  "metrics": [
    {"name": "iteration_latency", "unit": "ns", "direction": "lower_is_better"},
    {"name": "throughput", "unit": "ops_per_s", "direction": "higher_is_better"}
  ],
  "artifacts": {"binary_sha256": "` + sha + `"}
}`

const sha = "0000000000000000000000000000000000000000000000000000000000000000"

func TestParseValid(t *testing.T) {
	s, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	h1, _ := s.SHA256()
	s2, _ := Parse([]byte(strings.ReplaceAll(valid, "\n", " ")))
	h2, _ := s2.SHA256()
	if h1 != h2 || len(h1) != 64 {
		t.Errorf("whitespace changed the spec hash: %s %s", h1, h2)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"unknown field":      strings.Replace(valid, `"warmups"`, `"warmupz": 1, "warmups"`, 1),
		"unit outside vocab": strings.Replace(valid, `"unit": "ns"`, `"unit": "ms"`, 1),
		"percent not ratio":  strings.Replace(valid, `"max_cpu_util": 0.5`, `"max_cpu_util": 50`, 1),
		"builtin wrong unit": strings.Replace(valid, `"unit": "ns", "direction"`, `"unit": "count", "direction"`, 1),
		"no direction":       strings.Replace(valid, `"direction": "higher_is_better"`, `"direction": "up"`, 1),
		"bad driver":         strings.Replace(valid, `">=550"`, `">=r550"`, 1),
		"zero repetitions":   strings.Replace(valid, `"repetitions": 30`, `"repetitions": 0`, 1),
		"short binary sha":   strings.Replace(valid, sha, "abc", 1),
	}
	for label, in := range cases {
		if in == valid {
			t.Fatalf("%s: replacement did not apply", label)
		}
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
}

func TestConstraint(t *testing.T) {
	cases := []struct {
		c, v string
		ok   bool
	}{
		{">=550", "550.54", true},
		{">=550", "535.183.01", false},
		{">=550.54", "550.54.0", true},
		{"<560", "560", false},
		{"550.54", "550.54", true},
		{">=550", "not-a-version", false},
	}
	for _, c := range cases {
		k, err := ParseConstraint(c.c)
		if err != nil {
			t.Fatal(err)
		}
		if got := k.Allows(c.v); got != c.ok {
			t.Errorf("%s allows %s = %v", c.c, c.v, got)
		}
	}
}
