package evidence

import (
	"strconv"

	"github.com/rajeev-chaurasia/benchgrid/internal/agent"
)

type Freeze struct {
	Server     string `json:"server"`
	Kind       string `json:"kind"`
	DetectedNS int64  `json:"detected_ns"`
	ResumedNS  int64  `json:"resumed_ns"`
}

type FenceSummary struct {
	Mode        string         `json:"mode"`
	Replicas    int            `json:"replicas"`
	Rigs        int            `json:"rigs"`
	Experiments int            `json:"experiments"`
	States      map[string]int `json:"states"`
	Freezes     int            `json:"freezes"`
	// Partitions counts times a rig was cut off from the network while
	// running, for longer than its lease. Only the Linux run can do this,
	// because only a container's network can be disconnected from outside.
	Partitions       int `json:"partitions"`
	ProcessIntervals int `json:"process_intervals"`
	SessionIntervals int `json:"session_intervals"`
	// StaleArrivals counts dispatches that reached a rig carrying a fence
	// lower than one the rig had already seen. In the fenced mode they were
	// refused; in the control they were run. Either way a nonzero count is
	// what shows the freezes landed where they matter.
	StaleArrivals   int `json:"stale_arrivals"`
	StaleRefused    int `json:"stale_refused"`
	ProcessOverlaps int `json:"process_overlaps"`
	SessionOverlaps int `json:"session_overlaps"`
}

// SummarizeFence derives every interval count from the agents' raw logs.
// States and the experiment count come from the database at the end of the
// run and are published alongside, not recomputed.
func SummarizeFence(base FenceSummary, intervals []agent.Interval, freezes []Freeze, partitions []Fault) FenceSummary {
	s := base
	s.Freezes = len(freezes)
	s.Partitions = len(partitions)
	var proc, sess []Span
	for _, iv := range intervals {
		sp := Span{Resource: iv.RigID, Owner: strconv.FormatInt(iv.Fence, 10), Start: iv.StartNS, End: iv.EndNS}
		switch iv.Kind {
		case "process":
			proc = append(proc, sp)
		case "session":
			sess = append(sess, sp)
		case "rejected":
			s.StaleRefused++
			s.StaleArrivals++
		case "stale_accepted":
			s.StaleArrivals++
		}
	}
	s.ProcessIntervals, s.SessionIntervals = len(proc), len(sess)
	s.ProcessOverlaps = OverlapPairs(proc)
	s.SessionOverlaps = OverlapPairs(sess)
	return s
}
