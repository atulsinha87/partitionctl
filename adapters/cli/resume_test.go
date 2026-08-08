package cli

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/atulsinha/partitionctl/engine/executor"
	"github.com/atulsinha/partitionctl/engine/planner"
	"github.com/atulsinha/partitionctl/engine/protocol"
	"github.com/atulsinha/partitionctl/engine/state"
)

// TestCancelStopsAtABoundaryAndResumeCompletes is AC-24 end to end:
// "`cancel` issued against a live run stops it at the next node boundary, never
// mid-statement, and the run remains resumable to completion."
//
// The second clause is the one that was broken. `cancel` set a flag that
// nothing ever cleared, so the adopted run stopped again on its first boundary
// with zero nodes dispatched. The run reported itself resumable and could never
// be resumed.
func TestCancelStopsAtABoundaryAndResumeCompletes(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()

	// `cancel` arrives while the first index build is in flight.
	var cancelled bool
	h.SQL.OnExec = func(stmt executor.Statement) error {
		if cancelled || stmt.Kind != protocol.KindIndexCreateConcurrently {
			return nil
		}
		cancelled = true
		runs := h.Runs()
		if len(runs) == 0 {
			t.Error("no run to cancel")
			return nil
		}
		if _, err := h.Store.RequestCancel(ctx(), runs[0].RunID, "op", "maintenance window closed"); err != nil {
			t.Errorf("RequestCancel: %v", err)
		}
		return nil
	}

	code := h.Run("execute", plan)
	if code == int(protocol.ExitSuccess) {
		t.Fatalf("execute completed despite a cancellation request: %s", h.Out())
	}
	if !cancelled {
		t.Fatal("the run never reached an index build, so nothing was cancelled")
	}
	stoppedAfter := h.SQL.DDLCount()
	if stoppedAfter == 0 {
		t.Fatal("the run stopped before issuing anything")
	}

	// The statement in flight was allowed to finish: stopping mid-statement is
	// what leaves a half-built CREATE INDEX CONCURRENTLY behind.
	if !strings.Contains(h.Out(), "node boundary") {
		t.Errorf("output does not report a node-boundary stop: %s", h.Out())
	}

	runs := h.Runs()
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	if !runs[0].Status.IsResumable() {
		t.Fatalf("run status %s is not resumable, so AC-24's second clause fails", runs[0].Status)
	}

	// Now resume. Without the revocation this dispatches nothing and stops
	// again on the first boundary.
	h.SQL.OnExec = nil
	if code := h.Run("resume", plan); code != int(protocol.ExitSuccess) {
		t.Fatalf("resume after cancel exited %d, want 0: %s", code, h.Out())
	}
	if h.SQL.DDLCount() <= stoppedAfter {
		t.Fatalf("resume dispatched no new statements (%d before, %d after): the cancellation "+
			"request was never revoked, so the run can never complete (AC-24)",
			stoppedAfter, h.SQL.DDLCount())
	}
}

// TestResumeConvergesToTheSameCatalogStateAsAnUninterruptedRun is AC-4:
// "SIGKILL at any point during execution, followed by `resume`, converges to
// the same final catalog state as an uninterrupted run."
//
// The criterion is about the *final catalog state*, so the test compares
// against a reference run rather than asserting node counts. It sweeps the kill
// point across every DDL statement in the graph, which is what "at any point"
// means; the existing engine-level coverage pins two fixed points.
func TestResumeConvergesToTheSameCatalogStateAsAnUninterruptedRun(t *testing.T) {
	reference := newHarness(t)
	refPlan := reference.MustPlan()
	if code := reference.Run("execute", refPlan); code != int(protocol.ExitSuccess) {
		t.Fatalf("reference run exited %d: %s", code, reference.Out())
	}
	want := reference.IndexNames()
	if len(want) == 0 {
		t.Fatal("the reference run built nothing")
	}
	total := reference.SQL.DDLCount()

	for kill := 1; kill <= total; kill++ {
		h := newHarness(t)
		plan := h.MustPlan()

		// The process dies after the kill-th DDL statement. A killed process
		// issues no further statements and records nothing more.
		issued := 0
		h.SQL.OnExec = func(stmt executor.Statement) error {
			if stmt.Kind.IssuesDDL() {
				issued++
				if issued > kill {
					return errKilled
				}
			}
			return nil
		}

		_ = h.Run("execute", plan)
		h.SQL.OnExec = nil

		// Roll forward until it converges, exactly as an operator would.
		var code int
		for attempt := 0; attempt < 10; attempt++ {
			code = h.Run("resume", plan)
			if code == int(protocol.ExitSuccess) {
				break
			}
		}
		if code != int(protocol.ExitSuccess) {
			t.Fatalf("kill after DDL %d: resume never converged (exit %d): %s", kill, code, h.Out())
		}

		if got := h.IndexNames(); !reflect.DeepEqual(got, want) {
			t.Errorf("kill after DDL %d: final catalog state differs from an uninterrupted run\n got: %v\nwant: %v",
				kill, got, want)
		}
	}
}

