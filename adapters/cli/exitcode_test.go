package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/atulsinha87/partitionctl/engine/protocol"
	"github.com/atulsinha87/partitionctl/engine/state"
)

// AC-26: "Each failure class in §7.2.12 produces its distinct exit code,
// verified by one test per code."
//
// These tests drive App.Run end to end, which is what the criterion literally
// requires. Asserting on the executor's error before the CLI wraps it does not
// establish the criterion: the CLI is what the process returns, and it is where
// a re-typed error silently collapses a class.

// TestExitSuccess is exit 0: a run that completes.
func TestExitSuccess(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()
	if code := h.Run("execute", plan); code != int(protocol.ExitSuccess) {
		t.Fatalf("execute exited %d, want 0: %s", code, h.Out())
	}
	if !strings.Contains(h.Out(), "complete") {
		t.Errorf("output does not report completion: %s", h.Out())
	}
}

// TestExitGenericFailure is exit 1: a plan file that is not there.
func TestExitGenericFailure(t *testing.T) {
	h := newHarness(t)
	if code := h.Run("execute", h.Dir+"/absent.json"); code != int(protocol.ExitFailure) {
		t.Fatalf("execute exited %d, want 1: %s", code, h.Out())
	}
}

// TestExitDigestMismatch is exit 10 (FR-PLANFILE-3, AC-2, T1).
func TestExitDigestMismatch(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()

	data, err := os.ReadFile(plan)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	// Change the plan without changing its digest: exactly threat T1. The
	// partition a build targets is a value, so the file still round-trips and
	// the digest is what catches it.
	tampered := strings.Replace(string(data), `"orders_2026_01"`, `"orders_2026_99"`, 1)
	if tampered == string(data) {
		t.Fatal("plan did not contain the value to tamper with")
	}
	if err := os.WriteFile(plan, []byte(tampered), 0o644); err != nil {
		t.Fatalf("write plan: %v", err)
	}

	if code := h.Run("execute", plan); code != int(protocol.ExitDigestMismatch) {
		t.Fatalf("execute exited %d, want 10: %s", code, h.Out())
	}
	if h.SQL.DDLCount() != 0 {
		t.Errorf("a tampered plan issued %d statement(s); it must issue none", h.SQL.DDLCount())
	}
}

// TestExitTopologyDrift is exit 11 (FR-PLANFILE-5, AC-3).
func TestExitTopologyDrift(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()

	// The expected drift for a time-partitioned table: a new partition.
	h.addLeaf("orders_2026_04", 3)

	code := h.Run("execute", plan)
	if code != int(protocol.ExitTopologyDrift) {
		t.Fatalf("execute exited %d, want 11: %s", code, h.Out())
	}
	if h.SQL.DDLCount() != 0 {
		t.Errorf("drift refusal issued %d statement(s); it must issue none", h.SQL.DDLCount())
	}

	// AC-3's second clause: the refusal must *name* the drift. Printing two
	// hashes and the whole live partition list is detection, not naming; at
	// 400 partitions it leaves the operator to diff by hand.
	out := h.Out()
	if !strings.Contains(out, "orders_2026_04") {
		t.Errorf("the refusal does not name the partition that appeared:\n%s", out)
	}
	if !strings.Contains(out, string(protocol.TopologyPartitionAdded)) {
		t.Errorf("the refusal does not say what kind of change happened:\n%s", out)
	}
	// And it must not name the partitions that did not change, or "named" is
	// indistinguishable from "listed everything".
	if strings.Contains(out, "orders_2026_02") {
		t.Errorf("the refusal lists unchanged partitions, so it does not isolate the drift:\n%s", out)
	}
}

// TestExitLockHeld is exit 12 (FR-LOCK-1, FR-LOCK-2, AC-10).
func TestExitLockHeld(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()

	p := h.LoadPlan(plan)
	lock, err := h.Store.TryLock(ctx(), lockKeyFor(p))
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	defer func() { _ = lock.Unlock(ctx()) }()

	if code := h.Run("execute", plan); code != int(protocol.ExitLockHeld) {
		t.Fatalf("execute exited %d, want 12: %s", code, h.Out())
	}
	if h.SQL.DDLCount() != 0 {
		t.Errorf("a refused run issued %d statement(s)", h.SQL.DDLCount())
	}
}

// TestExitAuthorizationUnsatisfied is exit 13 (FR-AUTH-5, INV-2, AC-6).
//
// Exit 13 can only arise during the walk, so it is unreachable by any test that
// stops at the planner or asserts on the executor's error directly.
func TestExitAuthorizationUnsatisfied(t *testing.T) {
	h := newHarness(t)
	// A drop node authorized by provenance, with no provenance record to
	// satisfy it. resume is the command permitted to run destructive cleanup.
	p := h.dropPlan()
	plan := h.WritePlan(p)
	h.seedRun(h.LoadPlan(plan))

	code := h.Run("resume", plan)
	if code != int(protocol.ExitAuthorizationUnsatisfied) {
		t.Fatalf("resume exited %d, want 13: %s", code, h.Out())
	}
	if h.SQL.Issued("DROP INDEX") {
		t.Error("an unauthorized destructive node issued its DROP (INV-2)")
	}
}

