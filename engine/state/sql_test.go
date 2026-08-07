package state

import (
	"context"
	"database/sql/driver"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

func newSQLStoreForTest(t *testing.T, opts SQLOptions) (*SQLStore, *fakeDB) {
	t.Helper()
	db, fake := openFakeDB(t)
	if opts.Clock == nil {
		c := newClock()
		opts.Clock = c.Clock()
	}
	if opts.Holder == "" {
		opts.Holder = "test/1"
	}
	// The DDL is exercised by its own tests; skipping it here keeps the
	// recorded call log about the statement under test.
	opts.SkipBootstrap = true
	s, err := NewSQLStore(db, opts)
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}
	return s, fake
}

var placeholderRE = regexp.MustCompile(`\$(\d+)`)

// assertPlaceholdersDense checks that a statement uses $1..$n with no gap and
// no reuse beyond what is intentional, and that the highest placeholder matches
// the argument count.
func assertPlaceholdersDense(t *testing.T, name, stmt string, args int) {
	t.Helper()
	seen := map[int]bool{}
	max := 0
	for _, m := range placeholderRE.FindAllStringSubmatch(stmt, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("%s: bad placeholder %q", name, m[0])
		}
		seen[n] = true
		if n > max {
			max = n
		}
	}
	if max != args {
		t.Errorf("%s: highest placeholder is $%d but %d arguments are bound:\n%s", name, max, args, stmt)
	}
	for i := 1; i <= max; i++ {
		if !seen[i] {
			t.Errorf("%s: placeholder $%d is never used:\n%s", name, i, stmt)
		}
	}
}

func TestSQLStatementsAreWellFormed(t *testing.T) {
	s, _ := newSQLStoreForTest(t, SQLOptions{Schema: "pctl"})

	tests := []struct {
		name string
		args int
	}{
		{name: "insert_run", args: 21},
		{name: "select_run", args: 1},
		{name: "update_run_status", args: 7},
		{name: "update_run_cancel", args: 4},
		{name: "select_running_for", args: 3},
		{name: "insert_node", args: 9},
		{name: "select_node", args: 2},
		{name: "select_nodes", args: 1},
		{name: "transition_node", args: 9},
		{name: "insert_provenance", args: 13},
		{name: "insert_authorization", args: 15},
		{name: "select_authorizations", args: 1},
		{name: "upsert_lease", args: 4},
		{name: "heartbeat", args: 3},
		{name: "select_lease", args: 1},
		{name: "delete_lease", args: 2},
		{name: "insert_audit", args: 5},
		{name: "select_audit", args: 2},
		{name: "try_advisory_lock", args: 2},
		{name: "advisory_unlock", args: 2},
	}
	stmts := s.Statements()
	if len(stmts) != len(tests) {
		t.Fatalf("the store exposes %d statements but the table covers %d", len(stmts), len(tests))
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmt, ok := stmts[tc.name]
			if !ok {
				t.Fatalf("no statement named %q", tc.name)
			}
			assertPlaceholdersDense(t, tc.name, stmt, tc.args)
			// Every table reference must be schema-qualified and quoted.
			if strings.Contains(stmt, "FROM ") || strings.Contains(stmt, "INTO ") || strings.HasPrefix(stmt, "UPDATE ") {
				if strings.Contains(stmt, "pctl.") && !strings.Contains(stmt, `"pctl".`) {
					t.Errorf("%s references the schema unquoted:\n%s", tc.name, stmt)
				}
			}
		})
	}
}

// INV-6 again, from the SQL side: no statement the store can issue may assign
// plan_digest on an existing row.
func TestNoStatementRebindsThePlanDigest(t *testing.T) {
	s, _ := newSQLStoreForTest(t, SQLOptions{})
	for name, stmt := range s.Statements() {
		if !strings.HasPrefix(strings.ToUpper(stmt), "UPDATE ") {
			continue
		}
		if strings.Contains(stmt, "plan_digest =") {
			t.Errorf("statement %q assigns plan_digest in an UPDATE (INV-6):\n%s", name, stmt)
		}
	}
}

