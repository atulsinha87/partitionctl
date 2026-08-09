package cli

import (
	"strings"
	"testing"

	"github.com/atulsinha87/partitionctl/engine/protocol"
	"github.com/atulsinha87/partitionctl/engine/state"
)

// FR-CLI-9: "`resume` SHALL be the only command permitted to perform
// `provenance`-authorized cleanup."
//
// TRD §7.2.12 explains why the two commands are separate at all: resume is
// where INVALID indexes left by a previous process get dropped, and "silently
// doing that under `execute` would mean a routine re-run can destroy catalog
// objects".
//
// The rule is now narrower and evaluated later, and both changes are
// deliberate. The old guard scanned the artifact for any provenance-authorized
// destructive node and refused, which a re-plan defeated anyway (a fresh
// CreatedAt gives a new digest, so no prior run matched) and which was too
// broad besides: an object carrying PartitionCTL's ownership marker is a
// catalog fact, and dropping it is not a recovery decision. What is reserved to
// `resume` is *adoption*: dropping an object that carries no marker and is ours
// only because an interrupted run still claims it. That question can only be
// answered against live state, so it is answered at dispatch (FR-AUTH-5).

// TestExecuteRefusesToAdoptAnUnmarkedIndex is the FR-CLI-9 refusal.
func TestExecuteRefusesToAdoptAnUnmarkedIndex(t *testing.T) {
	h := newHarness(t)
	p := h.dropPlan()
	plan := h.WritePlan(p)

	// A live claim exists, so the drop *would* be authorized under resume.
	// That is the dangerous case: the authorization check passes and only the
	// command split stands between a routine re-run and a dropped index.
	h.seedClaim(h.LoadPlan(plan))

	code := h.Run("execute", plan)
	if code == int(protocol.ExitSuccess) {
		t.Fatalf("execute adopted and dropped an unmarked index; FR-CLI-9 reserves that to resume: %s", h.Out())
	}
	if h.SQL.Issued("DROP INDEX") {
		t.Errorf("execute issued an adoption-authorized DROP INDEX CONCURRENTLY (FR-CLI-9):\n%v", h.SQL.SQLTexts())
	}
	if !strings.Contains(h.Out(), "resume") {
		t.Errorf("the refusal does not name `resume`: %s", h.Out())
	}
	if h.SQL.Issued("COMMENT ON INDEX") {
		t.Error("execute stamped its ownership marker onto an object it was not allowed to adopt")
	}
}

// The complement: an object carrying PartitionCTL's marker is provably ours
// from the catalog alone, so `execute` may drop it. Refusing that was the
// deadlock in the old design — a re-plan after a crash produced a node that
// `execute` refused and `resume` could not reach, because the digest changed.
func TestExecuteMayDropAnIndexThatCarriesOurMarker(t *testing.T) {
	h := newHarness(t)
	p := h.dropPlan()
	plan := h.WritePlan(p)
	h.Verify.Mark(obj("public", protocol.ChildIndexName(testIndex, "orders_2026_01")), "run-earlier")

	if code := h.Run("execute", plan); code != int(protocol.ExitSuccess) {
		t.Fatalf("execute exited %d on a marker-authorized drop: %s", code, h.Out())
	}
	if !h.SQL.Issued("DROP INDEX CONCURRENTLY") {
		t.Errorf("execute did not issue the marker-authorized drop:\n%v", h.SQL.SQLTexts())
	}
}

// TestResumeAdoptsAndDrops is the other half of FR-CLI-9: the command that IS
// permitted must still do it, or the refusal above would just be a dead end
// with no path forward.
func TestResumeAdoptsAndDrops(t *testing.T) {
	h := newHarness(t)
	p := h.dropPlan()
	plan := h.WritePlan(p)
	loaded := h.LoadPlan(plan)
	h.seedClaim(loaded)
	h.seedRun(loaded)

	if code := h.Run("resume", plan); code != int(protocol.ExitSuccess) {
		t.Fatalf("resume exited %d, want 0: %s", code, h.Out())
	}
	if !h.SQL.Issued("DROP INDEX CONCURRENTLY") {
		t.Errorf("resume did not issue the authorized drop:\n%v", h.SQL.SQLTexts())
	}
	// The adoption is the marker written immediately before the drop, so the
	// destruction stays auditable once the claim is gone.
	if !h.SQL.Issued("COMMENT ON INDEX") {
		t.Errorf("resume dropped an adopted object without first marking it:\n%v", h.SQL.SQLTexts())
	}
}

