package state

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// A whole run, in the order the executor performs it, including the
// interruption and the resume. It is the closest this package gets to
// documenting the port's intended usage, and it is what would break first if
// the invariants stopped composing.
func TestFullRunLifecycleWithInterruptionAndResume(t *testing.T) {
	ctx := context.Background()
	s, c := newFileStore(t)

	plan := testClaimPlan(t, "orders_idx_leaf_1", "orders_idx_leaf_2")
	key := LockKey{Database: plan.Target.Database, Table: plan.Target.Table}

	// --- first attempt -------------------------------------------------
	lock1, err := s.TryLock(ctx, key) // FR-LOCK-1: before any node runs
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	run := mustCreateRun(t, s, plan, "run-life")
	mustLease(t, s, run.RunID, "host-a/100", 30*time.Second)

	drive := func(id protocol.NodeID, states ...protocol.NodeState) {
		t.Helper()
		from := protocol.NodePending
		for _, to := range states {
			if _, err := s.TransitionNode(ctx, NodeTransition{
				RunID: run.RunID, NodeID: id, From: from, To: to,
				IncrementAttempt: to == protocol.NodeRunning,
			}); err != nil {
				t.Fatalf("%s %s->%s: %v", id, from, to, err)
			}
			from = to
		}
	}
	drive("parent", protocol.NodeReady, protocol.NodeRunning, protocol.NodeVerifying, protocol.NodeDone)

	// leaf-1 dispatches. INV-1 as amended: the claim on the object it is about
	// to create was committed by CreateRun and became live at READY, so the
	// object can never exist without a durable record naming it.
	leaf1 := protocol.NodeID("cic:orders_idx_leaf_1")
	leaf2 := protocol.NodeID("cic:orders_idx_leaf_2")
	leafIdx := protocol.NewObjectName("public", "orders_idx_leaf_1")
	drive(leaf1, protocol.NodeReady, protocol.NodeRunning)
	if _, found, cerr := ClaimsObject(ctx, s, leafIdx); cerr != nil || !found {
		t.Fatalf("ClaimsObject before the DDL: found=%t err=%v (INV-1)", found, cerr)
	}
	// The process now dies mid-CREATE INDEX CONCURRENTLY, leaving an INVALID
	// index behind with no ownership marker on it.

	// --- the process dies here -----------------------------------------
	// No status change, no lease release: exactly what SIGKILL leaves behind.
	c.Advance(31 * time.Second)

	// --- resume --------------------------------------------------------
	// The first process is gone, so its lock file has expired too.
	if _, err := s.TryLock(ctx, key); !errors.Is(err, protocol.ErrLockHeld) {
		t.Fatalf("the lock was not still claimed: %v", err)
	}
	_ = lock1 // the dead process never released it
	c.Advance(DefaultLeaseTTL)

	lock2, err := s.TryLock(ctx, key)
	if err != nil {
		t.Fatalf("TryLock on resume: %v", err)
	}

	orphans, err := RecoverOrphans(ctx, s, lock2, c.Now())
	if err != nil {
		t.Fatalf("RecoverOrphans: %v", err)
	}
	if len(orphans) != 1 || orphans[0].RunID != run.RunID {
		t.Fatalf("recovered %v", orphans)
	}

	// leaf-1 was in flight and is back to PENDING (INV-5).
	rec, err := s.GetNode(ctx, run.RunID, leaf1)
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if rec.State != protocol.NodePending {
		t.Fatalf("leaf-1 is %s, want PENDING after orphan recovery", rec.State)
	}
	if rec.Attempts != 1 {
		t.Errorf("attempts = %d, want the previous dispatch to still count", rec.Attempts)
	}

	// The unmarked INVALID index it left behind is still claimed, so `resume`
	// may adopt it and clean it up (FR-PLAN-6, AC-5). The claim survived
	// orphan recovery, which is the only reason this works.
	claimRun, claimed, err := ClaimsObject(ctx, s, leafIdx)
	if err != nil {
		t.Fatalf("ClaimsObject: %v", err)
	}
	if !claimed || claimRun != run.RunID {
		t.Fatal("the interrupted build left no claim; resume could not prove it owns the index")
	}
	verdict := protocol.DecideProvenanceDrop(protocol.ProvenanceDropInput{
		Object: leafIdx, Status: protocol.MarkerAbsent, ClaimRun: string(claimRun),
	})
	if verdict.Action != protocol.DropAdoptThenDrop {
		t.Fatalf("verdict = %s, want adopt_then_drop (A.5.1)", verdict.Action)
	}

	if _, err := AdoptOrphan(ctx, s, lock2, run.RunID, "host-b/200", "bob", 30*time.Second, c.Now()); err != nil {
		t.Fatalf("AdoptOrphan: %v", err)
	}

	// The cleanup drop is justified before it runs (INV-2).
	dropped := false
	if _, err := s.RecordAuthorization(ctx, AuthorizationRecord{
		RunID: run.RunID, NodeID: leaf1, Mode: protocol.AuthProvenance,
		Object: leafIdx, Evidence: verdict.Evidence,
	}, func(ctx context.Context) error {
		dropped = true
		return nil
	}); err != nil {
		t.Fatalf("RecordAuthorization: %v", err)
	}
	if !dropped {
		t.Fatal("the drop did not run")
	}

	// Roll forward.
	drive(leaf1, protocol.NodeReady, protocol.NodeRunning, protocol.NodeVerifying, protocol.NodeDone)
	drive(leaf2, protocol.NodeReady, protocol.NodeRunning, protocol.NodeVerifying, protocol.NodeDone)

	// And the run that finished claims nothing at all any more (AC-6).
	if _, found, cerr := ClaimsObject(ctx, s, leafIdx); cerr != nil || found {
		t.Fatalf("a run whose nodes are all DONE still claims %s: found=%t err=%v", leafIdx, found, cerr)
	}

	if _, err := s.SetRunStatus(ctx, RunStatusUpdate{
		RunID: run.RunID, From: RunRunning, To: RunCompleted,
	}); err != nil {
		t.Fatalf("SetRunStatus: %v", err)
	}
	if err := s.ReleaseLease(ctx, run.RunID, "host-b/200"); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if err := lock2.Unlock(ctx); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	// --- what the operator and the auditor see -------------------------
	nodes, err := s.ListNodes(ctx, run.RunID)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	counts := CountNodes(nodes)
	if counts.Complete != 3 || counts.Remaining != 0 {
		t.Errorf("counts = %+v, want everything complete", counts)
	}

	// FR-CLI-9: nothing incomplete remains for this digest.
	incomplete, err := IncompleteRunsForPlan(ctx, s, plan.Digest)
	if err != nil {
		t.Fatalf("IncompleteRunsForPlan: %v", err)
	}
	if len(incomplete) != 0 {
		t.Errorf("got %d incomplete runs after completion", len(incomplete))
	}

	// The run carried one digest throughout (INV-6).
	final, err := s.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if final.PlanDigest != plan.Digest {
		t.Errorf("plan digest = %q, want %q", final.PlanDigest, plan.Digest)
	}

	// The trail records every destructive act with its justification, before
	// the act (AC-20), and its sequence has no gaps (INV-3).
	trail, err := s.ListAudit(ctx, run.RunID, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	for i, ev := range trail {
		if ev.Seq != int64(i+1) {
			t.Fatalf("audit seq %d at position %d: the append-only trail has a gap", ev.Seq, i)
		}
	}
	wantTypes := []AuditEventType{
		EventRunOpened, EventLeaseAcquired, EventNodeTransition,
		EventRunOrphaned, EventRunAdopted,
		EventAuthorizationRecorded, EventDestructiveExecuted,
		EventRunStatusChanged, EventLeaseReleased,
	}
	present := map[AuditEventType]bool{}
	for _, ev := range trail {
		present[ev.Type] = true
	}
	for _, want := range wantTypes {
		if !present[want] {
			t.Errorf("the trail is missing %s", want)
		}
	}
}

// The store is safe for concurrent use within one process. Cross-process safety
// is the advisory lock's job, but a single executor still touches the store
// from its dispatch loop and its heartbeat timer at once.
func TestFileStoreConcurrentUse(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)

	const n = 24
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("node-%02d", i)
	}
	run := mustCreateRun(t, s, testPlan(t, ids...), "run-concurrent")
	mustLease(t, s, run.RunID, "holder", time.Minute)

	var wg sync.WaitGroup
	errs := make(chan error, n+2)

	for _, id := range ids {
		wg.Add(1)
		go func(id protocol.NodeID) {
			defer wg.Done()
			from := protocol.NodePending
			for _, to := range []protocol.NodeState{
				protocol.NodeReady, protocol.NodeRunning, protocol.NodeVerifying, protocol.NodeDone,
			} {
				if _, err := s.TransitionNode(ctx, NodeTransition{
					RunID: run.RunID, NodeID: id, From: from, To: to,
				}); err != nil {
					errs <- fmt.Errorf("%s %s->%s: %w", id, from, to, err)
					return
				}
				from = to
			}
		}(protocol.NodeID(id))
	}

	// A heartbeat timer and a status poller, running alongside.
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := s.Heartbeat(ctx, run.RunID, "holder"); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := s.ListNodes(ctx, run.RunID); err != nil {
				errs <- err
				return
			}
		}
	}()

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent use: %v", err)
	}

	nodes, err := s.ListNodes(ctx, run.RunID)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if counts := CountNodes(nodes); counts.Complete != n {
		t.Fatalf("complete = %d, want %d", counts.Complete, n)
	}
	// The audit trail's sequence is dense even under concurrent appends.
	trail, err := s.ListAudit(ctx, run.RunID, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	for i, ev := range trail {
		if ev.Seq != int64(i+1) {
			t.Fatalf("audit seq %d at position %d under concurrency", ev.Seq, i)
		}
	}
}

