package state

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

func TestAuditAppendAndPage(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t, "n1"), "run-audit")

	// CreateRun already wrote one event; every append must extend the sequence
	// without gaps or reuse.
	seen, err := s.ListAudit(ctx, run.RunID, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(seen) == 0 {
		t.Fatal("CreateRun emitted no audit event")
	}
	for i, ev := range seen {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d, want %d", i, ev.Seq, i+1)
		}
		if ev.EventID == "" {
			t.Fatalf("event %d has no id", i)
		}
	}
	last := seen[len(seen)-1]

	added, err := s.AppendAudit(ctx, AuditEvent{
		RunID: run.RunID, NodeID: "n1", Type: EventNodeTransition,
		Detail: map[string]string{"from": "PENDING", "to": "READY"},
	})
	if err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
	if added.Seq != last.Seq+1 {
		t.Fatalf("seq = %d, want %d", added.Seq, last.Seq+1)
	}

	page, err := s.ListAudit(ctx, run.RunID, last.Seq)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(page) != 1 || page[0].Seq != added.Seq {
		t.Fatalf("paged read returned %d events, want just seq %d", len(page), added.Seq)
	}
	if page[0].Detail["to"] != "READY" {
		t.Errorf("detail lost in round trip: %+v", page[0].Detail)
	}
}

