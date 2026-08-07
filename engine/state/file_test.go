package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

func TestFileStoreCreateRun(t *testing.T) {
	ctx := context.Background()
	plan := testPlan(t)

	tests := []struct {
		name    string
		req     NewRun
		wantErr error
	}{
		{
			name: "seeds every node as PENDING",
			req:  NewRun{Plan: plan, RunID: "run-a", Actor: "alice"},
		},
		{
			name:    "refuses a nil plan, because a run must bind a digest",
			req:     NewRun{RunID: "run-b"},
			wantErr: protocol.ErrInvalidPlan,
		},
		{
			name: "refuses an unsealed plan (INV-6)",
			req: NewRun{RunID: "run-c", Plan: func() *protocol.Plan {
				p := testPlan(t)
				p.Digest = ""
				return p
			}()},
			wantErr: protocol.ErrDigestMismatch,
		},
		{
			name: "refuses a digest with no sha256 prefix",
			req: NewRun{RunID: "run-d", Plan: func() *protocol.Plan {
				p := testPlan(t)
				p.Digest = "deadbeef"
				return p
			}()},
			wantErr: protocol.ErrDigestMismatch,
		},
		{
			name: "refuses a plan with no nodes",
			req: NewRun{RunID: "run-e", Plan: func() *protocol.Plan {
				p := testPlan(t)
				p.Nodes = nil
				return p
			}()},
			wantErr: protocol.ErrInvalidPlan,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newFileStore(t)
			run, err := s.CreateRun(ctx, tc.req)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			if run.Status != RunRunning {
				t.Errorf("status = %s, want %s", run.Status, RunRunning)
			}
			if run.PlanDigest != tc.req.Plan.Digest {
				t.Errorf("plan digest = %q, want %q", run.PlanDigest, tc.req.Plan.Digest)
			}
			nodes, err := s.ListNodes(ctx, run.RunID)
			if err != nil {
				t.Fatalf("ListNodes: %v", err)
			}
			if len(nodes) != len(tc.req.Plan.Nodes) {
				t.Fatalf("seeded %d node records, want %d", len(nodes), len(tc.req.Plan.Nodes))
			}
			for _, n := range nodes {
				if n.State != protocol.InitialNodeState() {
					t.Errorf("node %s state = %s, want %s", n.NodeID, n.State, protocol.InitialNodeState())
				}
			}
		})
	}
}

func TestFileStoreCreateRunRejectsDuplicate(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	plan := testPlan(t)
	mustCreateRun(t, s, plan, "run-dup")
	if _, err := s.CreateRun(ctx, NewRun{Plan: plan, RunID: "run-dup"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second CreateRun err = %v, want ErrConflict", err)
	}
}

// A node id with slashes, spaces, a colon and a non-ASCII rune must still round
// trip: the file store turns it into a path segment, and a lossy encoding would
// make two nodes share a file.
func TestFileStoreNodeIDEncoding(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	ids := []string{
		"leaf/orders_2026_03",
		"leaf/orders_2026_03/verify",
		"..",
		".hidden",
		"with space and:colon",
		"unicode-ünïcøde-Ω",
		strings.Repeat("very-long-node-id-", 20),
	}
	plan := testPlan(t, ids...)
	run := mustCreateRun(t, s, plan, "run-enc")

	nodes, err := s.ListNodes(ctx, run.RunID)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != len(ids) {
		t.Fatalf("got %d node records, want %d: an encoding collision lost one", len(nodes), len(ids))
	}
	for _, id := range ids {
		rec, err := s.GetNode(ctx, run.RunID, protocol.NodeID(id))
		if err != nil {
			t.Fatalf("GetNode(%q): %v", id, err)
		}
		if rec.NodeID != protocol.NodeID(id) {
			t.Errorf("GetNode(%q) returned %q", id, rec.NodeID)
		}
	}
	// Nothing may become a dotfile or escape the nodes directory.
	entries, err := os.ReadDir(s.nodesDir(run.RunID))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("node file %q is hidden", e.Name())
		}
		if strings.Contains(e.Name(), string(filepath.Separator)) {
			t.Errorf("node file %q contains a separator", e.Name())
		}
	}
}