func TestSQLRunQueryBuilder(t *testing.T) {
	text, err := newSQLText("pctl")
	if err != nil {
		t.Fatalf("newSQLText: %v", err)
	}
	table := protocol.NewObjectName("public", "orders")
	index := protocol.NewObjectName("public", "orders_idx")
	since := baseTime

	tests := []struct {
		name     string
		q        RunQuery
		wantSQL  []string
		wantArgs []any
	}{
		{
			name:     "no filters",
			q:        RunQuery{},
			wantSQL:  []string{`FROM "pctl".run`, "ORDER BY started_at, run_id"},
			wantArgs: nil,
		},
		{
			name:     "by digest",
			q:        RunQuery{PlanDigest: "sha256:abc"},
			wantSQL:  []string{"WHERE plan_digest = $1"},
			wantArgs: []any{"sha256:abc"},
		},
		{
			name:     "by table",
			q:        RunQuery{Database: "appdb", Table: &table},
			wantSQL:  []string{"database_name = $1", "table_schema = $2", "table_name = $3"},
			wantArgs: []any{"appdb", "public", "orders"},
		},
		{
			name:     "by index requires has_index",
			q:        RunQuery{Index: &index},
			wantSQL:  []string{"has_index", "index_schema = $1", "index_name = $2"},
			wantArgs: []any{"public", "orders_idx"},
		},
		{
			name:     "status set",
			q:        RunQuery{Statuses: []RunStatus{RunRunning, RunOrphaned}},
			wantSQL:  []string{"status IN ($1, $2)"},
			wantArgs: []any{"RUNNING", "ORPHANED"},
		},
		{
			name:     "finished since",
			q:        RunQuery{FinishedSince: since},
			wantSQL:  []string{"finished_at >= $1"},
			wantArgs: []any{since.UTC()},
		},
		{
			name:     "limit",
			q:        RunQuery{Limit: 5},
			wantSQL:  []string{"LIMIT 5"},
			wantArgs: nil,
		},
		{
			name:     "operation",
			q:        RunQuery{Operation: protocol.OpReindexIndex},
			wantSQL:  []string{"operation = $1"},
			wantArgs: []any{"reindex-index"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmt, args := text.runQuery(tc.q)
			for _, want := range tc.wantSQL {
				if !strings.Contains(stmt, want) {
					t.Errorf("statement does not contain %q:\n%s", want, stmt)
				}
			}
			if len(args) != len(tc.wantArgs) {
				t.Fatalf("got %d args %v, want %d %v", len(args), args, len(tc.wantArgs), tc.wantArgs)
			}
			for i := range args {
				if args[i] != tc.wantArgs[i] {
					t.Errorf("arg %d = %v, want %v", i, args[i], tc.wantArgs[i])
				}
			}
			assertPlaceholdersDense(t, tc.name, stmt, len(args))
		})
	}
}

func TestSQLProvenanceQueryBuilder(t *testing.T) {
	text, err := newSQLText("pctl")
	if err != nil {
		t.Fatalf("newSQLText: %v", err)
	}
	rel := protocol.NewObjectName("public", "orders_2026_03")

	tests := []struct {
		name     string
		q        ProvenanceQuery
		wantSQL  []string
		wantArgs []any
	}{
		{
			name:     "object only",
			q:        ProvenanceQuery{Object: protocol.NewObjectName("public", "idx")},
			wantSQL:  []string{"object_schema = $1", "object_name = $2"},
			wantArgs: []any{"public", "idx"},
		},
		{
			name: "narrowed by relation",
			q: ProvenanceQuery{
				Object: protocol.NewObjectName("public", "idx"), Database: "appdb", Relation: &rel,
			},
			wantSQL:  []string{"has_relation", "relation_schema = $4", "relation_name = $5"},
			wantArgs: []any{"public", "idx", "appdb", "public", "orders_2026_03"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stmt, args := text.provenanceQuery(tc.q)
			for _, want := range tc.wantSQL {
				if !strings.Contains(stmt, want) {
					t.Errorf("statement does not contain %q:\n%s", want, stmt)
				}
			}
			if len(args) != len(tc.wantArgs) {
				t.Fatalf("got %d args %v, want %d", len(args), args, len(tc.wantArgs))
			}
			assertPlaceholdersDense(t, tc.name, stmt, len(args))
		})
	}
}

