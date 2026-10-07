//go:build linux

package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// PinMain is `rigagent pin --cpus LIST [--cgroup DIR] -- COMMAND...`. It pins
// itself to the bench CPUs, joins the bench cgroup, and then becomes the
// benchmark with exec, so the benchmark is pinned from its first instruction
// and keeps the pid and process group the agent is already tracking.
func PinMain(args []string) error {
	cpus, cgroup, argv, err := parsePinArgs(args)
	if err != nil {
		return err
	}
	var set unix.CPUSet
	set.Zero()
	for _, c := range cpus {
		set.Set(c)
	}
	if err := unix.SchedSetaffinity(0, &set); err != nil {
		return fmt.Errorf("pin: %w", err)
	}
	if cgroup != "" {
		if err := os.WriteFile(filepath.Join(cgroup, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			return fmt.Errorf("pin: join %s: %w", cgroup, err)
		}
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, argv, os.Environ())
}

// setupCgroups gives the agent a cgroup tree of its own under the one systemd
// started it in, which needs Delegate=yes on the unit: the agent itself moves
// to an "agent" leaf kept off the bench CPUs, and benchmarks join a "bench"
// leaf that owns them. cgroup v2 forbids processes in a cgroup that has
// children with controllers enabled, which is why the agent has to move into
// a leaf of its own first.
func setupCgroups(benchCPUs []int) (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	var rel string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(line, "0::") {
			rel = strings.TrimPrefix(line, "0::")
		}
	}
	if rel == "" {
		return "", fmt.Errorf("not on cgroup v2")
	}
	root := filepath.Join("/sys/fs/cgroup", rel)
	agent, bench := filepath.Join(root, "agent"), filepath.Join(root, "bench")
	for _, d := range []string{agent, bench} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(agent, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return "", fmt.Errorf("move agent: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte("+cpuset"), 0o644); err != nil {
		return "", fmt.Errorf("enable cpuset: %w", err)
	}
	all, other := []string{}, []string{}
	n := 0
	if b, err := os.ReadFile("/sys/fs/cgroup/cpuset.cpus.effective"); err == nil {
		n = len(ParseCPUs(strings.TrimSpace(string(b))))
	}
	bset := map[int]bool{}
	for _, c := range benchCPUs {
		bset[c] = true
		all = append(all, strconv.Itoa(c))
	}
	for c := 0; c < n; c++ {
		if !bset[c] {
			other = append(other, strconv.Itoa(c))
		}
	}
	if err := os.WriteFile(filepath.Join(bench, "cpuset.cpus"), []byte(strings.Join(all, ",")), 0o644); err != nil {
		return "", fmt.Errorf("bench cpuset: %w", err)
	}
	if len(other) > 0 {
		os.WriteFile(filepath.Join(agent, "cpuset.cpus"), []byte(strings.Join(other, ",")), 0o644)
	}
	return bench, nil
}

func pinSupported() bool { return true }