func TestFileStoreNodeTransitions(t *testing.T) {
	ctx := context.Background()
	nodeID := protocol.NodeID("assert")

	tests := []struct {
		name    string
		steps   []NodeTransition
		wantErr error
	}{
		{
			name: "the happy path through D7",
			steps: []NodeTransition{
				{From: protocol.NodePending, To: protocol.NodeReady},
				{From: protocol.NodeReady, To: protocol.NodeRunning, IncrementAttempt: true},
				{From: protocol.NodeRunning, To: protocol.NodeVerifying},
				{From: protocol.NodeVerifying, To: protocol.NodeDone},
			},
		},
		{
			name: "retry loop",
			steps: []NodeTransition{
				{From: protocol.NodePending, To: protocol.NodeReady},
				{From: protocol.NodeReady, To: protocol.NodeRunning, IncrementAttempt: true},
				{From: protocol.NodeRunning, To: protocol.NodeRetryWait, Err: protocol.ErrLockHeld},
				{From: protocol.NodeRetryWait, To: protocol.NodeReady},
				{From: protocol.NodeReady, To: protocol.NodeRunning, IncrementAttempt: true},
				{From: protocol.NodeRunning, To: protocol.NodeFailed, Err: protocol.ErrFailure},
			},
		},
		{
			name: "an edge D7 does not have is refused",
			steps: []NodeTransition{
				{From: protocol.NodePending, To: protocol.NodeDone},
			},
			wantErr: protocol.ErrInvalidTransition,
		},
		{
			name: "RUNNING -> PENDING without orphan recovery is refused (INV-5)",
			steps: []NodeTransition{
				{From: protocol.NodePending, To: protocol.NodeReady},
				{From: protocol.NodeReady, To: protocol.NodeRunning},
				{From: protocol.NodeRunning, To: protocol.NodePending},
			},
			wantErr: protocol.ErrInvalidTransition,
		},
		{
			name: "RUNNING -> PENDING with orphan recovery is allowed (INV-5)",
			steps: []NodeTransition{
				{From: protocol.NodePending, To: protocol.NodeReady},
				{From: protocol.NodeReady, To: protocol.NodeRunning},
				{From: protocol.NodeRunning, To: protocol.NodePending, Reason: protocol.ReasonOrphanRecovery},
			},
		},
		{
			name: "orphan recovery authorizes no other edge",
			steps: []NodeTransition{
				{From: protocol.NodePending, To: protocol.NodeReady, Reason: protocol.ReasonOrphanRecovery},
			},
			wantErr: protocol.ErrInvalidTransition,
		},
		{
			name: "a stale From is a conflict, not a silent overwrite",
			steps: []NodeTransition{
				{From: protocol.NodePending, To: protocol.NodeReady},
				{From: protocol.NodePending, To: protocol.NodeReady},
			},
			wantErr: ErrConflict,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newFileStore(t)
			run := mustCreateRun(t, s, testPlan(t, string(nodeID), "second"), "run-t")

			var err error
			for i, step := range tc.steps {
				step.RunID = run.RunID
				step.NodeID = nodeID
				_, err = s.TransitionNode(ctx, step)
				if err != nil && i < len(tc.steps)-1 && tc.wantErr == nil {
					t.Fatalf("step %d: %v", i, err)
				}
				if err != nil {
					break
				}
			}
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			final := tc.steps[len(tc.steps)-1].To
			rec, gerr := s.GetNode(ctx, run.RunID, nodeID)
			if gerr != nil {
				t.Fatalf("GetNode: %v", gerr)
			}
			if rec.State != final {
				t.Errorf("state = %s, want %s", rec.State, final)
			}
		})
	}
}