// CreateRun must be one transaction: the run, every node record and the audit
// event land together or not at all.
func TestSQLCreateRunIsOneTransaction(t *testing.T) {
	ctx := context.Background()
	s, fake := newSQLStoreForTest(t, SQLOptions{})
	plan := testPlan(t, "a", "b", "c")
	fake.Reply("audit_event", strings.Split(auditColumns, ", "),
		[]driver.Value{"evt-1", "run-sql", int64(1), "", "run.opened", []byte(`{}`), baseTime})

	run, err := s.CreateRun(ctx, NewRun{Plan: plan, RunID: "run-sql", Actor: "alice"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if run.PlanDigest != plan.Digest {
		t.Errorf("plan digest = %q, want %q", run.PlanDigest, plan.Digest)
	}

	calls := fake.Calls()
	if len(calls) < 2 || calls[0].Kind != "begin" {
		t.Fatalf("the first call was %+v, want BEGIN", calls[0])
	}
	if calls[len(calls)-1].Kind != "commit" {
		t.Fatalf("the last call was %+v, want COMMIT", calls[len(calls)-1])
	}
	if n := fake.CountMatching("INTO \"partitionctl\".node_state"); n != 3 {
		t.Errorf("inserted %d node records, want 3", n)
	}
	if n := fake.CountMatching("INTO \"partitionctl\".run"); n != 1 {
		t.Errorf("inserted %d run records, want 1", n)
	}
	if n := fake.CountMatching("audit_event"); n != 1 {
		t.Errorf("appended %d audit events, want 1", n)
	}

	insert, ok := fake.Find(`INTO "partitionctl".run`)
	if !ok {
		t.Fatal("no run insert recorded")
	}
	if insert.Args[0] != "run-sql" {
		t.Errorf("run id argument = %v", insert.Args[0])
	}
	if insert.Args[2] != plan.Digest {
		t.Errorf("digest argument = %v, want %v", insert.Args[2], plan.Digest)
	}
	if insert.Args[10] != string(RunRunning) {
		t.Errorf("status argument = %v, want %s", insert.Args[10], RunRunning)
	}
}

// The advisory lock is a session-level PostgreSQL lock keyed on the target
// table (FR-LOCK-1), and refusing it names the holding run (FR-LOCK-2, AC-10).
func TestSQLAdvisoryLock(t *testing.T) {
	ctx := context.Background()
	key := testKey()
	wantClass, wantObj := key.AdvisoryKeys()

	t.Run("acquired", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		fake.Reply("pg_try_advisory_lock", []string{"pg_try_advisory_lock"}, []driver.Value{true})
		fake.Reply("pg_advisory_unlock", []string{"pg_advisory_unlock"}, []driver.Value{true})

		lock, err := s.TryLock(ctx, key)
		if err != nil {
			t.Fatalf("TryLock: %v", err)
		}
		got, ok := fake.Find("pg_try_advisory_lock")
		if !ok {
			t.Fatal("no advisory lock statement issued")
		}
		if got.Args[0] != int64(wantClass) || got.Args[1] != int64(wantObj) {
			t.Errorf("lock keys = %v, %v; want %d, %d", got.Args[0], got.Args[1], wantClass, wantObj)
		}
		if lock.Key() != key {
			t.Errorf("Key() = %v, want %v", lock.Key(), key)
		}
		if held, err := lock.Held(ctx); err != nil || !held {
			t.Errorf("Held = %v, %v; want true", held, err)
		}
		if err := lock.Refresh(ctx); err != nil {
			t.Errorf("Refresh: %v", err)
		}
		if err := lock.Unlock(ctx); err != nil {
			t.Fatalf("Unlock: %v", err)
		}
		if _, ok := fake.Find("pg_advisory_unlock"); !ok {
			t.Error("Unlock issued no pg_advisory_unlock")
		}
		if held, _ := lock.Held(ctx); held {
			t.Error("Held is still true after Unlock")
		}
		if err := lock.Unlock(ctx); err != nil {
			t.Errorf("second Unlock: %v", err)
		}
		if err := lock.Refresh(ctx); !errors.Is(err, protocol.ErrLockHeld) {
			t.Errorf("Refresh after Unlock = %v, want ErrLockHeld", err)
		}
	})

	t.Run("held by another run", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		fake.Reply("pg_try_advisory_lock", []string{"pg_try_advisory_lock"}, []driver.Value{false})
		fake.Reply("status = 'RUNNING'", strings.Split(runColumns, ", "), runRow("run-holder"))

		_, err := s.TryLock(ctx, key)
		if !errors.Is(err, protocol.ErrLockHeld) {
			t.Fatalf("err = %v, want ErrLockHeld", err)
		}
		if !strings.Contains(err.Error(), "run-holder") {
			t.Errorf("the message does not name the holding run (FR-LOCK-2): %v", err)
		}
		if protocol.ExitCodeFor(err) != protocol.ExitLockHeld {
			t.Errorf("exit code = %d, want %d", protocol.ExitCodeFor(err), protocol.ExitLockHeld)
		}
	})

	t.Run("held with no run recorded", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		fake.Reply("pg_try_advisory_lock", []string{"pg_try_advisory_lock"}, []driver.Value{false})

		_, err := s.TryLock(ctx, key)
		if !errors.Is(err, protocol.ErrLockHeld) {
			t.Fatalf("err = %v, want ErrLockHeld", err)
		}
		if !strings.Contains(err.Error(), "no RUNNING run") {
			t.Errorf("the message does not say the holder is unidentified: %v", err)
		}
	})

	t.Run("an invalid key is refused before any SQL", func(t *testing.T) {
		s, fake := newSQLStoreForTest(t, SQLOptions{})
		if _, err := s.TryLock(ctx, LockKey{Database: "appdb"}); err == nil {
			t.Fatal("want an error")
		}
		if n := len(fake.Calls()); n != 0 {
			t.Errorf("issued %d statements for an invalid key", n)
		}
	})
}

