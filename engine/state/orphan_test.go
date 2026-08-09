package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// INV-4: a run in RUNNING whose lease has expired and whose advisory lock is
// unheld is ORPHANED and resumable. The table walks the four combinations of
// the two conditions plus the "no lease at all" case.
func TestFindOrphans(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		// setup leaves the store in the state under test and returns the run.
		setup func(t *testing.T, s *FileStore, c *fakeClock) Run
		want  bool
	}{
		{
			name: "RUNNING with a live lease is not orphaned",
			setup: func(t *testing.T, s *FileStore, c *fakeClock) Run {
				run := mustCreateRun(t, s, testPlan(t), "run-live")
				mustLease(t, s, run.RunID, "holder", 60*time.Second)
				c.Advance(10 * time.Second)
				return run
			},
			want: false,
		},
		{
			name: "RUNNING with an expired lease is orphaned",
			setup: func(t *testing.T, s *FileStore, c *fakeClock) Run {
				run := mustCreateRun(t, s, testPlan(t), "run-expired")
				mustLease(t, s, run.RunID, "holder", 30*time.Second)
				c.Advance(31 * time.Second)
				return run
			},
			want: true,
		},
		{
			name: "RUNNING with no lease at all is orphaned",
			setup: func(t *testing.T, s *FileStore, c *fakeClock) Run {
				return mustCreateRun(t, s, testPlan(t), "run-noleaSE")
			},
			want: true,
		},
		{
			name: "a completed run is never an orphan",
			setup: func(t *testing.T, s *FileStore, c *fakeClock) Run {
				run := mustCreateRun(t, s, testPlan(t), "run-done")
				mustLease(t, s, run.RunID, "holder", 30*time.Second)
				c.Advance(31 * time.Second)
				if _, err := s.SetRunStatus(ctx, RunStatusUpdate{
					RunID: run.RunID, From: RunRunning, To: RunCompleted,
				}); err != nil {
					t.Fatalf("SetRunStatus: %v", err)
				}
				return run
			},
			want: false,
		},
		{
			name: "an already-orphaned run is not re-detected",
			setup: func(t *testing.T, s *FileStore, c *fakeClock) Run {
				run := mustCreateRun(t, s, testPlan(t), "run-already")
				mustLease(t, s, run.RunID, "holder", 30*time.Second)
				c.Advance(31 * time.Second)
				if _, err := s.SetRunStatus(ctx, RunStatusUpdate{
					RunID: run.RunID, From: RunRunning, To: RunOrphaned,
				}); err != nil {
					t.Fatalf("SetRunStatus: %v", err)
				}
				return run
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newFileStore(t)
			run := tc.setup(t, s, c)
			lock := mustLock(t, s, run.LockKey())

			orphans, err := FindOrphans(ctx, s, lock, c.Now())
			if err != nil {
				t.Fatalf("FindOrphans: %v", err)
			}
			found := false
			for _, o := range orphans {
				if o.RunID == run.RunID {
					found = true
				}
			}
			if found != tc.want {
				t.Fatalf("orphan detected = %v, want %v", found, tc.want)
			}
		})
	}
}

// The advisory-lock half of INV-4 is structural: the helpers cannot be called
// without a held lock covering the same target.
func TestFindOrphansRequiresTheLock(t *testing.T) {
	ctx := context.Background()
	s, c := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t), "run-lockproof")

	if _, err := FindOrphans(ctx, s, nil, c.Now()); !errors.Is(err, protocol.ErrLockHeld) {
		t.Fatalf("FindOrphans(nil lock) = %v, want ErrLockHeld", err)
	}

	other := LockKey{Database: "appdb", Table: protocol.NewObjectName("public", "other_table")}
	wrong := mustLock(t, s, other)
	if _, err := MarkOrphaned(ctx, s, wrong, run.RunID, c.Now()); !errors.Is(err, protocol.ErrLockHeld) {
		t.Fatalf("MarkOrphaned with the wrong key = %v, want ErrLockHeld", err)
	}

	right := mustLock(t, s, run.LockKey())
	if err := right.Unlock(ctx); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if _, err := FindOrphans(ctx, s, right, c.Now()); !errors.Is(err, protocol.ErrLockHeld) {
		t.Fatalf("FindOrphans with a released lock = %v, want ErrLockHeld", err)
	}
}

