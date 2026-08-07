package cli

import (
	"strings"
	"testing"

	"github.com/atulsinha/partitionctl/engine/protocol"
	"github.com/atulsinha/partitionctl/engine/state"
)

// FR-CLI-9: "`resume` SHALL be the only command permitted to perform
// `provenance`-authorized cleanup."
//
// TRD §7.2.12 explains why the two commands are separate at all: resume is
// where INVALID indexes left by a previous process get dropped, and "silently
// doing that under `execute` would mean a routine re-run can destroy catalog
// objects".
//
// The guard that was in place keyed on the plan *digest*, which any re-plan
// defeats: a fresh plan has a new CreatedAt and therefore a new digest, so no
// prior run matches and execute proceeds straight into the drop.

// TestExecuteRefusesProvenanceCleanup is the FR-CLI-9 refusal.
func TestExecuteRefusesProvenanceCleanup(t *testing.T) {
	h := newHarness(t)
	p := h.dropPlan()
	plan := h.WritePlan(p)

	// Provenance exists, so the drop *would* be authorized. That is the
	// dangerous case: the authorization check passes and only the command
	// split stands between a routine re-run and a dropped index.
	h.seedProvenance(h.LoadPlan(plan))

	code := h.Run("execute", plan)
	if code == int(protocol.ExitSuccess) {
		t.Fatalf("execute completed a provenance-authorized drop; FR-CLI-9 reserves that to resume: %s", h.Out())
	}
	if h.SQL.Issued("DROP INDEX") {
		t.Errorf("execute issued a provenance-authorized DROP INDEX CONCURRENTLY (FR-CLI-9):\n%v", h.SQL.SQLTexts())
	}
	if !strings.Contains(h.Out(), "resume") {
		t.Errorf("the refusal does not name `resume`: %s", h.Out())
	}
	if h.SQL.DDLCount() != 0 {
		t.Errorf("the refusal issued %d statement(s); it must issue none before the lock", h.SQL.DDLCount())
	}
}

// TestExecuteRefusesBeforeTakingTheLock proves the refusal is a pre-flight
// check rather than a mid-run halt: a run that got as far as the executor would
// already have created a run record and taken the advisory lock.
func TestExecuteRefusesProvenanceCleanupWithoutCreatingARun(t *testing.T) {
	h := newHarness(t)
	plan := h.WritePlan(h.dropPlan())
	loaded := h.LoadPlan(plan)
	h.seedProvenance(loaded)

	_ = h.Run("execute", plan)

	// Only runs of *this* plan count: seedProvenance deliberately leaves a
	// failed run of the older artifact behind.
	var mine []state.Run
	for _, r := range h.Runs() {
		if r.PlanDigest == loaded.Digest {
			mine = append(mine, r)
		}
	}
	if len(mine) != 0 {
		t.Errorf("execute created %d run(s) for a plan it must refuse: %+v", len(mine), mine)
	}
}

// TestResumePerformsProvenanceCleanup is the other half of FR-CLI-9: the
// command that IS permitted must still do it, or the refusal above would just
// be a dead end with no path forward.
func TestResumePerformsProvenanceCleanup(t *testing.T) {
	h := newHarness(t)
	p := h.dropPlan()
	plan := h.WritePlan(p)
	loaded := h.LoadPlan(plan)
	h.seedProvenance(loaded)
	h.seedRun(loaded)

	if code := h.Run("resume", plan); code != int(protocol.ExitSuccess) {
		t.Fatalf("resume exited %d, want 0: %s", code, h.Out())
	}
	if !h.SQL.Issued("DROP INDEX CONCURRENTLY") {
		t.Errorf("resume did not issue the authorized drop:\n%v", h.SQL.SQLTexts())
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

// seedProvenance commits a provenance record for every object a plan's
// destructive nodes name, which is what makes the drop authorizable.
//
// The record is written under a run of a *different* plan, because that is the
// scenario FR-CLI-9 is about: a run crashed, the operator re-planned, and the
// new artifact has a new digest. Writing it under this plan's own digest would
// instead trip the prior-run guard and prove nothing about the drop.
func (h *harness) seedProvenance(p *protocol.Plan) {
	h.t.Helper()

	older := h.dropPlan()
	older.PlanID = "plan-drop-fixture-older"
	if err := older.Seal(); err != nil {
		h.t.Fatalf("Seal: %v", err)
	}
	if older.Digest == p.Digest {
		h.t.Fatal("fixture error: the prior plan must have a different digest")
	}
	run := h.seedRun(older)
	if _, err := h.Store.SetRunStatus(ctx(), state.RunStatusUpdate{
		RunID: run.RunID,
		From:  state.RunRunning,
		To:    state.RunFailed,
		Error: "killed mid-build",
		At:    h.Now(),
	}); err != nil {
		h.t.Fatalf("SetRunStatus: %v", err)
	}

	for i := range p.Nodes {
		n := &p.Nodes[i]
		if n.Authorization == nil || n.Authorization.Mode != protocol.AuthProvenance {
			continue
		}
		_, err := h.Store.WriteProvenance(ctx(), state.Provenance{
			RunID:      run.RunID,
			NodeID:     n.ID,
			PlanDigest: p.Digest,
			Database:   p.Target.Database,
			Object:     n.Authorization.Object,
			ObjectKind: state.ObjectKind("index"),
			Relation:   n.Authorization.Relation,
			RecordedAt: protocol.NewTimestamp(h.Now()),
		}, nil)
		if err != nil {
			h.t.Fatalf("WriteProvenance: %v", err)
		}
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
	h.seedProvenance(loaded)
	h.seedRun(loaded)

	h.Cat.Database = "prod"

	if code := h.Run("resume", plan); code == int(protocol.ExitSuccess) {
		t.Fatalf("resume ran against the wrong database: %s", h.Out())
	}
	if h.SQL.Issued("DROP INDEX") {
		t.Error("resume dropped an index in a database the plan was not built against (T8)")
	}
}

// TestUnscopedProvenanceLookupFailsClosed is FR-AUTH-2 at the bridge.
//
// ProvenanceQuery treats an empty Database as "match any", and a file store
// deliberately holds state for more than one target, so an unscoped lookup can
// return a record proving PartitionCTL built an index of this name in a
// *different* database. provenanceLookup already refused to answer from that;
// the executor's dispatch-time re-check, which FR-AUTH-5 makes the last line of
// defence, did not.
func TestUnscopedProvenanceLookupFailsClosed(t *testing.T) {
	h := newHarness(t)

	// A record exists, and it is the only record: an unscoped query matches it.
	p := h.dropPlan()
	plan := h.WritePlan(p)
	h.seedProvenance(h.LoadPlan(plan))

	victim := obj("public", protocol.ChildIndexName(testIndex, "orders_2026_01"))

	scoped := newExecutorStore(h.Store, testDBName)
	if _, found, err := scoped.LookupProvenance(ctx(), victim); err != nil || !found {
		t.Fatalf("scoped lookup found = %v, err = %v; want the record to be findable", found, err)
	}

	unscoped := newExecutorStore(h.Store, "")
	if _, found, err := unscoped.LookupProvenance(ctx(), victim); err != nil || found {
		t.Errorf("an unscoped LookupProvenance returned found = %v (err %v); ownership that "+
			"cannot be scoped to a database has not been proven (FR-AUTH-2)", found, err)
	}
	if _, err := unscoped.provenanceID(ctx(), victim); err == nil {
		t.Error("an unscoped provenanceID produced evidence for a destructive action (INV-2)")
	}
}
