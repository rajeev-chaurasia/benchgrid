package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// groupMarker is what an agent writes to disk for every benchmark process
// group it starts, so that if it dies, the next agent on the rig can find the
// group, prove the id still means the same group, kill it, and record how long
// it ran.
type groupMarker struct {
	PGID         int    `json:"pgid"`
	Fence        int64  `json:"fence"`
	ExperimentID string `json:"experiment_id"`
	Attempt      int    `json:"attempt"`
	StartNS      int64  `json:"start_ns"`
	// CreateTimeMS is the group leader's creation time as the kernel reports
	// it. A process group id is only reused once the old group is gone, and
	// the reused id then belongs to a leader with a different creation time,
	// so a mismatch means the id no longer names anything this agent started.
	CreateTimeMS int64 `json:"create_time_ms"`
}

const launchingPrefix = "launching-"

func (a *Agent) pgroupDir() string { return filepath.Join(a.cfg.StateDir, "pgroups") }

func (a *Agent) launchingPath(s *session) string {
	return filepath.Join(a.pgroupDir(), fmt.Sprintf("%s%d-%s-%d", launchingPrefix, s.fence, s.d.ExperimentID, s.d.Attempt))
}

// markLaunching is written before a benchmark is started. Its id is not known
// until after, so without this an agent killed between the two would leave a
// process nobody recorded. A launching marker found at startup cannot be
// resolved to a process, so it quarantines the rig rather than guess.
func (a *Agent) markLaunching(s *session) {
	writeSynced(a.launchingPath(s), nil)
}

func (a *Agent) trackGroup(pgid int, owner *session, startNS int64) {
	m := groupMarker{PGID: pgid, Fence: owner.fence, ExperimentID: owner.d.ExperimentID,
		Attempt: owner.d.Attempt, StartNS: startNS}
	if p, err := process.NewProcess(int32(pgid)); err == nil {
		m.CreateTimeMS, _ = p.CreateTime()
	}
	b, _ := json.Marshal(m)
	a.smu.Lock()
	a.pgroups[pgid] = owner
	a.smu.Unlock()
	writeSynced(filepath.Join(a.pgroupDir(), strconv.Itoa(pgid)), b)
	os.Remove(a.launchingPath(owner))
}

func (a *Agent) untrackGroup(pgid int) {
	a.smu.Lock()
	delete(a.pgroups, pgid)
	a.smu.Unlock()
	os.Remove(filepath.Join(a.pgroupDir(), strconv.Itoa(pgid)))
}

// reapPrevious deals with everything a previous incarnation of this agent
// started and did not see finish. Each group still alive is killed, and its
// lifetime is recorded as a process interval from the start its marker holds
// to the moment it was reaped. A group already gone is recorded up to now,
// which is an upper bound on when it stopped. Recording the upper bound is
// safe for the overlap count, because no new work could start on the rig
// while no agent was running, so the interval cannot be stretched over
// anything it did not actually share the rig with.
func (a *Agent) reapPrevious() (string, error) {
	entries, err := os.ReadDir(a.pgroupDir())
	if err != nil {
		return "", err
	}
	var launching, survived []string
	for _, e := range entries {
		path := filepath.Join(a.pgroupDir(), e.Name())
		if strings.HasPrefix(e.Name(), launchingPrefix) {
			launching = append(launching, e.Name())
			os.Remove(path)
			continue
		}
		b, err := os.ReadFile(path)
		var m groupMarker
		if err != nil || json.Unmarshal(b, &m) != nil || m.PGID <= 0 {
			// A marker that cannot be read names a group that cannot be
			// checked, which is the same position as a launching marker.
			launching = append(launching, e.Name())
			os.Remove(path)
			continue
		}
		outcome := a.reapOne(m)
		if outcome == "survived" {
			survived = append(survived, e.Name())
			continue
		}
		os.Remove(path)
		if outcome == "id_reused" {
			a.log.Warn("process group id reused since the last agent; left alone", "pgid", m.PGID)
			continue
		}
		a.intervals.Record(Interval{RigID: a.cfg.RigID, Kind: "process", Fence: m.Fence,
			ExperimentID: m.ExperimentID, Attempt: m.Attempt, PID: m.PGID,
			StartNS: m.StartNS, EndNS: a.clock(), Outcome: outcome})
		a.log.Warn("process group left by a previous agent", "pgid", m.PGID, "outcome", outcome)
	}
	switch {
	case len(survived) > 0:
		return "stale_process_at_start", fmt.Errorf("process groups survived SIGKILL: %v", survived)
	case len(launching) > 0:
		return "launch_interrupted", fmt.Errorf("a previous agent died while starting a process: %v", launching)
	}
	return "", nil
}

func (a *Agent) reapOne(m groupMarker) string {
	if p, err := process.NewProcess(int32(m.PGID)); err == nil && m.CreateTimeMS != 0 {
		if ct, err := p.CreateTime(); err == nil && ct != m.CreateTimeMS {
			return "id_reused"
		}
	}
	if !groupAlive(m.PGID) {
		return "orphan_gone"
	}
	killGroup(m.PGID, 9)
	deadline := time.Now().Add(2 * time.Second)
	for groupAlive(m.PGID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if groupAlive(m.PGID) {
		return "survived"
	}
	return "orphan_reaped"
}

// writeSynced makes a marker durable before the agent goes on to act as if it
// exists, which is the whole point of a marker.
func writeSynced(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