// MarkOrphaned rewinds the node that was in flight, which is the one edge
// INV-5 reserves for orphan recovery.
func TestMarkOrphanedRewindsRunningNodes(t *testing.T) {
	ctx := context.Background()
	s, c := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t, "a", "b", "c"), "run-rewind")
	mustLease(t, s, run.RunID, "dead-holder", 30*time.Second)

	// a finished, b was in flight when the process died, c never started.
	step := func(id protocol.NodeID, from, to protocol.NodeState) {
		t.Helper()
		if _, err := s.TransitionNode(ctx, NodeTransition{
			RunID: run.RunID, NodeID: id, From: from, To: to,
		}); err != nil {
			t.Fatalf("%s %s->%s: %v", id, from, to, err)
		}
	}
	step("a", protocol.NodePending, protocol.NodeReady)
	step("a", protocol.NodeReady, protocol.NodeRunning)
	step("a", protocol.NodeRunning, protocol.NodeVerifying)
	step("a", protocol.NodeVerifying, protocol.NodeDone)
	step("b", protocol.NodePending, protocol.NodeReady)
	step("b", protocol.NodeReady, protocol.NodeRunning)

	c.Advance(31 * time.Second)
	lock := mustLock(t, s, run.LockKey())

	orphaned, err := MarkOrphaned(ctx, s, lock, run.RunID, c.Now())
	if err != nil {
		t.Fatalf("MarkOrphaned: %v", err)
	}
	if orphaned.Status != RunOrphaned {
		t.Fatalf("status = %s, want %s", orphaned.Status, RunOrphaned)
	}

	want := map[protocol.NodeID]protocol.NodeState{
		"a": protocol.NodeDone,
		"b": protocol.NodePending,
		"c": protocol.NodePending,
	}
	nodes, err := s.ListNodes(ctx, run.RunID)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	for _, n := range nodes {
		if want[n.NodeID] != n.State {
			t.Errorf("node %s = %s, want %s", n.NodeID, n.State, want[n.NodeID])
		}
	}
}

func TestMarkOrphanedRefusesALiveRun(t *testing.T) {
	ctx := context.Background()
	s, c := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t), "run-alive")
	mustLease(t, s, run.RunID, "holder", 60*time.Second)
	lock := mustLock(t, s, run.LockKey())

	if _, err := MarkOrphaned(ctx, s, lock, run.RunID, c.Now()); !errors.Is(err, ErrConflict) {
		t.Fatalf("MarkOrphaned on a live run = %v, want ErrConflict", err)
	}
}

func TestRecoverAndAdoptOrphan(t *testing.T) {
	ctx := context.Background()
	s, c := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t, "a", "b"), "run-adopt")
	mustLease(t, s, run.RunID, "dead", 30*time.Second)
	c.Advance(31 * time.Second)

	lock := mustLock(t, s, run.LockKey())
	recovered, err := RecoverOrphans(ctx, s, lock, c.Now())
	if err != nil {
		t.Fatalf("RecoverOrphans: %v", err)
	}
	if len(recovered) != 1 || recovered[0].RunID != run.RunID {
		t.Fatalf("recovered %v, want just %s", recovered, run.RunID)
	}

	adopted, err := AdoptOrphan(ctx, s, lock, run.RunID, "new-holder", "bob", 30*time.Second, c.Now())
	if err != nil {
		t.Fatalf("AdoptOrphan: %v", err)
	}
	if adopted.Status != RunRunning {
		t.Fatalf("status = %s, want %s", adopted.Status, RunRunning)
	}
	if adopted.PlanDigest != run.PlanDigest {
		t.Errorf("adoption changed the plan digest (INV-6)")
	}
	lease, err := s.GetLease(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetLease: %v", err)
	}
	if lease.Holder != "new-holder" {
		t.Errorf("holder = %q, want new-holder", lease.Holder)
	}
}

// FR-CLI-11: a terminally cancelled run is never adopted, whatever `resume`
// wants.
func TestAdoptOrphanRefusesACancelledRun(t *testing.T) {
	ctx := context.Background()
	s, c := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t), "run-cancelled")
	mustLease(t, s, run.RunID, "dead", 30*time.Second)
	c.Advance(31 * time.Second)

	lock := mustLock(t, s, run.LockKey())
	if _, err := RecoverOrphans(ctx, s, lock, c.Now()); err != nil {
		t.Fatalf("RecoverOrphans: %v", err)
	}
	if _, err := s.RequestCancel(ctx, run.RunID, "op", "not needed any more"); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	got, err := s.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != RunCancelled {
		t.Fatalf("status = %s, want %s", got.Status, RunCancelled)
	}
	if _, err := AdoptOrphan(ctx, s, lock, run.RunID, "h", "bob", 0, c.Now()); !errors.Is(err, ErrConflict) {
		t.Fatalf("AdoptOrphan on a cancelled run = %v, want ErrConflict", err)
	}
}

