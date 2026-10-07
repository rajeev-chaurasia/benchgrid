package agent

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/rajeev-chaurasia/benchgrid/internal/probe"
)

// ParseCPUs reads a CPU list such as "1" or "2-3,6".
func ParseCPUs(s string) []int { return probe.ParseCPUList(s) }

func FormatCPUs(cpus []int) string {
	parts := make([]string, len(cpus))
	for i, c := range cpus {
		parts[i] = strconv.Itoa(c)
	}
	return strings.Join(parts, ",")
}

func parsePinArgs(args []string) (cpus []int, cgroup string, argv []string, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cpus":
			if i+1 >= len(args) {
				return nil, "", nil, fmt.Errorf("pin: --cpus needs a value")
			}
			i++
			cpus = ParseCPUs(args[i])
		case "--cgroup":
			if i+1 >= len(args) {
				return nil, "", nil, fmt.Errorf("pin: --cgroup needs a value")
			}
			i++
			cgroup = args[i]
		case "--":
			argv = args[i+1:]
			i = len(args)
		default:
			return nil, "", nil, fmt.Errorf("pin: unexpected %q", args[i])
		}
	}
	if len(cpus) == 0 || len(argv) == 0 {
		return nil, "", nil, fmt.Errorf("usage: rigagent pin --cpus LIST [--cgroup DIR] -- COMMAND")
	}
	return cpus, cgroup, argv, nil
}
