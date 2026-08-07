package verifier

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// resultsByCheck groups a report by check kind so a test can assert on one
// assertion without depending on the position of the others.
func resultsByCheck(r Report, kind protocol.VerifyCheckKind) []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Check == kind {
			out = append(out, res)
		}
	}
	return out
}

func TestVerifyPartitionedIndexOnACompletedBuild(t *testing.T) {
	r, err := New(healthyCatalog(3)).VerifyPartitionedIndex(context.Background(), table(), parentIndex())
	if err != nil {
		t.Fatalf("VerifyPartitionedIndex: %v", err)
	}
	if !r.Passed() {
		t.Fatalf("a completed build did not pass: %s\n%+v", r.Summary(), r.Failures())
	}
	// One parent-validity check, one count check, and an attached + valid pair
	// per leaf: FR-VER-1, FR-VER-2, FR-VER-3 and FR-VER-4 all reported
	// individually.
	if got := r.Counts().Total; got != 2+2*3 {
		t.Fatalf("total checks = %d, want %d", got, 2+2*3)
	}
	if got := len(resultsByCheck(r, protocol.CheckParentIndexValid)); got != 1 {
		t.Fatalf("parent_index_valid checks = %d, want 1", got)
	}
	if got := len(resultsByCheck(r, protocol.CheckLeafIndexCount)); got != 1 {
		t.Fatalf("leaf_index_count checks = %d, want 1", got)
	}
	if got := len(resultsByCheck(r, protocol.CheckIndexAttached)); got != 3 {
		t.Fatalf("index_attached checks = %d, want 3", got)
	}
	if got := len(resultsByCheck(r, protocol.CheckIndexValid)); got != 3 {
		t.Fatalf("index_valid checks = %d, want 3", got)
	}
}

// TestVerifyPartitionedIndexFailuresInIsolation breaks exactly one thing at a
// time and asserts exactly one assertion notices. This is the property that
// makes a gate's failure message actionable (FR-LB-6).
func TestVerifyPartitionedIndexFailuresInIsolation(t *testing.T) {
	tests := []struct {
		name       string
		req        string
		catalog    func() *fakeCatalog
		wantFailed int
		wantCheck  protocol.VerifyCheckKind
		reason     string
	}{
		{
			name: "a leaf index is invalid",
			req:  "FR-VER-1",
			catalog: func() *fakeCatalog {
				return healthyCatalog(3).mutate(childIndex(leaf(2)), func(s *IndexState) { s.Valid = false })
			},
			wantFailed: 1,
			wantCheck:  protocol.CheckIndexValid,
			reason:     "indisvalid=false",
		},
		{
			name: "a leaf index is not ready",
			req:  "FR-VER-1",
			catalog: func() *fakeCatalog {
				return healthyCatalog(3).mutate(childIndex(leaf(1)), func(s *IndexState) { s.Ready = false })
			},
			wantFailed: 1,
			wantCheck:  protocol.CheckIndexValid,
			reason:     "indisready=false",
		},
		{
			name: "a leaf index is not live",
			req:  "FR-VER-1",
			catalog: func() *fakeCatalog {
				return healthyCatalog(3).mutate(childIndex(leaf(3)), func(s *IndexState) { s.Live = false })
			},
			wantFailed: 1,
			wantCheck:  protocol.CheckIndexValid,
			reason:     "indislive=false",
		},
		{
			// Detaching one child breaks the attachment for that partition and
			// the tree-wide count: two assertions, both true, neither
			// duplicating the other.
			name:       "a leaf index was built but never attached",
			req:        "FR-VER-2",
			catalog:    func() *fakeCatalog { return healthyCatalog(3).detach(childIndex(leaf(2))) },
			wantFailed: 2,
			wantCheck:  protocol.CheckIndexAttached,
			reason:     "carries no index attached to",
		},
		{
			name: "the parent index never became valid",
			req:  "FR-VER-3",
			catalog: func() *fakeCatalog {
				return healthyCatalog(3).mutate(parentIndex(), func(s *IndexState) { s.Valid = false })
			},
			wantFailed: 1,
			wantCheck:  protocol.CheckParentIndexValid,
			reason:     "is not valid",
		},
		{
			name: "a partition was added after the build finished",
			req:  "FR-VER-4",
			catalog: func() *fakeCatalog {
				f := healthyCatalog(3)
				// A fourth partition exists with no index on it at all: the
				// drift a time-partitioned table produces between plan and
				// verify.
				f.setLeaves(table(), leaf(1), leaf(2), leaf(3), leaf(4))
				return f
			},
			wantFailed: 2,
			wantCheck:  protocol.CheckLeafIndexCount,
			reason:     "has 3 attached leaf indexes, expected 4",
		},
		{
			name:       "the parent index does not exist",
			req:        "FR-VER-3",
			catalog:    func() *fakeCatalog { return healthyCatalog(3).dropIndex(parentIndex()) },
			wantFailed: 2,
			wantCheck:  protocol.CheckParentIndexValid,
			reason:     "does not exist",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := New(tc.catalog()).VerifyPartitionedIndex(context.Background(), table(), parentIndex())
			if err != nil {
				t.Fatalf("VerifyPartitionedIndex: %v", err)
			}
			c := r.Counts()
			if c.Failed != tc.wantFailed {
				t.Fatalf("%s: %d checks failed, want %d\n%+v", tc.req, c.Failed, tc.wantFailed, r.Failures())
			}
			if c.Errored != 0 {
				t.Fatalf("%s: %d checks errored, want none", tc.req, c.Errored)
			}
			var found bool
			for _, res := range r.Failures() {
				if res.Check == tc.wantCheck && strings.Contains(res.Reason, tc.reason) {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s: no %s failure containing %q\n%+v", tc.req, tc.wantCheck, tc.reason, r.Failures())
			}
			if !errors.Is(r.Err(), protocol.ErrVerificationFailed) {
				t.Fatalf("%s: Err() = %v, want ErrVerificationFailed", tc.req, r.Err())
			}
		})
	}
}

