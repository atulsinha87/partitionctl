package executor

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// ---------------------------------------------------------------------------
// Ordering: FR-ORD-1, FR-ORD-2
// ---------------------------------------------------------------------------

func TestRunDispatchesInTopologicalOrder(t *testing.T) {
	h := newHarness()
	// Nodes are listed in reverse dependency order on purpose: the executor
	// must walk the edges, not the slice.
	plan := newPlan(t,
		node("nD", protocol.KindIndexAttach, &protocol.AttachParams{
			ParentIndex: obj(t, "public.parent_idx"),
			ChildIndex:  obj(t, "public.child_d"),
		}, "nB", "nC"),
		node("nB", protocol.KindIndexAttach, &protocol.AttachParams{
			ParentIndex: obj(t, "public.parent_idx"),
			ChildIndex:  obj(t, "public.child_b"),
		}, "nA"),
		node("nC", protocol.KindIndexAttach, &protocol.AttachParams{
			ParentIndex: obj(t, "public.parent_idx"),
			ChildIndex:  obj(t, "public.child_c"),
		}, "nA"),
		node("nA", protocol.KindIndexAttach, &protocol.AttachParams{
			ParentIndex: obj(t, "public.parent_idx"),
			ChildIndex:  obj(t, "public.child_a"),
		}),
	)

	res, err := h.run(t, plan)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"exec:nA", "exec:nB", "exec:nC", "exec:nD"}
	if got := h.rec.withPrefix("exec:"); !reflect.DeepEqual(got, want) {
		t.Fatalf("dispatch order = %v, want %v", got, want)
	}
	wantOrder, err := plan.TopologicalOrder()
	if err != nil {
		t.Fatalf("TopologicalOrder: %v", err)
	}
	if !reflect.DeepEqual(res.Order, wantOrder) {
		t.Fatalf("Result.Order = %v, want %v", res.Order, wantOrder)
	}
	if !res.Complete() {
		t.Fatalf("run did not complete: %+v", res)
	}
}

func TestRunRefusesCyclicGraph(t *testing.T) {
	h := newHarness()
	// Built by hand: newPlan validates, and this graph must not validate.
	plan := &protocol.Plan{
		FormatVersion: protocol.PlanFormatVersion,
		PlanID:        "plan-cycle",
		Operation:     protocol.OpCreateIndex,
		Target:        protocol.Target{Table: obj(t, "public.orders")},
		CreatedAt:     protocol.NewTimestamp(time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)),
		Nodes: []protocol.Node{
			node("n1", protocol.KindIndexAttach, &protocol.AttachParams{
				ParentIndex: obj(t, "public.parent_idx"),
				ChildIndex:  obj(t, "public.child_1"),
			}, "n2"),
			node("n2", protocol.KindIndexAttach, &protocol.AttachParams{
				ParentIndex: obj(t, "public.parent_idx"),
				ChildIndex:  obj(t, "public.child_2"),
			}, "n1"),
		},
	}
	// Seal it, so the refusal is about the cycle and not about the digest.
	if err := plan.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	_, err := h.run(t, plan)
	if err == nil {
		t.Fatal("expected a cyclic plan to be refused")
	}
	if !errors.Is(err, protocol.ErrInvalidPlan) {
		t.Fatalf("error = %v, want ErrInvalidPlan", err)
	}
	if h.sql.execCount() != 0 {
		t.Fatalf("a cyclic plan issued %d statements; want 0", h.sql.execCount())
	}
}

// TestDispatchRefusesADependencyThatDidNotComplete exercises the FR-ORD-1 guard
// directly.
//
// The topological walk makes this unreachable through [Executor.Run] by
// construction, which is the point: the guard exists so that a corrupted or
// buggy state store degrades into a refusal rather than into dependency-order
// violations against a live database. Driving runNode is the only honest way to
// prove it fires.
func TestDispatchRefusesADependencyThatDidNotComplete(t *testing.T) {
	h := newHarness()
	plan := createChainPlan(t)
	e := h.executor(t)

	n3, _ := plan.NodeByID("n3")
	states := map[protocol.NodeID]*nodeStatus{
		"n2": {State: protocol.NodeReady}, // never completed
		"n3": {State: protocol.NodePending},
	}
	ctx := context.Background()

	err := e.runNode(ctx, ctx, "run-1", plan, n3, states["n3"], states)
	if !errors.Is(err, ErrDependencyNotComplete) {
		t.Fatalf("error = %v, want ErrDependencyNotComplete", err)
	}
	if h.sql.execCount() != 0 {
		t.Fatal("a node ran before its dependency completed")
	}
}

// ---------------------------------------------------------------------------
// Checkpointing: FR-EXEC-2
// ---------------------------------------------------------------------------

func TestCheckpointPrecedesAndFollowsEveryDispatch(t *testing.T) {
	h := newHarness()
	plan := createChainPlan(t)

	if _, err := h.run(t, plan); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The statement is bracketed by durable checkpoints on both sides.
	h.rec.mustPrecede(t, "transition:n3:READY->RUNNING", "exec:n3")
	h.rec.mustPrecede(t, "exec:n3", "transition:n3:RUNNING->VERIFYING")
	h.rec.mustPrecede(t, "transition:n3:RUNNING->VERIFYING", "transition:n3:VERIFYING->DONE")
	// And a node is never dispatched before its predecessor is recorded done.
	h.rec.mustPrecede(t, "transition:n2:VERIFYING->DONE", "transition:n3:PENDING->READY")
}

func TestCheckpointFailureHaltsBeforeTheStatement(t *testing.T) {
	h := newHarness()
	boom := errors.New("state store unavailable")
	h.store.failTransition = func(tr Transition) error {
		if tr.NodeID == "n2" && tr.To == protocol.NodeRunning {
			return boom
		}
		return nil
	}
	plan := createChainPlan(t)

	_, err := h.run(t, plan)
	if !errors.Is(err, ErrCheckpointFailed) {
		t.Fatalf("error = %v, want ErrCheckpointFailed", err)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("error %v does not wrap the store failure", err)
	}
	if h.sql.execCount() != 0 {
		t.Fatalf("a statement ran after an unrecorded checkpoint (%d executed); FR-EXEC-2 requires halting", h.sql.execCount())
	}
}

