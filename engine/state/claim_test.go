package state

import (
	"context"
	"testing"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// The claim exists before any statement runs. That is INV-1 as amended: the
// durable record naming the object and the run is committed by CreateRun,
// which happens before the executor dispatches anything.
func TestCreateRunSeedsTheClaimedObject(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testClaimPlan(t, "orders_idx_p1"), "run-claim")

	nodes, err := s.ListNodes(ctx, run.RunID)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	want := map[protocol.NodeID]string{
		"parent":            "public.orders_idx",
		"cic:orders_idx_p1": "public.orders_idx_p1",
	}
	for _, n := range nodes {
		if got := n.Object.String(); got != want[n.NodeID] {
			t.Errorf("node %q claims %q, want %q", n.NodeID, got, want[n.NodeID])
		}
	}

	// The record exists before anything is dispatched, which is INV-1; it
	// starts *claiming* the object at the first dispatch, which is what keeps a
	// plan's mere intent from authorizing a drop.
	idx := protocol.NewObjectName("public", "orders_idx_p1")
	dispatch(t, s, run.RunID, "cic:orders_idx_p1")
	id, found, err := ClaimsObject(ctx, s, idx)
	if err != nil {
		t.Fatalf("ClaimsObject: %v", err)
	}
	if !found || id != run.RunID {
		t.Fatalf("ClaimsObject = %q, %t; want %q, true", id, found, run.RunID)
	}
}

// The whole point of moving off a side table. A run that finished leaves no
// claim, so a same-named index somebody else creates afterwards cannot be
// destroyed on the strength of our history (AC-6, NFR-REL-3).
func TestCompletedRunLeavesNoClaim(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	plan := testClaimPlan(t, "orders_idx_p1")
	run := mustCreateRun(t, s, plan, "run-done")
	idx := protocol.NewObjectName("public", "orders_idx_p1")

	// A node that has never dispatched claims nothing: intending to create an
	// object must not authorize destroying whatever already occupies the name.
	if _, found, _ := ClaimsObject(ctx, s, idx); found {
		t.Fatal("a plan that has issued nothing already claims its objects (AC-6)")
	}
	dispatch(t, s, run.RunID, "cic:orders_idx_p1")
	if _, found, _ := ClaimsObject(ctx, s, idx); !found {
		t.Fatal("a dispatched node holds no claim on the object it is about to create")
	}

	// Walk every node to DONE, exactly as a successful run does.
	for _, n := range plan.Nodes {
		walkToDone(t, s, run.RunID, n.ID)
	}
	if _, err := s.SetRunStatus(ctx, RunStatusUpdate{
		RunID: run.RunID, From: RunRunning, To: RunCompleted,
	}); err != nil {
		t.Fatalf("SetRunStatus: %v", err)
	}

	id, found, err := ClaimsObject(ctx, s, idx)
	if err != nil {
		t.Fatalf("ClaimsObject: %v", err)
	}
	if found {
		t.Fatalf("a completed run still claims %s (run %s); that is the stale record AC-6 forbids", idx, id)
	}
}

// A crash mid-node is exactly the window the claim covers: the run is FAILED or
// ORPHANED and the node is still in flight, so the object is adoptable.
func TestAnInterruptedRunKeepsItsClaim(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	plan := testClaimPlan(t, "orders_idx_p1")
	run := mustCreateRun(t, s, plan, "run-crashed")
	idx := protocol.NewObjectName("public", "orders_idx_p1")
	node := protocol.NodeID("cic:orders_idx_p1")

	for _, to := range []protocol.NodeState{protocol.NodeReady, protocol.NodeRunning} {
		if _, err := s.TransitionNode(ctx, NodeTransition{
			RunID: run.RunID, NodeID: node, From: priorState(to), To: to,
			IncrementAttempt: to == protocol.NodeRunning,
		}); err != nil {
			t.Fatalf("%s: %v", to, err)
		}
	}
	if _, err := s.SetRunStatus(ctx, RunStatusUpdate{
		RunID: run.RunID, From: RunRunning, To: RunOrphaned,
	}); err != nil {
		t.Fatalf("SetRunStatus: %v", err)
	}

	id, found, err := ClaimsObject(ctx, s, idx)
	if err != nil {
		t.Fatalf("ClaimsObject: %v", err)
	}
	if !found || id != run.RunID {
		t.Fatalf("ClaimsObject = %q, %t; an orphaned run's in-flight node still claims its object", id, found)
	}
}