// TestVerifyPartitionedIndexIteratesPartitionsNotChildren is the reason the
// gate discovers leaf partitions instead of walking pg_inherits alone: a
// partition with no index at all is invisible from the children's side.
func TestVerifyPartitionedIndexIteratesPartitionsNotChildren(t *testing.T) {
	f := healthyCatalog(3).dropIndex(childIndex(leaf(2)))
	r, err := New(f).VerifyPartitionedIndex(context.Background(), table(), parentIndex())
	if err != nil {
		t.Fatalf("VerifyPartitionedIndex: %v", err)
	}
	var named bool
	for _, res := range r.Failures() {
		if res.Check == protocol.CheckIndexAttached && strings.Contains(res.Reason, leaf(2).String()) {
			named = true
		}
	}
	if !named {
		t.Fatalf("the indexless partition was not named:\n%+v", r.Failures())
	}
}

func TestVerifyPartitionedIndexSurfacesDiscoveryFailure(t *testing.T) {
	f := healthyCatalog(3)
	f.failLeaves = errCatalogDown
	if _, err := New(f).VerifyPartitionedIndex(context.Background(), table(), parentIndex()); !errors.Is(err, errCatalogDown) {
		t.Fatalf("err = %v, want the catalog error", err)
	}

	f = healthyCatalog(3)
	f.failAttached = errCatalogDown
	if _, err := New(f).VerifyPartitionedIndex(context.Background(), table(), parentIndex()); !errors.Is(err, errCatalogDown) {
		t.Fatalf("err = %v, want the catalog error", err)
	}

	if _, err := New(nil).VerifyPartitionedIndex(context.Background(), table(), parentIndex()); err == nil {
		t.Fatal("a verifier with no catalog should not report a pass")
	}
}

// TestVerifyPartitionedIndexIsDeterministic keeps a 400-partition report
// diffable: two runs over the same catalog must produce identical bytes.
func TestVerifyPartitionedIndexIsDeterministic(t *testing.T) {
	first, err := New(healthyCatalog(25)).VerifyPartitionedIndex(context.Background(), table(), parentIndex())
	if err != nil {
		t.Fatalf("VerifyPartitionedIndex: %v", err)
	}
	second, err := New(healthyCatalog(25)).VerifyPartitionedIndex(context.Background(), table(), parentIndex())
	if err != nil {
		t.Fatalf("VerifyPartitionedIndex: %v", err)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatal("two reports over the same catalog differ")
	}
}

// ---------------------------------------------------------------------------
// FR-DROP-7
// ---------------------------------------------------------------------------