func TestProvenanceIsCommittedBeforeTheCreatingDDL(t *testing.T) {
	h := newHarness()
	plan := createChainPlan(t)

	if _, err := h.run(t, plan); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// INV-1, for both kinds that create an object.
	h.rec.mustPrecede(t, "provenance:public.orders_created_at_idx", "exec:n2")
	h.rec.mustPrecede(t, "provenance:public.orders_created_at_idx_orders_2026_03", "exec:n3")

	if _, ok := h.store.provenance["public.orders_created_at_idx_orders_2026_03"]; !ok {
		t.Fatal("no provenance recorded for the leaf index")
	}
	// A node that creates nothing records nothing.
	if len(h.rec.withPrefix("provenance:")) != 2 {
		t.Fatalf("provenance events = %v, want exactly the two create nodes", h.rec.withPrefix("provenance:"))
	}
}

func TestProvenanceFailureHaltsBeforeTheDDL(t *testing.T) {
	h := newHarness()
	h.store.failProvenance = errors.New("cannot commit provenance")
	plan := createChainPlan(t)

	_, err := h.run(t, plan)
	if !errors.Is(err, ErrCheckpointFailed) {
		t.Fatalf("error = %v, want ErrCheckpointFailed", err)
	}
	if h.sql.execCount() != 0 {
		t.Fatalf("DDL ran without committed provenance; INV-1 forbids it")
	}
}

// ---------------------------------------------------------------------------
// Retry: FR-EXEC-3, FR-EXEC-4
// ---------------------------------------------------------------------------

// The retry drills use n5 (index.attach), the one DDL kind whose statement is
// idempotent and therefore may be re-issued in process. The other DDL kinds
// commit catalog state before they can fail, so re-sending them is a defect
// rather than a retry; that is covered by
// TestRetryableFailureOfANonIdempotentKindStopsTheRun.
func TestRetryableFailureRetriesWithBoundedBackoff(t *testing.T) {
	h := newHarness()
	h.sql.errs["n5"] = []error{
		&pgErr{code: "40P01", msg: "deadlock detected"},
		&pgErr{code: "08006", msg: "connection failure"},
	}
	plan := createChainPlan(t)

	res, err := h.run(t, plan)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Complete() {
		t.Fatalf("run did not converge after retries: %+v", res)
	}
	if got := len(h.rec.withPrefix("exec:n5")); got != 3 {
		t.Fatalf("n5 executed %d times, want 3 (two retryable failures then success)", got)
	}
	// Backoff doubles: 100ms then 200ms, with jitter disabled in the harness.
	sleeps := h.clock.sleptFor()
	if len(sleeps) < 2 || sleeps[0] != 100*time.Millisecond || sleeps[1] != 200*time.Millisecond {
		t.Fatalf("backoff delays = %v, want [100ms 200ms ...]", sleeps)
	}
	wantEdges := []string{
		"transition:n5:RUNNING->RETRY_WAIT",
		"transition:n5:RETRY_WAIT->READY",
	}
	for _, e := range wantEdges {
		if h.rec.indexOf(e) < 0 {
			t.Fatalf("missing transition %q; log: %v", e, h.rec.all())
		}
	}
}

func TestRetryBudgetExhaustionFailsTheNodeAndHaltsTheRun(t *testing.T) {
	h := newHarness()
	h.sql.errs["n5"] = []error{
		&pgErr{code: "55P03", msg: "lock timeout"},
		&pgErr{code: "55P03", msg: "lock timeout"},
		&pgErr{code: "55P03", msg: "lock timeout"},
	}
	plan := createChainPlan(t)

	res, err := h.run(t, plan)
	if err == nil {
		t.Fatal("expected the run to fail once the retry budget was exhausted")
	}
	if got := len(h.rec.withPrefix("exec:n5")); got != 3 {
		t.Fatalf("n5 executed %d times, want exactly MaxAttempts=3", got)
	}
	if got := h.store.stateOf("n5"); got != protocol.NodeFailed {
		t.Fatalf("n5 is %s, want FAILED", got)
	}
	if res.HaltedAt != "n5" {
		t.Fatalf("Result.HaltedAt = %q, want n5", res.HaltedAt)
	}
	// Everything downstream is untouched, so the run stays resumable.
	if got := h.store.stateOf("n7"); got != protocol.NodePending {
		t.Fatalf("downstream node n7 is %s, want PENDING", got)
	}
	if got := len(h.rec.withPrefix("exec:n7")); got != 0 {
		t.Fatal("a node downstream of a failure was dispatched")
	}
}

func TestTerminalFailureIsNotRetried(t *testing.T) {
	h := newHarness()
	h.sql.errs["n2"] = []error{&pgErr{code: "42601", msg: "syntax error at or near"}}
	plan := createChainPlan(t)

	_, err := h.run(t, plan)
	if err == nil {
		t.Fatal("expected a terminal failure to fail the run")
	}
	if got := len(h.rec.withPrefix("exec:n2")); got != 1 {
		t.Fatalf("n2 executed %d times; a syntax error must not be retried", got)
	}
	if got := len(h.clock.sleptFor()); got != 0 {
		t.Fatalf("backoff slept %d times after a terminal error", got)
	}
	if got := h.store.stateOf("n2"); got != protocol.NodeFailed {
		t.Fatalf("n2 is %s, want FAILED", got)
	}
}

func TestPrivilegeFailureCarriesItsExitCode(t *testing.T) {
	h := newHarness()
	h.sql.errs["n2"] = []error{&pgErr{code: "42501", msg: "must be owner of table orders"}}

	_, err := h.run(t, createChainPlan(t))
	if got := protocol.ExitCodeFor(err); got != protocol.ExitInsufficientPrivilege {
		t.Fatalf("exit code = %d, want %d (AC-26)", got, protocol.ExitInsufficientPrivilege)
	}
}