// errKilled stands in for the process dying mid-run.
var errKilled = &killedError{}

type killedError struct{}

func (*killedError) Error() string { return "connection reset by peer: process killed" }

// TestResumeRefusesARunWithNoPriorRun keeps the resume entry conditions honest:
// resume adopts an existing run, it does not start one.
func TestResumeRefusesWithNoPriorRun(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()
	if code := h.Run("resume", plan); code == int(protocol.ExitSuccess) {
		t.Fatalf("resume succeeded with no run to adopt: %s", h.Out())
	}
	if h.SQL.DDLCount() != 0 {
		t.Errorf("resume issued %d statement(s) with no run to adopt", h.SQL.DDLCount())
	}
}

// TestExecuteRefusesAnIncompletePriorRun is AC-23: "`execute` against a plan
// with an incomplete prior run exits non-zero without issuing DDL and names
// `resume` in its message."
//
// state.IncompleteRunsForPlan was tested; priorRunSet.refuseExecute, which is
// what the command actually calls, was not.
func TestExecuteRefusesAnIncompletePriorRun(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()
	h.seedRun(h.LoadPlan(plan))

	code := h.Run("execute", plan)
	if code == int(protocol.ExitSuccess) {
		t.Fatalf("execute proceeded past an incomplete prior run: %s", h.Out())
	}
	if h.SQL.DDLCount() != 0 {
		t.Errorf("execute issued %d statement(s) before refusing (AC-23)", h.SQL.DDLCount())
	}
	if !strings.Contains(h.Out(), "resume") {
		t.Errorf("the refusal does not name `resume` (AC-23): %s", h.Out())
	}
}

// TestCompletedPlanIsANoOp is AC-7: re-executing a finished plan converges.
func TestCompletedPlanIsANoOp(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()
	if code := h.Run("execute", plan); code != int(protocol.ExitSuccess) {
		t.Fatalf("first execute exited %d: %s", code, h.Out())
	}
	issued := h.SQL.DDLCount()

	if code := h.Run("execute", plan); code != int(protocol.ExitSuccess) {
		t.Fatalf("second execute exited %d, want 0 (AC-7): %s", code, h.Out())
	}
	if h.SQL.DDLCount() != issued {
		t.Errorf("the second execute issued %d extra statement(s); a converged plan is a no-op (AC-7)",
			h.SQL.DDLCount()-issued)
	}
	if !strings.Contains(h.Out(), "already complete") {
		t.Errorf("the no-op does not say so: %s", h.Out())
	}
}

// runStatuses is a debugging aid for the tests above.
func runStatuses(runs []state.Run) []string {
	out := make([]string, len(runs))
	for i, r := range runs {
		out[i] = string(r.Status)
	}
	return out
}

// ---------------------------------------------------------------------------
// FR-AUTH-5: the cleanup verdict is re-read immediately before the statement
// ---------------------------------------------------------------------------

// cleanup decides every verdict under one REPEATABLE READ snapshot, releases
// it, and only then issues the drops. Releasing early is what stops the command
// deadlocking against its own snapshot -- DROP INDEX CONCURRENTLY calls
// WaitForOlderSnapshots -- but it opens a long window: each drop waits for every
// transaction that can see the index, minutes at a time on a busy table, so a
// 30-item cleanup can act on a verdict that is hours old.
//
// [App.recheck] is what makes that safe. These tests drive it directly with a
// pinned "snapshot" verdict and a live catalog that disagrees, which is the
// situation the window actually produces: the tool's own halt message tells the
// operator to "resolve it by hand, then resume again", so a DBA working in that
// window is the expected case, not a freak one.

