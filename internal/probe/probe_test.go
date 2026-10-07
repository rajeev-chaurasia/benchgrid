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