// TestExecuteAllowsAPlanWithNoDestructiveNode guards against the refusal being
// too broad: the ordinary create-index plan has no destructive node and must
// still run under `execute`.
func TestExecuteAllowsAPlanWithNoDestructiveNode(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()
	if code := h.Run("execute", plan); code != int(protocol.ExitSuccess) {
		t.Fatalf("execute exited %d on a plan with no destructive node: %s", code, h.Out())
	}
}

// ---------------------------------------------------------------------------

// seedClaim leaves behind exactly what a process killed mid-CREATE INDEX
// CONCURRENTLY leaves: a FAILED run whose *create* node for the victim object
// is still in flight, so the object is claimed but carries no marker.
//
// The claim has to come from a create node. A drop node names the object it
// would destroy, for the audit trail, and that is deliberately not a claim: a
// plan saying "drop X" must never be the proof that X is ours to drop (AC-6,
// protocol.NodeKind.ClaimsOwnership).
//
// The run belongs to a *different* plan, because that is the scenario FR-CLI-9
// is about: a run crashed, the operator re-planned, and the new artifact has a
// new digest.
func (h *harness) seedClaim(p *protocol.Plan) {
	h.t.Helper()

	victim := obj("public", protocol.ChildIndexName(testIndex, "orders_2026_01"))
	leaf := obj("public", "orders_2026_01")
	parentIndex := obj("public", testIndex)

	crashed := &protocol.Plan{
		FormatVersion: protocol.PlanFormatVersion,
		PlanID:        "plan-crashed-build",
		Operation:     protocol.OpCreateIndex,
		CreatedAt:     protocol.NewTimestamp(h.Now()),
		Target: protocol.Target{
			Database: testDBName,
			Table:    obj("public", "orders"),
			Index:    &parentIndex,
		},
		TopologyFingerprint: h.liveFingerprint(),
		Nodes: []protocol.Node{{
			ID:   "create:orders_2026_01",
			Kind: protocol.KindIndexCreateConcurrently,
			Params: &protocol.CreateConcurrentlyParams{
				Partition:   leaf,
				Index:       victim,
				ParentIndex: &parentIndex,
				Definition: protocol.IndexDefinition{
					Columns: []protocol.IndexColumn{{Name: "created_at"}},
				},
			},
		}},
	}
	if err := crashed.Seal(); err != nil {
		h.t.Fatalf("Seal: %v", err)
	}
	if crashed.Digest == p.Digest {
		h.t.Fatal("fixture error: the crashed run's plan must have a different digest")
	}
	run := h.seedRun(crashed)

	// The node dispatched, so its record claims the object. The process then
	// died before the ownership marker could be written.
	for _, to := range []protocol.NodeState{protocol.NodeReady, protocol.NodeRunning} {
		from := protocol.NodePending
		if to == protocol.NodeRunning {
			from = protocol.NodeReady
		}
		if _, err := h.Store.TransitionNode(ctx(), state.NodeTransition{
			RunID: run.RunID, NodeID: "create:orders_2026_01", From: from, To: to,
			IncrementAttempt: to == protocol.NodeRunning,
		}); err != nil {
			h.t.Fatalf("TransitionNode: %v", err)
		}
	}

	if _, err := h.Store.SetRunStatus(ctx(), state.RunStatusUpdate{
		RunID: run.RunID,
		From:  state.RunRunning,
		To:    state.RunFailed,
		Error: "killed mid-build",
		At:    h.Now(),
	}); err != nil {
		h.t.Fatalf("SetRunStatus: %v", err)
	}
}

