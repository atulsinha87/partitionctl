package state

import (
	"time"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// NodeRecord is one node's state within one run (TRD §10, NODE_STATE).
type NodeRecord struct {
	RunID  RunID              `json:"run_id"`
	NodeID protocol.NodeID    `json:"node_id"`
	Kind   protocol.NodeKind  `json:"kind"`
	State  protocol.NodeState `json:"state"`

	// Object is the catalog object this node acts on, seeded from the plan when
	// the run is created and never changed (FR-STATE-6 as amended). It is what
	// makes a node record a *claim*: the durable record that exists before the
	// DDL runs, and that stops authorizing anything the moment the node reaches
	// a complete state.
	//
	// This is what replaced the provenance table. A provenance row keyed on a
	// name outlived the run that wrote it and therefore authorized destroying
	// whatever later occupied that name, which is precisely the hole AC-6
	// describes. A claim cannot: see [ClaimsObject].
	Object protocol.ObjectName `json:"object,omitempty"`

	// Attempts counts dispatches, not transitions. It is what FR-EXEC-4's
	// bounded attempt budget is measured against.
	Attempts int `json:"attempts"`

	// LastError is the message from the most recent failure, and ErrorKind its
	// machine-readable class for structured logs (NFR-OBS-2).
	LastError string             `json:"last_error,omitempty"`
	ErrorKind protocol.ErrorKind `json:"error_kind,omitempty"`

	// StartedAt is when the node first entered RUNNING in this run.
	StartedAt *protocol.Timestamp `json:"started_at,omitempty"`
	UpdatedAt protocol.Timestamp  `json:"updated_at"`
}

// NodeTransition is a compare-and-set on a node's state.
//
// From is required and checked against the stored value. The executor is the
// only writer while it holds the advisory lock, but an adopting process reaches
// the same records during orphan recovery, and a compare-and-set is what keeps
// the two from silently overwriting each other.
type NodeTransition struct {
	RunID  RunID
	NodeID protocol.NodeID

	// From is the state the caller believes the node is in.
	From protocol.NodeState
	// To is the state to move to. The edge is checked against
	// [protocol.CheckTransition], so diagram D7 is enforced here and is not
	// re-implemented by callers.
	To protocol.NodeState

	// Reason distinguishes an ordinary transition from orphan recovery. Only
	// [protocol.ReasonOrphanRecovery] authorizes RUNNING -> PENDING (INV-5).
	Reason protocol.TransitionReason

	// IncrementAttempt adds one to the attempt counter. The executor sets it
	// on READY -> RUNNING.
	IncrementAttempt bool

	// Err is the failure that caused the transition, if any. Its message and
	// [protocol.KindOf] class are recorded. Passing nil clears both.
	Err error

	// At is optional. When zero the store's clock is used.
	At time.Time
}

func (t NodeTransition) validate() error {
	if t.RunID == "" {
		return ErrNotFound.Detailf("node transition has an empty run id")
	}
	if t.NodeID == "" {
		return ErrNotFound.Detailf("node transition has an empty node id")
	}
	reason := t.Reason
	if reason == "" {
		reason = protocol.ReasonNormal
	}
	return protocol.CheckTransition(t.From, t.To, reason)
}

func (t NodeTransition) reason() protocol.TransitionReason {
	if t.Reason == "" {
		return protocol.ReasonNormal
	}
	return t.Reason
}

// NodeCounts summarizes a run's node states, which is what `status` reports
// (FR-ORD-5) without needing the plan file or a live connection (FR-CLI-12).
type NodeCounts struct {
	Total     int                        `json:"total"`
	ByState   map[protocol.NodeState]int `json:"by_state"`
	Complete  int                        `json:"complete"`
	Remaining int                        `json:"remaining"`
	Failed    int                        `json:"failed"`
	InFlight  int                        `json:"in_flight"`
}

// CountNodes summarizes a slice of node records.
func CountNodes(recs []NodeRecord) NodeCounts {
	c := NodeCounts{Total: len(recs), ByState: make(map[protocol.NodeState]int, len(protocol.AllNodeStates()))}
	for _, r := range recs {
		c.ByState[r.State]++
		switch {
		case r.State.IsComplete():
			c.Complete++
		case r.State == protocol.NodeFailed:
			c.Failed++
		default:
			c.Remaining++
		}
		if r.State == protocol.NodeRunning || r.State == protocol.NodeVerifying {
			c.InFlight++
		}
	}
	return c
}
