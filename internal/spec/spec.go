// Package spec defines the ExperimentSpec, the immutable description of one
// benchmark that every result carries with it. A number without the spec that
// produced it is not a result.
package spec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/rajeev-chaurasia/benchgrid/internal/canon"
)

type Spec struct {
	Benchmark      string       `json:"benchmark"`
	Revision       string       `json:"revision"`
	Command        []string     `json:"command"`
	Warmups        int          `json:"warmups"`
	Repetitions    int          `json:"repetitions"`
	TimeoutSeconds int          `json:"timeout_seconds"`
	Requirements   Requirements `json:"requirements"`
	Environment    Environment  `json:"environment"`
	Metrics        []Metric     `json:"metrics"`
	Artifacts      Artifacts    `json:"artifacts"`
	// Collectors wrap every iteration in a measuring tool. A collector changes
	// what is measured, perf most of all, so it is part of the spec and of its
	// hash, and a run with one is never comparable to a run without.
	Collectors []string `json:"collectors,omitempty"`
	// Affinity is a soft placement preference: experiments sharing a key
	// prefer the rig that most recently ran that key. Paired baseline and
	// candidate runs share one, so they tend to be measured on the same
	// machine, without either being pinned to a rig by name.
	Affinity string `json:"affinity,omitempty"`
}

type Requirements struct {
	OS                string   `json:"os,omitempty"`
	Arch              string   `json:"arch,omitempty"`
	HardwareClass     string   `json:"hardware_class,omitempty"`
	GPUVendor         string   `json:"gpu_vendor,omitempty"`
	MinGPUMemoryBytes int64    `json:"min_gpu_memory_bytes,omitempty"`
	MinGPUCount       int      `json:"min_gpu_count,omitempty"`
	Driver            string   `json:"driver,omitempty"`
	MinCPUCores       int      `json:"min_cpu_cores,omitempty"`
	MinMemBytes       int64    `json:"min_mem_bytes,omitempty"`
	Tags              []string `json:"tags,omitempty"`
	Profilers         []string `json:"profilers,omitempty"`
	// An experiment gating a real hardware target must not be satisfiable by a
	// rig that only claims to be one, so emulated rigs are opt in.
	AllowEmulated bool `json:"allow_emulated"`
}

// Thresholds are pointers so that an absent gate is distinguishable from a
// gate of zero. A spec with no max_load1 does not check load at all.
type Environment struct {
	CPUGovernor             string   `json:"cpu_governor,omitempty"`
	GovernorRequired        bool     `json:"governor_required,omitempty"`
	MaxLoad1                *float64 `json:"max_load1,omitempty"`
	MaxCPUUtil              *float64 `json:"max_cpu_util,omitempty"`
	MaxGPUUtil              *float64 `json:"max_gpu_util,omitempty"`
	MaxTempC                *float64 `json:"max_temp_c,omitempty"`
	MinMemFreeBytes         int64    `json:"min_mem_free_bytes,omitempty"`
	PreflightTimeoutSeconds int      `json:"preflight_timeout_seconds,omitempty"`
	// GateDuringMeasurement checks max_cpu_util against the background load
	// across the measured iterations, excluding the benchmark's own CPU time,
	// and declares the run INVALID if it was exceeded. Two earlier designs
	// failed and are recorded in PLAN.md: waiting for quiet before each
	// iteration lined iterations up with the onset of the next burst and made
	// results worse, and checking each iteration on its own needed a CPU
	// counter finer than the one macOS provides.
	GateDuringMeasurement bool `json:"gate_during_measurement,omitempty"`
	// RequireIsolation needs the benchmark pinned to CPUs the kernel has
	// isolated from the scheduler, so nothing else is ever placed on them.
	RequireIsolation bool `json:"require_isolation,omitempty"`
	// MaxClockOffsetMS needs the rig's clock within this many milliseconds of
	// true time, judged by chrony's error bound and not its offset alone, so
	// timestamps from different rigs can be lined up.
	MaxClockOffsetMS *float64 `json:"max_clock_offset_ms,omitempty"`
}

type Metric struct {
	Name      string `json:"name"`
	Unit      string `json:"unit"`
	Direction string `json:"direction"`
}

type Artifacts struct {
	BinarySHA256 string `json:"binary_sha256"`
	ConfigSHA256 string `json:"config_sha256,omitempty"`
}

// Units is the closed vocabulary in docs/run-artifact.md.
var Units = map[string]bool{
	"ns": true, "bytes": true, "ops_per_s": true, "ratio": true,
	"celsius": true, "count": true, "unitless": true,
}

