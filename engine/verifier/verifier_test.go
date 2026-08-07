package verifier

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

func ptr[T any](v T) *T { return &v }

var errCatalogDown = errors.New("connection reset by peer")

// TestCheck drives every assertion in the vocabulary through every distinct
// outcome, with each failure produced in isolation from an otherwise healthy
// catalog. Isolation is the point: it proves a reported failure names the thing
// that is actually broken rather than a downstream symptom.
func TestCheck(t *testing.T) {
	tests := []struct {
		name    string
		req     string // the requirement the case covers
		catalog func() *fakeCatalog
		check   protocol.VerifyCheck
		want    Status
		reason  string // substring of the expected reason
	}{
		// ---- FR-VER-1: indisvalid AND indisready AND indislive ----
		{
			name:    "index_valid/all three true",
			req:     "FR-VER-1",
			catalog: func() *fakeCatalog { return healthyCatalog(2) },
			check:   protocol.VerifyCheck{Check: protocol.CheckIndexValid, Index: ptr(childIndex(leaf(1)))},
			want:    StatusPass,
			reason:  "is valid, ready and live",
		},
		{
			name: "index_valid/indisvalid false",
			req:  "FR-VER-1",
			catalog: func() *fakeCatalog {
				return healthyCatalog(2).mutate(childIndex(leaf(1)), func(s *IndexState) { s.Valid = false })
			},
			check:  protocol.VerifyCheck{Check: protocol.CheckIndexValid, Index: ptr(childIndex(leaf(1)))},
			want:   StatusFail,
			reason: "indisvalid=false indisready=true indislive=true",
		},
		{
			name: "index_valid/indisready false",
			req:  "FR-VER-1",
			catalog: func() *fakeCatalog {
				return healthyCatalog(2).mutate(childIndex(leaf(1)), func(s *IndexState) { s.Ready = false })
			},
			check:  protocol.VerifyCheck{Check: protocol.CheckIndexValid, Index: ptr(childIndex(leaf(1)))},
			want:   StatusFail,
			reason: "indisvalid=true indisready=false indislive=true",
		},
		{
			name: "index_valid/indislive false",
			req:  "FR-VER-1",
			catalog: func() *fakeCatalog {
				return healthyCatalog(2).mutate(childIndex(leaf(1)), func(s *IndexState) { s.Live = false })
			},
			check:  protocol.VerifyCheck{Check: protocol.CheckIndexValid, Index: ptr(childIndex(leaf(1)))},
			want:   StatusFail,
			reason: "indisvalid=true indisready=true indislive=false",
		},
		{
			name:    "index_valid/index absent",
			req:     "FR-VER-1",
			catalog: func() *fakeCatalog { return healthyCatalog(2).dropIndex(childIndex(leaf(1))) },
			check:   protocol.VerifyCheck{Check: protocol.CheckIndexValid, Index: ptr(childIndex(leaf(1)))},
			want:    StatusFail,
			reason:  "does not exist",
		},
		{
			name: "index_valid/catalog unreadable",
			req:  "FR-VER-1",
			catalog: func() *fakeCatalog {
				f := healthyCatalog(2)
				f.failIndex = errCatalogDown
				return f
			},
			check:  protocol.VerifyCheck{Check: protocol.CheckIndexValid, Index: ptr(childIndex(leaf(1)))},
			want:   StatusError,
			reason: "could not read index",
		},

		// ---- FR-VER-2: the parent-child relationship exists in pg_inherits ----
		{
			name:    "index_attached/attached to the expected parent",
			req:     "FR-VER-2",
			catalog: func() *fakeCatalog { return healthyCatalog(2) },
			check: protocol.VerifyCheck{
				Check:       protocol.CheckIndexAttached,
				Index:       ptr(childIndex(leaf(1))),
				ParentIndex: ptr(parentIndex()),
			},
			want:   StatusPass,
			reason: "is attached to",
		},
		{
			name:    "index_attached/built but never attached",
			req:     "FR-VER-2",
			catalog: func() *fakeCatalog { return healthyCatalog(2).detach(childIndex(leaf(1))) },
			check: protocol.VerifyCheck{
				Check:       protocol.CheckIndexAttached,
				Index:       ptr(childIndex(leaf(1))),
				ParentIndex: ptr(parentIndex()),
			},
			want:   StatusFail,
			reason: "is not attached to any partitioned index",
		},
		{
			name: "index_attached/attached to a different parent index",
			req:  "FR-VER-2",
			catalog: func() *fakeCatalog {
				other := protocol.NewObjectName(testSchema, "orders_other_idx")
				return healthyCatalog(2).attach(childIndex(leaf(1)), other)
			},
			check: protocol.VerifyCheck{
				Check:       protocol.CheckIndexAttached,
				Index:       ptr(childIndex(leaf(1))),
				ParentIndex: ptr(parentIndex()),
			},
			want:   StatusFail,
			reason: `is attached to "public.orders_other_idx", not to`,
		},
		{
			name:    "index_attached/index does not exist at all",
			req:     "FR-VER-2",
			catalog: func() *fakeCatalog { return healthyCatalog(2).dropIndex(childIndex(leaf(1))) },
			check: protocol.VerifyCheck{
				Check:       protocol.CheckIndexAttached,
				Index:       ptr(childIndex(leaf(1))),
				ParentIndex: ptr(parentIndex()),
			},
			want:   StatusFail,
			reason: "does not exist, so it is not attached",
		},
		{
			name: "index_attached/catalog unreadable",
			req:  "FR-VER-2",
			catalog: func() *fakeCatalog {
				f := healthyCatalog(2)
				f.failIndexParent = errCatalogDown
				return f
			},
			check: protocol.VerifyCheck{
				Check:       protocol.CheckIndexAttached,
				Index:       ptr(childIndex(leaf(1))),
				ParentIndex: ptr(parentIndex()),
			},
			want:   StatusError,
			reason: "could not read pg_inherits",
		},

		// ---- FR-VER-3: the parent index is indisvalid after the final attach ----
		{
			name:    "parent_index_valid/valid",
			req:     "FR-VER-3",
			catalog: func() *fakeCatalog { return healthyCatalog(2) },
			check:   protocol.VerifyCheck{Check: protocol.CheckParentIndexValid, ParentIndex: ptr(parentIndex())},
			want:    StatusPass,
			reason:  "is valid",
		},
		{
			name: "parent_index_valid/still invalid mid-build",
			req:  "FR-VER-3",
			catalog: func() *fakeCatalog {
				return healthyCatalog(2).mutate(parentIndex(), func(s *IndexState) { s.Valid = false })
			},
			check:  protocol.VerifyCheck{Check: protocol.CheckParentIndexValid, ParentIndex: ptr(parentIndex())},
			want:   StatusFail,
			reason: "PostgreSQL marks it valid automatically when the final child index attaches",
		},
		{
			name:    "parent_index_valid/absent",
			req:     "FR-VER-3",
			catalog: func() *fakeCatalog { return healthyCatalog(2).dropIndex(parentIndex()) },
			check:   protocol.VerifyCheck{Check: protocol.CheckParentIndexValid, ParentIndex: ptr(parentIndex())},
			want:    StatusFail,
			reason:  "does not exist",
		},
		{
			// FR-VER-3 names indisvalid alone. indisready is reported for
			// diagnosis but must not decide the verdict, or the assertion would
			// be stricter than the requirement.
			name: "parent_index_valid/indisready false does not fail the assertion",
			req:  "FR-VER-3",
			catalog: func() *fakeCatalog {
				return healthyCatalog(2).mutate(parentIndex(), func(s *IndexState) { s.Ready = false })
			},
			check:  protocol.VerifyCheck{Check: protocol.CheckParentIndexValid, ParentIndex: ptr(parentIndex())},
			want:   StatusPass,
			reason: "is valid",
		},
		{
			name: "parent_index_valid/catalog unreadable",
			req:  "FR-VER-3",
			catalog: func() *fakeCatalog {
				f := healthyCatalog(2)
				f.failIndex = errCatalogDown
				return f
			},
			check:  protocol.VerifyCheck{Check: protocol.CheckParentIndexValid, ParentIndex: ptr(parentIndex())},
			want:   StatusError,
			reason: "could not read parent index",
		},

		// ---- FR-VER-4: leaf index count equals leaf partition count ----
		{
			name:    "leaf_index_count/matches",
			req:     "FR-VER-4",
			catalog: func() *fakeCatalog { return healthyCatalog(4) },
			check: protocol.VerifyCheck{
				Check:         protocol.CheckLeafIndexCount,
				ParentIndex:   ptr(parentIndex()),
				ExpectedCount: ptr(4),
			},
			want:   StatusPass,
			reason: "has 4 attached leaf indexes, matching the expected 4",
		},
		{
			name:    "leaf_index_count/one leaf never attached",
			req:     "FR-VER-4",
			catalog: func() *fakeCatalog { return healthyCatalog(4).detach(childIndex(leaf(3))) },
			check: protocol.VerifyCheck{
				Check:         protocol.CheckLeafIndexCount,
				ParentIndex:   ptr(parentIndex()),
				ExpectedCount: ptr(4),
			},
			want:   StatusFail,
			reason: "has 3 attached leaf indexes, expected 4",
		},
		{
			name: "leaf_index_count/an unexpected extra child is attached",
			req:  "FR-VER-4",
			catalog: func() *fakeCatalog {
				stray := protocol.NewObjectName(testSchema, "orders_stray_idx")
				strayRel := protocol.NewObjectName(testSchema, "orders_1999_01")
				f := healthyCatalog(4)
				f.putIndex(usable(stray, strayRel)).attach(stray, parentIndex())
				return f
			},
			check: protocol.VerifyCheck{
				Check:         protocol.CheckLeafIndexCount,
				ParentIndex:   ptr(parentIndex()),
				ExpectedCount: ptr(4),
			},
			want:   StatusFail,
			reason: "has 5 attached leaf indexes, expected 4",
		},
		{
			name:    "leaf_index_count/parent index absent reports itself",
			req:     "FR-VER-4",
			catalog: func() *fakeCatalog { return healthyCatalog(4).dropIndex(parentIndex()) },
			check: protocol.VerifyCheck{
				Check:         protocol.CheckLeafIndexCount,
				ParentIndex:   ptr(parentIndex()),
				ExpectedCount: ptr(4),
			},
			want:   StatusFail,
			reason: "does not exist, so it has no attached leaf indexes",
		},
		{
			name: "leaf_index_count/pg_inherits unreadable",
			req:  "FR-VER-4",
			catalog: func() *fakeCatalog {
				f := healthyCatalog(4)
				f.failAttached = errCatalogDown
				return f
			},
			check: protocol.VerifyCheck{
				Check:         protocol.CheckLeafIndexCount,
				ParentIndex:   ptr(parentIndex()),
				ExpectedCount: ptr(4),
			},
			want:   StatusError,
			reason: "could not read pg_inherits",
		},
		{
			name: "leaf_index_count/pg_index unreadable",
			req:  "FR-VER-4",
			catalog: func() *fakeCatalog {
				f := healthyCatalog(4)
				f.failIndex = errCatalogDown
				return f
			},
			check: protocol.VerifyCheck{
				Check:         protocol.CheckLeafIndexCount,
				ParentIndex:   ptr(parentIndex()),
				ExpectedCount: ptr(4),
			},
			want:   StatusError,
			reason: "could not read parent index",
		},
		{
			name:    "leaf_index_count/zero expected on an empty tree",
			req:     "FR-VER-4",
			catalog: func() *fakeCatalog { return healthyCatalog(0) },
			check: protocol.VerifyCheck{
				Check:         protocol.CheckLeafIndexCount,
				ParentIndex:   ptr(parentIndex()),
				ExpectedCount: ptr(0),
			},
			want:   StatusPass,
			reason: "has 0 attached leaf indexes",
		},

		// ---- FR-DROP-7: absence ----
		{
			name:    "index_absent/absent",
			req:     "FR-DROP-7",
			catalog: func() *fakeCatalog { return healthyCatalog(2).dropIndex(parentIndex()) },
			check:   protocol.VerifyCheck{Check: protocol.CheckIndexAbsent, Index: ptr(parentIndex())},
			want:    StatusPass,
			reason:  "is absent from pg_index",
		},
		{
			name:    "index_absent/still present",
			req:     "FR-DROP-7",
			catalog: func() *fakeCatalog { return healthyCatalog(2) },
			check:   protocol.VerifyCheck{Check: protocol.CheckIndexAbsent, Index: ptr(parentIndex())},
			want:    StatusFail,
			reason:  "is still present in pg_index",
		},
		{
			name: "index_absent/catalog unreadable",
			req:  "FR-DROP-7",
			catalog: func() *fakeCatalog {
				f := healthyCatalog(2)
				f.failIndex = errCatalogDown
				return f
			},
			check:  protocol.VerifyCheck{Check: protocol.CheckIndexAbsent, Index: ptr(parentIndex())},
			want:   StatusError,
			reason: "could not read index",
		},

		// ---- FR-REIDX-6: no _ccnew/_ccold leftovers ----
		{
			name:    "no_leftover_indexes/clean tree",
			req:     "FR-REIDX-6",
			catalog: func() *fakeCatalog { return healthyCatalog(3) },
			check:   protocol.VerifyCheck{Check: protocol.CheckNoLeftoverIndexes, Relation: ptr(table())},
			want:    StatusPass,
			reason:  "carry no _ccnew/_ccold leftover indexes",
		},
		{
			name: "no_leftover_indexes/a _ccnew survives on a leaf",
			req:  "FR-REIDX-6",
			catalog: func() *fakeCatalog {
				f := healthyCatalog(3)
				lo := leftoverName(childIndex(leaf(2)), "_ccnew")
				return f.putIndex(usable(lo, leaf(2)))
			},
			check:  protocol.VerifyCheck{Check: protocol.CheckNoLeftoverIndexes, Relation: ptr(table())},
			want:   StatusFail,
			reason: "(ccnew)",
		},
		{
			name: "no_leftover_indexes/a _ccold survives on a leaf",
			req:  "FR-REIDX-6",
			catalog: func() *fakeCatalog {
				f := healthyCatalog(3)
				lo := leftoverName(childIndex(leaf(2)), "_ccold")
				return f.putIndex(usable(lo, leaf(2)))
			},
			check:  protocol.VerifyCheck{Check: protocol.CheckNoLeftoverIndexes, Relation: ptr(table())},
			want:   StatusFail,
			reason: "(ccold)",
		},
		{
			// PostgreSQL appends a disambiguating integer, so detection is a
			// pattern and not a literal suffix (TRD §7.2.11).
			name: "no_leftover_indexes/disambiguated suffix _ccnew1",
			req:  "FR-REIDX-6",
			catalog: func() *fakeCatalog {
				f := healthyCatalog(3)
				lo := leftoverName(childIndex(leaf(1)), "_ccnew1")
				return f.putIndex(usable(lo, leaf(1)))
			},
			check:  protocol.VerifyCheck{Check: protocol.CheckNoLeftoverIndexes, Relation: ptr(table())},
			want:   StatusFail,
			reason: "carry 1 REINDEX CONCURRENTLY leftover indexes",
		},
		{
			name: "no_leftover_indexes/a leftover on the parent table itself",
			req:  "FR-REIDX-6",
			catalog: func() *fakeCatalog {
				f := healthyCatalog(3)
				lo := protocol.NewObjectName(testSchema, testParentIndex+"_ccold2")
				return f.putIndex(usable(lo, table()))
			},
			check:  protocol.VerifyCheck{Check: protocol.CheckNoLeftoverIndexes, Relation: ptr(table())},
			want:   StatusFail,
			reason: "(ccold)",
		},
		{
			name: "no_leftover_indexes/catalog unreadable",
			req:  "FR-REIDX-6",
			catalog: func() *fakeCatalog {
				f := healthyCatalog(3)
				f.failTree = errCatalogDown
				return f
			},
			check:  protocol.VerifyCheck{Check: protocol.CheckNoLeftoverIndexes, Relation: ptr(table())},
			want:   StatusError,
			reason: "could not read the indexes of",
		},

		// ---- malformed checks are errors, not verdicts ----
		{
			name:    "malformed/index_valid without an index",
			req:     "FR-CLI-14",
			catalog: func() *fakeCatalog { return healthyCatalog(1) },
			check:   protocol.VerifyCheck{Check: protocol.CheckIndexValid},
			want:    StatusError,
			reason:  "check is malformed",
		},
		{
			name:    "malformed/leaf_index_count without an expected count",
			req:     "FR-CLI-14",
			catalog: func() *fakeCatalog { return healthyCatalog(1) },
			check: protocol.VerifyCheck{
				Check:       protocol.CheckLeafIndexCount,
				ParentIndex: ptr(parentIndex()),
			},
			want:   StatusError,
			reason: "check is malformed",
		},
		{
			name:    "malformed/unknown check kind",
			req:     "FR-CLI-14",
			catalog: func() *fakeCatalog { return healthyCatalog(1) },
			check:   protocol.VerifyCheck{Check: protocol.VerifyCheckKind("index_smells_nice")},
			want:    StatusError,
			reason:  "check is malformed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := New(tc.catalog())
			got := v.Check(context.Background(), tc.check)
			if got.Status != tc.want {
				t.Fatalf("%s: status = %s, want %s (reason %q)", tc.req, got.Status, tc.want, got.Reason)
			}
			if !strings.Contains(got.Reason, tc.reason) {
				t.Fatalf("%s: reason = %q, want it to contain %q", tc.req, got.Reason, tc.reason)
			}
			if got.Check != tc.check.Check {
				t.Fatalf("result check = %q, want %q", got.Check, tc.check.Check)
			}
			if got.Status == StatusError && got.Err() == nil {
				t.Fatalf("%s: error result carries no cause", tc.req)
			}
			if got.Status != StatusError && got.Err() != nil {
				t.Fatalf("%s: non-error result carries a cause: %v", tc.req, got.Err())
			}
		})
	}
}

