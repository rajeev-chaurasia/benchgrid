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
	// ClockErrorMS bounds how far the clock can be from true time: the offset
	// from chrony's reference plus half the round trip to the root and the
	// root's dispersion. An offset alone says nothing when the reference is
	// itself unreliable, which is what a clock synced to distant pool servers
	// through NAT looks like.
	ClockErrorMS *float64 `json:"clock_error_bound_ms"`
	ClockSource  string   `json:"clock_source"`
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
	t.ClockSynced, t.ClockOffsetMS, t.ClockErrorMS = clockState(ctx)
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
func clockState(ctx context.Context) (*bool, *float64, *float64) {
	if _, err := exec.LookPath("chronyc"); err != nil {
		return nil, nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "chronyc", "-c", "tracking").Output()
	if err != nil {
		return nil, nil, nil
	}
	return ParseChronyTracking(string(out))
}

// ParseChronyTracking reads `chronyc -c tracking`: field 4 is the system
// clock's offset from the reference, fields 10 and 11 the root delay and root
// dispersion, all in seconds, and the last is the leap status, which is
// "Not synchronised" when chrony has no reference.
func ParseChronyTracking(out string) (synced *bool, offsetMS, errorMS *float64) {
	f := strings.Split(strings.TrimSpace(out), ",")
	if len(f) < 14 {
		return nil, nil, nil
	}
	off, err1 := strconv.ParseFloat(f[4], 64)
	delay, err2 := strconv.ParseFloat(f[10], 64)
	disp, err3 := strconv.ParseFloat(f[11], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, nil, nil
	}
	s := !strings.Contains(strings.ToLower(f[len(f)-1]), "not synchronised")
	if off < 0 {
		off = -off
	}
	o := off * 1000
	e := (off + delay/2 + disp) * 1000
	return &s, &o, &e
}
