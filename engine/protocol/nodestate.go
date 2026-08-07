package protocol

// NodeState is the status of one node within one run (TRD §10.2, diagram D7).
type NodeState string

// The node lifecycle states.
const (
	// NodePending is the initial state: dependencies not yet satisfied.
	NodePending NodeState = "PENDING"
	// NodeReady means every predecessor reached DONE or SKIPPED (FR-ORD-1).
	NodeReady NodeState = "READY"
	// NodeRunning means the statement is in flight.
	NodeRunning NodeState = "RUNNING"
	// NodeVerifying means the statement returned and assertions are being
	// evaluated.
	NodeVerifying NodeState = "VERIFYING"
	// NodeDone is terminal success.
	NodeDone NodeState = "DONE"
	// NodeFailed is terminal failure.
	NodeFailed NodeState = "FAILED"
	// NodeRetryWait means a retryable error occurred and backoff is elapsing
	// (FR-EXEC-3, FR-EXEC-4).
	NodeRetryWait NodeState = "RETRY_WAIT"
	// NodeSkipped is terminal: the work was already complete (FR-PLAN-5).
	NodeSkipped NodeState = "SKIPPED"
)

var allNodeStates = []NodeState{
	NodePending, NodeReady, NodeRunning, NodeVerifying,
	NodeDone, NodeFailed, NodeRetryWait, NodeSkipped,
}

// AllNodeStates returns every state. The returned slice is a copy.
func AllNodeStates() []NodeState {
	out := make([]NodeState, len(allNodeStates))
	copy(out, allNodeStates)
	return out
}

// InitialNodeState is the state every node starts a run in.
func InitialNodeState() NodeState { return NodePending }

// Valid reports whether s is a known state.
func (s NodeState) Valid() bool {
	switch s {
	case NodePending, NodeReady, NodeRunning, NodeVerifying,
		NodeDone, NodeFailed, NodeRetryWait, NodeSkipped:
		return true
	}
	return false
}

// IsTerminal reports whether s is an end state with no outgoing transition.
func (s NodeState) IsTerminal() bool {
	switch s {
	case NodeDone, NodeFailed, NodeSkipped:
		return true
	}
	return false
}

// IsComplete reports whether s satisfies a dependency edge: a successor may be
// dispatched once every predecessor is DONE or SKIPPED (FR-ORD-1). FAILED is
// terminal but does not satisfy a dependency.
func (s NodeState) IsComplete() bool {
	switch s {
	case NodeDone, NodeSkipped:
		return true
	}
	return false
}

func (s NodeState) String() string { return string(s) }

// TransitionReason distinguishes an ordinary transition from orphan recovery.
// It exists because exactly one edge in D7, RUNNING -> PENDING, is legal only
// during orphan recovery (INV-5).
type TransitionReason string

// The transition reasons.
const (
	// ReasonNormal is any transition made by a live executor.
	ReasonNormal TransitionReason = "normal"
	// ReasonOrphanRecovery is the adoption of a run whose lease expired and
	// whose advisory lock is unheld (INV-4). It authorizes RUNNING -> PENDING
	// and nothing else.
	ReasonOrphanRecovery TransitionReason = "orphan_recovery"

	// ReasonResumeRetry is `resume` adopting a run that stopped with a node
	// FAILED. It authorizes FAILED -> PENDING and nothing else.
	//
	// FAILED is terminal for every ordinary transition, which is what stops a
	// running executor from looping on a node that keeps failing. It cannot
	// also be terminal across runs, because [state.RunFailed] is resumable:
	// `execute` refuses a failed run and directs the operator to `resume`
	// (FR-CLI-9, AC-23), so if adoption could not rewind the node then neither
	// command could make progress and the run would be exactly the
	// unrecoverable intermediate NFR-REL-2 forbids.
	//
	// Rewinding is safe because roll-forward is the whole failure model: the
	// wreckage a failed CREATE INDEX CONCURRENTLY leaves is dropped by resume's
	// provenance-gated cleanup before the walk, and a node that fails for a
	// deterministic reason simply fails again with the same message and the
	// same exit code. Adoption is an operator decision, not an automatic retry.
	ReasonResumeRetry TransitionReason = "resume_retry"
)

// Valid reports whether r is a known reason.
func (r TransitionReason) Valid() bool {
	return r == ReasonNormal || r == ReasonOrphanRecovery || r == ReasonResumeRetry
}

// nodeTransitions is diagram D7, minus the two recovery edges, which are
// handled separately in [ValidTransition].
var nodeTransitions = map[NodeState]map[NodeState]bool{
	NodePending: {
		NodeReady:   true, // dependencies satisfied
		NodeSkipped: true, // work already complete
	},
	NodeReady: {
		NodeRunning: true, // dispatched
	},
	NodeRunning: {
		NodeVerifying: true, // statement returned
		NodeFailed:    true, // terminal error
		NodeRetryWait: true, // retryable error
		// NodeRunning -> NodePending is orphan recovery only; see below.
	},
	NodeVerifying: {
		NodeDone:   true, // assertions pass
		NodeFailed: true, // assertions fail
	},
	NodeRetryWait: {
		NodeReady:  true, // backoff elapsed
		NodeFailed: true, // attempts exhausted
	},
	NodeDone:    {},
	NodeFailed:  {},
	NodeSkipped: {},
}

// ValidTransition reports whether from -> to is permitted by diagram D7.
//
// Transitions are monotonic except RUNNING -> PENDING, which is legal only when
// reason is [ReasonOrphanRecovery] (INV-5). Self-transitions are not
// transitions and are rejected: re-writing the same state is a checkpoint, not
// a state change.
func ValidTransition(from, to NodeState, reason TransitionReason) bool {
	if !from.Valid() || !to.Valid() || !reason.Valid() {
		return false
	}
	if from == NodeRunning && to == NodePending {
		return reason == ReasonOrphanRecovery
	}
	if from == NodeFailed && to == NodePending {
		return reason == ReasonResumeRetry
	}
	if reason == ReasonOrphanRecovery || reason == ReasonResumeRetry {
		// Each recovery reason justifies exactly one edge. Anything else an
		// adopting process wants to do is an ordinary transition.
		return false
	}
	return nodeTransitions[from][to]
}

// CheckTransition returns nil if from -> to is permitted, and an error matching
// [ErrInvalidTransition] otherwise.
func CheckTransition(from, to NodeState, reason TransitionReason) error {
	if ValidTransition(from, to, reason) {
		return nil
	}
	if from == NodeRunning && to == NodePending {
		return ErrInvalidTransition.Detailf(
			"%s -> %s is legal only on orphan recovery (INV-5), reason was %q", from, to, reason)
	}
	if from == NodeFailed && to == NodePending {
		return ErrInvalidTransition.Detailf(
			"%s -> %s is legal only when `resume` adopts the run (INV-5), reason was %q", from, to, reason)
	}
	return ErrInvalidTransition.Detailf("%s -> %s (reason %q) is not permitted by D7", from, to, reason)
}

// NextStates returns the states reachable from s under reason, in
// [AllNodeStates] order. It is derived from the same table [ValidTransition]
// uses, so the two cannot drift.
func NextStates(s NodeState, reason TransitionReason) []NodeState {
	var out []NodeState
	for _, to := range allNodeStates {
		if ValidTransition(s, to, reason) {
			out = append(out, to)
		}
	}
	return out
}
