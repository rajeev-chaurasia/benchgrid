//go:build !linux

package agent

import "fmt"

// PinMain exists only on Linux, which is the only platform where a rig
// agent pins benchmarks: macOS offers no CPU affinity to a process.
func PinMain(args []string) error { return fmt.Errorf("pin: not supported on this platform") }

func setupCgroups(benchCPUs []int) (string, error) {
	return "", fmt.Errorf("cgroups: not supported on this platform")
}

func pinSupported() bool { return false }