// resumeCatalog is a one-leaf tree carrying one index, used as the LIVE
// catalog recheck reads. The snapshot's answer is passed in separately as the
// decision that was recorded.
func resumeCatalog(t *testing.T, comment string, valid bool) (*planner.FakeCatalog, planner.ChildIndexPlan) {
	t.Helper()
	const (
		leafOID  uint32 = 4001
		indexOID uint32 = 5001
		ownerOID uint32 = 10
	)
	leaf := planner.Relation{
		OID: leafOID, Name: protocol.NewObjectName("public", "orders_2026_01"),
		Kind: planner.RelKindTable, OwnerOID: ownerOID,
	}
	index := planner.Index{
		OID: indexOID, Name: protocol.NewObjectName("public", "orders_idx_orders_2026_01"),
		Kind: planner.RelKindIndex, OwnerOID: ownerOID, TableOID: leafOID,
		Table: leaf.Name, IsValid: valid, IsReady: true, IsLive: true, Comment: comment,
	}
	f := planner.NewFakeCatalog()
	f.AddRelation(leaf)
	f.AddIndex(index)

	return f, planner.ChildIndexPlan{
		Leaf: leaf, ChildIndex: index.Name,
		Existing: &index, Condition: index.Condition(),
	}
}

func ourMarker(t *testing.T) string {
	t.Helper()
	text, err := protocol.FormatMarker(protocol.Marker{
		Run: "run-earlier", Plan: "sha256:earlier", Op: string(protocol.OpCreateIndex),
		Role: protocol.MarkerRoleLeaf, At: "2026-08-07T11:00:00Z",
	})
	if err != nil {
		t.Fatalf("FormatMarker: %v", err)
	}
	return text
}

// A DBA does what the halt message told them to: drops the wrecked leaf index
// and rebuilds it under the same name with a plain CREATE INDEX. The rebuilt
// index is healthy and in use. Acting on the recorded verdict would destroy it.
func TestCleanupRefusesToDropAnIndexThatBecameHealthy(t *testing.T) {
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Now: time.Now}
	cat, child := resumeCatalog(t, ourMarker(t), true) // live: VALID again

	// What the snapshot saw: invalid, marked, and therefore droppable.
	child.Condition = planner.IndexInvalid

	_, _, err := app.recheck(context.Background(), &Target{Catalog: cat},
		claimLookup{}, child, planner.CleanupDropWithProvenance)
	if err == nil {
		t.Fatal("recheck accepted a stale verdict against an index that is now valid; " +
			"the drop would have destroyed a healthy, in-use index")
	}
	if !strings.Contains(err.Error(), "no longer this run's to drop") {
		t.Errorf("error does not explain the change: %v", err)
	}
}

// The other thing a DBA plausibly does in that window: write their own comment
// on the index. That makes it MarkerForeign, which directive A.5.1 says must
// HALT -- "never overwrite a human's comment, never drop the object under it".
// The adopt path is the dangerous one here, because it writes PartitionCTL's
// marker over that comment before the drop.
func TestCleanupRefusesAfterAHumanCommentsTheIndex(t *testing.T) {
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Now: time.Now}
	cat, child := resumeCatalog(t, "CHG-4471 pre-build by dba-team; DO NOT DROP", false)
	child.Condition = planner.IndexInvalid

	_, _, err := app.recheck(context.Background(), &Target{Catalog: cat},
		claimLookup{}, child, planner.CleanupAdoptThenDrop)
	if err == nil {
		t.Fatal("recheck accepted a stale adopt-then-drop verdict against an index now carrying " +
			"a human's comment; resume would have overwritten it and dropped the index under it")
	}
	if !errors.Is(err, planner.ErrForeignInvalidIndex) {
		t.Errorf("err = %v, want ErrForeignInvalidIndex", err)
	}
}

// Nothing changed: the verdict stands and the drop proceeds. Without this the
// two tests above would pass against a recheck that refused everything.
func TestCleanupProceedsWhenLiveStateStillAgrees(t *testing.T) {
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Now: time.Now}
	cat, child := resumeCatalog(t, ourMarker(t), false)

	got, verdict, err := app.recheck(context.Background(), &Target{Catalog: cat},
		claimLookup{}, child, planner.CleanupDropWithProvenance)
	if err != nil {
		t.Fatalf("recheck refused an unchanged verdict: %v", err)
	}
	if got != planner.CleanupDropWithProvenance {
		t.Errorf("decision = %v, want %v", got, planner.CleanupDropWithProvenance)
	}
	if verdict.Evidence["source"] != "marker" {
		t.Errorf("evidence source = %q, want %q", verdict.Evidence["source"], "marker")
	}
}

// recheck must never fall back to the recorded verdict when it cannot re-read.
// A missing catalog is "cannot decide", and cannot decide halts.
func TestCleanupRefusesWhenItCannotReRead(t *testing.T) {
	app := &App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Now: time.Now}
	_, child := resumeCatalog(t, ourMarker(t), false)

	_, _, err := app.recheck(context.Background(), &Target{}, claimLookup{}, child,
		planner.CleanupDropWithProvenance)
	if !errors.Is(err, executor.ErrMissingPort) {
		t.Fatalf("err = %v, want ErrMissingPort", err)
	}
}