// TestCheckNamesEveryLeftoverUpToTheCap keeps a tree with hundreds of leftovers
// from producing an unreadable single-line reason.
func TestCheckNamesEveryLeftoverUpToTheCap(t *testing.T) {
	f := healthyCatalog(20)
	for i := 1; i <= 20; i++ {
		lo := leftoverName(childIndex(leaf(i)), "_ccnew")
		f.putIndex(usable(lo, leaf(i)))
	}
	v := New(f)
	got := v.Check(context.Background(), protocol.VerifyCheck{
		Check:    protocol.CheckNoLeftoverIndexes,
		Relation: ptr(table()),
	})
	mustFail(t, got)
	if !strings.Contains(got.Reason, "carry 20 REINDEX CONCURRENTLY leftover indexes") {
		t.Fatalf("reason does not state the true total: %q", got.Reason)
	}
	if !strings.Contains(got.Reason, "and 12 more") {
		t.Fatalf("reason does not elide past the cap: %q", got.Reason)
	}
	if n := strings.Count(got.Reason, "(ccnew)"); n != maxNamedLeftovers {
		t.Fatalf("named %d leftovers, want %d", n, maxNamedLeftovers)
	}
}

// TestCheckWithUnqualifiedNames covers a plan or CLI flag that names an object
// without a schema: the catalog resolves it, and the comparison must not then
// reject the qualified name it got back.
func TestCheckWithUnqualifiedNames(t *testing.T) {
	f := healthyCatalog(2)
	v := New(f)
	child := protocol.NewObjectName("", childIndex(leaf(1)).Name)
	parent := protocol.NewObjectName("", testParentIndex)

	mustPass(t, v.Check(context.Background(), protocol.VerifyCheck{
		Check: protocol.CheckIndexValid, Index: &child,
	}))
	mustPass(t, v.Check(context.Background(), protocol.VerifyCheck{
		Check: protocol.CheckIndexAttached, Index: &child, ParentIndex: &parent,
	}))
}