// Orphan recovery is RUNNING -> PENDING (INV-5), and it fires before the resume
// walk. The claim has to survive it, or `resume` loses the ability to adopt the
// half-built object at exactly the moment it needs to.
func TestTheClaimSurvivesOrphanRecovery(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testClaimPlan(t, "orders_idx_p1"), "run-recovered")
	node := protocol.NodeID("cic:orders_idx_p1")
	idx := protocol.NewObjectName("public", "orders_idx_p1")

	for _, to := range []protocol.NodeState{protocol.NodeReady, protocol.NodeRunning} {
		if _, err := s.TransitionNode(ctx, NodeTransition{
			RunID: run.RunID, NodeID: node, From: priorState(to), To: to,
			IncrementAttempt: to == protocol.NodeRunning,
		}); err != nil {
			t.Fatalf("%s: %v", to, err)
		}
	}
	if _, err := s.TransitionNode(ctx, NodeTransition{
		RunID: run.RunID, NodeID: node,
		From: protocol.NodeRunning, To: protocol.NodePending,
		Reason: protocol.ReasonOrphanRecovery,
	}); err != nil {
		t.Fatalf("orphan recovery: %v", err)
	}

	id, found, err := ClaimsObject(ctx, s, idx)
	if err != nil {
		t.Fatalf("ClaimsObject: %v", err)
	}
	if !found || id != run.RunID {
		t.Fatalf("ClaimsObject = %q, %t; a recovered in-flight node still claims its object", id, found)
	}
}

// The executor checkpoints PENDING -> READY before it authorizes or dispatches
// anything, and only increments the attempt counter on READY -> RUNNING. A
// process killed in that gap leaves a durably READY node with attempts = 0 that
// issued no statement, so there is nothing of ours under that name.
//
// If such a node claimed the name, `resume` would read MarkerAbsent plus that
// stale claim, stamp PartitionCTL's ownership marker onto whatever unmarked
// INVALID index a DBA had since left there, and drop it — destroying an object
// this tool never created, authorized by a node that never ran. That is the
// circularity claim.go's own doc says it excludes (AC-6).
func TestAReadyNodeThatNeverDispatchedClaimsNothing(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testClaimPlan(t, "orders_idx_p1"), "run-killed-at-ready")
	idx := protocol.NewObjectName("public", "orders_idx_p1")

	// PENDING -> READY, checkpointed, with no attempt recorded: the exact state
	// a SIGKILL in that gap leaves behind.
	rec, err := s.TransitionNode(ctx, NodeTransition{
		RunID: run.RunID, NodeID: "cic:orders_idx_p1",
		From: protocol.NodePending, To: protocol.NodeReady,
	})
	if err != nil {
		t.Fatalf("TransitionNode: %v", err)
	}
	if rec.State != protocol.NodeReady || rec.Attempts != 0 {
		t.Fatalf("setup: node is %s with %d attempts, want READY with 0", rec.State, rec.Attempts)
	}
	if _, err := s.SetRunStatus(ctx, RunStatusUpdate{
		RunID: run.RunID, From: RunRunning, To: RunOrphaned,
	}); err != nil {
		t.Fatalf("SetRunStatus: %v", err)
	}

	if id, found, err := ClaimsObject(ctx, s, idx); err != nil || found {
		t.Fatalf("ClaimsObject = %q, %t; a node that issued nothing claims nothing (AC-6)", id, found)
	}

	// The claim goes live at the first dispatch, which is checkpointed before
	// the statement is sent — so the crash window that actually needs covering
	// is still covered.
	if _, err := s.TransitionNode(ctx, NodeTransition{
		RunID: run.RunID, NodeID: "cic:orders_idx_p1",
		From: protocol.NodeReady, To: protocol.NodeRunning, IncrementAttempt: true,
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, found, err := ClaimsObject(ctx, s, idx); err != nil || !found {
		t.Fatalf("found=%t err=%v; a dispatched node must claim its object", found, err)
	}
}

// A retry sits in RETRY_WAIT and comes back through READY with its attempt
// count intact, so requiring a dispatch for READY does not cost the resume path
// the claim it needs.
func TestAReadyNodeAwaitingItsRetryStillClaims(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testClaimPlan(t, "orders_idx_p1"), "run-retry")
	node := protocol.NodeID("cic:orders_idx_p1")
	idx := protocol.NewObjectName("public", "orders_idx_p1")

	dispatch(t, s, run.RunID, node)
	for _, step := range []struct{ from, to protocol.NodeState }{
		{protocol.NodeRunning, protocol.NodeRetryWait},
		{protocol.NodeRetryWait, protocol.NodeReady},
	} {
		if _, err := s.TransitionNode(ctx, NodeTransition{
			RunID: run.RunID, NodeID: node, From: step.from, To: step.to,
		}); err != nil {
			t.Fatalf("%s -> %s: %v", step.from, step.to, err)
		}
	}
	if _, err := s.SetRunStatus(ctx, RunStatusUpdate{
		RunID: run.RunID, From: RunRunning, To: RunInterrupted,
	}); err != nil {
		t.Fatalf("SetRunStatus: %v", err)
	}
	if _, found, err := ClaimsObject(ctx, s, idx); err != nil || !found {
		t.Fatalf("found=%t err=%v; a node awaiting a retry has already dispatched", found, err)
	}
}

