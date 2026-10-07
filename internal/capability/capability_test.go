package capability

import (
	"strings"
	"testing"

	"github.com/rajeev-chaurasia/benchgrid/internal/spec"
)

var gpuRig = Rig{
	RigID: "rig-07", HardwareClass: "gpu-a", Arch: "x86_64", CPUCores: 16,
	MemBytes: 64 << 30, GPUVendor: "nvidia", GPUMemoryBytes: 24 << 30,
	DriverVersion: "550.54", Profilers: []string{"perf", "nsight"},
	Tags: []string{"gpu", "isolated"}, Governors: []string{"performance", "powersave"},
}

func TestMatchAccepts(t *testing.T) {
	req := spec.Requirements{
		Arch: "x86_64", GPUVendor: "nvidia", MinGPUMemoryBytes: 16 << 30,
		Driver: ">=550", Tags: []string{"gpu", "isolated"}, Profilers: []string{"perf"},
	}
	env := spec.Environment{CPUGovernor: "performance", GovernorRequired: true}
	if why := Match(req, env, gpuRig); len(why) != 0 {
		t.Errorf("rejected: %v", why)
	}
}

func TestMatchReportsEveryReason(t *testing.T) {
	r := gpuRig
	r.Emulated = true
	r.DriverVersion = "535.183.01"
	r.Tags = nil
	req := spec.Requirements{Driver: ">=550", Tags: []string{"isolated"}}
	env := spec.Environment{CPUGovernor: "userspace", GovernorRequired: true}
	why := strings.Join(Match(req, env, r), "; ")
	for _, want := range []string{"emulated", "driver", "isolated", "governor"} {
		if !strings.Contains(why, want) {
			t.Errorf("missing reason %q in %q", want, why)
		}
	}
}

func TestGovernorBestEffortDoesNotFilter(t *testing.T) {
	r := gpuRig
	r.Governors = nil
	env := spec.Environment{CPUGovernor: "performance"}
	if why := Match(spec.Requirements{}, env, r); len(why) != 0 {
		t.Errorf("best effort governor filtered a rig: %v", why)
	}
}

func TestMatchOSAndGPUCount(t *testing.T) {
	r := gpuRig
	r.OS, r.GPUCount = "linux", 1
	why := Match(spec.Requirements{OS: "darwin", MinGPUCount: 2}, spec.Environment{}, r)
	if len(why) != 2 {
		t.Errorf("%v", why)
	}
}