// ---------------------------------------------------------------------------
// Cancellation: FR-CLI-10, FR-ORD-4, FR-EXEC-8
// ---------------------------------------------------------------------------

func TestCancelFlagStopsAtTheNextNodeBoundary(t *testing.T) {
	h := newHarness()
	// The flag is polled once per incomplete node; trip it on the third poll,
	// which is the boundary before n3.
	h.store.cancelAfter = 3
	plan := createChainPlan(t)

	res, err := h.run(t, plan)
	if err != nil {
		t.Fatalf("a cancelled run is not a failure, got %v", err)
	}
	if !res.Cancelled || res.CancelReason != StopCancelFlag {
		t.Fatalf("Result = %+v, want Cancelled with reason %q", res, StopCancelFlag)
	}
	if got := h.store.stateOf("n2"); got != protocol.NodeDone {
		t.Fatalf("n2 is %s, want DONE: the node in flight settles", got)
	}
	if got := h.store.stateOf("n3"); got != protocol.NodePending {
		t.Fatalf("n3 is %s, want PENDING so that resume can pick it up (AC-24)", got)
	}
	if got := len(h.rec.withPrefix("exec:n3")); got != 0 {
		t.Fatal("a node was dispatched after cancellation was observed")
	}
	if res.Remaining == 0 {
		t.Fatal("a cancelled run must report remaining work")
	}
}

func TestSignalNeverInterruptsAnInFlightStatement(t *testing.T) {
	h := newHarness()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The signal arrives while n2's statement is on the wire.
	h.sql.hook = func(stmtCtx context.Context, stmt Statement) error {
		if stmt.NodeID == "n2" {
			cancel()
		}
		return nil
	}

	res, err := h.executor(t).Run(ctx, "run-1", createChainPlan(t))
	if err != nil {
		t.Fatalf("a signalled run is not a failure, got %v", err)
	}
	if got := h.sql.ctxErrAt["n2"]; got != nil {
		t.Fatalf("the in-flight statement's context was cancelled (%v); FR-CLI-10 forbids interrupting it", got)
	}
	if got := h.store.stateOf("n2"); got != protocol.NodeDone {
		t.Fatalf("n2 is %s, want DONE: the in-flight node runs to completion", got)
	}
	if got := len(h.rec.withPrefix("exec:n3")); got != 0 {
		t.Fatal("dispatch continued after the signal")
	}
	if !res.Cancelled || res.CancelReason != StopSignal {
		t.Fatalf("Result = %+v, want Cancelled with reason %q", res, StopSignal)
	}
}

func TestCancellationDuringBackoffLeavesTheNodeResumable(t *testing.T) {
	h := newHarness()
	h.sql.errs["n2"] = []error{&pgErr{code: "40001", msg: "serialization failure"}}
	h.clock.onSleep = func(time.Duration) error { return context.Canceled }

	res, err := h.executor(t).Run(context.Background(), "run-1", createChainPlan(t))
	if err != nil {
		t.Fatalf("stopping during backoff is not a failure, got %v", err)
	}
	if !res.Cancelled {
		t.Fatalf("Result = %+v, want Cancelled", res)
	}
	// RETRY_WAIT -> READY is a legal resume edge, so the node is not stranded.
	if got := h.store.stateOf("n2"); got != protocol.NodeRetryWait {
		t.Fatalf("n2 is %s, want RETRY_WAIT", got)
	}
}

// ---------------------------------------------------------------------------
// Authorization: FR-AUTH-5, FR-AUTH-6, INV-2
// ---------------------------------------------------------------------------

// cleanupPlan is the resume shape: drop an INVALID leaf index, then rebuild it.
func cleanupPlan(t *testing.T, auth *protocol.Authorization) *protocol.Plan {
	t.Helper()
	return newPlan(t,
		dropNode(t, "nDrop", "public.orders_created_at_idx_orders_2026_03", auth),
		node("nRebuild", protocol.KindIndexCreateConcurrently, &protocol.CreateConcurrentlyParams{
			Partition:  obj(t, "public.orders_2026_03"),
			Index:      obj(t, "public.orders_created_at_idx_orders_2026_03"),
			Definition: indexDef(),
		}, "nDrop"),
	)
}

func provenanceAuth(t *testing.T, index string) *protocol.Authorization {
	t.Helper()
	return &protocol.Authorization{
		Mode:     protocol.AuthProvenance,
		Object:   obj(t, index),
		Relation: objPtr(t, "public.orders_2026_03"),
		Note:     "left behind by a failed CREATE INDEX CONCURRENTLY",
	}
}

func TestDestructiveNodeHaltsWhenProvenanceIsAbsent(t *testing.T) {
	h := newHarness()
	plan := cleanupPlan(t, provenanceAuth(t, "public.orders_created_at_idx_orders_2026_03"))
	// No provenance seeded: the INVALID index is somebody else's (AC-6).

	res, err := h.run(t, plan)
	if !errors.Is(err, protocol.ErrAuthorizationUnsatisfied) {
		t.Fatalf("error = %v, want ErrAuthorizationUnsatisfied", err)
	}
	if got := protocol.ExitCodeFor(err); got != protocol.ExitAuthorizationUnsatisfied {
		t.Fatalf("exit code = %d, want 13", got)
	}
	if h.sql.execCount() != 0 {
		t.Fatalf("a destructive statement ran with unsatisfied authorization; INV-2 forbids it")
	}
	if got := h.store.stateOf("nDrop"); got == protocol.NodeRunning {
		t.Fatal("the node was marked RUNNING despite never being dispatched")
	}
	var denied bool
	for _, tp := range h.store.auditTypes() {
		if tp == AuditAuthorizationDenied {
			denied = true
		}
	}
	if !denied {
		t.Fatalf("no authorization_denied audit event; trail = %v", h.store.auditTypes())
	}
	if res.HaltedAt != "nDrop" {
		t.Fatalf("Result.HaltedAt = %q, want nDrop", res.HaltedAt)
	}
}

