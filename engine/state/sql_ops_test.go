package state

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

func runCols() []string  { return strings.Split(runColumns, ", ") }
func nodeCols() []string { return strings.Split(nodeColumns, ", ") }
func authCols() []string { return strings.Split(authorizationColumns, ", ") }
func auditCols() []string {
	return strings.Split(auditColumns, ", ")
}
func leaseCols() []string { return strings.Split(leaseColumns, ", ") }

func auditRow(runID string, seq int64) []driver.Value {
	return []driver.Value{"evt", runID, seq, "", "run.status_changed", []byte(`{}`), baseTime}
}

func nodeRow(runID, nodeID, state string, attempts int64) []driver.Value {
	return []driver.Value{runID, nodeID, "wait", state, "", "", attempts, "", "", nil, baseTime}
}

func TestSQLFindRuns(t *testing.T) {
	ctx := context.Background()
	s, fake := newSQLStoreForTest(t, SQLOptions{})
	fake.Reply("ORDER BY started_at, run_id", runCols(), runRow("run-1"), runRow("run-2"))

	got, err := s.FindRuns(ctx, RunQuery{Statuses: []RunStatus{RunRunning}})
	if err != nil {
		t.Fatalf("FindRuns: %v", err)
	}
	if len(got) != 2 || got[0].RunID != "run-1" || got[1].RunID != "run-2" {
		t.Fatalf("got %+v", got)
	}
	if got[0].Target.Table.Name != "orders" {
		t.Errorf("target table = %+v", got[0].Target.Table)
	}
	c, _ := fake.Find("ORDER BY started_at, run_id")
	if len(c.Args) != 1 || c.Args[0] != "RUNNING" {
		t.Errorf("args = %v", c.Args)
	}
}

func TestSQLFindRunsPropagatesQueryErrors(t *testing.T) {
	ctx := context.Background()
	s, fake := newSQLStoreForTest(t, SQLOptions{})
	fake.Fail("ORDER BY started_at, run_id", errors.New("connection reset by peer"))
	if _, err := s.FindRuns(ctx, RunQuery{}); !errors.Is(err, ErrStoreIO) {
		t.Fatalf("err = %v, want ErrStoreIO", err)
	}
}