func TestSQLTransitionNodeArguments(t *testing.T) {
	ctx := context.Background()
	clock := newClock()
	s, fake := newSQLStoreForTest(t, SQLOptions{Clock: clock.Clock()})

	fake.Reply("node_state SET", strings.Split(nodeColumns, ", "),
		[]driver.Value{"run-1", "n1", "wait", "RUNNING", int64(1), "", "", baseTime, baseTime})
	fake.Reply("audit_event", strings.Split(auditColumns, ", "),
		[]driver.Value{"evt-1", "run-1", int64(1), "n1", "node.transition", []byte(`{}`), baseTime})

	if _, err := s.TransitionNode(ctx, NodeTransition{
		RunID: "run-1", NodeID: "n1",
		From: protocol.NodeReady, To: protocol.NodeRunning, IncrementAttempt: true,
	}); err != nil {
		t.Fatalf("TransitionNode: %v", err)
	}

	got, ok := fake.Find("node_state SET")
	if !ok {
		t.Fatal("no transition statement issued")
	}
	want := []driver.Value{"run-1", "n1", "RUNNING", int64(1), "", "", clock.Now(), clock.Now(), "READY"}
	if len(got.Args) != len(want) {
		t.Fatalf("got %d args, want %d", len(got.Args), len(want))
	}
	for i := range want {
		if got.Args[i] != want[i] {
			t.Errorf("arg %d = %v, want %v", i, got.Args[i], want[i])
		}
	}
}