// TestCheckWithoutCatalog proves a misconfigured verifier reports a cause
// instead of panicking inside a CLI.
func TestCheckWithoutCatalog(t *testing.T) {
	var v *Verifier
	got := v.Check(context.Background(), protocol.VerifyCheck{
		Check: protocol.CheckIndexValid, Index: ptr(parentIndex()),
	})
	mustError(t, got)

	got = New(nil).Check(context.Background(), protocol.VerifyCheck{
		Check: protocol.CheckIndexValid, Index: ptr(parentIndex()),
	})
	mustError(t, got)
}

// TestCheckHonoursContextCancellation proves a cancelled run reports why rather
// than issuing another query.
func TestCheckHonoursContextCancellation(t *testing.T) {
	f := healthyCatalog(2)
	v := New(f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := v.Check(ctx, protocol.VerifyCheck{Check: protocol.CheckIndexValid, Index: ptr(parentIndex())})
	mustError(t, got)
	if !errors.Is(got.Err(), context.Canceled) {
		t.Fatalf("cause = %v, want context.Canceled", got.Err())
	}
	if len(f.calls) != 0 {
		t.Fatalf("catalog was read after cancellation: %v", f.calls)
	}
}

// TestCheckDoesNotAliasItsInput proves a Result holds its own copies, so a
// caller reusing a VerifyCheck cannot rewrite a report that was already made.
func TestCheckDoesNotAliasItsInput(t *testing.T) {
	f := healthyCatalog(1)
	index := childIndex(leaf(1))
	c := protocol.VerifyCheck{Check: protocol.CheckIndexValid, Index: &index}
	got := New(f).Check(context.Background(), c)
	mustPass(t, got)

	index.Name = "rewritten"
	if got.Index.Name == "rewritten" {
		t.Fatal("Result.Index aliases the caller's check")
	}
}

// TestVerifyReportsEveryCheckEvenAfterOneFails is the behaviour FR-CLI-14 asks
// for: the operator gets the whole picture, not the first symptom.
func TestVerifyReportsEveryCheckEvenAfterOneFails(t *testing.T) {
	f := healthyCatalog(2).
		mutate(childIndex(leaf(1)), func(s *IndexState) { s.Valid = false }).
		detach(childIndex(leaf(2)))
	v := New(f)

	params := &protocol.VerifyParams{Checks: []protocol.VerifyCheck{
		{Check: protocol.CheckIndexValid, Index: ptr(childIndex(leaf(1)))},
		{Check: protocol.CheckIndexValid, Index: ptr(childIndex(leaf(2)))},
		{Check: protocol.CheckIndexAttached, Index: ptr(childIndex(leaf(2))), ParentIndex: ptr(parentIndex())},
		{Check: protocol.CheckParentIndexValid, ParentIndex: ptr(parentIndex())},
	}}
	r := v.Verify(context.Background(), params)

	if got := len(r.Results); got != 4 {
		t.Fatalf("evaluated %d checks, want 4", got)
	}
	want := []Status{StatusFail, StatusPass, StatusFail, StatusPass}
	for i, w := range want {
		if r.Results[i].Status != w {
			t.Fatalf("result %d: status = %s, want %s (%s)", i, r.Results[i].Status, w, r.Results[i].Reason)
		}
	}
	if r.Passed() {
		t.Fatal("report passed with two failures in it")
	}
	if !errors.Is(r.Err(), protocol.ErrVerificationFailed) {
		t.Fatalf("Err() = %v, want ErrVerificationFailed", r.Err())
	}
	if code := protocol.ExitCodeFor(r.Err()); code != protocol.ExitVerificationFailed {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitVerificationFailed)
	}
}