// FR-CLI-9 / AC-23: `execute` refuses a plan with an incomplete prior run.
func TestIncompleteRunsForPlan(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name   string
		status RunStatus
		want   int
	}{
		{name: "running", status: RunRunning, want: 1},
		{name: "failed", status: RunFailed, want: 1},
		{name: "orphaned", status: RunOrphaned, want: 1},
		{name: "interrupted", status: RunInterrupted, want: 1},
		{name: "completed", status: RunCompleted, want: 0},
		{name: "cancelled", status: RunCancelled, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newFileStore(t)
			plan := testPlan(t)
			run := mustCreateRun(t, s, plan, "run-incomplete")
			if tc.status != RunRunning {
				if _, err := s.SetRunStatus(ctx, RunStatusUpdate{
					RunID: run.RunID, From: RunRunning, To: tc.status,
				}); err != nil {
					t.Fatalf("SetRunStatus: %v", err)
				}
			}
			got, err := IncompleteRunsForPlan(ctx, s, plan.Digest)
			if err != nil {
				t.Fatalf("IncompleteRunsForPlan: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d incomplete runs, want %d", len(got), tc.want)
			}
		})
	}
}

// FR-CLI-10 / AC-24: cancelling a live run sets a flag and leaves the run
// resumable. FR-CLI-11: cancelling an abandoned one is terminal.
func TestRequestCancel(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name       string
		setup      func(t *testing.T, s *FileStore, c *fakeClock, run Run)
		wantStatus RunStatus
		wantFlag   bool
	}{
		{
			name: "a live run only gets the flag",
			setup: func(t *testing.T, s *FileStore, c *fakeClock, run Run) {
				mustLease(t, s, run.RunID, "holder", 60*time.Second)
			},
			wantStatus: RunRunning,
			wantFlag:   true,
		},
		{
			name: "a run whose lease expired is cancelled terminally",
			setup: func(t *testing.T, s *FileStore, c *fakeClock, run Run) {
				mustLease(t, s, run.RunID, "holder", 30*time.Second)
				c.Advance(31 * time.Second)
			},
			wantStatus: RunCancelled,
			wantFlag:   true,
		},
		{
			name:       "a run with no lease is cancelled terminally",
			setup:      func(t *testing.T, s *FileStore, c *fakeClock, run Run) {},
			wantStatus: RunCancelled,
			wantFlag:   true,
		},
		{
			name: "an orphaned run is cancelled terminally",
			setup: func(t *testing.T, s *FileStore, c *fakeClock, run Run) {
				if _, err := s.SetRunStatus(ctx, RunStatusUpdate{
					RunID: run.RunID, From: RunRunning, To: RunOrphaned,
				}); err != nil {
					t.Fatalf("SetRunStatus: %v", err)
				}
			},
			wantStatus: RunCancelled,
			wantFlag:   true,
		},
		{
			name: "a completed run is left alone",
			setup: func(t *testing.T, s *FileStore, c *fakeClock, run Run) {
				if _, err := s.SetRunStatus(ctx, RunStatusUpdate{
					RunID: run.RunID, From: RunRunning, To: RunCompleted,
				}); err != nil {
					t.Fatalf("SetRunStatus: %v", err)
				}
			},
			wantStatus: RunCompleted,
			wantFlag:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newFileStore(t)
			run := mustCreateRun(t, s, testPlan(t), "run-cancel")
			tc.setup(t, s, c, run)

			got, err := s.RequestCancel(ctx, run.RunID, "operator", "please stop")
			if err != nil {
				t.Fatalf("RequestCancel: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s", got.Status, tc.wantStatus)
			}
			flag, err := s.CancellationRequested(ctx, run.RunID)
			if err != nil {
				t.Fatalf("CancellationRequested: %v", err)
			}
			if flag != tc.wantFlag {
				t.Errorf("cancellation flag = %v, want %v", flag, tc.wantFlag)
			}
			if tc.wantStatus == RunRunning && !got.Status.IsIncomplete() {
				t.Error("a cancelled-at-boundary run must stay resumable (AC-24)")
			}
		})
	}
}