// A terminally cancelled run is one `resume` will not adopt (FR-CLI-11), so its
// claims must not authorize an adoption either.
func TestACancelledRunClaimsNothing(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testClaimPlan(t, "orders_idx_p1"), "run-cancelled")
	dispatch(t, s, run.RunID, "cic:orders_idx_p1")
	if _, err := s.SetRunStatus(ctx, RunStatusUpdate{
		RunID: run.RunID, From: RunRunning, To: RunCancelled,
	}); err != nil {
		t.Fatalf("SetRunStatus: %v", err)
	}
	if _, found, err := ClaimsObject(ctx, s, protocol.NewObjectName("public", "orders_idx_p1")); err != nil || found {
		t.Fatalf("ClaimsObject found=%t err=%v; a cancelled run must claim nothing", found, err)
	}
}

// A file store deliberately holds state for more than one target, so an
// unscoped claim could adopt an index in one database on the strength of a run
// against another.
func TestClaimsObjectInScopesByDatabase(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testClaimPlan(t, "orders_idx_p1"), "run-appdb")
	dispatch(t, s, run.RunID, "cic:orders_idx_p1")
	idx := protocol.NewObjectName("public", "orders_idx_p1")

	if _, found, err := ClaimsObjectIn(ctx, s, "appdb", idx); err != nil || !found {
		t.Fatalf("the run's own database: found=%t err=%v", found, err)
	}
	if _, found, err := ClaimsObjectIn(ctx, s, "otherdb", idx); err != nil || found {
		t.Fatalf("a different database: found=%t err=%v; the claim must not cross databases", found, err)
	}
}