func TestVerifyWithNilParams(t *testing.T) {
	r := New(healthyCatalog(1)).Verify(context.Background(), nil)
	if len(r.Results) != 1 || r.Results[0].Status != StatusError {
		t.Fatalf("got %+v, want one error result", r.Results)
	}
}

// ---------------------------------------------------------------------------
// Node and plan entry points
// ---------------------------------------------------------------------------

func verifyNode(id string, deps []protocol.NodeID, checks ...protocol.VerifyCheck) protocol.Node {
	return protocol.Node{
		ID:        protocol.NodeID(id),
		Kind:      protocol.KindIndexVerify,
		Params:    &protocol.VerifyParams{Checks: checks},
		DependsOn: deps,
	}
}

func testPlan(nodes ...protocol.Node) *protocol.Plan {
	return &protocol.Plan{
		FormatVersion: protocol.PlanFormatVersion,
		PlanID:        "plan-verifier-test",
		Operation:     protocol.OpCreateIndex,
		Target:        protocol.Target{Database: "app", Table: table(), Index: ptr(parentIndex())},
		CreatedAt:     protocol.NewTimestamp(mustTime("2026-08-07T00:00:00Z")),
		Nodes:         nodes,
		// FR-PLANFILE-4: Validate requires a fingerprint.
		TopologyFingerprint: protocol.FingerprintPrefix +
			"0000000000000000000000000000000000000000000000000000000000000000",
	}
}