func TestVerifyIndexAbsentAfterADrop(t *testing.T) {
	f := healthyCatalog(3)
	// DROP INDEX on the parent cascades to every attached child.
	f.dropIndex(parentIndex())
	for i := 1; i <= 3; i++ {
		f.dropIndex(childIndex(leaf(i)))
	}

	r, err := New(f).VerifyIndexAbsent(context.Background(), table(), parentIndex())
	if err != nil {
		t.Fatalf("VerifyIndexAbsent: %v", err)
	}
	if !r.Passed() {
		t.Fatalf("post-drop catalog did not pass: %+v", r.Failures())
	}
	if got := r.Counts().Total; got != 1+3 {
		t.Fatalf("total checks = %d, want 4 (parent plus one per leaf)", got)
	}
}

func TestVerifyIndexAbsentFailuresInIsolation(t *testing.T) {
	t.Run("the parent index survives", func(t *testing.T) {
		f := healthyCatalog(2)
		for i := 1; i <= 2; i++ {
			f.dropIndex(childIndex(leaf(i)))
		}
		r, err := New(f).VerifyIndexAbsent(context.Background(), table(), parentIndex())
		if err != nil {
			t.Fatalf("VerifyIndexAbsent: %v", err)
		}
		if c := r.Counts(); c.Failed != 1 {
			t.Fatalf("%d failures, want 1: %+v", c.Failed, r.Failures())
		}
		if !strings.Contains(r.Failures()[0].Reason, testParentIndex) {
			t.Fatalf("the surviving parent was not named: %q", r.Failures()[0].Reason)
		}
	})

	t.Run("an unattached leaf orphan survives the cascade", func(t *testing.T) {
		// The residue an abandoned create leaves behind: a leaf index built but
		// never attached, so DROP INDEX on the parent never reached it
		// (TRD §7.2.13, DropPartitionedIndex step 1).
		f := healthyCatalog(2)
		f.dropIndex(parentIndex())
		f.dropIndex(childIndex(leaf(1)))
		f.detach(childIndex(leaf(2)))

		r, err := New(f).VerifyIndexAbsent(context.Background(), table(), parentIndex())
		if err != nil {
			t.Fatalf("VerifyIndexAbsent: %v", err)
		}
		if c := r.Counts(); c.Failed != 1 {
			t.Fatalf("%d failures, want 1: %+v", c.Failed, r.Failures())
		}
		got := r.Failures()[0]
		if got.Check != protocol.CheckIndexAbsent || !strings.Contains(got.Reason, childIndex(leaf(2)).Name) {
			t.Fatalf("the orphan was not named: %+v", got)
		}
	})
}

// TestVerifyIndexAbsentRegeneratesPlannerNames pins the coupling that makes
// FR-DROP-7 answerable at all: once the parent is gone there is nothing left in
// pg_inherits to enumerate the children from, so their names are rebuilt with
// the same deterministic function the planner used (FR-PLAN-11).
func TestVerifyIndexAbsentRegeneratesPlannerNames(t *testing.T) {
	f := newFakeCatalog()
	f.setLeaves(table(), leaf(1))
	r, err := New(f).VerifyIndexAbsent(context.Background(), table(), parentIndex())
	if err != nil {
		t.Fatalf("VerifyIndexAbsent: %v", err)
	}
	want := protocol.ChildIndexName(testParentIndex, leaf(1).Name)
	var found bool
	for _, res := range r.Results {
		if res.Index != nil && res.Index.Name == want && res.Index.Schema == leaf(1).Schema {
			found = true
		}
	}
	if !found {
		t.Fatalf("no check for the generated child index name %q: %+v", want, r.Results)
	}
}

func TestVerifyIndexAbsentSurfacesDiscoveryFailure(t *testing.T) {
	f := healthyCatalog(2)
	f.failLeaves = errCatalogDown
	if _, err := New(f).VerifyIndexAbsent(context.Background(), table(), parentIndex()); !errors.Is(err, errCatalogDown) {
		t.Fatalf("err = %v, want the catalog error", err)
	}
	if _, err := New(nil).VerifyIndexAbsent(context.Background(), table(), parentIndex()); err == nil {
		t.Fatal("a verifier with no catalog should not report a pass")
	}
}