func TestSQLSetRunStatus(t *testing.T) {
	ctx := context.Background()

	t.Run("applied", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		updated := runRow("run-1")
		updated[10] = string(RunCompleted)
		fake.Reply("run SET status", runCols(), updated)
		fake.Reply("audit_event", auditCols(), auditRow("run-1", 2))

		got, err := s.SetRunStatus(ctx, RunStatusUpdate{
			RunID: "run-1", From: RunRunning, To: RunCompleted,
		})
		if err != nil {
			t.Fatalf("SetRunStatus: %v", err)
		}
		if got.Status != RunCompleted {
			t.Errorf("status = %s", got.Status)
		}
		c, _ := fake.Find("run SET status")
		if c.Args[1] != string(RunCompleted) || c.Args[5] != string(RunRunning) {
			t.Errorf("args = %v; the compare-and-set does not carry both statuses", c.Args)
		}
		if c.Args[3] == nil {
			t.Error("finished_at was not stamped on a terminal status")
		}
		if _, ok := fake.Find("COMMIT"); !ok {
			t.Error("the status change was not committed")
		}
	})

	t.Run("a non-terminal target leaves finished_at null", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		updated := runRow("run-1")
		updated[10] = string(RunRunning)
		fake.Reply("run SET status", runCols(), updated)
		fake.Reply("audit_event", auditCols(), auditRow("run-1", 2))

		if _, err := s.SetRunStatus(ctx, RunStatusUpdate{
			RunID: "run-1", From: RunOrphaned, To: RunRunning,
		}); err != nil {
			t.Fatalf("SetRunStatus: %v", err)
		}
		c, _ := fake.Find("run SET status")
		if c.Args[3] != nil {
			t.Errorf("finished_at = %v, want NULL when the run resumes", c.Args[3])
		}
	})

	t.Run("matching no row is a conflict naming the real status", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		fake.Reply("run SET status", runCols())
		current := runRow("run-1")
		current[10] = string(RunFailed)
		fake.Reply(".run WHERE run_id", runCols(), current)

		_, err := s.SetRunStatus(ctx, RunStatusUpdate{
			RunID: "run-1", From: RunRunning, To: RunCompleted,
		})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
		if !strings.Contains(err.Error(), string(RunFailed)) {
			t.Errorf("the message does not name the actual status: %v", err)
		}
	})

	t.Run("matching no row on a vanished run is not found", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		fake.Reply("run SET status", runCols())
		fake.Reply(".run WHERE run_id", runCols())

		_, err := s.SetRunStatus(ctx, RunStatusUpdate{
			RunID: "gone", From: RunRunning, To: RunCompleted,
		})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

func TestSQLNodeReads(t *testing.T) {
	ctx := context.Background()

	t.Run("get", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		row := nodeRow("run-1", "n1", "RUNNING", 2)
		row[9] = baseTime // started_at
		row[7] = "lock timeout"
		row[8] = string(protocol.KindLockHeld)
		fake.Reply("node_state WHERE run_id = $1 AND node_id", nodeCols(), row)

		got, err := s.GetNode(ctx, "run-1", "n1")
		if err != nil {
			t.Fatalf("GetNode: %v", err)
		}
		if got.State != protocol.NodeRunning || got.Attempts != 2 {
			t.Errorf("got %+v", got)
		}
		if got.StartedAt == nil {
			t.Error("started_at was not scanned")
		}
		if got.ErrorKind != protocol.KindLockHeld {
			t.Errorf("error kind = %q", got.ErrorKind)
		}
	})

	t.Run("get missing", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		fake.Reply("node_state WHERE run_id = $1 AND node_id", nodeCols())
		if _, err := s.GetNode(ctx, "run-1", "n1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("list", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		fake.Reply("ORDER BY node_id", nodeCols(),
			nodeRow("run-1", "a", "DONE", 1),
			nodeRow("run-1", "b", "PENDING", 0))

		got, err := s.ListNodes(ctx, "run-1")
		if err != nil {
			t.Fatalf("ListNodes: %v", err)
		}
		if len(got) != 2 || got[0].NodeID != "a" {
			t.Fatalf("got %+v", got)
		}
		counts := CountNodes(got)
		if counts.Complete != 1 || counts.Remaining != 1 {
			t.Errorf("counts = %+v", counts)
		}
	})

	t.Run("transition matching no row is a conflict", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		fake.Reply("node_state SET", nodeCols())
		fake.Reply("node_state WHERE run_id = $1 AND node_id", nodeCols(),
			nodeRow("run-1", "n1", "DONE", 1))

		_, err := s.TransitionNode(ctx, NodeTransition{
			RunID: "run-1", NodeID: "n1", From: protocol.NodePending, To: protocol.NodeReady,
		})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
		if !strings.Contains(err.Error(), "DONE") {
			t.Errorf("the message does not name the actual state: %v", err)
		}
	})
}

