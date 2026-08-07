package protocol

import (
	"errors"
	"testing"
)

// The full D7 adjacency, written out independently of the implementation's
// table so that the test is a second statement of the diagram rather than a
// restatement of the code.
func TestValidTransitionMatchesD7(t *testing.T) {
	allowed := map[NodeState]map[NodeState]bool{
		NodePending:   {NodeReady: true, NodeSkipped: true},
		NodeReady:     {NodeRunning: true},
		NodeRunning:   {NodeVerifying: true, NodeFailed: true, NodeRetryWait: true},
		NodeVerifying: {NodeDone: true, NodeFailed: true},
		NodeRetryWait: {NodeReady: true, NodeFailed: true},
		NodeDone:      {},
		NodeFailed:    {},
		NodeSkipped:   {},
	}

	for _, from := range AllNodeStates() {
		for _, to := range AllNodeStates() {
			want := allowed[from][to]
			if got := ValidTransition(from, to, ReasonNormal); got != want {
				t.Errorf("ValidTransition(%s, %s, normal) = %v, want %v", from, to, got, want)
			}
		}
	}
}

// INV-5: transitions are monotonic except RUNNING -> PENDING, which happens
// only on orphan recovery.
func TestOrphanRecoveryIsTheOnlyBackwardEdge(t *testing.T) {
	if ValidTransition(NodeRunning, NodePending, ReasonNormal) {
		t.Error("RUNNING -> PENDING was allowed without orphan recovery")
	}
	if !ValidTransition(NodeRunning, NodePending, ReasonOrphanRecovery) {
		t.Error("RUNNING -> PENDING was refused during orphan recovery")
	}

	// Orphan recovery authorizes that edge and no other.
	for _, from := range AllNodeStates() {
		for _, to := range AllNodeStates() {
			if from == NodeRunning && to == NodePending {
				continue
			}
			if ValidTransition(from, to, ReasonOrphanRecovery) {
				t.Errorf("orphan recovery allowed %s -> %s", from, to)
			}
		}
	}
}

func TestSelfTransitionsAreRejected(t *testing.T) {
	for _, s := range AllNodeStates() {
		for _, r := range []TransitionReason{ReasonNormal, ReasonOrphanRecovery} {
			if ValidTransition(s, s, r) {
				t.Errorf("self-transition %s -> %s (%s) was allowed", s, s, r)
			}
		}
	}
}

func TestTerminalStatesHaveNoOutgoingEdges(t *testing.T) {
	for _, s := range AllNodeStates() {
		if !s.IsTerminal() {
			continue
		}
		for _, r := range []TransitionReason{ReasonNormal, ReasonOrphanRecovery} {
			if next := NextStates(s, r); len(next) != 0 {
				t.Errorf("terminal state %s has outgoing edges %v under %s", s, next, r)
			}
		}
	}
}

func TestTransitionRejectsUnknownValues(t *testing.T) {
	cases := []struct{ from, to NodeState }{
		{"NOPE", NodeReady},
		{NodePending, "NOPE"},
		{"", ""},
	}
	for _, c := range cases {
		if ValidTransition(c.from, c.to, ReasonNormal) {
			t.Errorf("ValidTransition(%q, %q) accepted an unknown state", c.from, c.to)
		}
	}
	if ValidTransition(NodePending, NodeReady, "whatever") {
		t.Error("an unknown transition reason was accepted")
	}
}

func TestCheckTransition(t *testing.T) {
	if err := CheckTransition(NodePending, NodeReady, ReasonNormal); err != nil {
		t.Fatalf("CheckTransition on a legal edge: %v", err)
	}

	err := CheckTransition(NodeDone, NodeRunning, ReasonNormal)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("error %v is not ErrInvalidTransition", err)
	}
	if code := ExitCodeFor(err); code != ExitFailure {
		t.Fatalf("exit code %d, want %d", code, ExitFailure)
	}

	// The orphan-recovery edge gets its own message, because "not permitted"
	// would send the reader looking in the wrong place.
	err = CheckTransition(NodeRunning, NodePending, ReasonNormal)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("error %v is not ErrInvalidTransition", err)
	}
	if !contains(err.Error(), "orphan recovery") {
		t.Fatalf("message %q does not mention orphan recovery", err)
	}
}

func TestNodeStateClassification(t *testing.T) {
	tests := []struct {
		state      NodeState
		valid      bool
		terminal   bool
		completing bool
	}{
		{NodePending, true, false, false},
		{NodeReady, true, false, false},
		{NodeRunning, true, false, false},
		{NodeVerifying, true, false, false},
		{NodeDone, true, true, true},
		{NodeSkipped, true, true, true},
		{NodeFailed, true, true, false},
		{"NOPE", false, false, false},
	}
	for _, tc := range tests {
		if got := tc.state.Valid(); got != tc.valid {
			t.Errorf("%s.Valid() = %v, want %v", tc.state, got, tc.valid)
		}
		if got := tc.state.IsTerminal(); got != tc.terminal {
			t.Errorf("%s.IsTerminal() = %v, want %v", tc.state, got, tc.terminal)
		}
		// FR-ORD-1: only DONE and SKIPPED satisfy a dependency edge. FAILED is
		// terminal but must not unblock a successor.
		if got := tc.state.IsComplete(); got != tc.completing {
			t.Errorf("%s.IsComplete() = %v, want %v", tc.state, got, tc.completing)
		}
	}
}

func TestInitialNodeState(t *testing.T) {
	if InitialNodeState() != NodePending {
		t.Fatalf("InitialNodeState() = %s", InitialNodeState())
	}
}

// Every state must be reachable from PENDING, or the diagram has a dead state.
func TestEveryStateIsReachable(t *testing.T) {
	reached := map[NodeState]bool{NodePending: true}
	for changed := true; changed; {
		changed = false
		for _, from := range AllNodeStates() {
			if !reached[from] {
				continue
			}
			for _, r := range []TransitionReason{ReasonNormal, ReasonOrphanRecovery} {
				for _, to := range NextStates(from, r) {
					if !reached[to] {
						reached[to] = true
						changed = true
					}
				}
			}
		}
	}
	for _, s := range AllNodeStates() {
		if !reached[s] {
			t.Errorf("state %s is unreachable from %s", s, NodePending)
		}
	}
}

func TestAllNodeStatesReturnsACopy(t *testing.T) {
	a := AllNodeStates()
	a[0] = "MUTATED"
	if AllNodeStates()[0] != NodePending {
		t.Fatal("AllNodeStates returned the package-level slice")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
