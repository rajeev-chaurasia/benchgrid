package spec

import (
	"fmt"
	"strconv"
	"strings"
)

// Constraint is a single comparison against a dotted numeric version, which is
// the shape of every driver and firmware version this scheduler matches on.
// Ranges are written as two requirements rather than a grammar.
type Constraint struct {
	Op      string
	Version []int
}

func ParseConstraint(s string) (Constraint, error) {
	s = strings.TrimSpace(s)
	for _, op := range []string{">=", "<=", ">", "<", "="} {
		if strings.HasPrefix(s, op) {
			v, err := ParseVersion(strings.TrimSpace(s[len(op):]))
			return Constraint{op, v}, err
		}
	}
	v, err := ParseVersion(s)
	return Constraint{"=", v}, err
}

func ParseVersion(s string) ([]int, error) {
	if s == "" {
		return nil, fmt.Errorf("empty version")
	}
	parts := strings.Split(s, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("version %q is not dotted numeric", s)
		}
		out[i] = n
	}
	return out, nil
}

// Compare pads the shorter version with zeros, so 550 and 550.0.0 are equal.
func Compare(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func (c Constraint) Allows(version string) bool {
	v, err := ParseVersion(version)
	if err != nil {
		return false
	}
	r := Compare(v, c.Version)
	switch c.Op {
	case ">=":
		return r >= 0
	case ">":
		return r > 0
	case "<=":
		return r <= 0
	case "<":
		return r < 0
	default:
		return r == 0
	}
}