func TestVerifyNodeStampsTheNodeID(t *testing.T) {
	n := verifyNode("verify-leaf-1", nil,
		protocol.VerifyCheck{Check: protocol.CheckIndexValid, Index: ptr(childIndex(leaf(1)))})
	r, err := New(healthyCatalog(2)).VerifyNode(context.Background(), &n)
	if err != nil {
		t.Fatalf("VerifyNode: %v", err)
	}
	if len(r.Results) != 1 || r.Results[0].NodeID != "verify-leaf-1" {
		t.Fatalf("got %+v, want one result stamped with the node id", r.Results)
	}
}

func TestVerifyNodeRejectsTheWrongKind(t *testing.T) {
	n := protocol.Node{
		ID:     "wait-1",
		Kind:   protocol.KindWait,
		Params: &protocol.WaitParams{Seconds: 1},
	}
	if _, err := New(healthyCatalog(1)).VerifyNode(context.Background(), &n); !errors.Is(err, protocol.ErrInvalidPlan) {
		t.Fatalf("err = %v, want ErrInvalidPlan", err)
	}
	if _, err := New(healthyCatalog(1)).VerifyNode(context.Background(), nil); !errors.Is(err, protocol.ErrInvalidPlan) {
		t.Fatalf("err on nil node = %v, want ErrInvalidPlan", err)
	}
}