// ---------------------------------------------------------------------------
// FR-REIDX-6
// ---------------------------------------------------------------------------

func TestVerifyNoLeftovers(t *testing.T) {
	r := New(healthyCatalog(3)).VerifyNoLeftovers(context.Background(), table())
	if !r.Passed() || r.Counts().Total != 1 {
		t.Fatalf("clean tree: %s %+v", r.Summary(), r.Failures())
	}

	f := healthyCatalog(3)
	f.putIndex(usable(leftoverName(childIndex(leaf(1)), "_ccold2"), leaf(1)))
	r = New(f).VerifyNoLeftovers(context.Background(), table())
	if r.Passed() {
		t.Fatal("a _ccold2 leftover was not reported")
	}
	if !errors.Is(r.Err(), protocol.ErrVerificationFailed) {
		t.Fatalf("Err() = %v, want ErrVerificationFailed", r.Err())
	}
}

// TestVerifyReindexedIndex covers FR-REIDX-6 end to end: valid, ready, live,
// still attached, parent still valid, and no leftovers.
func TestVerifyReindexedIndex(t *testing.T) {
	r, err := New(healthyCatalog(3)).VerifyReindexedIndex(context.Background(), table(), parentIndex())
	if err != nil {
		t.Fatalf("VerifyReindexedIndex: %v", err)
	}
	if !r.Passed() {
		t.Fatalf("a completed reindex did not pass: %+v", r.Failures())
	}
	if got := len(resultsByCheck(r, protocol.CheckNoLeftoverIndexes)); got != 1 {
		t.Fatalf("no_leftover_indexes checks = %d, want 1", got)
	}

	t.Run("attachment lost across the swap", func(t *testing.T) {
		// The §16.4 spike's failure mode: REINDEX CONCURRENTLY succeeds but the
		// leaf comes back detached from its parent.
		f := healthyCatalog(3).detach(childIndex(leaf(2)))
		r, err := New(f).VerifyReindexedIndex(context.Background(), table(), parentIndex())
		if err != nil {
			t.Fatalf("VerifyReindexedIndex: %v", err)
		}
		if r.Passed() {
			t.Fatal("a detached leaf passed a reindex verification")
		}
	})

	t.Run("a leftover remains", func(t *testing.T) {
		f := healthyCatalog(3)
		f.putIndex(usable(leftoverName(childIndex(leaf(3)), "_ccnew"), leaf(3)))
		r, err := New(f).VerifyReindexedIndex(context.Background(), table(), parentIndex())
		if err != nil {
			t.Fatalf("VerifyReindexedIndex: %v", err)
		}
		failures := r.Failures()
		if len(failures) != 1 || failures[0].Check != protocol.CheckNoLeftoverIndexes {
			t.Fatalf("want exactly one leftover failure, got %+v", failures)
		}
	})

	t.Run("discovery failure surfaces", func(t *testing.T) {
		f := healthyCatalog(3)
		f.failLeaves = errCatalogDown
		if _, err := New(f).VerifyReindexedIndex(context.Background(), table(), parentIndex()); !errors.Is(err, errCatalogDown) {
			t.Fatalf("err = %v, want the catalog error", err)
		}
	})
}

// TestGatesOrderAcrossSchemas covers a tree whose partitions live in more than
// one schema: the report still has to be stable between runs.
func TestGatesOrderAcrossSchemas(t *testing.T) {
	f := newFakeCatalog()
	parent := usable(parentIndex(), table())
	parent.Partitioned = true
	f.putIndex(parent)

	hot := protocol.NewObjectName("hot", "orders_2026_02")
	cold := protocol.NewObjectName("archive", "orders_2025_12")
	for _, l := range []protocol.ObjectName{hot, cold} {
		c := protocol.NewObjectName(l.Schema, protocol.ChildIndexName(testParentIndex, l.Name))
		f.putIndex(usable(c, l)).attach(c, parentIndex())
	}
	f.setLeaves(table(), hot, cold)

	r, err := New(f).VerifyPartitionedIndex(context.Background(), table(), parentIndex())
	if err != nil {
		t.Fatalf("VerifyPartitionedIndex: %v", err)
	}
	if !r.Passed() {
		t.Fatalf("multi-schema tree did not pass: %+v", r.Failures())
	}
	// archive sorts before hot, so the archive partition's checks come first.
	var order []string
	for _, res := range r.Results {
		if res.Relation != nil {
			order = append(order, res.Relation.Schema)
		}
	}
	if len(order) < 2 || order[0] != "archive" || order[len(order)-1] != "hot" {
		t.Fatalf("results are not ordered by schema: %v", order)
	}
}

