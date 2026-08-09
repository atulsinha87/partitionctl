package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/atulsinha87/partitionctl/engine/executor"
	"github.com/atulsinha87/partitionctl/engine/planner"
	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// This file covers the seams between a plan, the command that runs it and the
// operation it belongs to. Every test here corresponds to a defect that the
// unit suites could not see, because each unit suite was correct in isolation:
// the planners were tested with schema-qualified names, the verifier gates were
// tested by calling them directly, and the renderer was tested against
// create-index. The bugs lived in which one the CLI picked.

// buildIndexFamily populates the planner's fake catalog with a parent
// partitioned index and one attached, valid child per leaf, so that the two
// operations that act on an existing index have something to act on.
//
// The harness's applyEffects only maintains the *verifier's* catalog, so
// planning drop-index or reindex-index after a create-index run would otherwise
// see no index at all.
func buildIndexFamily(h *harness, leaves ...string) {
	const parentOID = 90000
	parent := obj("public", testIndex)
	h.Cat.AddIndex(planner.Index{
		OID:      parentOID,
		Name:     parent,
		Kind:     planner.RelKindPartitionedIndex,
		OwnerOID: ownerOID,
		TableOID: rootOID,
		Table:    obj("public", "orders"),
		IsValid:  true, IsReady: true, IsLive: true,
	})
	for i, leaf := range leaves {
		child := protocol.ChildIndexName(testIndex, leaf)
		h.Cat.AddIndex(planner.Index{
			OID:      uint32(90001 + i),
			Name:     obj("public", child),
			Kind:     planner.RelKindIndex,
			OwnerOID: ownerOID,
			RelPages: 16,
			TableOID: leafBase + uint32(i),
			Table:    obj("public", leaf),
			IsValid:  true, IsReady: true, IsLive: true,
			ParentIndexOID: parentOID,
		})
	}
}

// TestPlanSealsTheTargetIndexSchemaQualified pins the fix for the defect that
// made drop-index unusable through the CLI.
//
// A specification names the index unqualified — that is the documented form,
// and adapters/cli/spec.go's own example uses it. The plan used to seal that raw
// value into Target.Index while sealing Target.Table from the discovered root.
// FR-AUTH-4's `explicit` mode then compared the unqualified target against the
// catalog-resolved, always-qualified object the drop node names, found them
// unequal, and halted.
//
// Live symptom before the fix, PostgreSQL 17.10, 12 partitions: the plan
// validated and printed normally, `execute` ran the assert node, then exited 13
// with "the plan's target does not name public.orders_created_at_idx" — about a
// plan whose target was orders_created_at_idx.
func TestPlanSealsTheTargetIndexSchemaQualified(t *testing.T) {
	for _, op := range []string{"create-index", "reindex-index", "drop-index"} {
		t.Run(op, func(t *testing.T) {
			leaves := []string{"orders_2026_01", "orders_2026_02"}
			h := newHarness(t, leaves...)
			if op != "create-index" {
				buildIndexFamily(h, leaves...)
			}
			spec := SpecFile{Operation: op, Index: testIndex}
			if op == "drop-index" {
				spec.ConfirmExclusiveLock = true
			}
			plan := h.LoadPlan(h.MustPlan(spec))

			if plan.Target.Index == nil {
				t.Fatalf("%s: plan has no target index", op)
			}
			if plan.Target.Index.Schema == "" {
				t.Fatalf("%s: target index %q sealed without a schema; "+
					"explicit authorization compares this against the catalog-resolved "+
					"name and will never match", op, plan.Target.Index)
			}
			if plan.Target.Index.Schema != plan.Target.Table.Schema {
				t.Fatalf("%s: target index schema %q does not match the table's %q",
					op, plan.Target.Index.Schema, plan.Target.Table.Schema)
			}
		})
	}
}

// TestDropIndexExecutesWhenTheSpecNamesTheIndexUnqualified is the end-to-end
// half of the test above: not merely that the artifact records a schema, but
// that the destructive node's authorization is actually satisfied by it.
//
// Without the fix this exits 13 at index.drop_partitioned.
func TestDropIndexExecutesWhenTheSpecNamesTheIndexUnqualified(t *testing.T) {
	leaves := []string{"orders_2026_01", "orders_2026_02"}
	h := newHarness(t, leaves...)
	buildIndexFamily(h, leaves...)

	planPath := h.MustPlan(SpecFile{
		Operation:            "drop-index",
		Index:                testIndex, // unqualified, as documented
		ConfirmExclusiveLock: true,
	})
	if code := h.Run("execute", planPath); code != 0 {
		t.Fatalf("execute exited %d, want 0: %s", code, h.Out())
	}
}

