//go:build linux

package main

import "golang.org/x/sys/unix"

// allowedCPUs is how many CPUs this process may run on, which is how a test
// sees from inside a benchmark whether the agent pinned it.
func allowedCPUs() int {
	var set unix.CPUSet
	if unix.SchedGetaffinity(0, &set) != nil {
		return -1
	}
	return set.Count()
}
