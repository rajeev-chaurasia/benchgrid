package evidence

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/rajeev-chaurasia/benchgrid/internal/agent"
	"github.com/rajeev-chaurasia/benchgrid/internal/artifact"
	"github.com/rajeev-chaurasia/benchgrid/internal/capability"
)

type Fault struct {
	Kind    string `json:"kind"`
	Target  string `json:"target"`
	AtNS    int64  `json:"at_ns"`
	UntilNS int64  `json:"until_ns"`
}

type ChaosExperiment struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Expected string `json:"expected"`
	State    string `json:"state"`
	Reason   string `json:"reason"`
	Attempts int    `json:"attempts"`
}

// ExpectedFor is what an experiment of each kind should end as if the system
// recovered correctly from everything done to it: a sound benchmark succeeds,
// and a broken one fails rather than being retried into a false success.
func ExpectedFor(kind string) string {
	if kind == "normal" {
		return "SUCCEEDED"
	}
	return "FAILED"
}

type Quarantine struct {
	Rig    string `json:"rig"`
	Reason string `json:"reason"`
}

type ChaosEnd struct {
	RigsStillLeased int          `json:"rigs_still_leased"`
	Quarantined     []Quarantine `json:"quarantined"`
}

type Miss struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Expected string `json:"expected"`
	State    string `json:"state"`
	Reason   string `json:"reason"`
	Attempts int    `json:"attempts"`
}

type ChaosSummary struct {
	Replicas        int            `json:"replicas"`
	Rigs            int            `json:"rigs"`
	Experiments     int            `json:"experiments"`
	ByKind          map[string]int `json:"by_kind"`
	Faults          map[string]int `json:"faults"`
	AsExpected      int            `json:"as_expected"`
	Misses          []Miss         `json:"misses"`
	AttemptsHist    map[string]int `json:"attempts_histogram"`
	ArtifactsSealed int            `json:"final_attempts_sealed_and_verified"`
	ArtifactErrors  []string       `json:"artifact_errors"`
	// PlacementViolations counts verified runs whose rig, as the run itself
	// records it, does not satisfy the run's own spec.
	PlacementViolations int `json:"placement_violations"`
	// StaleRefused counts stale dispatches the rigs refused during chaos. The
	// fence run aims its freezes; these come from freezes and kills at random
	// moments, so this is how often the dangerous case arises unaided.
	StaleRefused    int          `json:"stale_refused"`
	ProcessOverlaps int          `json:"process_overlaps"`
	SessionOverlaps int          `json:"session_overlaps"`
	ProcessRuns     int          `json:"process_intervals"`
	RigsLeasedAtEnd int          `json:"rigs_still_leased_at_end"`
	QuarantinedRigs []Quarantine `json:"quarantined_rigs"`
}

// SummarizeChaos counts outcomes against expectations and checks, for every
// experiment, that the sealed artifact of its final attempt exists, verifies,
// and records the same status the control plane reports. A control plane that
// says SUCCEEDED with no verifiable run behind it has not recovered.
func SummarizeChaos(exps []ChaosExperiment, faults []Fault, intervals []agent.Interval, end ChaosEnd, store string) (ChaosSummary, error) {
	s := ChaosSummary{
		Experiments: len(exps), ByKind: map[string]int{}, Faults: map[string]int{},
		AttemptsHist: map[string]int{}, Misses: []Miss{}, ArtifactErrors: []string{},
		RigsLeasedAtEnd: end.RigsStillLeased, QuarantinedRigs: end.Quarantined,
	}
	if s.QuarantinedRigs == nil {
		s.QuarantinedRigs = []Quarantine{}
	}
	for _, f := range faults {
		s.Faults[f.Kind]++
	}
	for _, e := range exps {
		s.ByKind[e.Kind]++
		s.AttemptsHist[strconv.Itoa(e.Attempts)]++
		if e.State == e.Expected {
			s.AsExpected++
		} else {
			s.Misses = append(s.Misses, Miss(e))
		}
		want, required := finalArtifact(e)
		dir := artifact.AttemptDir(store, e.ID, e.Attempts)
		if !required {
			continue
		}
		if !sealed(dir) {
			s.ArtifactErrors = append(s.ArtifactErrors, fmt.Sprintf("%s attempt %d: not sealed", e.ID, e.Attempts))
			continue
		}
		run, _, err := artifact.Verify(dir)
		switch {
		case err != nil:
			s.ArtifactErrors = append(s.ArtifactErrors, err.Error())
		case run.Status != want:
			s.ArtifactErrors = append(s.ArtifactErrors, fmt.Sprintf("%s: control plane implies %s, artifact says %s", e.ID, want, run.Status))
		default:
			s.ArtifactsSealed++
		}
		if err == nil && !Placed(run) {
			s.PlacementViolations++
		}
	}
	sort.Slice(s.Misses, func(i, j int) bool { return s.Misses[i].ID < s.Misses[j].ID })
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
		}
	}
	s.ProcessRuns = len(proc)
	s.ProcessOverlaps, s.SessionOverlaps = OverlapPairs(proc), OverlapPairs(sess)
	return s, nil
}

func sealed(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, artifact.ManifestFile))
	return err == nil
}

// finalArtifact says what status the sealed artifact of an experiment's final
// attempt must carry, and whether one must exist at all. An experiment that
// ran out of attempts because its last lease was lost never ran that attempt,
// so there is nothing to seal; one that ran out because its last attempt was
// invalid has an INVALID artifact behind a FAILED experiment.
func finalArtifact(e ChaosExperiment) (status string, required bool) {
	rest, exhausted := strings.CutPrefix(e.Reason, "attempts_exhausted:")
	if !exhausted {
		return e.State, true
	}
	for _, st := range []string{"INVALID", "FAILED"} {
		if strings.HasPrefix(rest, st+":") {
			return st, true
		}
	}
	return "", false
}

// Placed reports whether a run's rig satisfied its spec, judged from the run
// alone. Tags, profilers and GPU count are scheduling inputs the run does not
// record, so they are checked through the hardware class they come with.
func Placed(run artifact.Run) bool {
	r := run.Rig
	req := run.Spec.Requirements
	req.Tags, req.Profilers, req.MinGPUCount = nil, nil, 0
	rig := capability.Rig{
		RigID: r.RigID, HardwareClass: r.HardwareClass, Arch: r.Arch, OS: r.OS,
		CPUCores: r.CPUCores, MemBytes: r.MemBytes, GPUVendor: r.GPUVendor,
		GPUModel: r.GPUModel, GPUMemoryBytes: r.GPUMemoryBytes,
		DriverVersion: r.DriverVersion, Firmware: r.Firmware, Emulated: r.Emulated,
	}
	env := run.Spec.Environment
	env.GovernorRequired = false
	return len(capability.Match(req, env, rig)) == 0
}