func TestDestructiveNodeRecordsAuthorizationBeforeTheStatement(t *testing.T) {
	h := newHarness()
	index := "public.orders_created_at_idx_orders_2026_03"
	h.store.provenance[index] = Provenance{
		RunID: "run-0", NodeID: "n3", Object: obj(t, index), ObjectKind: ObjectKindIndex,
	}
	plan := cleanupPlan(t, provenanceAuth(t, index))

	if _, err := h.run(t, plan); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// FR-AUTH-6: both the record and its audit event precede the statement.
	h.rec.mustPrecede(t, "authorization:nDrop:provenance:"+index, "exec:nDrop")
	h.rec.mustPrecede(t, "audit:authorization_granted:nDrop", "exec:nDrop")

	if len(h.store.authz) != 1 {
		t.Fatalf("recorded %d authorizations, want exactly one (AC-20)", len(h.store.authz))
	}
	got := h.store.authz[0]
	if got.Mode != protocol.AuthProvenance {
		t.Fatalf("recorded mode = %q, want provenance", got.Mode)
	}
	if got.Evidence["provenance_run_id"] != "run-0" {
		t.Fatalf("evidence = %v, want the run that created the object", got.Evidence)
	}
}

func TestDestructiveNodeHaltsWhenAuthorizationNamesAnotherObject(t *testing.T) {
	h := newHarness()
	// Provenance exists for the *other* index, and the authorization points at
	// it, but the node would drop something else.
	other := "public.some_other_idx"
	h.store.provenance[other] = Provenance{RunID: "run-0", Object: obj(t, other)}
	plan := cleanupPlan(t, provenanceAuth(t, other))

	_, err := h.run(t, plan)
	if !errors.Is(err, protocol.ErrAuthorizationUnsatisfied) {
		t.Fatalf("error = %v, want ErrAuthorizationUnsatisfied", err)
	}
	if h.sql.execCount() != 0 {
		t.Fatal("a mismatched authorization let a drop through")
	}
}

// ---------------------------------------------------------------------------
// Session settings: FR-EXEC-5, FR-EXEC-6
// ---------------------------------------------------------------------------

func TestSessionSettingsPerNodeKind(t *testing.T) {
	h := newHarness()
	h.cfg.StatementTimeout = 30 * time.Second
	plan := createChainPlan(t)

	if _, err := h.run(t, plan); err != nil {
		t.Fatalf("Run: %v", err)
	}

	cases := []struct {
		node                 protocol.NodeID
		wantStatementTimeout time.Duration
		wantOutsideTx        bool
		wantLockTimeout      time.Duration
	}{
		// CREATE INDEX CONCURRENTLY: no finite statement_timeout, ever
		// (FR-EXEC-5), and outside any transaction block (FR-EXEC-6). Its
		// lock_timeout is the build bound, not the ordinary one, because the
		// statement waits for application transactions as part of its work.
		{"n3", 0, true, DefaultBuildLockTimeout},
		// CREATE INDEX ON ONLY is catalog-only and bounded.
		{"n2", 30 * time.Second, false, 3 * time.Second},
		{"n5", 30 * time.Second, false, 3 * time.Second},
	}
	for _, tc := range cases {
		stmt, ok := h.sql.statementFor(tc.node)
		if !ok {
			t.Fatalf("node %q issued no statement", tc.node)
		}
		if stmt.Settings.LockTimeout != tc.wantLockTimeout {
			t.Errorf("node %q lock_timeout = %s, want %s",
				tc.node, stmt.Settings.LockTimeout, tc.wantLockTimeout)
		}
		if stmt.Settings.LockTimeout <= 0 {
			t.Errorf("node %q carries no finite lock_timeout; FR-EXEC-5 requires one on every DDL statement",
				tc.node)
		}
		if stmt.Settings.StatementTimeout != tc.wantStatementTimeout {
			t.Errorf("node %q statement_timeout = %s, want %s",
				tc.node, stmt.Settings.StatementTimeout, tc.wantStatementTimeout)
		}
		if stmt.MustRunOutsideTransaction != tc.wantOutsideTx {
			t.Errorf("node %q MustRunOutsideTransaction = %v, want %v",
				tc.node, stmt.MustRunOutsideTransaction, tc.wantOutsideTx)
		}
	}
}

func TestEveryDDLStatementCarriesALockTimeout(t *testing.T) {
	h := newHarness()
	h.cfg.LockTimeout = 0 // fall back to the default
	plan := createChainPlan(t)

	if _, err := h.run(t, plan); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(h.sql.stmts) == 0 {
		t.Fatal("no statements were issued")
	}
	for _, s := range h.sql.stmts {
		if s.Settings.LockTimeout <= 0 {
			t.Fatalf("node %q issued a statement with lock_timeout %s; FR-EXEC-5 requires a finite one",
				s.NodeID, s.Settings.LockTimeout)
		}
	}
}

// ---------------------------------------------------------------------------
// Node vocabulary: FR-EXEC-1
// ---------------------------------------------------------------------------

func TestUnsupportedKindsAreRefusedBeforeAnyStatement(t *testing.T) {
	tests := []struct {
		name string
		plan func(*testing.T) *protocol.Plan
	}{
		{
			name: "reindex_concurrently",
			plan: func(t *testing.T) *protocol.Plan {
				return newPlan(t,
					node("n1", protocol.KindIndexAttach, &protocol.AttachParams{
						ParentIndex: obj(t, "public.parent_idx"),
						ChildIndex:  obj(t, "public.child_a"),
					}),
					node("n2", protocol.KindIndexReindexConcurrently, &protocol.ReindexConcurrentlyParams{
						Index:    obj(t, "public.child_a"),
						Relation: objPtr(t, "public.orders_2026_03"),
					}, "n1"),
				)
			},
		},
		{
			name: "drop_partitioned",
			plan: func(t *testing.T) *protocol.Plan { return dropPartitionedPlan(t) },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			_, err := h.run(t, tc.plan(t))
			if !errors.Is(err, ErrUnsupportedNodeKind) {
				t.Fatalf("error = %v, want ErrUnsupportedNodeKind", err)
			}
			if h.sql.execCount() != 0 {
				t.Fatalf("%d statements ran before the unsupported kind was reached; "+
					"the check must be a pre-flight", h.sql.execCount())
			}
			if len(h.store.transitions) != 0 {
				t.Fatalf("%d checkpoints were written for a plan that cannot run", len(h.store.transitions))
			}
		})
	}
}

