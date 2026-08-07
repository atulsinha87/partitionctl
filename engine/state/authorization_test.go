package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

func ptrName(schema, name string) *protocol.ObjectName {
	o := protocol.NewObjectName(schema, name)
	return &o
}

// FR-AUTH-1…4 and INV-7 on the record's shape: a justification that cites
// nothing is not a justification, and the store refuses to write one.
func TestAuthorizationRecordValidate(t *testing.T) {
	base := AuthorizationRecord{
		RunID:  "run-1",
		NodeID: "drop",
		Object: protocol.NewObjectName("public", "idx"),
	}

	tests := []struct {
		name    string
		mutate  func(a *AuthorizationRecord)
		wantErr bool
	}{
		{
			name: "provenance with a cited record",
			mutate: func(a *AuthorizationRecord) {
				a.Mode = protocol.AuthProvenance
				a.ProvenanceID = "run-1:prov:00000001"
			},
		},
		{
			name:    "provenance citing nothing",
			mutate:  func(a *AuthorizationRecord) { a.Mode = protocol.AuthProvenance },
			wantErr: true,
		},
		{
			name: "leftover with both conditions",
			mutate: func(a *AuthorizationRecord) {
				a.Mode = protocol.AuthLeftover
				a.Object = protocol.NewObjectName("public", "orders_idx_ccnew1")
				a.Relation = ptrName("public", "orders_2026_03")
				a.ReindexRunID = "run-reindex-9"
			},
		},
		{
			name: "leftover with the naming match but no run history is refused (INV-7, AC-19)",
			mutate: func(a *AuthorizationRecord) {
				a.Mode = protocol.AuthLeftover
				a.Object = protocol.NewObjectName("public", "orders_idx_ccnew1")
				a.Relation = ptrName("public", "orders_2026_03")
			},
			wantErr: true,
		},
		{
			name: "leftover with run history but no naming match is refused (INV-7)",
			mutate: func(a *AuthorizationRecord) {
				a.Mode = protocol.AuthLeftover
				a.Object = protocol.NewObjectName("public", "just_an_index")
				a.Relation = ptrName("public", "orders_2026_03")
				a.ReindexRunID = "run-reindex-9"
			},
			wantErr: true,
		},
		{
			name: "leftover without a relation is refused (FR-AUTH-3)",
			mutate: func(a *AuthorizationRecord) {
				a.Mode = protocol.AuthLeftover
				a.Object = protocol.NewObjectName("public", "orders_idx_ccold")
				a.ReindexRunID = "run-reindex-9"
			},
			wantErr: true,
		},
		{
			name: "explicit with a confirmation",
			mutate: func(a *AuthorizationRecord) {
				a.Mode = protocol.AuthExplicit
				a.Confirmation = protocol.ConfirmExclusiveLock
			},
		},
		{
			name:    "explicit without a confirmation is refused (FR-AUTH-4)",
			mutate:  func(a *AuthorizationRecord) { a.Mode = protocol.AuthExplicit },
			wantErr: true,
		},
		{
			name:    "an unknown mode is refused",
			mutate:  func(a *AuthorizationRecord) { a.Mode = "because-i-said-so" },
			wantErr: true,
		},
		{
			name: "a record with no node id is refused",
			mutate: func(a *AuthorizationRecord) {
				a.Mode = protocol.AuthExplicit
				a.Confirmation = protocol.ConfirmExclusiveLock
				a.NodeID = ""
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := base
			tc.mutate(&rec)
			err := rec.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatal("want an error")
				}
				if !errors.Is(err, protocol.ErrAuthorizationUnsatisfied) {
					t.Fatalf("err = %v, want ErrAuthorizationUnsatisfied", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

// INV-2 / FR-AUTH-6 / AC-20: the justification is committed before the
// destructive statement runs, and a statement whose justification cannot be
// committed does not run at all.
func TestRecordAuthorizationCommitsBeforeTheStatement(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t, "drop"), "run-auth")

	object := protocol.NewObjectName("public", "orders_idx_2026_03")
	var seenDuringStatement int

	rec := AuthorizationRecord{
		RunID:        run.RunID,
		NodeID:       "drop",
		Mode:         protocol.AuthProvenance,
		Object:       object,
		ProvenanceID: "run-auth:prov:00000001",
		Evidence:     map[string]string{"indisvalid": "false"},
	}
	got, err := s.RecordAuthorization(ctx, rec, func(ctx context.Context) error {
		recs, lerr := s.ListAuthorizations(ctx, run.RunID)
		if lerr != nil {
			return lerr
		}
		seenDuringStatement = len(recs)
		return nil
	})
	if err != nil {
		t.Fatalf("RecordAuthorization: %v", err)
	}
	if seenDuringStatement != 1 {
		t.Fatalf("the destructive statement saw %d authorization records, want 1 (INV-2)", seenDuringStatement)
	}
	if got.AuthorizationID == "" {
		t.Error("the store assigned no authorization id")
	}
}

func TestRecordAuthorizationRefusesTheStatementWhenTheRecordFails(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		rec  AuthorizationRecord
	}{
		{
			name: "an unsatisfiable leftover claim",
			rec: AuthorizationRecord{
				RunID: "run-auth2", NodeID: "drop", Mode: protocol.AuthLeftover,
				Object:   protocol.NewObjectName("public", "orders_idx_ccnew"),
				Relation: ptrName("public", "orders_2026_03"),
				// No ReindexRunID: naming alone is forgeable (AC-19).
			},
		},
		{
			name: "an unknown run",
			rec: AuthorizationRecord{
				RunID: "nope", NodeID: "drop", Mode: protocol.AuthExplicit,
				Object: protocol.NewObjectName("public", "idx"), Confirmation: protocol.ConfirmExclusiveLock,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newFileStore(t)
			mustCreateRun(t, s, testPlan(t, "drop"), "run-auth2")

			called := false
			_, err := s.RecordAuthorization(ctx, tc.rec, func(ctx context.Context) error {
				called = true
				return nil
			})
			if !errors.Is(err, ErrAuthorizationNotRecorded) {
				t.Fatalf("err = %v, want ErrAuthorizationNotRecorded", err)
			}
			if called {
				t.Fatal("the destructive statement ran with no committed justification (INV-2)")
			}
			if code := protocol.ExitCodeFor(err); code != protocol.ExitAuthorizationUnsatisfied {
				t.Errorf("exit code = %d, want %d", code, protocol.ExitAuthorizationUnsatisfied)
			}
		})
	}
}

// A destructive statement that fails still leaves its justification behind: the
// audit trail records what was attempted, not only what succeeded.
func TestRecordAuthorizationRetainsTheRecordWhenTheStatementFails(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t, "drop"), "run-auth3")

	stmtErr := errors.New("canceling statement due to lock timeout")
	_, err := s.RecordAuthorization(ctx, AuthorizationRecord{
		RunID: run.RunID, NodeID: "drop", Mode: protocol.AuthExplicit,
		Object:       protocol.NewObjectName("public", "orders_idx"),
		Confirmation: protocol.ConfirmExclusiveLock,
	}, func(ctx context.Context) error { return stmtErr })

	if !errors.Is(err, stmtErr) {
		t.Fatalf("err = %v, want the statement error", err)
	}
	recs, lerr := s.ListAuthorizations(ctx, run.RunID)
	if lerr != nil {
		t.Fatalf("ListAuthorizations: %v", lerr)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d authorization records, want 1", len(recs))
	}

	trail, terr := s.ListAudit(ctx, run.RunID, 0)
	if terr != nil {
		t.Fatalf("ListAudit: %v", terr)
	}
	var sawRecorded, sawFailed bool
	for _, ev := range trail {
		switch ev.Type {
		case EventAuthorizationRecorded:
			sawRecorded = true
		case EventDestructiveFailed:
			if !sawRecorded {
				t.Fatal("the failure was recorded before the authorization (INV-2 ordering)")
			}
			sawFailed = true
		}
	}
	if !sawRecorded || !sawFailed {
		t.Fatalf("trail is missing events: recorded=%v failed=%v", sawRecorded, sawFailed)
	}
}