// TestVerifyPlanWalksVerifyNodesInTopologicalOrder covers FR-VER-5: the standalone
// command evaluates a completed plan's assertions, in the order the executor
// would have run them, issuing no DDL.
func TestVerifyPlanWalksVerifyNodesInTopologicalOrder(t *testing.T) {
	// Declared out of order on purpose: the final whole-tree verify is listed
	// first, but depends on the per-leaf verifies.
	p := testPlan(
		verifyNode("verify-tree", []protocol.NodeID{"verify-leaf-1", "verify-leaf-2"},
			protocol.VerifyCheck{Check: protocol.CheckParentIndexValid, ParentIndex: ptr(parentIndex())},
			protocol.VerifyCheck{
				Check:         protocol.CheckLeafIndexCount,
				ParentIndex:   ptr(parentIndex()),
				ExpectedCount: ptr(2),
			}),
		verifyNode("verify-leaf-1", nil,
			protocol.VerifyCheck{Check: protocol.CheckIndexValid, Index: ptr(childIndex(leaf(1)))}),
		verifyNode("verify-leaf-2", nil,
			protocol.VerifyCheck{Check: protocol.CheckIndexValid, Index: ptr(childIndex(leaf(2)))}),
		protocol.Node{
			ID:     "pause",
			Kind:   protocol.KindWait,
			Params: &protocol.WaitParams{Seconds: 30},
		},
	)
	if err := p.Validate(); err != nil {
		t.Fatalf("fixture plan is invalid: %v", err)
	}

	r, err := New(healthyCatalog(2)).VerifyPlan(context.Background(), p)
	if err != nil {
		t.Fatalf("VerifyPlan: %v", err)
	}
	gotOrder := make([]string, 0, len(r.Results))
	for _, res := range r.Results {
		gotOrder = append(gotOrder, string(res.NodeID)+"/"+string(res.Check))
	}
	want := []string{
		"verify-leaf-1/index_valid",
		"verify-leaf-2/index_valid",
		"verify-tree/parent_index_valid",
		"verify-tree/leaf_index_count",
	}
	if fmt.Sprint(gotOrder) != fmt.Sprint(want) {
		t.Fatalf("order = %v, want %v", gotOrder, want)
	}
	if !r.Passed() {
		t.Fatalf("healthy catalog did not pass: %s", r.Summary())
	}
	if err := r.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
}