// A node record names the object its node acts on for every kind that acts on
// one, so a destructive node names what it would destroy. That must never be a
// claim: "drop X" would otherwise be the proof that X is ours to drop (AC-6).
func TestADestructiveNodeClaimsNothing(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)

	victim := protocol.NewObjectName("public", "somebody_elses_idx")
	relation := protocol.NewObjectName("public", "orders_p1")
	plan := &protocol.Plan{
		FormatVersion: protocol.PlanFormatVersion,
		PlanID:        "plan-drop",
		Operation:     protocol.OpCreateIndex,
		Target: protocol.Target{
			Database: "appdb",
			Table:    protocol.NewObjectName("public", "orders"),
		},
		CreatedAt: protocol.NewTimestamp(baseTime),
		Nodes: []protocol.Node{{
			ID:   "drop",
			Kind: protocol.KindIndexDropConcurrently,
			Params: &protocol.DropConcurrentlyParams{
				Index: victim, Relation: &relation, Reason: protocol.DropInvalidBuild,
			},
			Authorization: &protocol.Authorization{
				Mode: protocol.AuthProvenance, Object: victim, Relation: &relation,
			},
		}},
		TopologyFingerprint: protocol.FingerprintPrefix +
			"0000000000000000000000000000000000000000000000000000000000000000",
	}
	if err := plan.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	run := mustCreateRun(t, s, plan, "run-drop")

	// The record still names the object, for the audit trail.
	rec, err := s.GetNode(ctx, run.RunID, "drop")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if rec.Object != victim {
		t.Fatalf("node record object = %v, want %v; the audit trail needs it", rec.Object, victim)
	}

	// But it is not a claim, in any state.
	for _, to := range []protocol.NodeState{protocol.NodeReady, protocol.NodeRunning} {
		if _, err := s.TransitionNode(ctx, NodeTransition{
			RunID: run.RunID, NodeID: "drop", From: priorState(to), To: to,
			IncrementAttempt: to == protocol.NodeRunning,
		}); err != nil {
			t.Fatalf("%s: %v", to, err)
		}
		if _, found, err := ClaimsObject(ctx, s, victim); err != nil || found {
			t.Fatalf("state %s: found = %t; a drop node authorized its own drop (AC-6)", to, found)
		}
	}
}

func TestClaimsObjectIgnoresOtherObjects(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testClaimPlan(t, "orders_idx_p1"), "run-other")
	dispatch(t, s, run.RunID, "cic:orders_idx_p1")

	if _, found, err := ClaimsObject(ctx, s, protocol.NewObjectName("public", "somebody_elses_idx")); err != nil || found {
		t.Fatalf("found=%t err=%v", found, err)
	}
	// Schema is part of the identity: a same-named index in another schema is
	// a different object.
	if _, found, err := ClaimsObject(ctx, s, protocol.NewObjectName("archive", "orders_idx_p1")); err != nil || found {
		t.Fatalf("found=%t err=%v; schema must be part of the match", found, err)
	}
}

// dispatch drives a node to the state the executor leaves it in when it sends a
// statement: RUNNING, with the attempt counter incremented. That increment is
// the durable record of a dispatch, and it is what makes a claim live.
func dispatch(t *testing.T, s StateStore, run RunID, node protocol.NodeID) {
	t.Helper()
	ctx := context.Background()
	for _, to := range []protocol.NodeState{protocol.NodeReady, protocol.NodeRunning} {
		if _, err := s.TransitionNode(ctx, NodeTransition{
			RunID: run, NodeID: node, From: priorState(to), To: to,
			IncrementAttempt: to == protocol.NodeRunning,
		}); err != nil {
			t.Fatalf("dispatch %s -> %s: %v", priorState(to), to, err)
		}
	}
}

// walkToDone drives a node from wherever it is to DONE along the happy path.
func walkToDone(t *testing.T, s StateStore, run RunID, node protocol.NodeID) {
	t.Helper()
	ctx := context.Background()
	order := []protocol.NodeState{
		protocol.NodePending, protocol.NodeReady, protocol.NodeRunning,
		protocol.NodeVerifying, protocol.NodeDone,
	}
	cur, err := s.GetNode(ctx, run, node)
	if err != nil {
		t.Fatalf("GetNode %q: %v", node, err)
	}
	at := 0
	for i, st := range order {
		if cur.State == st {
			at = i
		}
	}
	for _, to := range order[at+1:] {
		rec, err := s.TransitionNode(ctx, NodeTransition{
			RunID: run, NodeID: node, From: cur.State, To: to,
			IncrementAttempt: to == protocol.NodeRunning,
		})
		if err != nil {
			t.Fatalf("node %q %s -> %s: %v", node, cur.State, to, err)
		}
		cur = rec
	}
}

// priorState is the D7 predecessor of to along the happy path, for tests that
// drive a node forward.
func priorState(to protocol.NodeState) protocol.NodeState {
	switch to {
	case protocol.NodeReady:
		return protocol.NodePending
	case protocol.NodeRunning:
		return protocol.NodeReady
	case protocol.NodeVerifying:
		return protocol.NodeRunning
	case protocol.NodeDone:
		return protocol.NodeVerifying
	}
	return protocol.NodePending
}
