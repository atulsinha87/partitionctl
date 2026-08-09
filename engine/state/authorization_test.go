package state

import (
	"context"
	"errors"
	"testing"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

func ptrName(schema, name string) *protocol.ObjectName {
	o := protocol.NewObjectName(schema, name)
	return &o
}

// The three evidence shapes the decision table produces. Building them here
// rather than by hand is the point: RequiredEvidence is checked against what
// protocol.DecideProvenanceDrop and protocol.DecideLeftoverDrop actually emit.
func markerEvidence(object protocol.ObjectName) map[string]string {
	return protocol.DecideProvenanceDrop(protocol.ProvenanceDropInput{
		Object: object,
		Status: protocol.MarkerOurs,
		Marker: protocol.Marker{
			Run: "run-1", Plan: "sha256:aaa", Op: string(protocol.OpCreateIndex),
			Role: protocol.MarkerRoleLeaf, At: "2026-08-07T00:00:00Z",
		},
	}).Evidence
}

func leftoverEvidence(object protocol.ObjectName, base string) map[string]string {
	e := protocol.DecideLeftoverDrop(protocol.LeftoverDropInput{
		Object: object, BaseExists: true, BaseStatus: protocol.MarkerOurs,
		BaseMarker: protocol.Marker{
			Run: "run-1", Plan: "sha256:aaa", Op: string(protocol.OpCreateIndex),
			Role: protocol.MarkerRoleLeaf, At: "2026-08-07T00:00:00Z",
		},
	}).Evidence
	if e == nil {
		// The name is not a leftover; the caller is asserting the refusal, so
		// hand back a shape that satisfies RequiredEvidence and let Validate's
		// naming check be the thing that fails.
		e = map[string]string{
			"mode": string(protocol.AuthLeftover), "object": object.String(),
			"leftover_class": "ccnew", "base_index": base,
		}
	}
	return e
}

func explicitEvidence(object protocol.ObjectName) map[string]string {
	return map[string]string{
		"mode": string(protocol.AuthExplicit), "object": object.String(),
		"confirmation": protocol.ConfirmExclusiveLock,
	}
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
			name: "provenance sourced from the object's own marker",
			mutate: func(a *AuthorizationRecord) {
				a.Mode = protocol.AuthProvenance
				a.Evidence = markerEvidence(a.Object)
			},
		},
		{
			name: "provenance sourced from a live claim",
			mutate: func(a *AuthorizationRecord) {
				a.Mode = protocol.AuthProvenance
				a.Evidence = map[string]string{
					"mode": string(protocol.AuthProvenance), "object": a.Object.String(),
					"source": "claim", "claim_run": "run-9",
				}
			},
		},
		{
			name:    "provenance citing nothing",
			mutate:  func(a *AuthorizationRecord) { a.Mode = protocol.AuthProvenance },
			wantErr: true,
		},
		{
			name: "provenance naming a marker source but no marker run",
			mutate: func(a *AuthorizationRecord) {
				a.Mode = protocol.AuthProvenance
				a.Evidence = map[string]string{
					"mode": string(protocol.AuthProvenance), "object": a.Object.String(),
					"source": "marker",
				}
			},
			wantErr: true,
		},
		{
			name: "provenance naming a claim source but no claiming run",
			mutate: func(a *AuthorizationRecord) {
				a.Mode = protocol.AuthProvenance
				a.Evidence = map[string]string{
					"mode": string(protocol.AuthProvenance), "object": a.Object.String(),
					"source": "claim",
				}
			},
			wantErr: true,
		},
		{
			name: "provenance naming an invented source",
			mutate: func(a *AuthorizationRecord) {
				a.Mode = protocol.AuthProvenance
				a.Evidence = map[string]string{
					"mode": string(protocol.AuthProvenance), "object": a.Object.String(),
					"source": "because-i-said-so",
				}
			},
			wantErr: true,
		},
		{
			name: "leftover with both conditions",
			mutate: func(a *AuthorizationRecord) {
				a.Mode = protocol.AuthLeftover
				a.Object = protocol.NewObjectName("public", "orders_idx_ccnew1")
				a.Relation = ptrName("public", "orders_2026_03")
				a.Evidence = leftoverEvidence(a.Object, "public.orders_idx")
			},
		},
		{
			name: "leftover with the naming match but no evidence is refused (INV-7, AC-19)",
			mutate: func(a *AuthorizationRecord) {
				a.Mode = protocol.AuthLeftover
				a.Object = protocol.NewObjectName("public", "orders_idx_ccnew1")
				a.Relation = ptrName("public", "orders_2026_03")
			},
			wantErr: true,
		},
		{
			name: "leftover with evidence but no naming match is refused (INV-7)",
			mutate: func(a *AuthorizationRecord) {
				a.Mode = protocol.AuthLeftover
				a.Object = protocol.NewObjectName("public", "just_an_index")
				a.Relation = ptrName("public", "orders_2026_03")
				a.Evidence = leftoverEvidence(a.Object, "public.just")
			},
			wantErr: true,
		},
		{
			name: "leftover without a relation is refused (FR-AUTH-3)",
			mutate: func(a *AuthorizationRecord) {
				a.Mode = protocol.AuthLeftover
				a.Object = protocol.NewObjectName("public", "orders_idx_ccold")
				a.Evidence = leftoverEvidence(a.Object, "public.orders_idx")
			},
			wantErr: true,
		},
		{
			name: "explicit with a confirmation",
			mutate: func(a *AuthorizationRecord) {
				a.Mode = protocol.AuthExplicit
				a.Confirmation = protocol.ConfirmExclusiveLock
				a.Evidence = explicitEvidence(a.Object)
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
				a.Evidence = explicitEvidence(a.Object)
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
		RunID:    run.RunID,
		NodeID:   "drop",
		Mode:     protocol.AuthProvenance,
		Object:   object,
		Evidence: markerEvidence(object),
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
				// No evidence: naming alone is forgeable (AC-19).
			},
		},
		{
			name: "an unknown run",
			rec: AuthorizationRecord{
				RunID: "nope", NodeID: "drop", Mode: protocol.AuthExplicit,
				Object: protocol.NewObjectName("public", "idx"), Confirmation: protocol.ConfirmExclusiveLock,
				Evidence: explicitEvidence(protocol.NewObjectName("public", "idx")),
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
	object := protocol.NewObjectName("public", "orders_idx")
	_, err := s.RecordAuthorization(ctx, AuthorizationRecord{
		RunID: run.RunID, NodeID: "drop", Mode: protocol.AuthExplicit,
		Object:       object,
		Confirmation: protocol.ConfirmExclusiveLock,
		Evidence:     explicitEvidence(object),
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