// TestVerifyPlanOnAnIncompleteBuild is the AC-7 / partial-create shape: exactly
// the checks that should fail do, and no others.
func TestVerifyPlanOnAnIncompleteBuild(t *testing.T) {
	p := testPlan(
		verifyNode("verify-leaf-1", nil,
			protocol.VerifyCheck{Check: protocol.CheckIndexValid, Index: ptr(childIndex(leaf(1)))}),
		verifyNode("verify-leaf-2", nil,
			protocol.VerifyCheck{Check: protocol.CheckIndexValid, Index: ptr(childIndex(leaf(2)))}),
		verifyNode("verify-tree", []protocol.NodeID{"verify-leaf-1", "verify-leaf-2"},
			protocol.VerifyCheck{Check: protocol.CheckParentIndexValid, ParentIndex: ptr(parentIndex())}),
	)
	// The second leaf never finished: its index is invalid and unattached, so
	// PostgreSQL never marked the parent valid.
	f := healthyCatalog(2).
		mutate(childIndex(leaf(2)), func(s *IndexState) { s.Valid = false }).
		detach(childIndex(leaf(2))).
		mutate(parentIndex(), func(s *IndexState) { s.Valid = false })

	r, err := New(f).VerifyPlan(context.Background(), p)
	if err != nil {
		t.Fatalf("VerifyPlan: %v", err)
	}
	c := r.Counts()
	if c.Total != 3 || c.Passed != 1 || c.Failed != 2 || c.Errored != 0 {
		t.Fatalf("counts = %+v, want 3 total / 1 passed / 2 failed", c)
	}
}

