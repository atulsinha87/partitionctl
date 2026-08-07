package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// Record identifiers are assigned per run and must keep advancing across
// records, across store reopens, and past files that are not records.
func TestFileStoreRecordSequences(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t, "cic"), "run-seq")

	for i := 0; i < 3; i++ {
		if _, err := s.WriteProvenance(ctx, Provenance{
			RunID: run.RunID, NodeID: "cic",
			Object: protocol.NewObjectName("public", "idx"), ObjectKind: ObjectIndex,
		}, nil); err != nil {
			t.Fatalf("WriteProvenance %d: %v", i, err)
		}
	}
	found, err := s.FindProvenance(ctx, ProvenanceQuery{Object: protocol.NewObjectName("public", "idx")})
	if err != nil {
		t.Fatalf("FindProvenance: %v", err)
	}
	if len(found) != 3 {
		t.Fatalf("got %d records, want 3", len(found))
	}
	seen := map[string]bool{}
	for _, p := range found {
		if seen[p.ProvenanceID] {
			t.Fatalf("provenance id %q was reused", p.ProvenanceID)
		}
		seen[p.ProvenanceID] = true
	}

	// A stray file in the record directory must not derail the sequence or the
	// scan.
	if err := os.WriteFile(filepath.Join(s.provDir(run.RunID), "README.txt"), []byte("hi"), 0o640); err != nil {
		t.Fatalf("write stray file: %v", err)
	}
	reopened, err := OpenFileStore(s.Root(), FileOptions{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := reopened.WriteProvenance(ctx, Provenance{
		RunID: run.RunID, Object: protocol.NewObjectName("public", "idx2"), ObjectKind: ObjectIndex,
	}, nil); err != nil {
		t.Fatalf("WriteProvenance after reopen: %v", err)
	}
	again, err := reopened.FindProvenance(ctx, ProvenanceQuery{Object: protocol.NewObjectName("public", "idx2")})
	if err != nil {
		t.Fatalf("FindProvenance: %v", err)
	}
	if len(again) != 1 {
		t.Fatalf("got %d records, want 1", len(again))
	}
	if seen[again[0].ProvenanceID] {
		t.Fatalf("a reopened store reused the provenance id %q", again[0].ProvenanceID)
	}
}

func TestFileStoreListsForMissingAndEmptyRuns(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t), "run-empty")

	t.Run("authorizations of a run that recorded none", func(t *testing.T) {
		got, err := s.ListAuthorizations(ctx, run.RunID)
		if err != nil {
			t.Fatalf("ListAuthorizations: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d records", len(got))
		}
	})

	t.Run("authorizations of an unknown run", func(t *testing.T) {
		if _, err := s.ListAuthorizations(ctx, "no-such-run"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("nodes of an unknown run", func(t *testing.T) {
		if _, err := s.ListNodes(ctx, "no-such-run"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("a node that is not in the plan", func(t *testing.T) {
		if _, err := s.GetNode(ctx, run.RunID, "not-a-node"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("a lease for an unknown run", func(t *testing.T) {
		if _, err := s.GetLease(ctx, "no-such-run"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("cancelling an unknown run", func(t *testing.T) {
		if _, err := s.RequestCancel(ctx, "no-such-run", "op", ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		if _, err := s.CancellationRequested(ctx, "no-such-run"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

// The encoded path is derived from the id, so a record found at that path must
// carry the id that was asked for. Anything else is corruption, and it must be
// reported rather than returned as if it were the right record.
func TestFileStoreDetectsRecordIdentityMismatch(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t, "n1"), "run-mismatch")

	t.Run("run record", func(t *testing.T) {
		imposter := Run{RunID: "someone-else", Status: RunRunning}
		if err := s.w.writeJSON(s.runPath(run.RunID), imposter); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := s.GetRun(ctx, run.RunID); !errors.Is(err, ErrStoreIO) {
			t.Fatalf("err = %v, want ErrStoreIO", err)
		}
	})

	t.Run("node record", func(t *testing.T) {
		imposter := NodeRecord{RunID: run.RunID, NodeID: "other", State: protocol.NodePending}
		if err := s.w.writeJSON(s.nodePath(run.RunID, "n1"), imposter); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := s.GetNode(ctx, run.RunID, "n1"); !errors.Is(err, ErrStoreIO) {
			t.Fatalf("err = %v, want ErrStoreIO", err)
		}
	})
}

func TestFileStoreRejectsCorruptRecords(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t), "run-bad")

	if err := os.WriteFile(s.runPath(run.RunID), []byte("{not json"), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := s.GetRun(ctx, run.RunID); !errors.Is(err, ErrStoreIO) {
		t.Fatalf("err = %v, want ErrStoreIO", err)
	}
	if _, err := s.FindRuns(ctx, RunQuery{}); !errors.Is(err, ErrStoreIO) {
		t.Fatalf("FindRuns err = %v, want ErrStoreIO", err)
	}
}

func TestAdoptOrphanRejections(t *testing.T) {
	ctx := context.Background()
	s, c := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t), "run-adopt-bad")
	lock := mustLock(t, s, run.LockKey())

	t.Run("an unknown run", func(t *testing.T) {
		if _, err := AdoptOrphan(ctx, s, lock, "nope", "h", "a", 0, c.Now()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("a RUNNING run is not resumable", func(t *testing.T) {
		if _, err := AdoptOrphan(ctx, s, lock, run.RunID, "h", "a", 0, c.Now()); !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
	})

	t.Run("a completed run is not resumable", func(t *testing.T) {
		if _, err := s.SetRunStatus(ctx, RunStatusUpdate{
			RunID: run.RunID, From: RunRunning, To: RunCompleted,
		}); err != nil {
			t.Fatalf("SetRunStatus: %v", err)
		}
		if _, err := AdoptOrphan(ctx, s, lock, run.RunID, "h", "a", 0, c.Now()); !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
	})
}

func TestMarkOrphanedRejectsUnknownRun(t *testing.T) {
	ctx := context.Background()
	s, c := newFileStore(t)
	lock := mustLock(t, s, testKey())
	if _, err := MarkOrphaned(ctx, s, lock, "nope", c.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// The audit trail must survive a torn write in the middle of a guarded action,
// and the guarded action's own error must still reach the caller.
func TestGuardedActionErrorSurvivesAnAuditFailure(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t, "cic"), "run-guard-audit")

	ddlErr := errors.New("deadlock detected")
	_, err := s.WriteProvenance(ctx, Provenance{
		RunID: run.RunID, NodeID: "cic",
		Object: protocol.NewObjectName("public", "idx"), ObjectKind: ObjectIndex,
	}, func(ctx context.Context) error {
		// Make the trail unwritable while the action runs, so the outcome
		// event cannot be appended.
		if err := os.Chmod(s.auditPath(run.RunID), 0o400); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		return ddlErr
	})
	_ = os.Chmod(s.auditPath(run.RunID), 0o640)

	if !errors.Is(err, ddlErr) {
		t.Fatalf("err = %v, want the action's own error", err)
	}
}

func TestFileStoreHeartbeatWithoutALease(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t), "run-nohb")
	if _, err := s.Heartbeat(ctx, run.RunID, "holder"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestFileStoreAcquireLeaseRejectsABadTTL(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t), "run-badttl")
	if _, err := s.AcquireLease(ctx, run.RunID, "h", 100*time.Millisecond); err == nil {
		t.Fatal("want an error for a sub-second ttl")
	}
}