// An edge diagram D7 does not have must never reach the database.
func TestSQLTransitionNodeRejectsInvalidEdgesBeforeSQL(t *testing.T) {
	ctx := context.Background()
	s, fake := newSQLStoreForTest(t, SQLOptions{})
	if _, err := s.TransitionNode(ctx, NodeTransition{
		RunID: "run-1", NodeID: "n1", From: protocol.NodePending, To: protocol.NodeDone,
	}); !errors.Is(err, protocol.ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("issued %d statements for an illegal transition", n)
	}
}

func TestSQLSetRunStatusRejectsInvalidEdgesBeforeSQL(t *testing.T) {
	ctx := context.Background()
	s, fake := newSQLStoreForTest(t, SQLOptions{})
	if _, err := s.SetRunStatus(ctx, RunStatusUpdate{
		RunID: "run-1", From: RunCompleted, To: RunRunning,
	}); !errors.Is(err, protocol.ErrInvalidTransition) {
		t.Fatalf("err = %v, want ErrInvalidTransition", err)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("issued %d statements for an illegal transition", n)
	}
}

// INV-1 from the SQL side: the record's transaction commits before the guarded
// DDL is invoked.
func TestSQLWriteProvenanceCommitsBeforeTheDDL(t *testing.T) {
	ctx := context.Background()
	s, fake := newSQLStoreForTest(t, SQLOptions{})
	fake.Reply("run WHERE run_id", strings.Split(runColumns, ", "), runRow("run-1"))
	fake.Reply("audit_event", strings.Split(auditColumns, ", "),
		[]driver.Value{"evt-1", "run-1", int64(1), "", "provenance.recorded", []byte(`{}`), baseTime})

	var callsAtDDL []call
	if _, err := s.WriteProvenance(ctx, Provenance{
		RunID: "run-1", NodeID: "cic",
		Object: protocol.NewObjectName("public", "idx"), ObjectKind: ObjectIndex,
	}, func(ctx context.Context) error {
		callsAtDDL = fake.Calls()
		return nil
	}); err != nil {
		t.Fatalf("WriteProvenance: %v", err)
	}

	sawInsert, sawCommit := false, false
	for _, c := range callsAtDDL {
		if strings.Contains(c.Query, "provenance") && c.Kind == "exec" {
			sawInsert = true
		}
		if c.Kind == "commit" {
			sawCommit = true
		}
	}
	if !sawInsert {
		t.Error("the DDL ran before the provenance insert (INV-1)")
	}
	if !sawCommit {
		t.Error("the DDL ran before the provenance transaction committed (INV-1)")
	}
}

// If the insert fails, the transaction rolls back and the DDL never runs.
func TestSQLWriteProvenanceRollsBackAndSkipsTheDDL(t *testing.T) {
	ctx := context.Background()
	s, fake := newSQLStoreForTest(t, SQLOptions{})
	fake.Reply("run WHERE run_id", strings.Split(runColumns, ", "), runRow("run-1"))
	fake.Fail("INTO \"partitionctl\".provenance", errors.New("disk full"))

	called := false
	_, err := s.WriteProvenance(ctx, Provenance{
		RunID: "run-1", Object: protocol.NewObjectName("public", "idx"), ObjectKind: ObjectIndex,
	}, func(ctx context.Context) error { called = true; return nil })

	if !errors.Is(err, ErrProvenanceNotRecorded) {
		t.Fatalf("err = %v, want ErrProvenanceNotRecorded", err)
	}
	if called {
		t.Fatal("the guarded DDL ran after the insert failed (INV-1)")
	}
	if _, ok := fake.Find("ROLLBACK"); !ok {
		t.Error("the transaction was not rolled back")
	}
}

