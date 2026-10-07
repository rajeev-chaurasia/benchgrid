// Package capability describes what a rig is and decides whether it can run a
// spec. Matching is a hard filter that runs before any ranking, so scheduling
// quickly can never mean scheduling onto the wrong machine.
package capability

import (
	"fmt"
	"slices"

	"github.com/rajeev-chaurasia/benchgrid/internal/spec"
)

type Rig struct {
	RigID          string   `json:"rig_id"`
	HardwareClass  string   `json:"hardware_class"`
	Arch           string   `json:"arch"`
	OS             string   `json:"os"`
	Kernel         string   `json:"kernel"`
	CPUModel       string   `json:"cpu_model"`
	CPUCores       int      `json:"cpu_cores"`
	MemBytes       int64    `json:"mem_bytes"`
	GPUVendor      string   `json:"gpu_vendor"`
	GPUModel       string   `json:"gpu_model"`
	GPUMemoryBytes int64    `json:"gpu_memory_bytes"`
	GPUCount       int      `json:"gpu_count"`
	DriverVersion  string   `json:"driver_version"`
	Firmware       string   `json:"firmware"`
	Profilers      []string `json:"profilers"`
	Tags           []string `json:"tags"`
	Governors      []string `json:"governors"`
	Emulated       bool     `json:"emulated"`
	// Endpoint is where the control plane reaches the agent. It is not a
	// capability and never takes part in matching.
	Endpoint string `json:"endpoint"`
}

// Match returns every reason a rig is unsuitable rather than the first, so an
// experiment stuck in the queue can say exactly why no rig will take it.
func Match(req spec.Requirements, env spec.Environment, r Rig) []string {
	var why []string
	miss := func(f string, a ...any) { why = append(why, fmt.Sprintf(f, a...)) }

	if r.Emulated && !req.AllowEmulated {
		miss("rig is emulated and the spec does not allow emulated rigs")
	}
	if req.OS != "" && req.OS != r.OS {
		miss("os %s, need %s", r.OS, req.OS)
	}
	if req.Arch != "" && req.Arch != r.Arch {
		miss("arch %s, need %s", r.Arch, req.Arch)
	}
	if req.HardwareClass != "" && req.HardwareClass != r.HardwareClass {
		miss("hardware_class %s, need %s", r.HardwareClass, req.HardwareClass)
	}
	if req.GPUVendor != "" && req.GPUVendor != r.GPUVendor {
		miss("gpu_vendor %q, need %s", r.GPUVendor, req.GPUVendor)
	}
	if req.MinGPUCount > r.GPUCount {
		miss("gpu count %d, need %d", r.GPUCount, req.MinGPUCount)
	}
	if req.MinGPUMemoryBytes > r.GPUMemoryBytes {
		miss("gpu memory %d, need %d", r.GPUMemoryBytes, req.MinGPUMemoryBytes)
	}
	if req.Driver != "" {
		c, err := spec.ParseConstraint(req.Driver)
		if err != nil || !c.Allows(r.DriverVersion) {
			miss("driver %q does not satisfy %s", r.DriverVersion, req.Driver)
		}
	}
	if req.MinCPUCores > r.CPUCores {
		miss("cpu cores %d, need %d", r.CPUCores, req.MinCPUCores)
	}
	if req.MinMemBytes > r.MemBytes {
		miss("memory %d, need %d", r.MemBytes, req.MinMemBytes)
	}
	for _, t := range req.Tags {
		if !slices.Contains(r.Tags, t) {
			miss("missing tag %s", t)
		}
	}
	for _, p := range req.Profilers {
		if !slices.Contains(r.Profilers, p) {
			miss("missing profiler %s", p)
		}
	}
	if env.GovernorRequired && !slices.Contains(r.Governors, env.CPUGovernor) {
		miss("cannot set governor %s", env.CPUGovernor)
	}
	return why
}