func TestAuditRejectsInvalidEvents(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	mustCreateRun(t, s, testPlan(t), "run-badaudit")

	tests := []struct {
		name string
		ev   AuditEvent
	}{
		{name: "no run id", ev: AuditEvent{Type: EventRunOpened}},
		{name: "no type", ev: AuditEvent{RunID: "run-badaudit"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.AppendAudit(ctx, tc.ev); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

// INV-3 is enforced by the absence of a path, so the test that matters is a
// structural one: no exported method on either store may name an update or a
// delete of the audit trail, and the interface must expose none.
func TestAuditHasNoMutationPath(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	pkg, ok := pkgs["state"]
	if !ok {
		t.Fatal("package state not found")
	}

	banned := []string{"UpdateAudit", "DeleteAudit", "RemoveAudit", "TruncateAudit", "PurgeAudit", "RewriteAudit"}
	for name, file := range pkg.Files {
		for _, decl := range file.Decls {
			fn, isFn := decl.(*ast.FuncDecl)
			if !isFn {
				continue
			}
			for _, b := range banned {
				if fn.Name.Name == b {
					t.Errorf("%s declares %s; the audit trail is append-only (INV-3)", filepath.Base(name), b)
				}
			}
		}
	}

	// And the SQL the store can issue must contain no UPDATE or DELETE
	// against audit_event.
	store, err := NewSQLStore(openNilDB(t), SQLOptions{})
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}
	for name, stmt := range store.Statements() {
		upper := strings.ToUpper(stmt)
		if !strings.Contains(upper, "AUDIT_EVENT") {
			continue
		}
		for _, verb := range []string{"UPDATE ", "DELETE ", "TRUNCATE "} {
			if strings.Contains(upper, verb) {
				t.Errorf("statement %q both touches audit_event and contains %q (INV-3):\n%s", name, verb, stmt)
			}
		}
	}
}

// Every guarded write leaves the trail in the order the invariants require:
// the justification lands before the outcome.
func TestAuditOrderingAroundGuardedWrites(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t, "drop"), "run-order")

	object := protocol.NewObjectName("public", "idx")
	if _, err := s.RecordAuthorization(ctx, AuthorizationRecord{
		RunID: run.RunID, NodeID: "drop", Mode: protocol.AuthProvenance,
		Object: object, Evidence: markerEvidence(object),
	}, func(ctx context.Context) error { return nil }); err != nil {
		t.Fatalf("RecordAuthorization: %v", err)
	}

	trail, err := s.ListAudit(ctx, run.RunID, 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	authIdx, execIdx := -1, -1
	for i, ev := range trail {
		switch ev.Type {
		case EventAuthorizationRecorded:
			authIdx = i
		case EventDestructiveExecuted:
			execIdx = i
		}
	}
	if authIdx < 0 || execIdx < 0 {
		t.Fatalf("trail is missing events: %v", trail)
	}
	if authIdx > execIdx {
		t.Fatalf("the authorization landed at %d, after the statement at %d (INV-2)", authIdx, execIdx)
	}
}

func TestAuditListForUnknownRun(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	evs, err := s.ListAudit(ctx, "no-such-run", 0)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("got %d events for an unknown run", len(evs))
	}
}

func TestAuditDetailHelper(t *testing.T) {
	tests := []struct {
		name string
		kv   []string
		want map[string]string
	}{
		{name: "empty", kv: nil, want: nil},
		{name: "drops empty values", kv: []string{"a", "", "b", "2"}, want: map[string]string{"b": "2"}},
		{name: "drops empty keys", kv: []string{"", "1", "b", "2"}, want: map[string]string{"b": "2"}},
		{name: "ignores an odd trailing key", kv: []string{"a", "1", "b"}, want: map[string]string{"a": "1"}},
		{name: "all empty collapses to nil", kv: []string{"a", ""}, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := auditDetail(tc.kv...)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestCountNodes(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t, "a", "b", "c", "d"), "run-count")

	move := func(id protocol.NodeID, states ...protocol.NodeState) {
		t.Helper()
		from := protocol.NodePending
		for _, to := range states {
			if _, err := s.TransitionNode(ctx, NodeTransition{
				RunID: run.RunID, NodeID: id, From: from, To: to,
			}); err != nil {
				t.Fatalf("%s %s->%s: %v", id, from, to, err)
			}
			from = to
		}
	}
	move("a", protocol.NodeReady, protocol.NodeRunning, protocol.NodeVerifying, protocol.NodeDone)
	move("b", protocol.NodeSkipped)
	move("c", protocol.NodeReady, protocol.NodeRunning)

	nodes, err := s.ListNodes(ctx, run.RunID)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	counts := CountNodes(nodes)
	if counts.Total != 4 {
		t.Errorf("total = %d, want 4", counts.Total)
	}
	if counts.Complete != 2 {
		t.Errorf("complete = %d, want 2 (DONE + SKIPPED)", counts.Complete)
	}
	if counts.InFlight != 1 {
		t.Errorf("in flight = %d, want 1", counts.InFlight)
	}
	if counts.Remaining != 2 {
		t.Errorf("remaining = %d, want 2", counts.Remaining)
	}
	if counts.Failed != 0 {
		t.Errorf("failed = %d, want 0", counts.Failed)
	}
	if counts.ByState[protocol.NodeDone] != 1 {
		t.Errorf("by-state DONE = %d, want 1", counts.ByState[protocol.NodeDone])
	}
}

func TestErrorsCarryTheContractExitCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want protocol.ExitCode
	}{
		{name: "not found", err: ErrNotFound, want: protocol.ExitFailure},
		{name: "conflict", err: ErrConflict, want: protocol.ExitFailure},
		{name: "lease lost", err: ErrLeaseLost, want: protocol.ExitFailure},
		{name: "authorization not recorded", err: ErrAuthorizationNotRecorded, want: protocol.ExitAuthorizationUnsatisfied},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := protocol.ExitCodeFor(tc.err); got != tc.want {
				t.Errorf("exit code = %d, want %d", got, tc.want)
			}
			// Detail and wrapping must preserve the class, or errors.Is at the
			// call sites stops working.
			var typed *protocol.Error
			if !errors.As(tc.err, &typed) {
				t.Fatalf("%v is not a protocol.Error", tc.err)
			}
			derived := typed.Detailf("with detail").Wrap(errors.New("cause"))
			if !errors.Is(derived, tc.err) {
				t.Errorf("a derived error no longer matches its sentinel")
			}
		})
	}
}
