// Package wire holds the JSON messages exchanged between the control plane
// and rig agents, so neither side imports the other.
package wire

import (
	"github.com/rajeev-chaurasia/benchgrid/internal/capability"
	"github.com/rajeev-chaurasia/benchgrid/internal/probe"
	"github.com/rajeev-chaurasia/benchgrid/internal/spec"
)

// Dispatch is the only command a scheduler sends a rig. Everything the agent
// does for an attempt follows from it.
type Dispatch struct {
	ExperimentID  string    `json:"experiment_id"`
	Attempt       int       `json:"attempt"`
	Fence         int64     `json:"fence"`
	LeaseAcquired string    `json:"lease_acquired"`
	Spec          spec.Spec `json:"spec"`
}

// Rejection reasons a scheduler can act on.
const (
	RejectStaleFence  = "stale_fence"
	RejectQuarantined = "quarantined"
	RejectConflict    = "fence_conflict"
)

type DispatchReply struct {
	Accepted  bool   `json:"accepted"`
	Duplicate bool   `json:"duplicate,omitempty"`
	Reason    string `json:"reason,omitempty"`
	HighFence int64  `json:"high_fence"`
}

type ActiveRun struct {
	ExperimentID string `json:"experiment_id"`
	Attempt      int    `json:"attempt"`
	Fence        int64  `json:"fence"`
	Phase        string `json:"phase"`
}

type Heartbeat struct {
	Descriptor  capability.Rig `json:"descriptor"`
	AgentState  string         `json:"agent_state"`
	AgentReason string         `json:"agent_reason"`
	HighFence   int64          `json:"high_fence"`
	Active      []ActiveRun    `json:"active"`
	Readings    probe.Readings `json:"readings"`
	Canary      *Canary        `json:"canary,omitempty"`
}

// Canary is a rig's latest calibration: a fixed workload's spread across
// iterations, run on the rig's bench CPUs while it was idle.
type Canary struct {
	CV         float64 `json:"cv"`
	MedianNS   float64 `json:"median_ns"`
	Iterations int     `json:"iterations"`
	At         string  `json:"at"`
}

type Completion struct {
	RigID        string `json:"rig_id"`
	Fence        int64  `json:"fence"`
	Status       string `json:"status"`
	StatusReason string `json:"status_reason"`
}

type RunStatus struct {
	ExperimentID string `json:"experiment_id"`
	Attempt      int    `json:"attempt"`
	Fence        int64  `json:"fence"`
	Phase        string `json:"phase"`
	Status       string `json:"status,omitempty"`
	StatusReason string `json:"status_reason,omitempty"`
}

const SHA256Header = "X-Content-SHA256"