// Only one process may execute against a target at a time (AC-10), and the
// loser's message must be actionable.
func TestTwoStoresOverOneDirectoryExcludeEachOther(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newClock()

	a, err := OpenFileStore(dir, FileOptions{Clock: c.Clock(), Holder: "proc-a/1"})
	if err != nil {
		t.Fatalf("OpenFileStore a: %v", err)
	}
	b, err := OpenFileStore(dir, FileOptions{Clock: c.Clock(), Holder: "proc-b/2"})
	if err != nil {
		t.Fatalf("OpenFileStore b: %v", err)
	}

	plan := testPlan(t)
	run, err := a.CreateRun(ctx, NewRun{Plan: plan, RunID: "run-shared", Actor: "alice"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if _, err := a.TryLock(ctx, run.LockKey()); err != nil {
		t.Fatalf("a.TryLock: %v", err)
	}
	_, err = b.TryLock(ctx, run.LockKey())
	if !errors.Is(err, protocol.ErrLockHeld) {
		t.Fatalf("b.TryLock = %v, want ErrLockHeld", err)
	}
	if !strings.Contains(err.Error(), "proc-a/1") {
		t.Errorf("the message does not name the holder: %v", err)
	}

	// The second store sees the first store's run, which is what makes
	// `status` from another terminal work.
	seen, err := b.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatalf("b.GetRun: %v", err)
	}
	if seen.PlanDigest != plan.Digest {
		t.Errorf("digest = %q, want %q", seen.PlanDigest, plan.Digest)
	}
}

// Two stores over one directory must not both mint the same audit sequence
// number. Only one process should be writing at a time, but the trail is the
// compliance artifact and a duplicated event id would be indistinguishable from
// a forged one.
func TestAuditSequenceSurvivesASecondWriter(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := newClock()

	a, err := OpenFileStore(dir, FileOptions{Clock: c.Clock(), Holder: "proc-a/1"})
	if err != nil {
		t.Fatalf("OpenFileStore a: %v", err)
	}
	b, err := OpenFileStore(dir, FileOptions{Clock: c.Clock(), Holder: "proc-b/2"})
	if err != nil {
		t.Fatalf("OpenFileStore b: %v", err)
	}

	run := mustCreateRun(t, a, testPlan(t), "run-two-writers")

	// Alternate writers so each one's cached cursor is stale every other turn.
	for i := 0; i < 4; i++ {
		for _, s := range []*FileStore{a, b} {
			if _, err := s.AppendAudit(ctx, AuditEvent{
				RunID: run.RunID, Type: EventNodeTransition,
			}); err != nil {
				t.Fatalf("AppendAudit: %v", err)
			}
		}
	}

	trail, err := a.ListAudit(ctx, run.RunID, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	ids := map[string]bool{}
	for i, ev := range trail {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d: the sequence is not dense", i, ev.Seq)
		}
		if ids[ev.EventID] {
			t.Fatalf("event id %q was minted twice", ev.EventID)
		}
		ids[ev.EventID] = true
	}
	if len(trail) != 9 { // one run.opened plus eight appends
		t.Fatalf("got %d events, want 9", len(trail))
	}
}