// Builtin metrics are measured by the agent around each iteration, so a
// benchmark gets them without printing anything. Their units are fixed.
var Builtin = map[string]string{
	"iteration_latency": "ns",
	"cpu_user":          "ns",
	"cpu_sys":           "ns",
	"max_rss":           "bytes",
}

// PerfMetrics are produced by the perf collector, in fixed units.
var PerfMetrics = map[string]string{
	"cycles":           "count",
	"instructions":     "count",
	"cache_misses":     "count",
	"context_switches": "count",
	"ipc":              "unitless",
}

var KnownCollectors = map[string]bool{"perf": true}

const BinaryPlaceholder = "{binary}"

var (
	hexSHA   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	revision = regexp.MustCompile(`^[0-9a-f]{40}$`)
	name     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// Parse rejects unknown fields, because a field the scheduler silently ignored
// would still be hashed into spec_sha256 by nobody and enforced by nothing.
func Parse(b []byte) (Spec, error) {
	var s Spec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("spec: %w", err)
	}
	if dec.More() {
		return Spec{}, fmt.Errorf("spec: trailing data")
	}
	return s, s.Validate()
}

func (s Spec) Validate() error {
	var errs []string
	bad := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if !name.MatchString(s.Benchmark) {
		bad("benchmark %q must be lower snake case", s.Benchmark)
	}
	if !revision.MatchString(s.Revision) {
		// An abbreviated SHA is ambiguous by design, and provenance that only
		// probably identifies a commit is not provenance.
		bad("revision must be a full 40 character lowercase hex commit id")
	}
	if len(s.Command) == 0 {
		bad("command is empty")
	}
	if s.Warmups < 0 || s.Warmups > 1000 {
		bad("warmups must be 0..1000")
	}
	if s.Repetitions < 1 || s.Repetitions > 10000 {
		bad("repetitions must be 1..10000")
	}
	if s.TimeoutSeconds < 1 || s.TimeoutSeconds > 86400 {
		bad("timeout_seconds must be 1..86400")
	}
	if !hexSHA.MatchString(s.Artifacts.BinarySHA256) {
		bad("artifacts.binary_sha256 must be 64 lowercase hex characters")
	}
	if c := s.Artifacts.ConfigSHA256; c != "" && !hexSHA.MatchString(c) {
		bad("artifacts.config_sha256 must be 64 lowercase hex characters")
	}
	if len(s.Metrics) == 0 {
		bad("metrics is empty")
	}
	seen := map[string]bool{}
	for _, m := range s.Metrics {
		if !name.MatchString(m.Name) {
			bad("metric name %q must be lower snake case", m.Name)
		}
		if seen[m.Name] {
			bad("metric %q declared twice", m.Name)
		}
		seen[m.Name] = true
		if !Units[m.Unit] {
			bad("metric %q has unit %q outside the vocabulary", m.Name, m.Unit)
		}
		if u, ok := Builtin[m.Name]; ok && u != m.Unit {
			bad("builtin metric %q is measured in %s, not %s", m.Name, u, m.Unit)
		}
		if u, ok := PerfMetrics[m.Name]; ok {
			if u != m.Unit {
				bad("perf metric %q is measured in %s, not %s", m.Name, u, m.Unit)
			}
			if !s.HasCollector("perf") {
				bad("metric %q needs the perf collector", m.Name)
			}
		}
		if m.Direction != "lower_is_better" && m.Direction != "higher_is_better" {
			bad("metric %q direction must be lower_is_better or higher_is_better", m.Name)
		}
	}
	for _, c := range s.Collectors {
		if !KnownCollectors[c] {
			bad("unknown collector %q", c)
		}
	}
	if d := s.Requirements.Driver; d != "" {
		if _, err := ParseConstraint(d); err != nil {
			bad("requirements.driver: %v", err)
		}
	}
	e := s.Environment
	for label, v := range map[string]*float64{"max_cpu_util": e.MaxCPUUtil, "max_gpu_util": e.MaxGPUUtil} {
		if v != nil && (*v < 0 || *v > 1) {
			bad("environment.%s is a ratio and must be 0..1", label)
		}
	}
	if e.GovernorRequired && e.CPUGovernor == "" {
		bad("environment.governor_required without cpu_governor")
	}
	if len(errs) > 0 {
		return fmt.Errorf("spec invalid: %v", errs)
	}
	return nil
}

func (s Spec) SHA256() (string, error) { return canon.SHA256(s) }

func (s Spec) Metric(name string) (Metric, bool) {
	for _, m := range s.Metrics {
		if m.Name == name {
			return m, true
		}
	}
	return Metric{}, false
}

func (s Spec) HasCollector(name string) bool {
	for _, c := range s.Collectors {
		if c == name {
			return true
		}
	}
	return false
}