func TestVerifyPlanWithNoVerifyNodesPassesVacuously(t *testing.T) {
	p := testPlan(protocol.Node{
		ID:     "pause",
		Kind:   protocol.KindWait,
		Params: &protocol.WaitParams{Seconds: 1},
	})
	r, err := New(healthyCatalog(1)).VerifyPlan(context.Background(), p)
	if err != nil {
		t.Fatalf("VerifyPlan: %v", err)
	}
	if !r.Passed() || r.Counts().Total != 0 {
		t.Fatalf("got %s, want an empty vacuously passing report", r.Summary())
	}
	if err := r.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
}

func TestVerifyPlanRejectsNilAndCyclicPlans(t *testing.T) {
	if _, err := New(healthyCatalog(1)).VerifyPlan(context.Background(), nil); !errors.Is(err, protocol.ErrInvalidPlan) {
		t.Fatalf("err = %v, want ErrInvalidPlan", err)
	}
	cyclic := testPlan(
		verifyNode("a", []protocol.NodeID{"b"},
			protocol.VerifyCheck{Check: protocol.CheckIndexValid, Index: ptr(parentIndex())}),
		verifyNode("b", []protocol.NodeID{"a"},
			protocol.VerifyCheck{Check: protocol.CheckIndexValid, Index: ptr(parentIndex())}),
	)
	if _, err := New(healthyCatalog(1)).VerifyPlan(context.Background(), cyclic); !errors.Is(err, protocol.ErrInvalidPlan) {
		t.Fatalf("err = %v, want ErrInvalidPlan", err)
	}
}

// TestVerifierIssuesOnlyReads is the structural half of FR-VER-5: the Catalog
// the verifier holds exposes reads and nothing else, so `verify` cannot issue
// DDL even by mistake.
func TestVerifierIssuesOnlyReads(t *testing.T) {
	f := healthyCatalog(3)
	v := New(f)
	if _, err := v.VerifyPartitionedIndex(context.Background(), table(), parentIndex()); err != nil {
		t.Fatalf("VerifyPartitionedIndex: %v", err)
	}
	if len(f.calls) == 0 {
		t.Fatal("the catalog was never read")
	}
	for _, c := range f.calls {
		switch {
		case strings.HasPrefix(c, "Index("),
			strings.HasPrefix(c, "IndexParent("),
			strings.HasPrefix(c, "AttachedIndexes("),
			strings.HasPrefix(c, "LeafPartitions("),
			strings.HasPrefix(c, "TreeIndexes("):
		default:
			t.Fatalf("unexpected catalog call %q", c)
		}
	}
}

// TestVerifyNodeRejectsMismatchedParams guards the case a hand-edited plan can
// reach: the declared kind is index.verify but the params are another kind's.
func TestVerifyNodeRejectsMismatchedParams(t *testing.T) {
	n := protocol.Node{
		ID:     "verify-1",
		Kind:   protocol.KindIndexVerify,
		Params: &protocol.WaitParams{Seconds: 5},
	}
	if _, err := New(healthyCatalog(1)).VerifyNode(context.Background(), &n); !errors.Is(err, protocol.ErrInvalidPlan) {
		t.Fatalf("err = %v, want ErrInvalidPlan", err)
	}

	p := testPlan(n)
	if _, err := New(healthyCatalog(1)).VerifyPlan(context.Background(), p); !errors.Is(err, protocol.ErrInvalidPlan) {
		t.Fatalf("VerifyPlan err = %v, want ErrInvalidPlan", err)
	}
}
