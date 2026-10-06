package agent

import (
	"encoding/json"
	"os"
	"sync"
)

// Interval is one span of activity on the rig, timed on the agent's monotonic
// clock. The fence evidence is computed from these and nothing else: if two
// spans with different fences overlap on one rig, the rig did work for two
// holders at once, whatever the lease table says.
type Interval struct {
	RigID        string `json:"rig_id"`
	Kind         string `json:"kind"`
	Fence        int64  `json:"fence"`
	ExperimentID string `json:"experiment_id"`
	Attempt      int    `json:"attempt"`
	PID          int    `json:"pid,omitempty"`
	StartNS      int64  `json:"start_ns"`
	EndNS        int64  `json:"end_ns"`
	Outcome      string `json:"outcome,omitempty"`
}

type IntervalLog struct {
	mu  sync.Mutex
	f   *os.File
	enc *json.Encoder
}

func OpenIntervalLog(path string) (*IntervalLog, error) {
	if path == "" {
		return &IntervalLog{}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &IntervalLog{f: f, enc: json.NewEncoder(f)}, nil
}

func (l *IntervalLog) Record(iv Interval) {
	if l.enc == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enc.Encode(iv)
}

func (l *IntervalLog) Close() error {
	if l.f == nil {
		return nil
	}
	return l.f.Close()
}