// TestEndStateGateIsChosenByOperationNotByTheFlag pins that `verify --end-state`
// asks the question the operation is about.
//
// It used to run VerifyPartitionedIndex for all three, which asserts the parent
// is valid and every leaf attached. On a successful drop that is false by
// design, so a drop that worked perfectly reported FAIL; on a reindex it
// silently skipped the leftover gate, so a rebuild that left a _ccold on every
// partition reported PASS. A live reindex end-state check ran 26 assertions and
// none of them mentioned leftovers.
func TestEndStateGateIsChosenByOperationNotByTheFlag(t *testing.T) {
	want := map[protocol.Operation]string{
		protocol.OpCreateIndex:  "VerifyPartitionedIndex",
		protocol.OpReindexIndex: "VerifyReindexedIndex",
		protocol.OpDropIndex:    "VerifyIndexAbsentForPlan",
	}
	for op := range want {
		entry, ok := operationRegistry[op]
		if !ok {
			t.Fatalf("%s is not in the registry", op)
		}
		if entry.EndState == nil {
			t.Fatalf("%s has no end-state gate, so `verify --end-state` would "+
				"fall back to another operation's question", op)
		}
	}

	// The behavioural half: a completed drop must PASS its own end-state gate.
	leaves := []string{"orders_2026_01", "orders_2026_02"}
	h := newHarness(t, leaves...)
	buildIndexFamily(h, leaves...)
	planPath := h.MustPlan(SpecFile{
		Operation:            "drop-index",
		Index:                testIndex,
		ConfirmExclusiveLock: true,
	})
	if code := h.Run("execute", planPath); code != 0 {
		t.Fatalf("execute exited %d: %s", code, h.Out())
	}
	// The verifier catalog never held this family, so the index is absent,
	// which is exactly the post-drop state the gate must accept.
	if code := h.Run("verify", "--end-state", planPath); code != 0 {
		t.Fatalf("verify --end-state on a successful drop exited %d, want 0: %s", code, h.Out())
	}
}

// TestRenderRollbackRefusesForOperationsThatCreateNothing pins that the unwind
// runbook is operation-aware.
//
// The single create-shaped body emitted `DROP INDEX <target>;` as the "unwind"
// of a reindex — destroying a pre-existing production index the run never
// created — and as the "unwind" of dropping that same index, then advised
// confirming the result with `verify --expect-absent`.
func TestRenderRollbackRefusesForOperationsThatCreateNothing(t *testing.T) {
	leaves := []string{"orders_2026_01", "orders_2026_02"}
	for _, tc := range []struct {
		op   string
		spec SpecFile
	}{
		{"reindex-index", SpecFile{Operation: "reindex-index", Index: testIndex}},
		{"drop-index", SpecFile{Operation: "drop-index", Index: testIndex, ConfirmExclusiveLock: true}},
	} {
		t.Run(tc.op, func(t *testing.T) {
			h := newHarness(t, leaves...)
			buildIndexFamily(h, leaves...)
			planPath := h.MustPlan(tc.spec)

			// --confirm-exclusive-lock is the flag that would have emitted the
			// bogus DROP INDEX live rather than commented out, so refuse under
			// it too.
			code := h.RunBare("render", "--rollback", "--confirm-exclusive-lock", planPath)
			if code == 0 {
				t.Fatalf("render --rollback on a %s plan exited 0; it must refuse", tc.op)
			}
			out := h.Out()
			if strings.Contains(out, "DROP INDEX") {
				t.Fatalf("render --rollback on a %s plan emitted a DROP INDEX:\n%s", tc.op, out)
			}
			if !strings.Contains(out, "no unwind runbook") {
				t.Fatalf("refusal does not explain itself:\n%s", out)
			}
		})
	}
}

// TestRenderRollbackStillWorksForCreateIndex guards the refusal above from
// becoming a blanket one.
func TestRenderRollbackStillWorksForCreateIndex(t *testing.T) {
	h := newHarness(t, "orders_2026_01", "orders_2026_02")
	planPath := h.MustPlan(SpecFile{Operation: "create-index"})
	if code := h.RunBare("render", "--rollback", planPath); code != 0 {
		t.Fatalf("render --rollback on a create-index plan exited %d: %s", code, h.Out())
	}
	if !strings.Contains(h.Out(), "DROP INDEX CONCURRENTLY") {
		t.Fatalf("create-index unwind lost its online phase:\n%s", h.Out())
	}
}