func TestSQLAuthorizationReadWrite(t *testing.T) {
	ctx := context.Background()
	s, fake := newSQLStoreForTest(t, SQLOptions{})
	fake.Reply(".run WHERE run_id", runCols(), runRow("run-1"))
	fake.Reply("audit_event", auditCols(), auditRow("run-1", 1))
	leftover := protocol.NewObjectName("public", "orders_idx_ccnew1")
	evidence := leftoverEvidence(leftover, "public.orders_idx")
	fake.Reply("FROM \"partitionctl\".authorization", authCols(), []driver.Value{
		"run-1:auth:1", "run-1", "drop", "leftover", "appdb",
		"public", "orders_idx_ccnew1", "public", "orders_2026_03", true,
		"", []byte(`{"base_index":"public.orders_idx"}`), baseTime,
	})

	rec, err := s.RecordAuthorization(ctx, AuthorizationRecord{
		RunID: "run-1", NodeID: "drop", Mode: protocol.AuthLeftover,
		Object:   leftover,
		Relation: ptrName("public", "orders_2026_03"),
		Evidence: evidence,
	}, nil)
	if err != nil {
		t.Fatalf("RecordAuthorization: %v", err)
	}
	insert, ok := fake.Find(`INTO "partitionctl".authorization`)
	if !ok {
		t.Fatal("no authorization insert")
	}
	if insert.Args[0] != rec.AuthorizationID {
		t.Errorf("id argument = %v", insert.Args[0])
	}
	if insert.Args[3] != string(protocol.AuthLeftover) {
		t.Errorf("mode argument = %v", insert.Args[3])
	}
	if !strings.Contains(insert.Args[11].(string), "base_index") {
		t.Errorf("evidence was not encoded: %v", insert.Args[11])
	}

	got, err := s.ListAuthorizations(ctx, "run-1")
	if err != nil {
		t.Fatalf("ListAuthorizations: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	if got[0].Mode != protocol.AuthLeftover {
		t.Errorf("got %+v", got[0])
	}
	if got[0].Evidence["base_index"] != "public.orders_idx" {
		t.Errorf("evidence = %v", got[0].Evidence)
	}
}

func TestSQLLeaseOperations(t *testing.T) {
	ctx := context.Background()

	t.Run("heartbeat", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		fake.Reply("lease SET heartbeat_at", leaseCols(),
			[]driver.Value{"run-1", "holder-a", baseTime, baseTime.Add(time.Minute), int64(60)})

		got, err := s.Heartbeat(ctx, "run-1", "holder-a")
		if err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
		if got.Holder != "holder-a" {
			t.Errorf("holder = %q", got.Holder)
		}
	})

	t.Run("heartbeat by a displaced holder", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		fake.Reply("lease SET heartbeat_at", leaseCols())
		fake.Reply("FROM \"partitionctl\".lease", leaseCols(),
			[]driver.Value{"run-1", "someone-else", baseTime, baseTime, int64(60)})

		_, err := s.Heartbeat(ctx, "run-1", "holder-a")
		if !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("err = %v, want ErrLeaseLost", err)
		}
		if !strings.Contains(err.Error(), "someone-else") {
			t.Errorf("the message does not name the real holder: %v", err)
		}
	})

	t.Run("heartbeat with no lease at all", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		fake.Reply("lease SET heartbeat_at", leaseCols())
		fake.Reply("FROM \"partitionctl\".lease", leaseCols())

		if _, err := s.Heartbeat(ctx, "run-1", "holder-a"); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("err = %v, want ErrLeaseLost", err)
		}
	})

	t.Run("release", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		fake.Reply("FROM \"partitionctl\".lease", leaseCols(),
			[]driver.Value{"run-1", "holder-a", baseTime, baseTime, int64(60)})
		fake.Reply("audit_event", auditCols(), auditRow("run-1", 3))

		if err := s.ReleaseLease(ctx, "run-1", "holder-a"); err != nil {
			t.Fatalf("ReleaseLease: %v", err)
		}
		if _, ok := fake.Find("DELETE FROM"); !ok {
			t.Error("no delete was issued")
		}
	})

	t.Run("release by the wrong holder", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		fake.Reply("FROM \"partitionctl\".lease", leaseCols(),
			[]driver.Value{"run-1", "holder-a", baseTime, baseTime, int64(60)})

		if err := s.ReleaseLease(ctx, "run-1", "intruder"); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("err = %v, want ErrLeaseLost", err)
		}
		if _, ok := fake.Find("DELETE FROM"); ok {
			t.Error("a delete was issued for the wrong holder")
		}
	})

	t.Run("release with no lease is a no-op", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		fake.Reply("FROM \"partitionctl\".lease", leaseCols())
		if err := s.ReleaseLease(ctx, "run-1", "holder-a"); err != nil {
			t.Fatalf("ReleaseLease: %v", err)
		}
		if _, ok := fake.Find("DELETE FROM"); ok {
			t.Error("a delete was issued when there was no lease")
		}
	})

	t.Run("get missing", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		fake.Reply("FROM \"partitionctl\".lease", leaseCols())
		if _, err := s.GetLease(ctx, "run-1"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("a sub-second ttl is refused before any SQL", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		if _, err := s.AcquireLease(ctx, "run-1", "h", 100*time.Millisecond); err == nil {
			t.Fatal("want an error")
		}
		if n := len(fake.Calls()); n != 0 {
			t.Errorf("issued %d statements for an invalid ttl", n)
		}
	})
}

