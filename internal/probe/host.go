package probe

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// HostTuning is the state of the kernel settings that change benchmark
// variance. It is read on every attempt and published with the run as
// host.json, so a reader can see what a run actually had rather than what the
// rig was provisioned to have.
type HostTuning struct {
	Kernel        string   `json:"kernel_cmdline"`
	IsolatedCPUs  []int    `json:"isolated_cpus"`
	NoHzFullCPUs  []int    `json:"nohz_full_cpus"`
	THP           string   `json:"transparent_hugepages"`
	ASLR          string   `json:"aslr"`
	Swappiness    string   `json:"swappiness"`
	CgroupV2      bool     `json:"cgroup_v2"`
	ClockSynced   *bool    `json:"clock_synced"`
	ClockOffsetMS *float64 `json:"clock_offset_ms"`
	ClockSource   string   `json:"clock_source"`
}

// HostRoots lets the readers run against a directory laid out like /proc and
// /sys, which is the only way to test them on a machine without them.
type HostRoots struct {
	Proc string
	Sys  string
}

func (r HostRoots) proc() string {
	if r.Proc == "" {
		return "/proc"
	}
	return r.Proc
}

func (r HostRoots) sys() string {
	if r.Sys == "" {
		return "/sys"
	}
	return r.Sys
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ReadHostTuning reads what it can and leaves the rest empty or nil. A
// setting that cannot be read is recorded as unknown, never as a default.
func ReadHostTuning(ctx context.Context, roots HostRoots) HostTuning {
	t := HostTuning{
		Kernel:     readTrim(filepath.Join(roots.proc(), "cmdline")),
		ASLR:       readTrim(filepath.Join(roots.proc(), "sys/kernel/randomize_va_space")),
		Swappiness: readTrim(filepath.Join(roots.proc(), "sys/vm/swappiness")),
	}
	t.IsolatedCPUs = ParseCPUList(readTrim(filepath.Join(roots.sys(), "devices/system/cpu/isolated")))
	t.NoHzFullCPUs = ParseCPUList(readTrim(filepath.Join(roots.sys(), "devices/system/cpu/nohz_full")))
	// The active THP mode is the bracketed word: "always [madvise] never".
	if thp := readTrim(filepath.Join(roots.sys(), "kernel/mm/transparent_hugepage/enabled")); thp != "" {
		if i, j := strings.IndexByte(thp, '['), strings.IndexByte(thp, ']'); i >= 0 && j > i {
			t.THP = thp[i+1 : j]
		}
	}
	_, err := os.Stat(filepath.Join(roots.sys(), "fs/cgroup/cgroup.controllers"))
	t.CgroupV2 = err == nil
	t.ClockSource = readTrim(filepath.Join(roots.sys(), "devices/system/clocksource/clocksource0/current_clocksource"))
	t.ClockSynced, t.ClockOffsetMS = clockState(ctx)
	return t
}

// ParseCPUList reads the kernel's CPU list format, "0-2,5", which is how
// isolcpus and nohz_full report. An empty list reads as none.
func ParseCPUList(s string) []int {
	out := []int{}
	for _, part := range strings.Split(strings.TrimSpace(s), ",") {
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return []int{}
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil || b < a {
				return []int{}
			}
		}
		for c := a; c <= b; c++ {
			out = append(out, c)
		}
	}
	return out
}

// clockState asks chrony how far the system clock is from its reference.
// GCE and most Linux distributions run chrony; where it is absent the state
// is unknown, which a spec that requires a synchronized clock treats as a
// failure.
func clockState(ctx context.Context) (*bool, *float64) {
	if _, err := exec.LookPath("chronyc"); err != nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "chronyc", "-c", "tracking").Output()
	if err != nil {
		return nil, nil
	}
	return ParseChronyTracking(string(out))
}

// ParseChronyTracking reads `chronyc -c tracking`: the fifth field is the
// system clock's offset from the reference in seconds, and the last is the
// leap status, which is "Not synchronised" when chrony has no reference.
func ParseChronyTracking(out string) (*bool, *float64) {
	f := strings.Split(strings.TrimSpace(out), ",")
	if len(f) < 14 {
		return nil, nil
	}
	off, err := strconv.ParseFloat(f[4], 64)
	if err != nil {
		return nil, nil
	}
	synced := !strings.Contains(strings.ToLower(f[len(f)-1]), "not synchronised")
	ms := off * 1000
	if ms < 0 {
		ms = -ms
	}
	return &synced, &ms
}