func TestFileStoreTransitionRecordsAttemptsAndErrors(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t, "n1"), "run-attempts")
	id := protocol.NodeID("n1")

	step := func(from, to protocol.NodeState, inc bool, err error) NodeRecord {
		t.Helper()
		rec, terr := s.TransitionNode(ctx, NodeTransition{
			RunID: run.RunID, NodeID: id, From: from, To: to, IncrementAttempt: inc, Err: err,
		})
		if terr != nil {
			t.Fatalf("%s -> %s: %v", from, to, terr)
		}
		return rec
	}

	step(protocol.NodePending, protocol.NodeReady, false, nil)
	rec := step(protocol.NodeReady, protocol.NodeRunning, true, nil)
	if rec.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", rec.Attempts)
	}
	if rec.StartedAt == nil {
		t.Error("StartedAt was not stamped on entry into RUNNING")
	}
	firstStart := rec.StartedAt.Time

	rec = step(protocol.NodeRunning, protocol.NodeRetryWait, false, protocol.ErrLockHeld)
	if rec.ErrorKind != protocol.KindLockHeld {
		t.Errorf("error kind = %q, want %q", rec.ErrorKind, protocol.KindLockHeld)
	}
	if rec.LastError == "" {
		t.Error("LastError was not recorded")
	}

	step(protocol.NodeRetryWait, protocol.NodeReady, false, nil)
	rec = step(protocol.NodeReady, protocol.NodeRunning, true, nil)
	if rec.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", rec.Attempts)
	}
	if !rec.StartedAt.Time.Equal(firstStart) {
		t.Errorf("StartedAt moved on the second dispatch: %s -> %s", firstStart, rec.StartedAt.Time)
	}

	step(protocol.NodeRunning, protocol.NodeVerifying, false, nil)
	rec = step(protocol.NodeVerifying, protocol.NodeDone, false, nil)
	if rec.LastError != "" || rec.ErrorKind != "" {
		t.Errorf("a DONE node still carries error %q/%q", rec.LastError, rec.ErrorKind)
	}
}

func TestFileStoreRunStatusMachine(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name    string
		from    RunStatus
		to      RunStatus
		wantErr error
	}{
		{name: "running to completed", from: RunRunning, to: RunCompleted},
		{name: "running to failed", from: RunRunning, to: RunFailed},
		{name: "running to orphaned", from: RunRunning, to: RunOrphaned},
		{name: "running to interrupted", from: RunRunning, to: RunInterrupted},
		{name: "completed is terminal", from: RunCompleted, to: RunRunning, wantErr: protocol.ErrInvalidTransition},
		{name: "cancelled is terminal", from: RunCancelled, to: RunRunning, wantErr: protocol.ErrInvalidTransition},
		{name: "self transition is not a transition", from: RunRunning, to: RunRunning, wantErr: protocol.ErrInvalidTransition},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newFileStore(t)
			run := mustCreateRun(t, s, testPlan(t), "run-status")

			// Drive the run into the `from` status first where it is not RUNNING.
			if tc.from != RunRunning {
				if _, err := s.SetRunStatus(ctx, RunStatusUpdate{
					RunID: run.RunID, From: RunRunning, To: tc.from,
				}); err != nil {
					t.Fatalf("setup transition to %s: %v", tc.from, err)
				}
			}
			_, err := s.SetRunStatus(ctx, RunStatusUpdate{RunID: run.RunID, From: tc.from, To: tc.to})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SetRunStatus: %v", err)
			}
			got, err := s.GetRun(ctx, run.RunID)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if got.Status != tc.to {
				t.Errorf("status = %s, want %s", got.Status, tc.to)
			}
		})
	}
}