// FR-CLI-10 and FR-CLI-11 through the SQL store.
func TestSQLRequestCancel(t *testing.T) {
	ctx := context.Background()
	clock := newClock()

	t.Run("a live run only gets the flag", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{Clock: clock.Clock()})
		fake.Reply(".run WHERE run_id", runCols(), runRow("run-1"))
		fake.Reply("FROM \"partitionctl\".lease", leaseCols(),
			[]driver.Value{"run-1", "holder", clock.Now(), clock.Now(), int64(60)})
		cancelled := runRow("run-1")
		cancelled[13] = true
		fake.Reply("run SET cancel_requested", runCols(), cancelled)
		fake.Reply("audit_event", auditCols(), auditRow("run-1", 4))

		got, err := s.RequestCancel(ctx, "run-1", "operator", "please stop")
		if err != nil {
			t.Fatalf("RequestCancel: %v", err)
		}
		if got.Status != RunRunning {
			t.Errorf("status = %s, want the run to stay RUNNING and resumable (AC-24)", got.Status)
		}
		if _, ok := fake.Find("run SET status"); ok {
			t.Error("a live run was terminally cancelled (FR-CLI-10)")
		}
		c, _ := fake.Find("run SET cancel_requested")
		if c.Args[2] != "operator" || c.Args[3] != "please stop" {
			t.Errorf("actor/note arguments = %v", c.Args)
		}
	})

	t.Run("an abandoned run is cancelled terminally", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{Clock: clock.Clock()})
		fake.Reply(".run WHERE run_id", runCols(), runRow("run-1"))
		fake.Reply("FROM \"partitionctl\".lease", leaseCols(),
			[]driver.Value{"run-1", "holder", baseTime.Add(-time.Hour), baseTime.Add(-time.Hour), int64(30)})
		cancelled := runRow("run-1")
		cancelled[13] = true
		fake.Reply("run SET cancel_requested", runCols(), cancelled)
		terminal := runRow("run-1")
		terminal[10] = string(RunCancelled)
		fake.Reply("run SET status", runCols(), terminal)
		fake.Reply("audit_event", auditCols(), auditRow("run-1", 5))

		got, err := s.RequestCancel(ctx, "run-1", "operator", "")
		if err != nil {
			t.Fatalf("RequestCancel: %v", err)
		}
		if got.Status != RunCancelled {
			t.Errorf("status = %s, want %s (FR-CLI-11)", got.Status, RunCancelled)
		}
	})

	t.Run("a terminal run is left alone", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{Clock: clock.Clock()})
		done := runRow("run-1")
		done[10] = string(RunCompleted)
		fake.Reply(".run WHERE run_id", runCols(), done)

		got, err := s.RequestCancel(ctx, "run-1", "operator", "")
		if err != nil {
			t.Fatalf("RequestCancel: %v", err)
		}
		if got.Status != RunCompleted {
			t.Errorf("status = %s", got.Status)
		}
		if _, ok := fake.Find("run SET cancel_requested"); ok {
			t.Error("a completed run was flagged for cancellation")
		}
	})

	t.Run("the flag is observable", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{Clock: clock.Clock()})
		flagged := runRow("run-1")
		flagged[13] = true
		fake.Reply(".run WHERE run_id", runCols(), flagged)

		got, err := s.CancellationRequested(ctx, "run-1")
		if err != nil {
			t.Fatalf("CancellationRequested: %v", err)
		}
		if !got {
			t.Error("the flag was not observed")
		}
	})
}