// TestVerifyIndexAbsentForPlanUsesTheRecordedNames is FR-PLAN-13: the recorded
// child index name is authoritative "rather than deriving it again at execution
// time".
//
// The scenario the re-derivation misses: a create is planned and partly
// executed, so public.orders_idx_leaf_1 exists unattached. The DBA then renames
// the partition. `verify --expect-absent` re-derives from the *live* leaf set,
// looks for a name built from the new partition name, finds it absent, and
// reports PASS. The parent assertion passes too, because the parent really is
// gone. Everything is green and the orphan survives, holding disk and write
// overhead on every insert.
func TestVerifyIndexAbsentForPlanUsesTheRecordedNames(t *testing.T) {
	f := healthyCatalog(1)
	f.dropIndex(parentIndex())

	// The child the plan built survives, unattached.
	orphan := childIndex(leaf(1))
	f.detach(orphan)

	// The partition has since been renamed, so re-derivation generates a name
	// for a leaf that carries no index.
	renamed := protocol.NewObjectName(leaf(1).Schema, leaf(1).Name+"_2025")
	f.setLeaves(table(), renamed)

	plan := absencePlan(t, orphan)

	// The re-deriving form is fooled, and that is the blind spot this test
	// exists to pin: it must keep reporting a clean bill of health here, or the
	// plan-driven assertion below is proving nothing.
	derived, err := New(f).VerifyIndexAbsent(context.Background(), table(), parentIndex())
	if err != nil {
		t.Fatalf("VerifyIndexAbsent: %v", err)
	}
	if !derived.Passed() {
		t.Fatalf("the fixture no longer reproduces the re-derivation blind spot, so the "+
			"comparison below is vacuous: %+v", derived.Failures())
	}

	// The plan-driven form is not.
	r, err := New(f).VerifyIndexAbsentForPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("VerifyIndexAbsentForPlan: %v", err)
	}
	if r.Passed() {
		t.Fatalf("the surviving orphan %s was reported absent; the check derived names from the "+
			"live leaf set instead of reading the ones the plan recorded (FR-PLAN-13)", orphan)
	}
	named := false
	for _, fl := range r.Failures() {
		if strings.Contains(fl.Reason, orphan.Name) {
			named = true
		}
	}
	if !named {
		t.Errorf("the failure does not name %s: %+v", orphan, r.Failures())
	}
}

// TestVerifyIndexAbsentForPlanPassesOnACleanCatalog keeps the union from being
// a permanent failure: with nothing left behind, everything is absent.
func TestVerifyIndexAbsentForPlanPassesOnACleanCatalog(t *testing.T) {
	f := healthyCatalog(2)
	f.dropIndex(parentIndex())
	for i := 1; i <= 2; i++ {
		f.dropIndex(childIndex(leaf(i)))
	}
	plan := absencePlan(t, childIndex(leaf(1)), childIndex(leaf(2)))

	r, err := New(f).VerifyIndexAbsentForPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("VerifyIndexAbsentForPlan: %v", err)
	}
	if !r.Passed() {
		t.Fatalf("a fully dropped catalog did not pass: %+v", r.Failures())
	}
}

// absencePlan builds a sealed create plan that records the given child indexes.
func absencePlan(t *testing.T, children ...protocol.ObjectName) *protocol.Plan {
	t.Helper()
	idx := parentIndex()
	nodes := make([]protocol.Node, 0, len(children))
	for i, c := range children {
		nodes = append(nodes, protocol.Node{
			ID:   protocol.NodeID("create:" + c.Name),
			Kind: protocol.KindIndexCreateConcurrently,
			Params: &protocol.CreateConcurrentlyParams{
				Partition: leaf(i + 1),
				Index:     c,
				Definition: protocol.IndexDefinition{
					Method:  "btree",
					Columns: []protocol.IndexColumn{{Name: "created_at"}},
				},
			},
		})
	}
	p := testPlan(nodes...)
	p.Target.Index = &idx
	if err := p.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return p
}