func TestSupportedKindsIsTheCreateVocabulary(t *testing.T) {
	got := SupportedKinds()
	want := []protocol.NodeKind{
		protocol.KindCatalogAssert,
		protocol.KindIndexCreateParentInvalid,
		protocol.KindIndexCreateConcurrently,
		protocol.KindIndexAttach,
		protocol.KindIndexVerify,
		protocol.KindWait,
		protocol.KindIndexDropConcurrently,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SupportedKinds() = %v, want %v", got, want)
	}
	// And the two absent kinds are genuinely in the protocol vocabulary, so
	// this is a build limit rather than an unknown kind.
	for _, k := range []protocol.NodeKind{protocol.KindIndexReindexConcurrently, protocol.KindIndexDropPartitioned} {
		if !k.Valid() {
			t.Fatalf("%q is not a protocol node kind", k)
		}
		if supportedKind(k) {
			t.Fatalf("%q should not be supported by this build", k)
		}
	}
}

func TestMissingCatalogPortIsRefusedUpFront(t *testing.T) {
	h := newHarness()
	h.cfg.Catalog = nil

	_, err := h.run(t, createChainPlan(t))
	if !errors.Is(err, ErrMissingPort) {
		t.Fatalf("error = %v, want ErrMissingPort", err)
	}
	if h.sql.execCount() != 0 {
		t.Fatal("statements ran before the missing port was noticed")
	}
}

// ---------------------------------------------------------------------------
// Verification and assertions
// ---------------------------------------------------------------------------

func TestFailedVerificationFailsTheNodeWithExit14(t *testing.T) {
	h := newHarness()
	h.catalog.verifyFn = func(cs []protocol.VerifyCheck) ([]CheckResult, error) {
		return []CheckResult{{Name: string(cs[0].Check), Passed: false, Detail: "indisvalid is false"}}, nil
	}

	_, err := h.run(t, createChainPlan(t))
	if !errors.Is(err, protocol.ErrVerificationFailed) {
		t.Fatalf("error = %v, want ErrVerificationFailed", err)
	}
	if got := protocol.ExitCodeFor(err); got != protocol.ExitVerificationFailed {
		t.Fatalf("exit code = %d, want 14", got)
	}
	if got := h.store.stateOf("n4"); got != protocol.NodeFailed {
		t.Fatalf("n4 is %s, want FAILED", got)
	}
	// VERIFYING -> FAILED is the edge D7 defines for a failed assertion.
	if h.rec.indexOf("transition:n4:VERIFYING->FAILED") < 0 {
		t.Fatalf("missing VERIFYING->FAILED edge; log: %v", h.rec.all())
	}
}