func TestSQLListAudit(t *testing.T) {
	ctx := context.Background()
	s, fake := newSQLStoreForTest(t, SQLOptions{})
	fake.Reply("FROM \"partitionctl\".audit_event", auditCols(),
		[]driver.Value{"evt-2", "run-1", int64(2), "n1", "node.transition", []byte(`{"to":"READY"}`), baseTime},
		[]driver.Value{"evt-3", "run-1", int64(3), "", "run.status_changed", []byte(`{}`), baseTime})

	got, err := s.ListAudit(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[0].Detail["to"] != "READY" {
		t.Errorf("detail = %v", got[0].Detail)
	}
	if got[1].Detail != nil {
		t.Errorf("an empty detail object should decode to nil, got %v", got[1].Detail)
	}
	c, _ := fake.Find("FROM \"partitionctl\".audit_event")
	if c.Args[1] != int64(1) {
		t.Errorf("cursor argument = %v, want 1", c.Args[1])
	}
}

func TestSQLAppendAuditRejectsInvalidEvents(t *testing.T) {
	ctx := context.Background()
	s, fake := newSQLStoreForTest(t, SQLOptions{})
	if _, err := s.AppendAudit(ctx, AuditEvent{Type: EventRunOpened}); err == nil {
		t.Fatal("want an error for an event with no run id")
	}
	if n := len(fake.Calls()); n != 0 {
		t.Errorf("issued %d statements for an invalid event", n)
	}
}

func TestSQLAppendAuditRequiresAReturnedRow(t *testing.T) {
	ctx := context.Background()
	s, fake := newSQLStoreForTest(t, SQLOptions{})
	fake.Reply("audit_event", auditCols()) // insert returns nothing
	if _, err := s.AppendAudit(ctx, AuditEvent{RunID: "run-1", Type: EventRunOpened}); !errors.Is(err, ErrStoreIO) {
		t.Fatalf("err = %v, want ErrStoreIO", err)
	}
}

func TestSQLStoreAccessors(t *testing.T) {
	s, _ := newSQLStoreForTest(t, SQLOptions{Schema: "pctl"})
	if s.Schema() != "pctl" {
		t.Errorf("Schema() = %q", s.Schema())
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Close does not close the caller's pool: the store never owned it.
	if _, err := s.FindRuns(context.Background(), RunQuery{}); err != nil {
		t.Errorf("the store closed the caller's *sql.DB: %v", err)
	}
}

func TestNonNilMap(t *testing.T) {
	if got := nonNilMap(nil); got == nil || len(got) != 0 {
		t.Errorf("nonNilMap(nil) = %v", got)
	}
	in := map[string]string{"a": "1"}
	if got := nonNilMap(in); got["a"] != "1" {
		t.Errorf("nonNilMap did not pass through: %v", got)
	}
}

func TestLeaseIsZero(t *testing.T) {
	if !(Lease{}).IsZero() {
		t.Error("the zero Lease is not IsZero")
	}
	if (Lease{RunID: "r"}).IsZero() {
		t.Error("a lease with a run id is IsZero")
	}
	l := Lease{HeartbeatAt: tsAt(baseTime), TTLSeconds: 30}
	if got := l.TTL(); got != 30*time.Second {
		t.Errorf("TTL = %s", got)
	}
	if got := l.ExpiresAt(); !got.Equal(baseTime.Add(30 * time.Second)) {
		t.Errorf("ExpiresAt = %s", got)
	}
}