// TestForwardRunbookRaisesLockTimeoutAroundConcurrentStatements pins that the
// hand-run path bounds locks the way the executor does.
//
// The preamble set one lock_timeout for the whole file — 5s by default — and
// every CONCURRENTLY statement inherited it. lock_timeout bounds each
// wait-for-lockers phase *inside* such a statement, not just its initial
// acquisition, which is why the executor gives those kinds BuildLockTimeout
// (15m). Under 5s a single six-second reader aborts a build that has already
// scanned the partition, with 55P03, leaving a _ccnew behind.
func TestForwardRunbookRaisesLockTimeoutAroundConcurrentStatements(t *testing.T) {
	h := newHarness(t, "orders_2026_01", "orders_2026_02")
	planPath := h.MustPlan(SpecFile{Operation: "create-index"})
	plan := h.LoadPlan(planPath)

	var buf bytes.Buffer
	if err := renderForward(&buf, plan, 5*time.Second, 15*time.Minute); err != nil {
		t.Fatalf("renderForward: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "SET lock_timeout = '15min';") {
		t.Fatalf("no raised lock_timeout anywhere in the runbook:\n%s", out)
	}

	// Every CONCURRENTLY statement must be under the raised value: walk the
	// file and track what the most recent SET established.
	current := ""
	sawConcurrent := false
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "SET lock_timeout") {
			current = trimmed
			continue
		}
		if strings.HasPrefix(trimmed, "--") || trimmed == "" {
			continue
		}
		if strings.Contains(trimmed, "CONCURRENTLY") {
			sawConcurrent = true
			if !strings.Contains(current, "15min") {
				t.Fatalf("a CONCURRENTLY statement runs under %q, not the build bound:\n  %s",
					current, trimmed)
			}
		}
	}
	if !sawConcurrent {
		t.Fatal("the create-index runbook contained no CONCURRENTLY statement; test proves nothing")
	}
}