func TestFailedAssertionUsesThePlannedExitCode(t *testing.T) {
	h := newHarness()
	h.catalog.assertFn = func(as []protocol.Assertion) ([]CheckResult, error) {
		return []CheckResult{{Name: string(as[0].Assertion), Passed: false, Detail: "orders is HASH partitioned"}}, nil
	}

	_, err := h.run(t, createChainPlan(t))
	if got := protocol.ExitCodeFor(err); got != protocol.ExitUnsupportedTopology {
		t.Fatalf("exit code = %d, want 15 from the assertion's failure_code (AC-11, AC-26)", got)
	}
	if h.sql.execCount() != 0 {
		t.Fatal("DDL ran after a failed precondition")
	}
	// A false predicate is terminal: retrying cannot make it true.
	if got := len(h.rec.withPrefix("assert:")); got != 1 {
		t.Fatalf("assertions evaluated %d times, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// Resume and idempotence: AC-4, AC-7
// ---------------------------------------------------------------------------

func TestCompletedPlanIsANoOp(t *testing.T) {
	h := newHarness()
	plan := createChainPlan(t)
	for _, n := range plan.Nodes {
		h.store.seed(n.ID, protocol.NodeDone, 1)
	}

	res, err := h.run(t, plan)
	if err != nil {
		t.Fatalf("re-running a completed plan must exit zero, got %v", err)
	}
	if !res.Complete() || res.Done != len(plan.Nodes) {
		t.Fatalf("Result = %+v, want every node done", res)
	}
	if h.sql.execCount() != 0 {
		t.Fatalf("a converged plan issued %d statements", h.sql.execCount())
	}
	if len(h.store.transitions) != 0 {
		t.Fatalf("a converged plan wrote %d checkpoints", len(h.store.transitions))
	}
}

func TestSkippedNodesSatisfyDependencies(t *testing.T) {
	h := newHarness()
	plan := createChainPlan(t)
	h.store.seed("n1", protocol.NodeSkipped, 0)
	h.store.seed("n2", protocol.NodeSkipped, 0)

	res, err := h.run(t, plan)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Skipped != 2 {
		t.Fatalf("Result.Skipped = %d, want 2", res.Skipped)
	}
	if h.rec.indexOf("exec:n3") < 0 {
		t.Fatal("n3 was not dispatched even though its predecessor was SKIPPED (FR-ORD-1)")
	}
}

func TestOrphanedRunningNodeIsRecoveredAndRetried(t *testing.T) {
	h := newHarness()
	plan := createChainPlan(t)
	h.store.seed("n1", protocol.NodeDone, 1)
	h.store.seed("n2", protocol.NodeDone, 1)
	// The process died with the CIC in flight.
	h.store.seed("n3", protocol.NodeRunning, 1)

	res, err := h.run(t, plan)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Complete() {
		t.Fatalf("resume did not converge: %+v", res)
	}
	// RUNNING -> PENDING is the one non-monotonic edge, and only under
	// orphan recovery (INV-5).
	if h.rec.indexOf("transition:n3:RUNNING->PENDING") < 0 {
		t.Fatalf("no orphan-recovery edge; log: %v", h.rec.all())
	}
	var found bool
	for _, tr := range h.store.transitions {
		if tr.NodeID == "n3" && tr.From == protocol.NodeRunning && tr.To == protocol.NodePending {
			found = true
			if tr.Reason != protocol.ReasonOrphanRecovery {
				t.Fatalf("orphan edge recorded with reason %q, want %q", tr.Reason, protocol.ReasonOrphanRecovery)
			}
		}
	}
	if !found {
		t.Fatal("orphan-recovery transition was never recorded")
	}
	if h.rec.indexOf("exec:n3") < 0 {
		t.Fatal("the recovered node was not re-dispatched")
	}
	if h.rec.indexOf("audit:orphan_recovered:n3") < 0 {
		t.Fatalf("no orphan_recovered audit event; trail = %v", h.store.auditTypes())
	}
}

func TestNodeLeftVerifyingIsReVerifiedNotReExecuted(t *testing.T) {
	h := newHarness()
	plan := createChainPlan(t)
	h.store.seed("n1", protocol.NodeDone, 1)
	h.store.seed("n2", protocol.NodeDone, 1)
	// The statement returned; the process died before the DONE checkpoint.
	h.store.seed("n3", protocol.NodeVerifying, 1)

	if _, err := h.run(t, plan); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(h.rec.withPrefix("exec:n3")); got != 0 {
		t.Fatalf("n3 was re-executed %d times; a completed statement should only be re-verified", got)
	}
	if got := h.store.stateOf("n3"); got != protocol.NodeDone {
		t.Fatalf("n3 is %s, want DONE", got)
	}
}

func TestPreviouslyFailedNodeHaltsTheRun(t *testing.T) {
	h := newHarness()
	plan := createChainPlan(t)
	h.store.seed("n1", protocol.NodeDone, 1)
	h.store.seed("n2", protocol.NodeFailed, 3)

	_, err := h.run(t, plan)
	if !errors.Is(err, ErrNodePreviouslyFailed) {
		t.Fatalf("error = %v, want ErrNodePreviouslyFailed", err)
	}
	if h.sql.execCount() != 0 {
		t.Fatal("the run continued past a FAILED node")
	}
}

// ---------------------------------------------------------------------------
// Pacing: FR-ORD-3
// ---------------------------------------------------------------------------

func TestExecutorIntroducesNoDelayOfItsOwn(t *testing.T) {
	h := newHarness()
	// A plan with no wait node and no retries must never sleep.
	plan := newPlan(t,
		node("n1", protocol.KindIndexAttach, &protocol.AttachParams{
			ParentIndex: obj(t, "public.parent_idx"),
			ChildIndex:  obj(t, "public.child_a"),
		}),
		node("n2", protocol.KindIndexAttach, &protocol.AttachParams{
			ParentIndex: obj(t, "public.parent_idx"),
			ChildIndex:  obj(t, "public.child_b"),
		}, "n1"),
	)

	if _, err := h.run(t, plan); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := h.clock.sleptFor(); len(got) != 0 {
		t.Fatalf("the executor slept %v with no wait node in the plan; FR-ORD-3 forbids it", got)
	}
}

func TestWaitNodePausesForItsPlannedDuration(t *testing.T) {
	h := newHarness()
	if _, err := h.run(t, createChainPlan(t)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := h.clock.sleptFor()
	if len(got) != 1 || got[0] != 2*time.Second {
		t.Fatalf("sleeps = %v, want exactly the plan's 2s wait node", got)
	}
}

// ---------------------------------------------------------------------------
// Dry run and configuration
// ---------------------------------------------------------------------------

func TestDryRunTouchesNothing(t *testing.T) {
	h := newHarness()
	h.cfg.DryRun = true

	res, err := h.run(t, createChainPlan(t))
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if h.sql.execCount() != 0 {
		t.Fatalf("--dry-run issued %d statements", h.sql.execCount())
	}
	if len(h.store.transitions) != 0 || len(h.store.audits) != 0 {
		t.Fatal("--dry-run wrote to the state store")
	}
	if len(res.Order) != 7 {
		t.Fatalf("Result.Order has %d nodes, want 7", len(res.Order))
	}
}

func TestNewRejectsAnUnusableConfiguration(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"no store", Config{SQL: newFakeSQL(&recorder{})}},
		{"no sql", Config{Store: newFakeStore(&recorder{})}},
		{"negative lock timeout", Config{
			Store: newFakeStore(&recorder{}), SQL: newFakeSQL(&recorder{}), LockTimeout: -time.Second,
		}},
		{"zero attempts", Config{
			Store: newFakeStore(&recorder{}), SQL: newFakeSQL(&recorder{}),
			Retry: RetryPolicy{MaxAttempts: 0, BaseDelay: time.Second, MaxDelay: time.Second},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatal("expected New to reject this configuration")
			}
		})
	}
}

func TestRunRejectsAnEmptyRunID(t *testing.T) {
	h := newHarness()
	if _, err := h.executor(t).Run(context.Background(), "", createChainPlan(t)); !errors.Is(err, ErrInvalidRun) {
		t.Fatalf("error = %v, want ErrInvalidRun", err)
	}
}

func TestHeartbeatRenewsTheLeaseWhileRunning(t *testing.T) {
	h := newHarness()
	hb := &fakeHeartbeat{beats: make(chan struct{}, 4)}
	h.cfg.Heartbeat = hb
	h.cfg.HeartbeatInterval = time.Millisecond
	// Hold the first statement long enough for the ticker to fire, the way a
	// long CREATE INDEX CONCURRENTLY would.
	h.sql.hook = func(context.Context, Statement) error {
		select {
		case <-hb.beats:
		case <-time.After(2 * time.Second):
			t.Error("no heartbeat within 2s while a statement was in flight")
		}
		return nil
	}

	if _, err := h.run(t, createChainPlan(t)); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestRetryableFailureOfANonIdempotentKindStopsTheRun is FR-EXEC-4's missing
// precondition: a statement may only be re-issued when re-issuing it is
// meaningful.
//
// CREATE INDEX CONCURRENTLY commits its phase-1 catalog entry before it waits
// for lockers. A failure in that wait (55P03 lock_not_available, classified
// retryable) therefore leaves an INVALID index behind. Re-issuing the identical
// statement in-process cannot succeed: PostgreSQL answers 42P07
// duplicate_table, which is terminal. The retry is guaranteed to fail, it
// overwrites the operator's diagnosis with a misleading one, and it burns the
// retry budget on attempts that cannot work.
//
// The correct recovery is resume, which drops the INVALID index under
// provenance and rebuilds (FR-PLAN-6, AC-5). So the run stops with the node
// resumable rather than re-issuing.
func TestRetryableFailureOfANonIdempotentKindStopsTheRun(t *testing.T) {
	h := newHarness()
	// The lock timeout fires in the wait-for-lockers phase, after phase 1 has
	// committed. The second answer is what PostgreSQL really gives a blind
	// retry, and the test proves we never get there.
	h.sql.errs["n3"] = []error{
		&pgErr{code: "55P03", msg: "canceling statement due to lock timeout"},
		&pgErr{code: "42P07", msg: `relation "orders_created_at_idx_orders_2026_03" already exists`},
	}
	plan := createChainPlan(t)

	res, err := h.run(t, plan)
	if err != nil {
		t.Fatalf("the run should stop cleanly and stay resumable, got: %v", err)
	}
	if got := len(h.rec.withPrefix("exec:n3")); got != 1 {
		t.Fatalf("CREATE INDEX CONCURRENTLY was issued %d times; a statement that leaves "+
			"committed catalog state behind must not be re-issued in process", got)
	}
	if !res.Cancelled {
		t.Error("the run did not report itself as stopped")
	}
	// RETRY_WAIT is resumable: resume re-enters it as READY.
	if got := h.store.stateOf("n3"); got != protocol.NodeRetryWait {
		t.Fatalf("n3 is %s, want %s so that resume can pick it up", got, protocol.NodeRetryWait)
	}
	if got := h.store.stateOf("n5"); got != protocol.NodePending {
		t.Errorf("downstream node n5 is %s, want PENDING", got)
	}
}

// TestRetryableFailureOfAnIdempotentKindIsStillRetried guards the other side:
// ALTER INDEX ... ATTACH PARTITION silently no-ops when the child is already
// attached, so it is safe to re-issue and must keep its in-process retry.
func TestRetryableFailureOfAnIdempotentKindIsStillRetried(t *testing.T) {
	h := newHarness()
	h.sql.errs["n5"] = []error{&pgErr{code: "55P03", msg: "canceling statement due to lock timeout"}}
	plan := createChainPlan(t)

	if _, err := h.run(t, plan); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := len(h.rec.withPrefix("exec:n5")); got != 2 {
		t.Fatalf("attach was issued %d times, want 2: it is idempotent and must still retry", got)
	}
	if got := h.store.stateOf("n5"); got != protocol.NodeDone {
		t.Fatalf("n5 is %s, want DONE", got)
	}
}

// TestRetrySafetyIsDeclaredPerKind pins the classification itself, so a new
// node kind cannot default into blind retry.
func TestRetrySafetyIsDeclaredPerKind(t *testing.T) {
	unsafe := []protocol.NodeKind{
		protocol.KindIndexCreateParentInvalid,
		protocol.KindIndexCreateConcurrently,
		protocol.KindIndexDropConcurrently,
		protocol.KindIndexReindexConcurrently,
		protocol.KindIndexDropPartitioned,
	}
	for _, k := range unsafe {
		if k.RetrySafe() {
			t.Errorf("%s reports itself retry-safe; re-issuing it after a late failure "+
				"cannot succeed, because it leaves committed catalog state behind", k)
		}
	}
	if !protocol.KindIndexAttach.RetrySafe() {
		t.Error("index.attach is idempotent and should stay retryable in process")
	}
}

// TestConcurrentBuildsGetTheirOwnLockTimeout is FR-EXEC-5 read against what
// lock_timeout actually bounds inside a CONCURRENTLY statement.
//
// lock_timeout is not confined to the initial relation lock. CREATE INDEX
// CONCURRENTLY has two wait-for-lockers phases, and each waits on a ShareLock
// against every concurrent transaction's virtual XID, taken through the regular
// lock manager. The lock timeout timer is armed for those waits too. So a 5s
// lock_timeout means any application transaction held open for six seconds
// aborts an index build that has already paid for a full table scan, and
// PostgreSQL leaves the index INVALID.
//
// The wait is not a lock queue that can be retried later; it is the visibility
// barrier the statement must clear to finish. So the CONCURRENTLY kinds get
// their own, much larger bound, and the short default keeps protecting the
// statements it was meant for: the ones that would otherwise queue in front of
// application traffic.
func TestConcurrentBuildsGetTheirOwnLockTimeout(t *testing.T) {
	h := newHarness()
	h.cfg.LockTimeout = 3 * time.Second
	h.cfg.BuildLockTimeout = 20 * time.Minute
	plan := createChainPlan(t)

	if _, err := h.run(t, plan); err != nil {
		t.Fatalf("Run: %v", err)
	}

	cases := []struct {
		node protocol.NodeID
		want time.Duration
	}{
		// CREATE INDEX CONCURRENTLY waits for lockers.
		{"n3", 20 * time.Minute},
		// CREATE INDEX ON ONLY and ALTER INDEX ... ATTACH take their lock and
		// return; the short bound is right for them.
		{"n2", 3 * time.Second},
		{"n5", 3 * time.Second},
	}
	for _, tc := range cases {
		stmt, ok := h.sql.statementFor(tc.node)
		if !ok {
			t.Fatalf("node %q issued no statement", tc.node)
		}
		if stmt.Settings.LockTimeout != tc.want {
			t.Errorf("node %q (%s) lock_timeout = %s, want %s",
				tc.node, stmt.Kind, stmt.Settings.LockTimeout, tc.want)
		}
	}
}

// TestBuildLockTimeoutDefaultsAboveTheOrdinaryOne pins the default, since the
// whole point is that the two are not the same number.
func TestBuildLockTimeoutDefaultsAboveTheOrdinaryOne(t *testing.T) {
	if DefaultBuildLockTimeout <= DefaultLockTimeout {
		t.Fatalf("DefaultBuildLockTimeout (%s) must exceed DefaultLockTimeout (%s): a concurrent "+
			"build waits for application transactions, not for a lock queue",
			DefaultBuildLockTimeout, DefaultLockTimeout)
	}
	for _, k := range []protocol.NodeKind{
		protocol.KindIndexCreateConcurrently,
		protocol.KindIndexDropConcurrently,
		protocol.KindIndexReindexConcurrently,
	} {
		if !k.WaitsForConcurrentTransactions() {
			t.Errorf("%s waits for concurrent transactions but does not say so", k)
		}
	}
	for _, k := range []protocol.NodeKind{
		protocol.KindIndexCreateParentInvalid,
		protocol.KindIndexAttach,
	} {
		if k.WaitsForConcurrentTransactions() {
			t.Errorf("%s does not wait for concurrent transactions", k)
		}
	}
}

// TestLosingTheLeaseStopsDispatch is the fencing half of FR-LOCK-1 and AC-10:
// "two concurrent execute invocations against the same target: exactly one
// proceeds".
//
// The lease exists so an abandoned run is detectable, and
// state.LeaseStore.Heartbeat documents its ErrLeaseLost as "the fencing signal
// that tells an executor to stop dispatching". Nothing consumed it: the
// heartbeat goroutine logged the failure and kept ticking, and no node boundary
// ever asked whether the lease was still held.
//
// The sequence that breaks AC-10: process A takes the lock and lease and enters
// a multi-hour build; A's heartbeat stalls past the TTL (blocked filesystem,
// suspended host, loaded runner); process B takes the lock, marks A's run
// orphaned and adopts it. A is now fenced out and must stop, or both processes
// are issuing DDL against the same partitioned table.
func TestLosingTheLeaseStopsDispatch(t *testing.T) {
	h := newHarness()
	hb := &fakeHeartbeat{beats: make(chan struct{}, 8)}
	h.cfg.Heartbeat = hb
	h.cfg.HeartbeatInterval = time.Millisecond

	// The lease is taken by another holder while the first statement runs.
	var once sync.Once
	h.sql.hook = func(context.Context, Statement) error {
		once.Do(func() {
			hb.fail(ErrFenced.Detailf(`run is leased by "hostB/200", not "hostA/100"`))
			// Let at least one beat observe the loss.
			select {
			case <-hb.beats:
			case <-time.After(2 * time.Second):
			}
			select {
			case <-hb.beats:
			case <-time.After(2 * time.Second):
			}
		})
		return nil
	}

	res, err := h.run(t, createChainPlan(t))
	if err != nil {
		t.Fatalf("a fenced run should stop cleanly, got: %v", err)
	}
	if !res.Cancelled {
		t.Fatal("the run kept dispatching after losing its lease; two processes can now issue " +
			"DDL against the same target (FR-LOCK-1, AC-10)")
	}
	if res.CancelReason != StopFenced {
		t.Errorf("CancelReason = %q, want %q", res.CancelReason, StopFenced)
	}
	if res.Remaining == 0 {
		t.Error("a fenced run reported no remaining work, so it did not stop early")
	}
}

// TestATransientHeartbeatErrorDoesNotStopTheRun keeps the fence narrow: only a
// definitive loss of the lease fences the run. A blip talking to the state
// store must not abandon a multi-hour build.
func TestATransientHeartbeatErrorDoesNotStopTheRun(t *testing.T) {
	h := newHarness()
	hb := &fakeHeartbeat{beats: make(chan struct{}, 8)}
	hb.fail(errors.New("state store i/o: temporary failure"))
	h.cfg.Heartbeat = hb
	h.cfg.HeartbeatInterval = time.Millisecond

	res, err := h.run(t, createChainPlan(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Cancelled {
		t.Fatal("a transient heartbeat error stopped the run; only losing the lease should fence it")
	}
	if !res.Complete() {
		t.Fatalf("run did not complete: %+v", res)
	}
}

// TestDryRunNeedsNoCatalogPort covers the regression that made --dry-run fail
// on every plan partitionctl can produce. create-index always emits
// catalog.assert and index.verify; preflight demanded a CatalogEvaluator for
// them; and printDispatch builds the executor without one, correctly, because
// a dry run walks the order and touches nothing (FR-CLI-5).
//
// The real-run half of this contract is TestMissingCatalogPortIsRefusedUpFront,
// which keeps the exemption narrow.
func TestDryRunNeedsNoCatalogPort(t *testing.T) {
	plan := createChainPlan(t)

	exec, err := New(Config{DryRun: true})
	if err != nil {
		t.Fatalf("New(DryRun) = %v", err)
	}
	res, err := exec.Run(context.Background(), RunID("dry-run"), plan)
	if err != nil {
		t.Fatalf("dry run with no CatalogEvaluator = %v, want nil", err)
	}
	if res.Total != len(plan.Nodes) {
		t.Errorf("Total = %d, want %d", res.Total, len(plan.Nodes))
	}
}
