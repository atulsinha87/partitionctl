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
	// starts *claiming* the object when the node goes READY, which is what
	// keeps a plan's mere intent from authorizing a drop.
	idx := protocol.NewObjectName("public", "orders_idx_p1")
	if _, err := s.TransitionNode(ctx, NodeTransition{
		RunID: run.RunID, NodeID: "cic:orders_idx_p1",
		From: protocol.NodePending, To: protocol.NodeReady,
	}); err != nil {
		t.Fatalf("TransitionNode: %v", err)
	}
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
	if _, err := s.TransitionNode(ctx, NodeTransition{
		RunID: run.RunID, NodeID: "cic:orders_idx_p1",
		From: protocol.NodePending, To: protocol.NodeReady,
	}); err != nil {
		t.Fatalf("TransitionNode: %v", err)
	}
	if _, found, _ := ClaimsObject(ctx, s, idx); !found {
		t.Fatal("a READY node holds no claim on the object it is about to create")
	}

	// Walk every node to DONE, exactly as a successful run does.
	for _, n := range plan.Nodes {
		cur, err := s.GetNode(ctx, run.RunID, n.ID)
		if err != nil {
			t.Fatalf("GetNode %q: %v", n.ID, err)
		}
		for _, to := range []protocol.NodeState{
			protocol.NodeReady, protocol.NodeRunning, protocol.NodeVerifying, protocol.NodeDone,
		} {
			if cur.State == to {
				continue
			}
			rec, err := s.TransitionNode(ctx, NodeTransition{
				RunID: run.RunID, NodeID: n.ID, From: cur.State, To: to,
			})
			if err != nil {
				t.Fatalf("node %q %s -> %s: %v", n.ID, cur.State, to, err)
			}
			cur = rec
		}
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

// A terminally cancelled run is one `resume` will not adopt (FR-CLI-11), so its
// claims must not authorize an adoption either.
func TestACancelledRunClaimsNothing(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testClaimPlan(t, "orders_idx_p1"), "run-cancelled")
	if _, err := s.TransitionNode(ctx, NodeTransition{
		RunID: run.RunID, NodeID: "cic:orders_idx_p1",
		From: protocol.NodePending, To: protocol.NodeReady,
	}); err != nil {
		t.Fatalf("TransitionNode: %v", err)
	}
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
	if _, err := s.TransitionNode(ctx, NodeTransition{
		RunID: run.RunID, NodeID: "cic:orders_idx_p1",
		From: protocol.NodePending, To: protocol.NodeReady,
	}); err != nil {
		t.Fatalf("TransitionNode: %v", err)
	}
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
	if _, err := s.TransitionNode(ctx, NodeTransition{
		RunID: run.RunID, NodeID: "cic:orders_idx_p1",
		From: protocol.NodePending, To: protocol.NodeReady,
	}); err != nil {
		t.Fatalf("TransitionNode: %v", err)
	}

	if _, found, err := ClaimsObject(ctx, s, protocol.NewObjectName("public", "somebody_elses_idx")); err != nil || found {
		t.Fatalf("found=%t err=%v", found, err)
	}
	// Schema is part of the identity: a same-named index in another schema is
	// a different object.
	if _, found, err := ClaimsObject(ctx, s, protocol.NewObjectName("archive", "orders_idx_p1")); err != nil || found {
		t.Fatalf("found=%t err=%v; schema must be part of the match", found, err)
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