// TestPgDurationEmitsPostgreSQLIntervals covers the helper the preamble uses.
//
// time.Duration.String() is not a PostgreSQL interval: it renders 15 minutes as
// "15m0s", which the server rejects, and its minute unit is "m" where
// PostgreSQL's is "min". The old preamble interpolated String() directly, so
// `render --lock-timeout 90s` emitted a runbook whose first statement failed.
func TestPgDurationEmitsPostgreSQLIntervals(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{15 * time.Minute, "15min"},
		{90 * time.Second, "90s"},
		{2 * time.Hour, "2h"},
		{1500 * time.Millisecond, "1500ms"},
		{0, "0"},
	}
	for _, tc := range cases {
		if got := pgDuration(tc.in); got != tc.want {
			t.Errorf("pgDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// The one property that matters: never Go's own format.
	for _, d := range []time.Duration{15 * time.Minute, 90 * time.Second, 2 * time.Hour} {
		if got := pgDuration(d); got == d.String() && strings.Contains(got, "m0s") {
			t.Errorf("pgDuration(%v) returned Go's format %q", d, got)
		}
	}
}

// TestReindexMarkerKeepsTheCreatingPlanDigest pins a defect found by reading the
// catalog after a live reindex, not by any unit test.
//
// The rewrite branch restored Op, Run and At from the prior marker but left
// Plan as the reindexing plan's digest. All 12 leaves ended up carrying
// run=<the create run> alongside plan=<the reindex plan> — a pairing that never
// happened, in the field whose purpose is to answer "which reviewed artifact
// authorized this object?".
func TestReindexMarkerKeepsTheCreatingPlanDigest(t *testing.T) {
	child := protocol.NewObjectName("public", "orders_idx_p1")
	n := protocol.Node{
		ID:     "reindex:p1",
		Kind:   protocol.KindIndexReindexConcurrently,
		Params: &protocol.ReindexConcurrentlyParams{Index: child},
	}
	prior := protocol.Marker{
		Run:  "run-created",
		Plan: "sha256:aaaa",
		Op:   string(protocol.OpCreateIndex),
		Role: protocol.MarkerRoleLeaf,
		At:   "2026-01-01T00:00:00Z",
	}
	base := protocol.Marker{
		Run:  "run-reindexed",
		Plan: "sha256:bbbb",
		Op:   string(protocol.OpReindexIndex),
		At:   "2026-08-07T00:00:00Z",
	}

	stmt, ok, err := protocol.RenderMarkerStatement(&n, base, prior, protocol.MarkerOurs)
	if err != nil || !ok {
		t.Fatalf("RenderMarkerStatement: ok=%t err=%v", ok, err)
	}
	text := stmt[strings.Index(stmt, "'")+1 : len(stmt)-1]
	got, status := protocol.ParseMarker(strings.ReplaceAll(text, "''", "'"))
	if status != protocol.MarkerOurs {
		t.Fatalf("status = %v", status)
	}
	if got.Plan != prior.Plan {
		t.Fatalf("plan digest = %q, want the creating plan %q; run and plan must "+
			"name the same event", got.Plan, prior.Plan)
	}
	if got.Run != prior.Run {
		t.Fatalf("run = %q, want %q", got.Run, prior.Run)
	}
	if got.ReindexRun != base.Run {
		t.Fatalf("reindex_run = %q, want %q", got.ReindexRun, base.Run)
	}
}

// TestEveryRegisteredOperationCarriesItsOwnCommandBehaviour is the NFR-EXT-1
// gate, stated as the thing that actually broke rather than as a file count.
//
// The claim under test is not "wiring an operation is one line" — it is that an
// operation cannot silently inherit another operation's answer from a command
// body. Every operation the plan format declares must either supply its own
// end-state gate and unwind, or be explicitly recorded as having none.
func TestEveryRegisteredOperationCarriesItsOwnCommandBehaviour(t *testing.T) {
	for _, op := range protocol.AllOperations() {
		entry, ok := operationRegistry[op]
		if !ok {
			t.Errorf("operation %q is in the plan format's vocabulary but has no "+
				"registry entry, so `plan` refuses it", op)
			continue
		}
		if entry.New == nil {
			t.Errorf("%s has no planner constructor", op)
		}
		if entry.EndState == nil {
			t.Errorf("%s has no end-state gate; `verify --end-state` would answer "+
				"another operation's question", op)
		}
		if entry.Rollback == nil {
			if _, explained := rollbackUnsupported[op]; !explained {
				t.Errorf("%s has neither an unwind nor a recorded reason it has none; "+
					"`render --rollback` would refuse without saying why", op)
			}
		}
	}
}

// TestExecutorAndRunbookAgreeOnWhichKindsNeedTheLongBound stops the two lock
// bounds drifting apart again. Both sides read the same predicate; this asserts
// the predicate still names the kinds that wait on concurrent transactions.
func TestExecutorAndRunbookAgreeOnWhichKindsNeedTheLongBound(t *testing.T) {
	want := map[protocol.NodeKind]bool{
		protocol.KindIndexCreateConcurrently:  true,
		protocol.KindIndexDropConcurrently:    true,
		protocol.KindIndexReindexConcurrently: true,
	}
	for _, k := range protocol.AllNodeKinds() {
		if got := k.WaitsForConcurrentTransactions(); got != want[k] {
			t.Errorf("%s.WaitsForConcurrentTransactions() = %t, want %t", k, got, want[k])
		}
	}
	if executor.DefaultBuildLockTimeout <= executor.DefaultLockTimeout {
		t.Fatal("the build bound is no longer longer than the ordinary one")
	}
}

// TestResumeCleanupDoesNotHoldACatalogSnapshotAcrossItsDrops is a structural
// guard for the deadlock.
//
// DROP INDEX CONCURRENTLY calls WaitForOlderSnapshots, so it waits for every
// older snapshot to end — including resume's own REPEATABLE READ catalog
// snapshot, which could not end until the drop it was blocking returned. The
// process deadlocked against itself and failed with a lock timeout after the
// full build bound. FR-CLI-9 reserves this cleanup to `resume`, so nothing in
// the tool could perform it.
//
// The fix is ordering, and ordering is what this asserts: the snapshot must be
// released before dropOwnedIndex is reachable. A source-level check is the
// honest test here, because the in-memory fakes have no snapshot semantics and
// would pass either way.
func TestResumeCleanupDoesNotHoldACatalogSnapshotAcrossItsDrops(t *testing.T) {
	src := readSourceFile(t, "cmd_resume.go")
	body := functionBody(t, src, "func (a *App) cleanup(")

	release := strings.Index(body, "release()")
	drop := strings.Index(body, "a.dropOwnedIndex(")
	if release < 0 || drop < 0 {
		t.Fatalf("cleanup no longer has the shape this test checks "+
			"(release=%d drop=%d); re-derive the guard", release, drop)
	}
	// The deferred release must sit inside a scope that closes before the
	// drops: textually, the drop call comes after the release's scope.
	snapshot := strings.Index(body, "tgt.snapshot(")
	if snapshot > drop {
		t.Fatal("the catalog snapshot is opened after the drops; unexpected shape")
	}
	if drop < release {
		t.Fatal("dropOwnedIndex is called before release(): the drop runs inside the " +
			"catalog snapshot and will wait on it forever (WaitForOlderSnapshots)")
	}
	// And it must not be a top-level defer, which would hold it to the end.
	prefix := body[:snapshot]
	if strings.Count(prefix, "func()") == 0 {
		t.Fatal("the snapshot is not scoped inside an inner function, so a top-level " +
			"defer holds it across the drop loop")
	}
}

// readSourceFile reads a file from this package's own directory.
func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// functionBody returns the text from a function's opening line to the first
// line that closes it at column zero.
func functionBody(t *testing.T, src, signature string) string {
	t.Helper()
	start := strings.Index(src, signature)
	if start < 0 {
		t.Fatalf("function %q not found", signature)
	}
	rest := src[start:]
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}
