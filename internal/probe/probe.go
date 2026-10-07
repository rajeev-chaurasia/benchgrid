// Package probe answers two questions about the machine an agent runs on:
// what it is, for capability matching, and what state it is in right now, for
// the preflight gate. A reading the machine cannot take is nil, never zero,
// because zero GPU utilization and no GPU are different facts.
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/rajeev-chaurasia/benchgrid/internal/capability"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/sensors"
)

type Readings struct {
	Load1   *float64 `json:"load1"`
	CPUUtil *float64 `json:"cpu_util"`
	MemFree *int64   `json:"mem_free"`
	GPUUtil *float64 `json:"gpu_util"`
	TempC   *float64 `json:"temp_c"`
}

// Profile overrides what the machine reports about itself, so one host can
// stand in for a rig it is not. Any override marks the rig emulated, and that
// flag is copied into every run the rig produces, so an emulated result can
// never be mistaken for a measurement on the hardware it imitates.
type Profile struct {
	HardwareClass  string   `json:"hardware_class,omitempty"`
	GPUVendor      string   `json:"gpu_vendor,omitempty"`
	GPUModel       string   `json:"gpu_model,omitempty"`
	GPUMemoryBytes int64    `json:"gpu_memory_bytes,omitempty"`
	GPUCount       int      `json:"gpu_count,omitempty"`
	DriverVersion  string   `json:"driver_version,omitempty"`
	Firmware       string   `json:"firmware,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	Profilers      []string `json:"profilers,omitempty"`
	// Injected readings for hardware the host does not have. A file holding a
	// number is read on every preflight, so a test can drive an emulated rig
	// hot or busy and watch the gate respond.
	GPUUtilFile string `json:"gpu_util_file,omitempty"`
	TempFile    string `json:"temp_file,omitempty"`
}

func LoadProfile(path string) (*Profile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Profile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("profile %s: %w", path, err)
	}
	return &p, nil
}

type Prober struct {
	Profile *Profile
	// CPUWindow is how long cpu_util is sampled over. Shorter is noisier.
	CPUWindow time.Duration
	CPUFreq   CPUFreq
}

func (p *Prober) Describe(ctx context.Context, rigID string) capability.Rig {
	r := capability.Rig{
		RigID:     rigID,
		Arch:      NormalizeArch(runtime.GOARCH),
		OS:        runtime.GOOS,
		CPUCores:  runtime.NumCPU(),
		Profilers: detectProfilers(),
		Tags:      []string{},
	}
	r.HardwareClass = r.OS + "-" + r.Arch
	if k, err := host.KernelVersionWithContext(ctx); err == nil {
		r.Kernel = k
	}
	if infos, err := cpu.InfoWithContext(ctx); err == nil && len(infos) > 0 {
		r.CPUModel = strings.TrimSpace(infos[0].ModelName)
		if r.CPUModel == "" {
			// ARM Linux reports no model name, only implementer and part
			// numbers, which identify the core even if they read poorly.
			r.CPUModel = strings.TrimSpace(infos[0].VendorID + " " + infos[0].Family + " " + infos[0].Model)
		}
	}
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		r.MemBytes = int64(vm.Total)
	}
	if g, ok := queryNvidia(ctx); ok {
		r.GPUVendor, r.GPUModel, r.DriverVersion = "nvidia", g.Name, g.Driver
		r.GPUMemoryBytes, r.GPUCount = g.MemoryTotalBytes, g.Count
	}
	r.Governors = p.CPUFreq.Available()

	if pr := p.Profile; pr != nil {
		r.Emulated = true
		set := func(dst *string, v string) {
			if v != "" {
				*dst = v
			}
		}
		set(&r.HardwareClass, pr.HardwareClass)
		set(&r.GPUVendor, pr.GPUVendor)
		set(&r.GPUModel, pr.GPUModel)
		set(&r.DriverVersion, pr.DriverVersion)
		set(&r.Firmware, pr.Firmware)
		if pr.GPUMemoryBytes > 0 {
			r.GPUMemoryBytes = pr.GPUMemoryBytes
		}
		if pr.GPUCount > 0 {
			r.GPUCount = pr.GPUCount
		} else if pr.GPUVendor != "" && r.GPUCount == 0 {
			r.GPUCount = 1
		}
		if pr.Tags != nil {
			r.Tags = pr.Tags
		}
		if pr.Profilers != nil {
			r.Profilers = pr.Profilers
		}
	}
	return r
}

func (p *Prober) Read(ctx context.Context) Readings {
	var out Readings
	if avg, err := load.AvgWithContext(ctx); err == nil {
		out.Load1 = &avg.Load1
	}
	window := p.CPUWindow
	if window == 0 {
		window = 250 * time.Millisecond
	}
	if pct, err := cpu.PercentWithContext(ctx, window, false); err == nil && len(pct) == 1 {
		v := pct[0] / 100
		out.CPUUtil = &v
	}
	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		v := int64(vm.Available)
		out.MemFree = &v
	}
	if g, ok := queryNvidia(ctx); ok {
		out.GPUUtil, out.TempC = &g.Util, &g.TempC
	} else if t, ok := hottestSensor(ctx); ok {
		out.TempC = &t
	}
	if pr := p.Profile; pr != nil {
		if v, ok := readNumber(pr.GPUUtilFile); ok {
			out.GPUUtil = &v
		}
		if v, ok := readNumber(pr.TempFile); ok {
			out.TempC = &v
		}
	}
	return out
}

func NormalizeArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "aarch64":
		return "arm64"
	}
	return goarch
}

func detectProfilers() []string {
	found := []string{}
	for _, name := range []string{"perf", "nsys", "ncu"} {
		if _, err := exec.LookPath(name); err == nil {
			found = append(found, name)
		}
	}
	return found
}

// CPUFreq reads and sets the kernel's cpufreq governor under Root, which is
// /sys/devices/system/cpu on a real Linux rig. It is a type rather than a
// constant path so the logic can be exercised against a directory tree on any
// machine, including ones whose kernel exposes no cpufreq at all, which is
// every machine this repository's evidence ran on.
type CPUFreq struct{ Root string }

const DefaultCPUFreqRoot = "/sys/devices/system/cpu"

func (c CPUFreq) root() string {
	if c.Root == "" {
		return DefaultCPUFreqRoot
	}
	return c.Root
}

func (c CPUFreq) Available() []string {
	b, err := os.ReadFile(filepath.Join(c.root(), "cpu0", "cpufreq", "scaling_available_governors"))
	if err != nil {
		return []string{}
	}
	return strings.Fields(string(b))
}

// Current is empty where the kernel exposes no cpufreq, which includes macOS
// and most containers.
func (c CPUFreq) Current() string {
	b, err := os.ReadFile(filepath.Join(c.root(), "cpu0", "cpufreq", "scaling_governor"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Set asks for gov on every CPU and reports what is in force afterwards, on
// every CPU, never the requested value on faith. If any CPU disagrees with
// cpu0 the result is "mixed", which no spec can require.
func (c CPUFreq) Set(gov string) string {
	paths, _ := filepath.Glob(filepath.Join(c.root(), "cpu[0-9]*", "cpufreq", "scaling_governor"))
	if len(paths) == 0 {
		return ""
	}
	for _, p := range paths {
		os.WriteFile(p, []byte(gov), 0o644)
	}
	first := ""
	for i, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return "mixed"
		}
		g := strings.TrimSpace(string(b))
		if i == 0 {
			first = g
		} else if g != first {
			return "mixed"
		}
	}
	return first
}

func hottestSensor(ctx context.Context) (float64, bool) {
	temps, err := sensors.TemperaturesWithContext(ctx)
	if err != nil && len(temps) == 0 {
		return 0, false
	}
	best, ok := 0.0, false
	for _, t := range temps {
		if t.Temperature > 0 && t.Temperature < 150 && (!ok || t.Temperature > best) {
			best, ok = t.Temperature, true
		}
	}
	return best, ok
}

func readNumber(path string) (float64, bool) {
	if path == "" {
		return 0, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	return v, err == nil
}

// BusyCPUSeconds is the total non-idle CPU time across every core since boot.
// Two readings around an interval, less the time the benchmark itself used,
// give the background load during that interval, which a reading taken
// before it cannot.
func BusyCPUSeconds(ctx context.Context) (float64, bool) {
	t, err := cpu.TimesWithContext(ctx, false)
	if err != nil || len(t) != 1 {
		return 0, false
	}
	return t[0].Total() - t[0].Idle - t[0].Iowait, true
}