// FR-AUTH-3 / INV-7 / AC-19: the leftover mode's second condition is recorded
// run history, and a leaf's history lives on its partitioned parent.
func TestReindexRunFor(t *testing.T) {
	ctx := context.Background()
	s, c := newFileStore(t)

	reindexPlan := testPlanWithID(t, "p-reindex", "orders")
	reindexPlan.Operation = protocol.OpReindexIndex
	reindexPlan.Digest = ""
	if err := reindexPlan.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	run := mustCreateRun(t, s, reindexPlan, "run-reindex")
	c.Advance(time.Minute)
	if _, err := s.SetRunStatus(ctx, RunStatusUpdate{
		RunID: run.RunID, From: RunRunning, To: RunCompleted,
	}); err != nil {
		t.Fatalf("SetRunStatus: %v", err)
	}

	parent := protocol.NewObjectName("public", "orders")
	leaf := protocol.NewObjectName("public", "orders_2026_03")
	unrelated := protocol.NewObjectName("public", "events")

	tests := []struct {
		name string
		q    ReindexHistoryQuery
		want bool
	}{
		{
			name: "the leaf alone finds nothing, because the run targets the parent",
			q:    ReindexHistoryQuery{Database: "appdb", Relations: []protocol.ObjectName{leaf}},
			want: false,
		},
		{
			name: "leaf plus parent finds the run",
			q:    ReindexHistoryQuery{Database: "appdb", Relations: []protocol.ObjectName{leaf, parent}},
			want: true,
		},
		{
			name: "an unrelated relation finds nothing (AC-19)",
			q:    ReindexHistoryQuery{Database: "appdb", Relations: []protocol.ObjectName{unrelated}},
			want: false,
		},
		{
			name: "a since bound the run satisfies",
			q: ReindexHistoryQuery{Database: "appdb", Relations: []protocol.ObjectName{parent},
				Since: baseTime.Add(30 * time.Second)},
			want: true,
		},
		{
			name: "a since bound the run predates (FR-LB-4)",
			q: ReindexHistoryQuery{Database: "appdb", Relations: []protocol.ObjectName{parent},
				Since: baseTime.Add(2 * time.Hour)},
			want: false,
		},
		{
			name: "the wrong database finds nothing",
			q:    ReindexHistoryQuery{Database: "otherdb", Relations: []protocol.ObjectName{parent}},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, found, err := ReindexRunFor(ctx, s, tc.q)
			if err != nil {
				t.Fatalf("ReindexRunFor: %v", err)
			}
			if found != tc.want {
				t.Fatalf("found = %v, want %v", found, tc.want)
			}
			if found && got.Operation != protocol.OpReindexIndex {
				t.Errorf("matched a %s run", got.Operation)
			}
		})
	}
}

// A create run for the same table is not reindex history.
func TestReindexRunForIgnoresOtherOperations(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	mustCreateRun(t, s, testPlanWithID(t, "p-create", "orders"), "run-create")

	parent := protocol.NewObjectName("public", "orders")
	_, found, err := ReindexRunFor(ctx, s, ReindexHistoryQuery{
		Database: "appdb", Relations: []protocol.ObjectName{parent},
	})
	if err != nil {
		t.Fatalf("ReindexRunFor: %v", err)
	}
	if found {
		t.Fatal("a create run was accepted as reindex history (FR-AUTH-3)")
	}
}
