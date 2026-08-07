package state

import (
	"context"
	"errors"
	"testing"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

func leafIndex(name string) protocol.ObjectName { return protocol.NewObjectName("public", name) }

// INV-1: the provenance record is committed before the DDL that creates the
// object. The guard's whole point is that the DDL cannot observe a store where
// its own record is missing.
func TestWriteProvenanceCommitsBeforeTheDDL(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t, "cic"), "run-prov")

	object := leafIndex("orders_created_at_idx_orders_2026_03")
	var seenDuringDDL int
	rec := Provenance{
		RunID:      run.RunID,
		NodeID:     "cic",
		Object:     object,
		ObjectKind: ObjectIndex,
	}

	got, err := s.WriteProvenance(ctx, rec, func(ctx context.Context) error {
		// This runs where the DDL runs. The record must already be visible.
		found, err := s.FindProvenance(ctx, ProvenanceQuery{Object: object})
		if err != nil {
			return err
		}
		seenDuringDDL = len(found)
		return nil
	})
	if err != nil {
		t.Fatalf("WriteProvenance: %v", err)
	}
	if seenDuringDDL != 1 {
		t.Fatalf("the DDL saw %d provenance records, want 1 (INV-1)", seenDuringDDL)
	}
	if got.ProvenanceID == "" {
		t.Error("the store assigned no provenance id")
	}
	if got.PlanDigest != run.PlanDigest {
		t.Errorf("plan digest = %q, want the run's %q", got.PlanDigest, run.PlanDigest)
	}
	if got.Database != "appdb" {
		t.Errorf("database = %q, want the run target's appdb", got.Database)
	}
}

// A failed CREATE INDEX CONCURRENTLY leaves an INVALID index behind. The
// provenance record is what lets `resume` prove the wreckage is its own
// (FR-PLAN-6, AC-5), so it must survive the failure.
func TestWriteProvenanceRetainsTheRecordWhenTheDDLFails(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t, "cic"), "run-prov-fail")

	object := leafIndex("orders_idx_2026_03")
	ddlErr := errors.New("canceling statement due to lock timeout")

	_, err := s.WriteProvenance(ctx, Provenance{
		RunID: run.RunID, NodeID: "cic", Object: object, ObjectKind: ObjectIndex,
	}, func(ctx context.Context) error { return ddlErr })

	if !errors.Is(err, ddlErr) {
		t.Fatalf("err = %v, want the DDL error unwrapped", err)
	}
	found, ferr := s.FindProvenance(ctx, ProvenanceQuery{Object: object})
	if ferr != nil {
		t.Fatalf("FindProvenance: %v", ferr)
	}
	if len(found) != 1 {
		t.Fatalf("the provenance record was rolled back with the DDL; %d remain, want 1", len(found))
	}
}

// If the record cannot be committed the DDL must not run at all.
func TestWriteProvenanceDoesNotRunTheDDLWhenTheRecordFails(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		rec  Provenance
		run  bool
	}{
		{
			name: "unknown run",
			rec:  Provenance{RunID: "no-such-run", Object: leafIndex("i"), ObjectKind: ObjectIndex},
		},
		{
			name: "no object",
			rec:  Provenance{RunID: "run-guard", ObjectKind: ObjectIndex},
		},
		{
			name: "unknown object kind",
			rec:  Provenance{RunID: "run-guard", Object: leafIndex("i"), ObjectKind: "table"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newFileStore(t)
			mustCreateRun(t, s, testPlan(t), "run-guard")

			called := false
			_, err := s.WriteProvenance(ctx, tc.rec, func(ctx context.Context) error {
				called = true
				return nil
			})
			if !errors.Is(err, ErrProvenanceNotRecorded) {
				t.Fatalf("err = %v, want ErrProvenanceNotRecorded", err)
			}
			if called {
				t.Fatal("the guarded DDL ran even though no record was committed (INV-1)")
			}
		})
	}
}