func TestSQLAcquireLeaseArguments(t *testing.T) {
	ctx := context.Background()
	clock := newClock()
	s, fake := newSQLStoreForTest(t, SQLOptions{Clock: clock.Clock()})
	fake.Reply("INTO \"partitionctl\".lease", strings.Split(leaseColumns, ", "),
		[]driver.Value{"run-1", "holder-a", baseTime, baseTime, int64(45)})
	fake.Reply("audit_event", strings.Split(auditColumns, ", "),
		[]driver.Value{"evt-1", "run-1", int64(1), "", "lease.acquired", []byte(`{}`), baseTime})

	lease, err := s.AcquireLease(ctx, "run-1", "holder-a", 45*time.Second)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if lease.TTLSeconds != 45 || lease.Holder != "holder-a" {
		t.Fatalf("lease = %+v", lease)
	}
	got, _ := fake.Find("INTO \"partitionctl\".lease")
	want := []driver.Value{"run-1", "holder-a", clock.Now(), int64(45)}
	for i := range want {
		if got.Args[i] != want[i] {
			t.Errorf("arg %d = %v, want %v", i, got.Args[i], want[i])
		}
	}
}

// An upsert that matches no row means the lease belongs to a live holder.
func TestSQLAcquireLeaseLost(t *testing.T) {
	ctx := context.Background()
	s, fake := newSQLStoreForTest(t, SQLOptions{})
	fake.Reply("INTO \"partitionctl\".lease", strings.Split(leaseColumns, ", "))
	fake.Reply("FROM \"partitionctl\".lease", strings.Split(leaseColumns, ", "),
		[]driver.Value{"run-1", "someone-else", baseTime, baseTime, int64(60)})

	if _, err := s.AcquireLease(ctx, "run-1", "me", time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("err = %v, want ErrLeaseLost", err)
	}
}

func TestSQLGetRunNotFound(t *testing.T) {
	ctx := context.Background()
	s, fake := newSQLStoreForTest(t, SQLOptions{})
	fake.Reply("run WHERE run_id", strings.Split(runColumns, ", "))
	if _, err := s.GetRun(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSQLScanRunRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, fake := newSQLStoreForTest(t, SQLOptions{})
	row := runRow("run-scan")
	row[7] = "public"  // index_schema
	row[8] = "idx"     // index_name
	row[9] = true      // has_index
	row[13] = true     // cancel_requested
	row[14] = baseTime // cancel_requested_at
	row[20] = baseTime // finished_at
	fake.Reply("run WHERE run_id", strings.Split(runColumns, ", "), row)

	got, err := s.GetRun(ctx, "run-scan")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Target.Index == nil || got.Target.Index.Name != "idx" {
		t.Errorf("index = %+v, want public.idx", got.Target.Index)
	}
	if !got.CancelRequested || got.CancelRequestedAt == nil {
		t.Error("cancellation was not scanned")
	}
	if got.FinishedAt == nil {
		t.Error("finished_at was not scanned")
	}
	if got.Status != RunRunning {
		t.Errorf("status = %s", got.Status)
	}
}

// runRow builds a driver row matching runColumns.
func runRow(id string) []driver.Value {
	return []driver.Value{
		id,                 // run_id
		"plan-1",           // plan_id
		"sha256:deadbeef",  // plan_digest
		"create-index",     // operation
		"appdb",            // database_name
		"public",           // table_schema
		"orders",           // table_name
		"",                 // index_schema
		"",                 // index_name
		false,              // has_index
		string(RunRunning), // status
		"alice",            // actor
		int64(3),           // node_count
		false,              // cancel_requested
		nil,                // cancel_requested_at
		"",                 // cancel_actor
		"",                 // cancel_note
		"",                 // last_error
		baseTime,           // started_at
		baseTime,           // updated_at
		nil,                // finished_at
	}
}