// TestAdoptOrphanRevokesTheCancellationRequest is AC-24's second clause: "the
// run remains resumable to completion".
//
// RequestCancel sets a flag the executor polls at every node boundary. If
// adoption does not clear it, `resume` hands the executor a run that stops
// again on its very first boundary, with zero nodes dispatched, forever. The
// run advertises itself as resumable and can never be resumed.
func TestAdoptOrphanRevokesTheCancellationRequest(t *testing.T) {
	ctx := context.Background()
	s, c := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t), "run-cancel-resume")
	// A live lease is what makes this the AC-24 path rather than the FR-CLI-11
	// terminal one: cancelling a live run sets the flag and leaves the run
	// resumable, and only a run nothing is executing is cancelled terminally.
	mustLease(t, s, run.RunID, "holder-1", 30*time.Second)

	// The operator cancels a live run; the executor stops at a node boundary
	// and the CLI records it as INTERRUPTED, which is resumable.
	if _, err := s.RequestCancel(ctx, run.RunID, "op", "pausing for the maintenance window"); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	if _, err := s.SetRunStatus(ctx, RunStatusUpdate{
		RunID: run.RunID, From: RunRunning, To: RunInterrupted, At: c.Now(),
	}); err != nil {
		t.Fatalf("SetRunStatus: %v", err)
	}
	if requested, err := s.CancellationRequested(ctx, run.RunID); err != nil || !requested {
		t.Fatalf("CancellationRequested before adoption = %v, %v; want true", requested, err)
	}
	// The stopped executor released its lease on the way out.
	if err := s.ReleaseLease(ctx, run.RunID, "holder-1"); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}

	lock := mustLock(t, s, run.LockKey())
	adopted, err := AdoptOrphan(ctx, s, lock, run.RunID, "h2", "bob", 30*time.Second, c.Now())
	if err != nil {
		t.Fatalf("AdoptOrphan: %v", err)
	}
	if adopted.Status != RunRunning {
		t.Fatalf("adopted status = %s, want %s", adopted.Status, RunRunning)
	}

	// Adoption is an explicit revocation of the stop request: the operator
	// asked for this run to continue.
	if adopted.CancelRequested {
		t.Error("AdoptOrphan returned a run that still carries CancelRequested")
	}
	if adopted.CancelRequestedAt != nil {
		t.Error("AdoptOrphan left CancelRequestedAt set")
	}
	requested, err := s.CancellationRequested(ctx, run.RunID)
	if err != nil {
		t.Fatalf("CancellationRequested: %v", err)
	}
	if requested {
		t.Fatal("the run is still flagged for cancellation after adoption, so resume would stop " +
			"at the first node boundary with nothing dispatched (AC-24)")
	}
}

// TestAdoptOrphanRewindsFailedNodes: a node recorded FAILED must not make the
// run permanently unresumable (AC-4, NFR-REL-2).
//
// The trap this closes: RunFailed.IsResumable() is true, so `execute` refuses a
// failed run and directs the operator to `resume` (FR-CLI-9, AC-23). If
// adoption could not rewind the node, `resume` would adopt the run, perform its
// provenance-gated destructive cleanup, and only then discover it cannot
// dispatch anything. Neither command could make progress and the operator would
// be left with an INVALID index and no tool path forward.
func TestAdoptOrphanRewindsFailedNodes(t *testing.T) {
	ctx := context.Background()
	s, c := newFileStore(t)
	plan := testPlan(t)
	run := mustCreateRun(t, s, plan, "run-failed-node")

	// Drive one node to FAILED the way an exhausted retry budget would.
	node := plan.Nodes[0].ID
	for _, step := range []struct{ from, to protocol.NodeState }{
		{protocol.NodePending, protocol.NodeReady},
		{protocol.NodeReady, protocol.NodeRunning},
		{protocol.NodeRunning, protocol.NodeFailed},
	} {
		if _, err := s.TransitionNode(ctx, NodeTransition{
			RunID: run.RunID, NodeID: node,
			From: step.from, To: step.to,
			Reason: protocol.ReasonNormal, At: c.Now(),
		}); err != nil {
			t.Fatalf("TransitionNode %s -> %s: %v", step.from, step.to, err)
		}
	}
	if _, err := s.SetRunStatus(ctx, RunStatusUpdate{
		RunID: run.RunID, From: RunRunning, To: RunFailed, At: c.Now(),
	}); err != nil {
		t.Fatalf("SetRunStatus: %v", err)
	}

	lock := mustLock(t, s, run.LockKey())
	if _, err := AdoptOrphan(ctx, s, lock, run.RunID, "h2", "bob", 30*time.Second, c.Now()); err != nil {
		t.Fatalf("AdoptOrphan: %v", err)
	}

	got, err := s.GetNode(ctx, run.RunID, node)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if got.State != protocol.NodePending {
		t.Fatalf("node %s is %s after adoption, want %s; the run cannot make progress (AC-4)",
			node, got.State, protocol.NodePending)
	}
}

// TestFailedIsStillTerminalWithinARun guards the other side: the rewind is
// authorized only for adoption, so a live executor still cannot loop on a node
// that keeps failing.
func TestFailedIsStillTerminalWithinARun(t *testing.T) {
	if protocol.ValidTransition(protocol.NodeFailed, protocol.NodePending, protocol.ReasonNormal) {
		t.Error("FAILED -> PENDING is permitted under the normal reason; FAILED must stay terminal within a run")
	}
	if !protocol.ValidTransition(protocol.NodeFailed, protocol.NodePending, protocol.ReasonResumeRetry) {
		t.Error("FAILED -> PENDING is not permitted on adoption, so a failed run is unresumable")
	}
	if protocol.ValidTransition(protocol.NodeDone, protocol.NodePending, protocol.ReasonResumeRetry) {
		t.Error("resume_retry authorizes more than the one edge it is scoped to")
	}
}