// TestExecuteRefusesADifferentDatabase is T8: protocol.Target records the
// database "so that a plan cannot be executed against an unintended database by
// accident", and nothing was checking it.
//
// The topology fingerprint does not cover this. It is OID-based, and
// `CREATE DATABASE staging TEMPLATE prod` is a physical copy that preserves
// every pg_class OID, so the two databases fingerprint identically and the
// drift check passes.
func TestExecuteRefusesADifferentDatabase(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()

	// PGDATABASE still points at production; the plan was built against
	// staging. Every OID, and therefore the fingerprint, is unchanged.
	h.Cat.Database = "prod"

	code := h.Run("execute", plan)
	if code == int(protocol.ExitSuccess) {
		t.Fatalf("execute ran a plan against the wrong database: %s", h.Out())
	}
	if h.SQL.DDLCount() != 0 {
		t.Errorf("execute issued %d statement(s) against the wrong database", h.SQL.DDLCount())
	}
	if !strings.Contains(h.Out(), "prod") || !strings.Contains(h.Out(), testDBName) {
		t.Errorf("the refusal does not name both databases: %s", h.Out())
	}
}

// TestAllowDriftDoesNotOverrideTheDatabaseCheck: --allow-drift is about the
// shape of a tree that is still the right tree, not about which database it is
// in, so it must not open this door.
func TestAllowDriftDoesNotOverrideTheDatabaseCheck(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()
	h.Cat.Database = "prod"

	if code := h.Run("execute", "--allow-drift", plan); code == int(protocol.ExitSuccess) {
		t.Fatalf("--allow-drift let a plan run against the wrong database: %s", h.Out())
	}
	if h.SQL.DDLCount() != 0 {
		t.Errorf("issued %d statement(s) under --allow-drift against the wrong database", h.SQL.DDLCount())
	}
}

// TestResumeRefusesADifferentDatabase closes the same hole on the command that
// actually issues destructive statements.
func TestResumeRefusesADifferentDatabase(t *testing.T) {
	h := newHarness(t)
	p := h.dropPlan()
	plan := h.WritePlan(p)
	loaded := h.LoadPlan(plan)
	h.seedClaim(loaded)
	h.seedRun(loaded)

	h.Cat.Database = "prod"

	if code := h.Run("resume", plan); code == int(protocol.ExitSuccess) {
		t.Fatalf("resume ran against the wrong database: %s", h.Out())
	}
	if h.SQL.Issued("DROP INDEX") {
		t.Error("resume dropped an index in a database the plan was not built against (T8)")
	}
}

// TestUnscopedClaimLookupFailsClosed is FR-AUTH-2 at the bridge.
//
// A file store deliberately holds state for more than one target, so an
// unscoped claim lookup can report a claim held by a run against a *different*
// database. Adoption is the one path that destroys an object on the strength of
// a claim alone, so answering it unscoped would let a crashed staging run
// authorize dropping a production index.
func TestUnscopedClaimLookupFailsClosed(t *testing.T) {
	h := newHarness(t)

	p := h.dropPlan()
	plan := h.WritePlan(p)
	h.seedClaim(h.LoadPlan(plan))

	victim := obj("public", protocol.ChildIndexName(testIndex, "orders_2026_01"))

	scoped := newExecutorStore(h.Store, testDBName)
	if _, found, err := scoped.ClaimsObject(ctx(), victim); err != nil || !found {
		t.Fatalf("scoped lookup found = %v, err = %v; want the claim to be findable", found, err)
	}

	unscoped := newExecutorStore(h.Store, "")
	if _, found, err := unscoped.ClaimsObject(ctx(), victim); err != nil || found {
		t.Errorf("an unscoped ClaimsObject returned found = %v (err %v); a claim that cannot be "+
			"scoped to a database has not been proven (FR-AUTH-2)", found, err)
	}

	// And the planner's side of the same port.
	if _, found, err := (claimLookup{store: h.Store}).ClaimsObject(ctx(), victim); err != nil || found {
		t.Errorf("an unscoped claimLookup returned found = %v (err %v)", found, err)
	}
}