// TestExitVerificationFailed is exit 14 (FR-VER-*).
//
// Like exit 13, it can only arise during the walk.
func TestExitVerificationFailed(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()

	// The statements succeed but the catalog never reflects them, which is what
	// a silently failed build looks like to the verifier.
	h.SQL.Effects = nil

	if code := h.Run("execute", plan); code != int(protocol.ExitVerificationFailed) {
		t.Fatalf("execute exited %d, want 14: %s", code, h.Out())
	}
}

// TestExitUnsupportedTopology is exit 15 (FR-PLAN-2, FR-PLAN-3, AC-11).
//
// The plan is reviewed on Monday and executed on Thursday; in between the table
// is re-partitioned. The catalog.assert node is what catches it, and it must
// fail with the planner's own exit code.
func TestExitUnsupportedTopology(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()

	h.Cat.SetStrategy(rootOID, protocol.StrategyHash)

	if code := h.Run("execute", plan); code != int(protocol.ExitUnsupportedTopology) {
		t.Fatalf("execute exited %d, want 15: %s", code, h.Out())
	}
	if h.SQL.DDLCount() != 0 {
		t.Errorf("a run halted on preconditions issued %d statement(s)", h.SQL.DDLCount())
	}
}

// TestExitInsufficientPrivilege is exit 16 (FR-PLAN-10, AC-12).
func TestExitInsufficientPrivilege(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()

	// The grant is revoked between review and execution.
	h.Cat.Members[ownerOID] = false

	if code := h.Run("execute", plan); code != int(protocol.ExitInsufficientPrivilege) {
		t.Fatalf("execute exited %d, want 16: %s", code, h.Out())
	}
	if h.SQL.DDLCount() != 0 {
		t.Errorf("a run halted on privileges issued %d statement(s)", h.SQL.DDLCount())
	}
}