// A crash between the record's commit and the rename must not lose it either:
// the record write is the atomic step, so an interrupted rename means no
// record and therefore no DDL.
func TestWriteProvenanceCrashBeforeCommitSkipsTheDDL(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t, "cic"), "run-prov-crash")

	crashAt(s, "prov")
	called := false
	_, err := s.WriteProvenance(ctx, Provenance{
		RunID: run.RunID, NodeID: "cic", Object: leafIndex("i"), ObjectKind: ObjectIndex,
	}, func(ctx context.Context) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrProvenanceNotRecorded) {
		t.Fatalf("err = %v, want ErrProvenanceNotRecorded", err)
	}
	if called {
		t.Fatal("the guarded DDL ran after the record write was interrupted (INV-1)")
	}
	found, ferr := s.FindProvenance(ctx, ProvenanceQuery{Object: leafIndex("i")})
	if ferr != nil {
		t.Fatalf("FindProvenance: %v", ferr)
	}
	if len(found) != 0 {
		t.Fatalf("a half-written provenance record became visible: %+v", found)
	}
}

func TestFindProvenance(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	runA := mustCreateRun(t, s, testPlanWithID(t, "pa", "orders"), "run-a")
	runB := mustCreateRun(t, s, testPlanWithID(t, "pb", "orders"), "run-b")

	leaf := protocol.NewObjectName("public", "orders_2026_03")
	write := func(run Run, obj protocol.ObjectName, rel *protocol.ObjectName) {
		t.Helper()
		if _, err := s.WriteProvenance(ctx, Provenance{
			RunID: run.RunID, Object: obj, ObjectKind: ObjectIndex, Relation: rel,
		}, nil); err != nil {
			t.Fatalf("WriteProvenance: %v", err)
		}
	}
	write(runA, leafIndex("idx_a"), &leaf)
	write(runB, leafIndex("idx_b"), nil)

	other := protocol.NewObjectName("public", "orders_2026_04")

	tests := []struct {
		name string
		q    ProvenanceQuery
		want int
	}{
		{name: "by object", q: ProvenanceQuery{Object: leafIndex("idx_a")}, want: 1},
		{name: "across runs, unknown object", q: ProvenanceQuery{Object: leafIndex("idx_z")}, want: 0},
		{name: "narrowed to the wrong run", q: ProvenanceQuery{Object: leafIndex("idx_a"), RunID: runB.RunID}, want: 0},
		{name: "narrowed to the right run", q: ProvenanceQuery{Object: leafIndex("idx_a"), RunID: runA.RunID}, want: 1},
		{name: "by relation", q: ProvenanceQuery{Object: leafIndex("idx_a"), Relation: &leaf}, want: 1},
		{name: "by the wrong relation", q: ProvenanceQuery{Object: leafIndex("idx_a"), Relation: &other}, want: 0},
		{name: "record with no relation cannot match one", q: ProvenanceQuery{Object: leafIndex("idx_b"), Relation: &leaf}, want: 0},
		{name: "by database", q: ProvenanceQuery{Object: leafIndex("idx_a"), Database: "appdb"}, want: 1},
		{name: "by the wrong database", q: ProvenanceQuery{Object: leafIndex("idx_a"), Database: "otherdb"}, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.FindProvenance(ctx, tc.q)
			if err != nil {
				t.Fatalf("FindProvenance: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d records, want %d", len(got), tc.want)
			}
		})
	}

	t.Run("a query with no object is refused", func(t *testing.T) {
		if _, err := s.FindProvenance(ctx, ProvenanceQuery{}); err == nil {
			t.Fatal("want an error")
		}
	})
}

// FR-AUTH-2: the provenance mode is satisfied by a committed record and by
// nothing else. AC-6: an INVALID index PartitionCTL did not create has none.
func TestHasProvenance(t *testing.T) {
	ctx := context.Background()
	s, _ := newFileStore(t)
	run := mustCreateRun(t, s, testPlan(t), "run-hasprov")

	ours := leafIndex("ours_idx")
	theirs := leafIndex("someone_elses_idx")
	if _, err := s.WriteProvenance(ctx, Provenance{
		RunID: run.RunID, Object: ours, ObjectKind: ObjectIndex,
	}, nil); err != nil {
		t.Fatalf("WriteProvenance: %v", err)
	}

	ok, id, err := HasProvenance(ctx, s, ProvenanceQuery{Object: ours})
	if err != nil {
		t.Fatalf("HasProvenance: %v", err)
	}
	if !ok || id == "" {
		t.Fatalf("HasProvenance(ours) = %v, %q; want true and an id", ok, id)
	}

	ok, id, err = HasProvenance(ctx, s, ProvenanceQuery{Object: theirs})
	if err != nil {
		t.Fatalf("HasProvenance: %v", err)
	}
	if ok || id != "" {
		t.Fatalf("HasProvenance(theirs) = %v, %q; a foreign index must have no provenance (AC-6)", ok, id)
	}
}
