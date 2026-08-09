package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// errCrash stands in for the process dying at a chosen instant.
var errCrash = errors.New("simulated crash")

// crashAt installs a hook that fails the next write whose target matches want,
// after the temporary file is fully written and synced but before the rename.
// That is the only window where a partial record could become visible, so it is
// the window the crash-safety claim has to survive.
func crashAt(s *FileStore, want string) *bool {
	fired := false
	s.hooks.beforeRename = func(target, temp string) error {
		if strings.Contains(target, want) {
			s.hooks.beforeRename = nil
			fired = true
			return errCrash
		}
		return nil
	}
	return &fired
}

func TestFileStoreCrashBetweenWriteAndRenameKeepsPreviousRecord(t *testing.T) {
	ctx := context.Background()
	nodeID := protocol.NodeID("assert")

	tests := []struct {
		name string
		// target names the file whose rename is interrupted.
		target string
		// mutate performs the write that is interrupted.
		mutate func(t *testing.T, s *FileStore, run Run) error
		// verify asserts the store still reads the pre-crash value.
		verify func(t *testing.T, s *FileStore, run Run)
	}{
		{
			name:   "node state",
			target: "nodes",
			mutate: func(t *testing.T, s *FileStore, run Run) error {
				_, err := s.TransitionNode(ctx, NodeTransition{
					RunID: run.RunID, NodeID: nodeID,
					From: protocol.NodePending, To: protocol.NodeReady,
				})
				return err
			},
			verify: func(t *testing.T, s *FileStore, run Run) {
				rec, err := s.GetNode(ctx, run.RunID, nodeID)
				if err != nil {
					t.Fatalf("GetNode after crash: %v", err)
				}
				if rec.State != protocol.NodePending {
					t.Fatalf("state = %s, want the pre-crash %s", rec.State, protocol.NodePending)
				}
			},
		},
		{
			name:   "run status",
			target: "run.json",
			mutate: func(t *testing.T, s *FileStore, run Run) error {
				_, err := s.SetRunStatus(ctx, RunStatusUpdate{
					RunID: run.RunID, From: RunRunning, To: RunCompleted,
				})
				return err
			},
			verify: func(t *testing.T, s *FileStore, run Run) {
				got, err := s.GetRun(ctx, run.RunID)
				if err != nil {
					t.Fatalf("GetRun after crash: %v", err)
				}
				if got.Status != RunRunning {
					t.Fatalf("status = %s, want the pre-crash %s", got.Status, RunRunning)
				}
			},
		},
		{
			name:   "lease",
			target: "lease.json",
			mutate: func(t *testing.T, s *FileStore, run Run) error {
				_, err := s.AcquireLease(ctx, run.RunID, "holder-a", 0)
				return err
			},
			verify: func(t *testing.T, s *FileStore, run Run) {
				_, err := s.GetLease(ctx, run.RunID)
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("GetLease after crash = %v, want ErrNotFound: a half-written lease became visible", err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newFileStore(t)
			run := mustCreateRun(t, s, testPlan(t, string(nodeID), "second"), "run-crash")

			fired := crashAt(s, tc.target)
			if err := tc.mutate(t, s, run); !errors.Is(err, errCrash) {
				t.Fatalf("mutate err = %v, want the simulated crash", err)
			}
			if !*fired {
				t.Fatal("the crash hook never fired; the test is not exercising the rename window")
			}
			tc.verify(t, s, run)

			// Reopening is what a `resume` does, and it must see the same
			// thing plus no leftover staging files.
			reopened, err := OpenFileStore(s.Root(), FileOptions{Clock: SystemClock})
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			tc.verify(t, reopened, run)
			assertNoTempFiles(t, reopened)
		})
	}
}

// The abandoned temporary file must be inert: no scan may mistake it for a
// record, and reopening the store must clear it.
func TestFileStoreCrashLeavesTempFileInert(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t, "n1", "n2"), "run-tmp")

	crashAt(s, "nodes")
	if err := func() error {
		_, err := s.TransitionNode(ctx, NodeTransition{
			RunID: run.RunID, NodeID: "n1", From: protocol.NodePending, To: protocol.NodeReady,
		})
		return err
	}(); !errors.Is(err, errCrash) {
		t.Fatalf("err = %v, want the simulated crash", err)
	}

	// The temp file exists.
	entries, err := os.ReadDir(s.w.tmpDir)
	if err != nil {
		t.Fatalf("ReadDir(tmp): %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no temporary file survived the crash; the test proves nothing")
	}

	// And no read is confused by it.
	nodes, err := s.ListNodes(ctx, run.RunID)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("ListNodes returned %d records, want 2", len(nodes))
	}
	runs, err := s.FindRuns(ctx, RunQuery{})
	if err != nil {
		t.Fatalf("FindRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("FindRuns returned %d runs, want 1", len(runs))
	}

	reopened, err := OpenFileStore(s.Root(), FileOptions{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	assertNoTempFiles(t, reopened)
}

// Every record on disk must be complete JSON. A reader that finds a truncated
// object is the failure mode temp-then-rename exists to prevent.
func TestFileStoreEveryRecordOnDiskIsCompleteJSON(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t, "n1", "n2"), "run-json")
	if _, err := s.TransitionNode(ctx, NodeTransition{
		RunID: run.RunID, NodeID: "n1", From: protocol.NodePending, To: protocol.NodeReady,
	}); err != nil {
		t.Fatalf("TransitionNode: %v", err)
	}
	mustLease(t, s, run.RunID, "h", 0)

	crashAt(s, "run.json")
	_, _ = s.SetRunStatus(ctx, RunStatusUpdate{RunID: run.RunID, From: RunRunning, To: RunCompleted})

	err := filepath.WalkDir(s.Root(), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		if strings.Contains(path, string(filepath.Separator)+".tmp"+string(filepath.Separator)) {
			return nil // staging, by definition not a record
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		var v any
		if jerr := json.Unmarshal(data, &v); jerr != nil {
			t.Errorf("%s is not complete JSON: %v", path, jerr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// A torn final line in the append-only trail is dropped, not fatal. An
// interrupted append is not an event that happened, and refusing to read the
// trail because of one would make a crash unrecoverable.
func TestFileStoreAuditToleratesTornFinalLine(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t), "run-torn")

	before, err := s.ListAudit(ctx, run.RunID, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}

	f, err := os.OpenFile(s.auditPath(run.RunID), os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatalf("open trail: %v", err)
	}
	if _, err := f.WriteString(`{"event_id":"partial","run_i`); err != nil {
		t.Fatalf("write partial line: %v", err)
	}
	_ = f.Close()

	after, err := s.ListAudit(ctx, run.RunID, 0)
	if err != nil {
		t.Fatalf("ListAudit after torn write: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("got %d events, want the %d that were complete", len(after), len(before))
	}

	// And a fresh store still appends correctly after the torn line.
	reopened, err := OpenFileStore(s.Root(), FileOptions{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	ev, err := reopened.AppendAudit(ctx, AuditEvent{RunID: run.RunID, Type: EventRunStatusChanged})
	if err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	if ev.Seq != int64(len(before))+1 {
		t.Errorf("seq = %d, want %d", ev.Seq, len(before)+1)
	}
}

// A corrupted line that is not the last one is real corruption and must be
// reported rather than silently skipped.
func TestFileStoreAuditRejectsCorruptionMidTrail(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t), "run-corrupt")

	path := s.auditPath(run.RunID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trail: %v", err)
	}
	if err := os.WriteFile(path, append([]byte("not json\n"), data...), 0o640); err != nil {
		t.Fatalf("write trail: %v", err)
	}
	if _, err := s.ListAudit(ctx, run.RunID, 0); !errors.Is(err, ErrStoreIO) {
		t.Fatalf("err = %v, want ErrStoreIO", err)
	}
}

func assertNoTempFiles(t *testing.T, s *FileStore) {
	t.Helper()
	entries, err := os.ReadDir(s.w.tmpDir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("ReadDir(tmp): %v", err)
	}
	for _, e := range entries {
		t.Errorf("temporary file %q survived a store reopen", e.Name())
	}
}

// TestFileStoreAuditSurvivesATornLineFollowedByAnAppend is NFR-REL-2: a crash
// mid-write must not leave an unrecoverable intermediate.
//
// TestFileStoreAuditToleratesTornFinalLine stops one step too early. It proves
// the append after a torn line *succeeds*, but never reads the trail again. The
// torn bytes carry no trailing newline, so the next append lands immediately
// after them and merges into one malformed line that is no longer last. From
// then on readAuditLocked rejects the whole trail, and because every append has
// to count existing events first, no further append, checkpoint, provenance
// write or status change can be recorded either. The run is dead.
func TestFileStoreAuditSurvivesATornLineFollowedByAnAppend(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t), "run-torn-then-append")

	before, err := s.ListAudit(ctx, run.RunID, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}

	// The process dies part way through writing a record.
	f, err := os.OpenFile(s.auditPath(run.RunID), os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatalf("open trail: %v", err)
	}
	if _, err := f.WriteString(`{"event_id":"partial","run_i`); err != nil {
		t.Fatalf("write partial line: %v", err)
	}
	_ = f.Close()

	// A second process starts up and appends, as `resume` would.
	second, err := OpenFileStore(s.Root(), FileOptions{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := second.AppendAudit(ctx, AuditEvent{RunID: run.RunID, Type: EventRunStatusChanged}); err != nil {
		t.Fatalf("AppendAudit after a torn line: %v", err)
	}

	// A third process must still be able to read and to write. This is the
	// step that was missing.
	third, err := OpenFileStore(s.Root(), FileOptions{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := third.ListAudit(ctx, run.RunID, 0)
	if err != nil {
		t.Fatalf("ListAudit after a torn line plus an append: %v", err)
	}
	if len(got) != len(before)+1 {
		t.Errorf("got %d events, want %d: the complete records must survive the torn one",
			len(got), len(before)+1)
	}
	if _, err := third.AppendAudit(ctx, AuditEvent{RunID: run.RunID, Type: EventRunStatusChanged}); err != nil {
		t.Fatalf("AppendAudit from a third process: %v", err)
	}

	// And the run is still checkpointable, which is what makes it resumable.
	if _, err := third.SetRunStatus(ctx, RunStatusUpdate{
		RunID: run.RunID, From: RunRunning, To: RunInterrupted,
	}); err != nil {
		t.Fatalf("SetRunStatus after a torn audit line: %v", err)
	}
}