// TestEveryExitCodeIsDistinct guards the contract itself: AC-26 is about codes
// being *distinct*, so a table that maps two classes to one code is a defect
// even when each individual test passes.
func TestEveryExitCodeIsDistinct(t *testing.T) {
	codes := map[protocol.ExitCode]string{}
	for _, e := range []*protocol.Error{
		protocol.ErrFailure,
		protocol.ErrDigestMismatch,
		protocol.ErrTopologyDrift,
		protocol.ErrLockHeld,
		protocol.ErrAuthorizationUnsatisfied,
		protocol.ErrVerificationFailed,
		protocol.ErrUnsupportedTopology,
		protocol.ErrInsufficientPrivilege,
	} {
		if prev, dup := codes[e.Code]; dup && prev != string(e.Kind) {
			t.Errorf("exit %d is used by both %q and %q", e.Code, prev, e.Kind)
		}
		codes[e.Code] = string(e.Kind)
	}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// addLeaf registers a new partition in the fake catalog, which is drift.
func (h *harness) addLeaf(name string, i int) {
	h.Cat.AddRelation(plannerRelation(leafBase+uint32(i), name))
	h.Verify.Leaves = append(h.Verify.Leaves, obj("public", name))
}

// dropPlan hand-builds a minimal plan whose only node is a provenance-authorized
// drop. The create-index planner emits one of these only after finding an
// INVALID index with provenance, and the point of the test is the case where the
// provenance is *absent* at dispatch.
func (h *harness) dropPlan() *protocol.Plan {
	victim := obj("public", protocol.ChildIndexName(testIndex, "orders_2026_01"))
	relation := obj("public", "orders_2026_01")
	index := obj("public", testIndex)
	table := obj("public", "orders")

	return &protocol.Plan{
		FormatVersion: protocol.PlanFormatVersion,
		PlanID:        "plan-drop-fixture",
		Operation:     protocol.OpCreateIndex,
		CreatedAt:     protocol.NewTimestamp(h.Now()),
		Target: protocol.Target{
			Database: testDBName,
			Table:    table,
			Index:    &index,
		},
		TopologyFingerprint: h.liveFingerprint(),
		Nodes: []protocol.Node{{
			ID:   "drop:orders_2026_01",
			Kind: protocol.KindIndexDropConcurrently,
			Params: &protocol.DropConcurrentlyParams{
				Index:    victim,
				Relation: &relation,
				Reason:   protocol.DropInvalidBuild,
			},
			Authorization: &protocol.Authorization{
				Mode:     protocol.AuthProvenance,
				Object:   victim,
				Relation: &relation,
			},
		}},
	}
}

// liveFingerprint is the fingerprint of the harness's current tree, so a
// hand-built plan passes the drift check and reaches the walk.
func (h *harness) liveFingerprint() string {
	p := &protocol.Plan{Target: protocol.Target{Table: obj("public", "orders")}}
	topo, err := discoverLive(ctx(), &Target{Catalog: h.Cat}, p)
	if err != nil {
		h.t.Fatalf("discoverLive: %v", err)
	}
	fp, err := topo.Input().Fingerprint()
	if err != nil {
		h.t.Fatalf("Fingerprint: %v", err)
	}
	return fp
}

// seedRun records an incomplete run for a plan, which is what `resume` adopts.
func (h *harness) seedRun(p *protocol.Plan) state.Run {
	h.t.Helper()
	now := h.Now()
	run, err := h.Store.CreateRun(ctx(), state.NewRun{
		Plan:      p,
		RunID:     state.NewRunID(now),
		Actor:     "tester",
		StartedAt: now,
	})
	if err != nil {
		h.t.Fatalf("CreateRun: %v", err)
	}
	return run
}

// TestRoleMembershipIsCheckedAgainstTheConnectedRole is FR-PLAN-10 and AC-12 at
// execute time.
//
// TRD §11.2 says role membership is validated "so a permission failure surfaces
// before a multi-hour run, not during it", and assert.go exists so that a plan
// reviewed Monday and executed Thursday fails with the planner's own exit code.
// Neither holds if the assertion re-checks the role recorded in the plan rather
// than the role that is actually connected: an engineer plans on a workstation
// as alice, CI executes as ci_deploy, and the assertion happily confirms
// alice's memberships while ci_deploy is the one that will run the DDL.
func TestRoleMembershipIsCheckedAgainstTheConnectedRole(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()

	// CI connects as a different role, which is not a member of the owning
	// role. The planned role, migrator, still is.
	h.Cat.Role = "ci_deploy"
	h.Cat.SetRoleMember("ci_deploy", ownerOID, false)

	code := h.Run("execute", plan)
	if code != int(protocol.ExitInsufficientPrivilege) {
		t.Fatalf("execute exited %d, want 16: the assertion checked the planned role rather than "+
			"the connected one, so the run would build for hours and fail on the first "+
			"partition it cannot touch: %s", code, h.Out())
	}
	if h.SQL.DDLCount() != 0 {
		t.Errorf("issued %d statement(s) before failing on privileges", h.SQL.DDLCount())
	}
	if !strings.Contains(h.Out(), "ci_deploy") {
		t.Errorf("the failure does not name the connected role: %s", h.Out())
	}
}

// TestADifferentConnectedRoleWithThePrivilegesMayProceed keeps the check from
// becoming a role-identity check. Executing a reviewed plan as a different role
// is normal (a workstation plans, CI executes); what matters is whether the
// connected role holds the owner's privileges, not whether it is the same
// string the plan recorded.
func TestADifferentConnectedRoleWithThePrivilegesMayProceed(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()

	// A different role that does hold the privileges.
	h.Cat.Role = "ci_deploy"
	h.Cat.SetRoleMember("ci_deploy", ownerOID, true)

	if code := h.Run("execute", plan); code != int(protocol.ExitSuccess) {
		t.Fatalf("execute exited %d; a connected role that holds the privileges may proceed: %s",
			code, h.Out())
	}
}

// TestPlanOpensTheStateStoreReadOnly is AC-1 and FR-PLAN-8: `plan` reads the
// catalog and issues no DDL.
//
// The planner core is clean: BeginReadOnly opens READ ONLY REPEATABLE READ and
// AssertReadOnly proves it. The hole was in the CLI's store wiring. The SQL
// state store bootstraps its schema lazily on the first read that needs it, and
// the read `plan` performs is the provenance lookup, which fires whenever an
// index is not healthy: precisely the post-crash re-plan. That bootstrap is
// eighteen statements including CREATE SCHEMA, CREATE FUNCTION, CREATE TRIGGER
// and REVOKE, issued against the target by a command documented as read-only.
func TestPlanOpensTheStateStoreReadOnly(t *testing.T) {
	h := newHarness(t)
	_ = h.MustPlan()

	if len(h.StoreIntents) == 0 {
		t.Fatal("plan opened no state store, so provenance was never consulted")
	}
	for i, intent := range h.StoreIntents {
		if intent != StoreReadOnly {
			t.Errorf("plan opened state store %d with intent %v, want StoreReadOnly: a writable "+
				"SQL store bootstraps its schema on first read, which makes `plan` issue DDL (AC-1)",
				i, intent)
		}
	}
}

// TestExecuteOpensTheStateStoreReadWrite is the other side: execute owns the
// run and must be able to create the schema it checkpoints into.
func TestExecuteOpensTheStateStoreReadWrite(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()
	h.StoreIntents = nil

	if code := h.Run("execute", plan); code != int(protocol.ExitSuccess) {
		t.Fatalf("execute exited %d: %s", code, h.Out())
	}
	if len(h.StoreIntents) == 0 {
		t.Fatal("execute opened no state store")
	}
	for i, intent := range h.StoreIntents {
		if intent != StoreReadWrite {
			t.Errorf("execute opened state store %d with intent %v, want StoreReadWrite", i, intent)
		}
	}
}