// INV-6: a run is bound to exactly one plan digest for its lifetime. There is
// no API that changes it, and every mutation must leave it alone.
func TestFileStoreRunDigestIsImmutable(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	plan := testPlan(t)
	run := mustCreateRun(t, s, plan, "run-inv6")
	want := run.PlanDigest

	if _, err := s.SetRunStatus(ctx, RunStatusUpdate{RunID: run.RunID, From: RunRunning, To: RunFailed, Error: "boom"}); err != nil {
		t.Fatalf("SetRunStatus: %v", err)
	}
	if _, err := s.RequestCancel(ctx, run.RunID, "op", "stop"); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	got, err := s.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.PlanDigest != want {
		t.Fatalf("plan digest changed from %q to %q (INV-6)", want, got.PlanDigest)
	}

	// A second plan means a second run, never a rebinding of the first.
	other := testPlanWithID(t, "plan-test-2", "orders")
	if other.Digest == plan.Digest {
		t.Fatal("test setup: the two plans have the same digest")
	}
	second, err := s.CreateRun(ctx, NewRun{Plan: other, RunID: "run-inv6b"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if second.PlanDigest == want {
		t.Fatal("the second run inherited the first run's digest")
	}
}

func TestFileStoreFindRuns(t *testing.T) {
	ctx := context.Background()
	s, c := newFileStore(t)

	orders := testPlanWithID(t, "p-orders", "orders")
	events := testPlanWithID(t, "p-events", "events")

	r1 := mustCreateRun(t, s, orders, "run-1")
	c.Advance(time.Minute)
	r2 := mustCreateRun(t, s, events, "run-2")
	c.Advance(time.Minute)
	r3 := mustCreateRun(t, s, orders, "run-3")

	if _, err := s.SetRunStatus(ctx, RunStatusUpdate{RunID: r1.RunID, From: RunRunning, To: RunCompleted}); err != nil {
		t.Fatalf("SetRunStatus: %v", err)
	}

	ordersTable := protocol.NewObjectName("public", "orders")

	tests := []struct {
		name string
		q    RunQuery
		want []RunID
	}{
		{name: "all, oldest first", q: RunQuery{}, want: []RunID{r1.RunID, r2.RunID, r3.RunID}},
		{name: "by table", q: RunQuery{Table: &ordersTable}, want: []RunID{r1.RunID, r3.RunID}},
		{name: "by digest", q: RunQuery{PlanDigest: events.Digest}, want: []RunID{r2.RunID}},
		{name: "by status", q: RunQuery{Statuses: []RunStatus{RunCompleted}}, want: []RunID{r1.RunID}},
		{name: "by operation", q: RunQuery{Operation: protocol.OpDropIndex}, want: nil},
		{name: "by database", q: RunQuery{Database: "appdb"}, want: []RunID{r1.RunID, r2.RunID, r3.RunID}},
		{name: "wrong database", q: RunQuery{Database: "otherdb"}, want: nil},
		{name: "limit", q: RunQuery{Limit: 2}, want: []RunID{r1.RunID, r2.RunID}},
		{name: "by run id", q: RunQuery{RunID: r3.RunID}, want: []RunID{r3.RunID}},
		{name: "unknown run id", q: RunQuery{RunID: "nope"}, want: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.FindRuns(ctx, tc.q)
			if err != nil {
				t.Fatalf("FindRuns: %v", err)
			}
			var ids []RunID
			for _, r := range got {
				ids = append(ids, r.RunID)
			}
			if len(ids) != len(tc.want) {
				t.Fatalf("got %v, want %v", ids, tc.want)
			}
			for i := range ids {
				if ids[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", ids, tc.want)
				}
			}
		})
	}
}

func TestFileStoreGetRunNotFound(t *testing.T) {
	s, _ := newFileStore(t)
	if _, err := s.GetRun(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// A run directory whose run.json never landed is invisible: that is the crash
// window inside CreateRun, and it must not surface a half-made run.
func TestFileStoreIgnoresRunDirWithoutRecord(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	mustCreateRun(t, s, testPlan(t), "run-real")

	orphanDir := filepath.Join(s.runsDir(), "run-partial-0000000000000000")
	if err := os.MkdirAll(filepath.Join(orphanDir, "nodes"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "nodes", "x.json"), []byte("{}"), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}

	runs, err := s.FindRuns(ctx, RunQuery{})
	if err != nil {
		t.Fatalf("FindRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].RunID != "run-real" {
		t.Fatalf("FindRuns saw %d runs, want only run-real", len(runs))
	}
}
