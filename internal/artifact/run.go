// Package artifact writes, seals, verifies, and stores the run directories
// that docs/run-artifact.md specifies. It is the only code in this repository
// that knows the layout, so the contract has one implementation to drift from.
package artifact

import (
	"github.com/rajeev-chaurasia/benchgrid/internal/capability"
	"github.com/rajeev-chaurasia/benchgrid/internal/probe"
	"github.com/rajeev-chaurasia/benchgrid/internal/spec"
	"github.com/rajeev-chaurasia/benchgrid/internal/stats"
)

const (
	RunSchema      = "benchgrid.run/v1"
	ManifestSchema = "benchgrid.manifest/v1"

	RunFile      = "run.json"
	SamplesFile  = "samples.jsonl"
	ManifestFile = "manifest.json"

	Succeeded = "SUCCEEDED"
	Failed    = "FAILED"
	Invalid   = "INVALID"
)

type Run struct {
	SchemaVersion string                   `json:"schema_version"`
	RunID         string                   `json:"run_id"`
	Attempt       int                      `json:"attempt"`
	Fence         int64                    `json:"fence"`
	Status        string                   `json:"status"`
	StatusReason  string                   `json:"status_reason"`
	SpecSHA256    string                   `json:"spec_sha256"`
	Spec          spec.Spec                `json:"spec"`
	Rig           Rig                      `json:"rig"`
	Environment   Environment              `json:"environment"`
	Timing        Timing                   `json:"timing"`
	Summary       map[string]stats.Summary `json:"summary"`
}

// Rig is the subset of the capability descriptor a consumer needs. The
// agent's endpoint, tags and profiler list are scheduling inputs and are left
// out, so a run does not change shape when an operator retags a rig.
type Rig struct {
	RigID          string `json:"rig_id"`
	HardwareClass  string `json:"hardware_class"`
	Arch           string `json:"arch"`
	OS             string `json:"os"`
	Kernel         string `json:"kernel"`
	CPUModel       string `json:"cpu_model"`
	CPUCores       int    `json:"cpu_cores"`
	MemBytes       int64  `json:"mem_bytes"`
	GPUVendor      string `json:"gpu_vendor"`
	GPUModel       string `json:"gpu_model"`
	GPUMemoryBytes int64  `json:"gpu_memory_bytes"`
	DriverVersion  string `json:"driver_version"`
	Firmware       string `json:"firmware"`
	Emulated       bool   `json:"emulated"`
}

func RigFrom(c capability.Rig) Rig {
	return Rig{
		RigID: c.RigID, HardwareClass: c.HardwareClass, Arch: c.Arch, OS: c.OS,
		Kernel: c.Kernel, CPUModel: c.CPUModel, CPUCores: c.CPUCores,
		MemBytes: c.MemBytes, GPUVendor: c.GPUVendor, GPUModel: c.GPUModel,
		GPUMemoryBytes: c.GPUMemoryBytes, DriverVersion: c.DriverVersion,
		Firmware: c.Firmware, Emulated: c.Emulated,
	}
}

type Environment struct {
	GitRevision     string         `json:"git_revision"`
	BinarySHA256    string         `json:"binary_sha256"`
	ConfigSHA256    string         `json:"config_sha256"`
	Governor        string         `json:"governor"`
	PreflightBefore probe.Readings `json:"preflight_before"`
	PreflightAfter  probe.Readings `json:"preflight_after"`
}

type Timing struct {
	LeaseAcquired string `json:"lease_acquired"`
	Started       string `json:"started"`
	Finished      string `json:"finished"`
}

type Sample struct {
	Metric    string  `json:"metric"`
	Iteration int     `json:"iteration"`
	Warmup    bool    `json:"warmup"`
	Value     float64 `json:"value"`
	Unit      string  `json:"unit"`
	TOffsetNS int64   `json:"t_offset_ns"`
}

type Manifest struct {
	SchemaVersion string      `json:"schema_version"`
	Files         []FileEntry `json:"files"`
}

type FileEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Summarize builds the summary for every declared metric, including ones that
// produced no measured samples, which get n 0 and nulls rather than vanishing.
func Summarize(s spec.Spec, samples []Sample) map[string]stats.Summary {
	values := map[string][]float64{}
	for _, x := range samples {
		if !x.Warmup {
			values[x.Metric] = append(values[x.Metric], x.Value)
		}
	}
	out := make(map[string]stats.Summary, len(s.Metrics))
	for _, m := range s.Metrics {
		out[m.Name] = stats.Summarize(m.Unit, values[m.Name])
	}
	return out
}
