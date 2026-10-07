package probe

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Hand written to the documented csv,noheader,nounits output of
// nvidia-smi, not captured from a device. There is no GPU on the machine this
// repository is developed on, so this fixture is the only thing exercising the
// parser, and docs/known-misses.md says so.
const nvidiaFixture = "NVIDIA RTX A5000, 550.54.14, 37, 54, 24564\n"

func TestParseNvidia(t *testing.T) {
	g, ok := parseNvidia(nvidiaFixture)
	if !ok {
		t.Fatal("did not parse")
	}
	if g.Name != "NVIDIA RTX A5000" || g.Driver != "550.54.14" || g.Util != 0.37 || g.TempC != 54 || g.MemoryTotalBytes != 24564<<20 {
		t.Errorf("%+v", g)
	}
	if _, ok := parseNvidia("[N/A], 550, [N/A], 40, 1"); ok {
		t.Error("parsed a not-available utilization as a number")
	}
}

func TestProfileMarksEmulatedAndInjectsReadings(t *testing.T) {
	dir := t.TempDir()
	gpu := filepath.Join(dir, "gpu")
	os.WriteFile(gpu, []byte("0.42\n"), 0o644)
	p := &Prober{Profile: &Profile{HardwareClass: "gpu-a", GPUVendor: "nvidia", DriverVersion: "550.54", GPUUtilFile: gpu}}
	r := p.Describe(context.Background(), "rig-x")
	if !r.Emulated || r.HardwareClass != "gpu-a" || r.DriverVersion != "550.54" {
		t.Errorf("%+v", r)
	}
	got := p.Read(context.Background())
	if got.GPUUtil == nil || *got.GPUUtil != 0.42 {
		t.Errorf("gpu util %v", got.GPUUtil)
	}
	if got.Load1 == nil || got.MemFree == nil {
		t.Error("host readings missing")
	}
}

func TestNoProfileIsNotEmulated(t *testing.T) {
	r := (&Prober{}).Describe(context.Background(), "rig-y")
	if r.Emulated || r.CPUCores == 0 || r.MemBytes == 0 {
		t.Errorf("%+v", r)
	}
}

func TestParseNvidiaEveryDevice(t *testing.T) {
	out := "NVIDIA RTX A5000, 550.54.14, 3, 41, 24564\nNVIDIA RTX A4000, 550.54.14, 88, 77, 16376\n"
	g, ok := parseNvidia(out)
	if !ok || g.Count != 2 || g.Name != "NVIDIA RTX A5000" || g.Util != 0.88 || g.TempC != 77 || g.MemoryTotalBytes != 16376<<20 {
		t.Errorf("%+v", g)
	}
}

func TestCPUFreqReportsMixed(t *testing.T) {
	root := t.TempDir()
	for i, g := range []string{"performance", "powersave"} {
		d := filepath.Join(root, "cpu"+string(rune('0'+i)), "cpufreq")
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "scaling_governor"), []byte(g), 0o444)
	}
	if got := (CPUFreq{Root: root}).Set("performance"); got != "mixed" && os.Geteuid() != 0 {
		t.Errorf("got %q", got)
	}
	if got := (CPUFreq{Root: t.TempDir()}).Set("performance"); got != "" {
		t.Errorf("no cpufreq must read as empty, got %q", got)
	}
}

func TestReadHostTuningFromFakeRoots(t *testing.T) {
	proc, sys := t.TempDir(), t.TempDir()
	write := func(root, rel, v string) {
		os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755)
		os.WriteFile(filepath.Join(root, rel), []byte(v), 0o644)
	}
	write(proc, "cmdline", "BOOT_IMAGE=/vmlinuz isolcpus=1 nohz_full=1\n")
	write(proc, "sys/kernel/randomize_va_space", "2\n")
	write(proc, "sys/vm/swappiness", "10\n")
	write(sys, "devices/system/cpu/isolated", "1\n")
	write(sys, "devices/system/cpu/nohz_full", "1-2,5\n")
	write(sys, "kernel/mm/transparent_hugepage/enabled", "always madvise [never]\n")
	write(sys, "fs/cgroup/cgroup.controllers", "cpuset cpu memory\n")
	h := ReadHostTuning(context.Background(), HostRoots{Proc: proc, Sys: sys})
	if len(h.IsolatedCPUs) != 1 || h.IsolatedCPUs[0] != 1 || len(h.NoHzFullCPUs) != 3 || h.THP != "never" ||
		h.ASLR != "2" || h.Swappiness != "10" || !h.CgroupV2 {
		t.Errorf("%+v", h)
	}
	empty := ReadHostTuning(context.Background(), HostRoots{Proc: t.TempDir(), Sys: t.TempDir()})
	if len(empty.IsolatedCPUs) != 0 || empty.THP != "" || empty.CgroupV2 {
		t.Errorf("unreadable settings must read as unknown: %+v", empty)
	}
}

// Hand written to chronyc's documented -c tracking format: reference id,
// name, stratum, reference time, system offset in seconds, and so on, ending
// in the leap status.
func TestParseChronyTracking(t *testing.T) {
	synced, off, bound := ParseChronyTracking("A9FEA9FE,169.254.169.254,3,1759860000.123,-0.000012345,0.000001,0.00002,-12.3,0.001,0.02,0.0005,0.0003,64.2,Normal\n")
	if synced == nil || !*synced || off == nil || *off < 0.0123 || *off > 0.0124 {
		t.Errorf("%v %v", synced, off)
	}
	// 0.012345 ms offset + 0.25 ms half delay + 0.3 ms dispersion.
	if bound == nil || *bound < 0.562 || *bound > 0.563 {
		t.Errorf("error bound %v", bound)
	}
	// Captured from a benchgrid rig on GCE whose chrony was syncing to public
	// pool servers through NAT: a small-looking offset under a huge root
	// dispersion, which only the error bound exposes.
	synced, off, bound = ParseChronyTracking("AC681CAF,172.104.28.175,3,1791413683.926132074,0.053211745,-0.046206482,0.046206482,0.000,-2243.897,1000000.000,0.156149998,13.649346352,64.7,Normal\n")
	if !*synced || *off > 54 || *bound < 13000 {
		t.Errorf("pool-synced rig: offset %v bound %v", *off, *bound)
	}
	synced, _, _ = ParseChronyTracking("00000000,,0,0.0,0.0,0.0,0.0,0.0,0.0,0.0,1.0,1.0,0.0,Not synchronised\n")
	if synced == nil || *synced {
		t.Error("an unsynchronised clock read as synchronised")
	}
	if s, o, e := ParseChronyTracking("garbage"); s != nil || o != nil || e != nil {
		t.Error("garbage parsed")
	}
}

func TestParseCPUList(t *testing.T) {
	got := ParseCPUList("0-2,5")
	if len(got) != 4 || got[3] != 5 {
		t.Errorf("%v", got)
	}
	if len(ParseCPUList("3-1")) != 0 || len(ParseCPUList("")) != 0 {
		t.Error("bad lists must read as none")
	}
}

func TestClassLabelsWithoutEmulating(t *testing.T) {
	r := (&Prober{Class: "n2-isolated"}).Describe(context.Background(), "rig-z")
	if r.HardwareClass != "n2-isolated" || r.Emulated {
		t.Errorf("%+v", r)
	}
}
