package reindexindex

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/atulsinha87/partitionctl/engine/planner"
	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

const (
	ownerOID    uint32 = 100
	rootOID     uint32 = 1000
	parentIdxID uint32 = 2000
	leafOIDBase uint32 = 1100
	leafIdxBase uint32 = 2100
)

func obj(name string) protocol.ObjectName { return protocol.NewObjectName("public", name) }

var (
	table       = obj("events")
	parentIndex = obj("events_created_idx")
)

func leafName(i int) protocol.ObjectName {
	return obj("events_p" + twoDigit(i))
}

func leafIndexName(i int) protocol.ObjectName {
	return obj("events_p" + twoDigit(i) + "_created_idx")
}

func twoDigit(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// markerText renders a well-formed leaf ownership marker. reindexed may be
// empty, which is what a leaf that was created but never rebuilt carries.
func markerText(t *testing.T, run, reindexed string) string {
	t.Helper()
	m := protocol.Marker{
		Run:    run,
		Plan:   "sha256:fixture",
		Op:     string(protocol.OpCreateIndex),
		Role:   protocol.MarkerRoleLeaf,
		Parent: parentIndex.String(),
		At:     "2026-08-01T00:00:00Z",
	}
	if reindexed != "" {
		m.Reindexed = reindexed
		m.ReindexRun = run + "-reindex"
	}
	text, err := protocol.FormatMarker(m)
	if err != nil {
		t.Fatalf("FormatMarker: %v", err)
	}
	return text
}

// fixture builds a partitioned table with n leaves, a valid partitioned index
// over it, and one attached leaf index per partition, every one of them marked
// as PartitionCTL's.
//
// FakeCatalog.IndexesOnRelations returns the Index values verbatim, so the
// ownership marker is set on Index.Comment directly. That is also how the SQL
// reader behaves: the comment rides along with the batched pass rather than
// costing a round trip per leaf (NFR-PERF-1).
func fixture(t *testing.T, n int) *planner.FakeCatalog {
	t.Helper()
	cat := planner.NewFakeCatalog()
	cat.AddRole(ownerOID, "app_owner", true)
	cat.AddRelation(planner.Relation{
		OID: rootOID, Name: table, Kind: planner.RelKindPartitionedTable, OwnerOID: ownerOID,
	})
	cat.SetStrategy(rootOID, protocol.StrategyRange)
	cat.AddIndex(planner.Index{
		OID: parentIdxID, Name: parentIndex, Kind: planner.RelKindPartitionedIndex,
		OwnerOID: ownerOID, TableOID: rootOID, Table: table,
		IsValid: true, IsReady: true, IsLive: true,
	})
	for i := 1; i <= n; i++ {
		loid := leafOIDBase + uint32(i)
		cat.AddRelation(planner.Relation{
			OID: loid, Name: leafName(i), Kind: planner.RelKindTable, OwnerOID: ownerOID,
			ParentOID: rootOID, PartitionBound: "FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')",
			RelPages: 1000,
		})
		cat.AddIndex(planner.Index{
			OID: leafIdxBase + uint32(i), Name: leafIndexName(i), Kind: planner.RelKindIndex,
			OwnerOID: ownerOID, TableOID: loid, Table: leafName(i),
			IsValid: true, IsReady: true, IsLive: true,
			ParentIndexOID: parentIdxID, RelPages: 200,
			Comment: markerText(t, "run-create", ""),
		})
	}
	return cat
}

// addLeftover attaches a REINDEX CONCURRENTLY leftover to a leaf. The suffix
// carries a disambiguating integer on purpose: PostgreSQL appends one when the
// plain name is taken, so any detection that matched a literal string would miss
// these (TRD §7.2.11).
func addLeftover(cat *planner.FakeCatalog, leaf int, suffix string, oid uint32) {
	cat.AddIndex(planner.Index{
		OID:  oid,
		Name: obj(leafIndexName(leaf).Name + suffix),
		Kind: planner.RelKindIndex, OwnerOID: ownerOID,
		TableOID: leafOIDBase + uint32(leaf), Table: leafName(leaf),
		IsReady: true, IsLive: true, RelPages: 200,
	})
}

func spec() planner.Specification {
	return planner.Specification{
		Operation: protocol.OpReindexIndex,
		Table:     table,
		Index:     parentIndex,
		Actor:     "tester",
	}
}

func run(t *testing.T, pl Planner, cat *planner.FakeCatalog, s planner.Specification) (*planner.Outcome, error) {
	t.Helper()
	h := &planner.Host{
		Catalog:   cat,
		Now:       func() time.Time { return time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC) },
		NewPlanID: func() protocol.PlanID { return "reindex-index-fixture" },
	}
	return h.Run(context.Background(), pl, s)
}

func mustRun(t *testing.T, pl Planner, cat *planner.FakeCatalog, s planner.Specification) *planner.Outcome {
	t.Helper()
	out, err := run(t, pl, cat, s)
	if err != nil {
		t.Fatalf("Plan: unexpected error: %v", err)
	}
	return out
}

func ids(p *protocol.Plan) []protocol.NodeID {
	out := make([]protocol.NodeID, len(p.Nodes))
	for i := range p.Nodes {
		out[i] = p.Nodes[i].ID
	}
	return out
}

func countKind(p *protocol.Plan, k protocol.NodeKind) int {
	n := 0
	for i := range p.Nodes {
		if p.Nodes[i].Kind == k {
			n++
		}
	}
	return n
}

func nodeByID(t *testing.T, p *protocol.Plan, id protocol.NodeID) protocol.Node {
	t.Helper()
	for i := range p.Nodes {
		if p.Nodes[i].ID == id {
			return p.Nodes[i]
		}
	}
	t.Fatalf("no node %q in plan; have %v", id, ids(p))
	return protocol.Node{}
}

func hasNode(p *protocol.Plan, id protocol.NodeID) bool {
	for i := range p.Nodes {
		if p.Nodes[i].ID == id {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Shape
// ---------------------------------------------------------------------------

func TestOperationIsReindexIndex(t *testing.T) {
	if got := New().Operation(); got != protocol.OpReindexIndex {
		t.Fatalf("Operation() = %q, want %q", got, protocol.OpReindexIndex)
	}
}

func TestTwelvePartitionsEmitOneChainPerLeaf(t *testing.T) {
	out := mustRun(t, New(), fixture(t, 12), spec())
	p := out.Plan

	if got := countKind(p, protocol.KindIndexReindexConcurrently); got != 12 {
		t.Errorf("reindex nodes = %d, want 12", got)
	}
	if got := countKind(p, protocol.KindIndexDropConcurrently); got != 0 {
		t.Errorf("drop nodes = %d, want 0", got)
	}
	if got := countKind(p, protocol.KindCatalogAssert); got != 1 {
		t.Errorf("assert nodes = %d, want 1", got)
	}
	// 12 per-leaf verifies plus the terminal one.
	if got := countKind(p, protocol.KindIndexVerify); got != 13 {
		t.Errorf("verify nodes = %d, want 13", got)
	}
	// No pacing was requested, so no wait nodes: a pause the operator did not
	// ask for is a pause they cannot see in the plan.
	if got := countKind(p, protocol.KindWait); got != 0 {
		t.Errorf("wait nodes = %d, want 0", got)
	}
	if got := len(p.Nodes); got != 1+12*2+1 {
		t.Errorf("total nodes = %d, want %d", got, 1+12*2+1)
	}
}

func TestEveryLeafChainHangsOffTheAssert(t *testing.T) {
	out := mustRun(t, New(), fixture(t, 12), spec())
	p := out.Plan

	for i := 1; i <= 12; i++ {
		reindex := nodeByID(t, p, nodeID("reindex", leafName(i)))
		if len(reindex.DependsOn) != 1 || reindex.DependsOn[0] != nodeAssert {
			t.Errorf("reindex for %s depends on %v, want [%s]", leafName(i), reindex.DependsOn, nodeAssert)
		}
		verify := nodeByID(t, p, nodeID("verify", leafName(i)))
		if len(verify.DependsOn) != 1 || verify.DependsOn[0] != reindex.ID {
			t.Errorf("verify for %s depends on %v, want [%s]", leafName(i), verify.DependsOn, reindex.ID)
		}
	}

	final := nodeByID(t, p, nodeFinalVerify)
	if len(final.DependsOn) != 12 {
		t.Fatalf("final verify has %d incoming edges, want 12", len(final.DependsOn))
	}
}

func TestLeafChainsAreIndependentSoTheyCanBePacedOrResumed(t *testing.T) {
	// Nothing chains leaf N to leaf N-1. That is deliberate: the leaves are
	// independent rebuilds, and the graph says so, which is what lets a resume
	// pick up mid-tree.
	out := mustRun(t, New(), fixture(t, 12), spec())
	for i := 2; i <= 12; i++ {
		n := nodeByID(t, out.Plan, nodeID("reindex", leafName(i)))
		for _, dep := range n.DependsOn {
			if strings.HasPrefix(string(dep), "verify:") || strings.HasPrefix(string(dep), "reindex:") {
				t.Fatalf("reindex for %s depends on another leaf's node %q", leafName(i), dep)
			}
		}
	}
}

func TestReindexNodeCarriesRelationParentAndPeakStorage(t *testing.T) {
	out := mustRun(t, New(), fixture(t, 12), spec())
	n := nodeByID(t, out.Plan, nodeID("reindex", leafName(4)))

	p, ok := n.Params.(*protocol.ReindexConcurrentlyParams)
	if !ok {
		t.Fatalf("params are %T, want *protocol.ReindexConcurrentlyParams", n.Params)
	}
	if p.Index != leafIndexName(4) {
		t.Errorf("index = %s, want %s", p.Index, leafIndexName(4))
	}
	if p.Relation == nil || *p.Relation != leafName(4) {
		t.Errorf("relation = %v, want %s", p.Relation, leafName(4))
	}
	if p.ParentIndex == nil || *p.ParentIndex != parentIndex {
		t.Errorf("parent_index = %v, want %s", p.ParentIndex, parentIndex)
	}
	if p.EstimatedPeakBytes <= 0 {
		t.Errorf("estimated_peak_bytes = %d, want > 0 (FR-REIDX-7)", p.EstimatedPeakBytes)
	}
	if n.EstimatedSeconds <= 0 {
		t.Errorf("estimated_seconds = %d, want > 0", n.EstimatedSeconds)
	}
	if n.Authorization != nil {
		t.Errorf("a reindex is not destructive and must carry no authorization; got %+v", n.Authorization)
	}
	if want := "REINDEX INDEX CONCURRENTLY " + leafIndexName(4).Quoted() + ";"; !strings.HasPrefix(n.RenderedSQL, want) {
		t.Errorf("rendered_sql = %q, want it to start with %q", n.RenderedSQL, want)
	}
}

// The plan artifact is the thing a human approves, and approval has to cover
// every statement the executor will send. index.reindex_concurrently writes an
// ownership marker after its DDL — a COMMENT that mutates catalog metadata the
// destructive-action table later reads as proof of ownership — and the preview
// used to show only the REINDEX. On the 12-partition fixture that hid 12
// catalog-mutating statements from the reviewer.
func TestReindexRenderedSQLShowsTheOwnershipMarkerStatement(t *testing.T) {
	out := mustRun(t, New(), fixture(t, 12), spec())

	for i := 1; i <= 12; i++ {
		n := nodeByID(t, out.Plan, nodeID("reindex", leafName(i)))
		if !n.Kind.ClaimsOwnership() {
			t.Fatalf("%s no longer claims ownership; this test is asserting the wrong thing", n.Kind)
		}
		want := "COMMENT ON INDEX " + leafIndexName(i).Quoted()
		if !strings.Contains(n.RenderedSQL, want) {
			t.Errorf("leaf %d rendered_sql does not show the marker statement the executor issues.\n"+
				"want a line containing %q\ngot:\n%s", i, want, n.RenderedSQL)
		}
	}
}

func TestLeafVerifyChecksValidityAndAttachment(t *testing.T) {
	// FR-REIDX-6. The attachment surviving REINDEX CONCURRENTLY's internal swap
	// is the property the v0.0 spike had to establish, so it is asserted on
	// every leaf rather than assumed.
	out := mustRun(t, New(), fixture(t, 3), spec())
	n := nodeByID(t, out.Plan, nodeID("verify", leafName(2)))

	p, ok := n.Params.(*protocol.VerifyParams)
	if !ok {
		t.Fatalf("params are %T, want *protocol.VerifyParams", n.Params)
	}
	if len(p.Checks) != 2 {
		t.Fatalf("checks = %d, want 2", len(p.Checks))
	}
	if p.Checks[0].Check != protocol.CheckIndexValid {
		t.Errorf("check[0] = %q, want %q", p.Checks[0].Check, protocol.CheckIndexValid)
	}
	if p.Checks[1].Check != protocol.CheckIndexAttached {
		t.Errorf("check[1] = %q, want %q", p.Checks[1].Check, protocol.CheckIndexAttached)
	}
	if p.Checks[1].ParentIndex == nil || *p.Checks[1].ParentIndex != parentIndex {
		t.Errorf("attachment check names parent %v, want %s", p.Checks[1].ParentIndex, parentIndex)
	}
}

func TestFinalVerifyCoversTheWholeTreeIncludingUntouchedLeaves(t *testing.T) {
	out := mustRun(t, New(), fixture(t, 12), spec())
	n := nodeByID(t, out.Plan, nodeFinalVerify)

	p := n.Params.(*protocol.VerifyParams)
	var parentValid, leafCount, leftover int
	for _, c := range p.Checks {
		switch c.Check {
		case protocol.CheckParentIndexValid:
			parentValid++
		case protocol.CheckLeafIndexCount:
			leafCount++
			if c.ExpectedCount == nil || *c.ExpectedCount != 12 {
				t.Errorf("expected_count = %v, want 12", c.ExpectedCount)
			}
		case protocol.CheckNoLeftoverIndexes:
			leftover++
		default:
			t.Errorf("unexpected check %q in the final verify", c.Check)
		}
	}
	if parentValid != 1 || leafCount != 1 || leftover != 12 {
		t.Errorf("final verify = %d parent-valid, %d leaf-count, %d no-leftover; want 1, 1, 12",
			parentValid, leafCount, leftover)
	}
}

func TestAssertNodeCarriesTheEightPreconditions(t *testing.T) {
	out := mustRun(t, New(), fixture(t, 4), spec())
	n := nodeByID(t, out.Plan, nodeAssert)

	p := n.Params.(*protocol.CatalogAssertParams)
	got := map[protocol.AssertionKind]bool{}
	for _, a := range p.Assertions {
		got[a.Assertion] = true
	}
	for _, want := range []protocol.AssertionKind{
		protocol.AssertRelationIsPartitioned,
		protocol.AssertPartitionStrategy,
		protocol.AssertPartitionDepth,
		protocol.AssertNoDefaultPartition,
		protocol.AssertRoleMembership,
		protocol.AssertIndexExists,
		protocol.AssertIndexIsPartitioned,
		protocol.AssertLeavesAttached,
	} {
		if !got[want] {
			t.Errorf("assert node is missing %q", want)
		}
	}
	if len(p.Assertions) != 8 {
		t.Errorf("assertions = %d, want 8; every kind must already exist and already be evaluated",
			len(p.Assertions))
	}
}

func TestPacingEmitsOneWaitPerRebuiltLeaf(t *testing.T) {
	s := spec()
	s.PaceSeconds = 30
	s.PaceReason = "give replicas room to catch up"

	out := mustRun(t, New(), fixture(t, 12), s)
	if got := countKind(out.Plan, protocol.KindWait); got != 12 {
		t.Fatalf("wait nodes = %d, want 12", got)
	}
	n := nodeByID(t, out.Plan, nodeID("wait", leafName(1)))
	p := n.Params.(*protocol.WaitParams)
	if p.Seconds != 30 {
		t.Errorf("wait seconds = %d, want 30", p.Seconds)
	}
	if p.Reason != s.PaceReason {
		t.Errorf("wait reason = %q, want %q", p.Reason, s.PaceReason)
	}
	verify := nodeByID(t, out.Plan, nodeID("verify", leafName(1)))
	if len(n.DependsOn) != 1 || n.DependsOn[0] != verify.ID {
		t.Errorf("wait depends on %v, want [%s]", n.DependsOn, verify.ID)
	}
}

// ---------------------------------------------------------------------------
// Leftovers (FR-REIDX-3, FR-REIDX-4, FR-REIDX-5, AC-19)
// ---------------------------------------------------------------------------

func TestTwelvePartitionsWithOneCCNewAndOneCCOld(t *testing.T) {
	cat := fixture(t, 12)
	addLeftover(cat, 3, "_ccnew1", 3001) // rebuild failed, original intact
	addLeftover(cat, 7, "_ccold2", 3002) // rebuild succeeded, old copy survived

	out := mustRun(t, New(), cat, spec())
	p := out.Plan

	ccnew := obj(leafIndexName(3).Name + "_ccnew1")
	ccold := obj(leafIndexName(7).Name + "_ccold2")

	// Both leftovers are dropped, online.
	if got := countKind(p, protocol.KindIndexDropConcurrently); got != 2 {
		t.Fatalf("drop nodes = %d, want 2", got)
	}
	for _, id := range []protocol.NodeID{nodeID("drop", ccnew), nodeID("drop", ccold)} {
		if !hasNode(p, id) {
			t.Errorf("missing drop node %q; have %v", id, ids(p))
		}
	}

	// FR-REIDX-3: the _ccnew leaf is still rebuilt, after its drop.
	if !hasNode(p, nodeID("reindex", leafName(3))) {
		t.Error("leaf 3 has a _ccnew, so it must still be reindexed (FR-REIDX-3)")
	}
	drop3 := nodeByID(t, p, nodeID("drop", ccnew))
	reindex3 := nodeByID(t, p, nodeID("reindex", leafName(3)))
	if len(drop3.DependsOn) != 1 || drop3.DependsOn[0] != nodeAssert {
		t.Errorf("the _ccnew drop depends on %v, want [%s]", drop3.DependsOn, nodeAssert)
	}
	if len(reindex3.DependsOn) != 1 || reindex3.DependsOn[0] != drop3.ID {
		t.Errorf("leaf 3's reindex depends on %v, want [%s]: the wreckage of the previous attempt is "+
			"cleared before the next one starts", reindex3.DependsOn, drop3.ID)
	}

	// FR-REIDX-4: the _ccold leaf is complete. Its drop is emitted; its rebuild
	// is not.
	if hasNode(p, nodeID("reindex", leafName(7))) {
		t.Error("leaf 7 has a _ccold, so the rebuild already succeeded and must not be repeated (FR-REIDX-4)")
	}
	if hasNode(p, nodeID("verify", leafName(7))) {
		t.Error("leaf 7 emits no rebuild, so it emits no per-leaf verify either")
	}

	// 11 leaves rebuilt: all twelve minus the _ccold one.
	if got := countKind(p, protocol.KindIndexReindexConcurrently); got != 11 {
		t.Errorf("reindex nodes = %d, want 11", got)
	}

	// The final verify still covers all twelve leaves, including leaf 7.
	final := nodeByID(t, p, nodeFinalVerify).Params.(*protocol.VerifyParams)
	leftovers := 0
	for _, c := range final.Checks {
		if c.Check == protocol.CheckNoLeftoverIndexes {
			leftovers++
		}
	}
	if leftovers != 12 {
		t.Errorf("no-leftover checks = %d, want 12", leftovers)
	}
	// The _ccold drop is a tail, so it must feed the barrier.
	dropOldID := nodeID("drop", ccold)
	found := false
	for _, d := range nodeByID(t, p, nodeFinalVerify).DependsOn {
		if d == dropOldID {
			found = true
		}
	}
	if !found {
		t.Errorf("the final verify does not depend on %q, so the leftover could still be there when "+
			"CheckNoLeftoverIndexes runs", dropOldID)
	}
}

func TestLeftoverDropsCarryLeftoverAuthorizationAndAReason(t *testing.T) {
	cat := fixture(t, 4)
	addLeftover(cat, 2, "_ccnew", 3001)
	addLeftover(cat, 3, "_ccold", 3002)

	out := mustRun(t, New(), cat, spec())

	for _, tc := range []struct {
		leaf   int
		suffix string
		reason protocol.DropReason
	}{
		{2, "_ccnew", protocol.DropCCNew},
		{3, "_ccold", protocol.DropCCOld},
	} {
		name := obj(leafIndexName(tc.leaf).Name + tc.suffix)
		n := nodeByID(t, out.Plan, nodeID("drop", name))
		if n.Authorization == nil {
			t.Fatalf("%s: drop node carries no authorization", name)
		}
		if n.Authorization.Mode != protocol.AuthLeftover {
			t.Errorf("%s: mode = %q, want %q", name, n.Authorization.Mode, protocol.AuthLeftover)
		}
		if n.Authorization.Object != name {
			t.Errorf("%s: authorization object = %s", name, n.Authorization.Object)
		}
		if n.Authorization.Relation == nil || *n.Authorization.Relation != leafName(tc.leaf) {
			t.Errorf("%s: authorization relation = %v, want %s", name, n.Authorization.Relation, leafName(tc.leaf))
		}
		p := n.Params.(*protocol.DropConcurrentlyParams)
		if p.Reason != tc.reason {
			t.Errorf("%s: reason = %q, want %q", name, p.Reason, tc.reason)
		}
		if want := "DROP INDEX CONCURRENTLY " + name.Quoted() + ";"; n.RenderedSQL != want {
			t.Errorf("%s: rendered_sql = %q, want %q", name, n.RenderedSQL, want)
		}
	}
}

func TestSuffixDigitsAreMatchedAsAPatternNotALiteral(t *testing.T) {
	// PostgreSQL appends a disambiguating integer when the plain name is taken.
	// A literal compare against "_ccnew" would miss every one of these.
	cat := fixture(t, 3)
	addLeftover(cat, 1, "_ccnew7", 3001)
	addLeftover(cat, 2, "_ccold42", 3002)

	out := mustRun(t, New(), cat, spec())
	if got := countKind(out.Plan, protocol.KindIndexDropConcurrently); got != 2 {
		t.Fatalf("drop nodes = %d, want 2", got)
	}
	if hasNode(out.Plan, nodeID("reindex", leafName(2))) {
		t.Error("_ccold42 must be recognised as a _ccold, so leaf 2 counts as complete")
	}
}

func TestLeftoverOnAnUnmarkedBaseHaltsThePlan(t *testing.T) {
	// AC-19. An operator ran REINDEX CONCURRENTLY by hand on an index this tool
	// never created and left their own _ccnew behind. Name matching alone is
	// forgeable, so the base index's ownership marker is the second condition,
	// and failing it stops the whole plan.
	cat := fixture(t, 4)
	cat.Indexes[2].Comment = "" // leaf 2's index: parent index is Indexes[0]
	addLeftover(cat, 2, "_ccnew", 3001)

	_, err := run(t, New(), cat, spec())
	if err == nil {
		t.Fatal("expected a halt, got a plan")
	}
	if !errors.Is(err, protocol.ErrAuthorizationUnsatisfied) {
		t.Fatalf("error = %v, want ErrAuthorizationUnsatisfied", err)
	}
	if !strings.Contains(err.Error(), "AC-19") {
		t.Errorf("the refusal should cite AC-19 so an operator can look it up; got %q", err)
	}
}

func TestLeftoverWithAForeignMarkerOnTheBaseHaltsThePlan(t *testing.T) {
	cat := fixture(t, 4)
	cat.Indexes[2].Comment = "created by the nightly maintenance script"
	addLeftover(cat, 2, "_ccnew", 3001)

	_, err := run(t, New(), cat, spec())
	if err == nil {
		t.Fatal("expected a halt, got a plan")
	}
	if !errors.Is(err, protocol.ErrAuthorizationUnsatisfied) {
		t.Fatalf("error = %v, want ErrAuthorizationUnsatisfied", err)
	}
}

func TestLeftoverWhoseBaseDoesNotExistHaltsThePlan(t *testing.T) {
	cat := fixture(t, 4)
	cat.AddIndex(planner.Index{
		OID: 3009, Name: obj("some_other_index_ccnew"), Kind: planner.RelKindIndex,
		OwnerOID: ownerOID, TableOID: leafOIDBase + 2, Table: leafName(2),
		IsReady: true, IsLive: true,
	})

	_, err := run(t, New(), cat, spec())
	if err == nil {
		t.Fatal("expected a halt, got a plan")
	}
	if !errors.Is(err, protocol.ErrAuthorizationUnsatisfied) {
		t.Fatalf("error = %v, want ErrAuthorizationUnsatisfied", err)
	}
}

func TestACCOldBelongingToAnUnrelatedIndexDoesNotMarkTheLeafComplete(t *testing.T) {
	// The leftover is dropped, because the terminal CheckNoLeftoverIndexes
	// demands the leaf be clean. It is not evidence that *our* rebuild already
	// succeeded, so the leaf is still rebuilt.
	cat := fixture(t, 4)
	other := obj("events_p02_other_idx")
	cat.AddIndex(planner.Index{
		OID: 3010, Name: other, Kind: planner.RelKindIndex, OwnerOID: ownerOID,
		TableOID: leafOIDBase + 2, Table: leafName(2),
		IsValid: true, IsReady: true, IsLive: true,
		Comment: markerText(t, "run-other", ""),
	})
	cat.AddIndex(planner.Index{
		OID: 3011, Name: obj(other.Name + "_ccold"), Kind: planner.RelKindIndex, OwnerOID: ownerOID,
		TableOID: leafOIDBase + 2, Table: leafName(2),
		IsReady: true, IsLive: true,
	})

	out := mustRun(t, New(), cat, spec())
	if !hasNode(out.Plan, nodeID("drop", obj(other.Name+"_ccold"))) {
		t.Error("the unrelated leftover must still be dropped: the final verify demands a clean leaf")
	}
	if !hasNode(out.Plan, nodeID("reindex", leafName(2))) {
		t.Error("an unrelated _ccold says nothing about our index, so leaf 2 must still be rebuilt")
	}
}

// ---------------------------------------------------------------------------
// The reindex_since watermark (FR-PLAN-5)
// ---------------------------------------------------------------------------

func TestNoWatermarkRebuildsEveryLeafEvenAFreshOne(t *testing.T) {
	cat := fixture(t, 12)
	cat.Indexes[5].Comment = markerText(t, "run-create", "2026-08-06T00:00:00Z")

	out := mustRun(t, New(), cat, spec())
	if got := countKind(out.Plan, protocol.KindIndexReindexConcurrently); got != 12 {
		t.Fatalf("reindex nodes = %d, want 12: an operator who asked for a reindex asked for a reindex",
			got)
	}
}

func TestWatermarkSkipsLeavesAlreadyRebuiltAtOrAfterIt(t *testing.T) {
	cat := fixture(t, 12)
	// Leaves 1..4 were rebuilt after the watermark; leaf 5 exactly on it;
	// leaf 6 before it. Index 0 is the parent, so leaf i is Indexes[i].
	for i := 1; i <= 4; i++ {
		cat.Indexes[i].Comment = markerText(t, "run-a", "2026-08-06T00:00:00Z")
	}
	cat.Indexes[5].Comment = markerText(t, "run-a", "2026-08-05T00:00:00Z")
	cat.Indexes[6].Comment = markerText(t, "run-a", "2026-08-04T00:00:00Z")

	pl := Planner{ReindexSince: time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)}
	out := mustRun(t, pl, cat, spec())

	for i := 1; i <= 5; i++ {
		if hasNode(out.Plan, nodeID("reindex", leafName(i))) {
			t.Errorf("leaf %d was rebuilt at or after the watermark and must be skipped (FR-PLAN-5)", i)
		}
	}
	for i := 6; i <= 12; i++ {
		if !hasNode(out.Plan, nodeID("reindex", leafName(i))) {
			t.Errorf("leaf %d is stale and must be rebuilt", i)
		}
	}
	if got := countKind(out.Plan, protocol.KindIndexReindexConcurrently); got != 7 {
		t.Errorf("reindex nodes = %d, want 7", got)
	}
}

func TestAnUnreadableOrForeignMarkerIsAlwaysRebuilt(t *testing.T) {
	// Every ambiguity resolves to "rebuild it". Rebuilding a fresh index wastes
	// time; skipping a stale one silently fails to do the job.
	cat := fixture(t, 4)
	cat.Indexes[1].Comment = ""
	cat.Indexes[2].Comment = "a comment somebody else wrote"
	cat.Indexes[3].Comment = protocol.MarkerSentinel + "not json at all"

	pl := Planner{ReindexSince: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}
	out := mustRun(t, pl, cat, spec())
	if got := countKind(out.Plan, protocol.KindIndexReindexConcurrently); got != 4 {
		t.Fatalf("reindex nodes = %d, want 4", got)
	}
}

func TestAFullyFreshTreeIsACheckedNoOpNotAnEmptyPlan(t *testing.T) {
	cat := fixture(t, 12)
	for i := 1; i <= 12; i++ {
		cat.Indexes[i].Comment = markerText(t, "run-a", "2026-08-06T00:00:00Z")
	}
	pl := Planner{ReindexSince: time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)}
	out := mustRun(t, pl, cat, spec())

	if got := countKind(out.Plan, protocol.KindIndexReindexConcurrently); got != 0 {
		t.Errorf("reindex nodes = %d, want 0", got)
	}
	if got := len(out.Plan.Nodes); got != 2 {
		t.Fatalf("nodes = %d, want 2 (the assert and the final verify)", got)
	}
	final := nodeByID(t, out.Plan, nodeFinalVerify)
	if len(final.DependsOn) != 1 || final.DependsOn[0] != nodeAssert {
		t.Errorf("final verify depends on %v, want [%s]", final.DependsOn, nodeAssert)
	}
}

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

func TestOrdinaryIndexIsRefused(t *testing.T) {
	cat := fixture(t, 3)
	cat.Indexes[0].Kind = planner.RelKindIndex

	_, err := run(t, New(), cat, spec())
	if err == nil {
		t.Fatal("expected a refusal, got a plan")
	}
	if !errors.Is(err, protocol.ErrUnsupportedTopology) {
		t.Fatalf("error = %v, want ErrUnsupportedTopology", err)
	}
}

func TestIndexOnADifferentTableIsRefused(t *testing.T) {
	cat := fixture(t, 3)
	cat.Indexes[0].TableOID = 9999

	_, err := run(t, New(), cat, spec())
	if err == nil {
		t.Fatal("expected a refusal, got a plan")
	}
	if !strings.Contains(err.Error(), "not on") {
		t.Fatalf("error = %v, want a message naming the wrong table", err)
	}
}

func TestInvalidParentIndexIsRefused(t *testing.T) {
	// The plan's own final verification asserts CheckParentIndexValid, so no
	// leaf rebuild could make a plan over an INVALID parent pass.
	cat := fixture(t, 3)
	cat.Indexes[0].IsValid = false

	_, err := run(t, New(), cat, spec())
	if err == nil {
		t.Fatal("expected a refusal, got a plan")
	}
	if !strings.Contains(err.Error(), "not valid") {
		t.Fatalf("error = %v, want a message about the parent index not being valid", err)
	}
}

func TestMissingIndexIsRefused(t *testing.T) {
	cat := fixture(t, 3)
	s := spec()
	s.Index = obj("no_such_index")

	_, err := run(t, New(), cat, s)
	if err == nil {
		t.Fatal("expected a refusal, got a plan")
	}
	if !errors.Is(err, planner.ErrIndexNotFound) {
		t.Fatalf("error = %v, want ErrIndexNotFound", err)
	}
}

func TestALeafWithNoAttachedIndexIsRefused(t *testing.T) {
	cat := fixture(t, 4)
	cat.Indexes[3].ParentIndexOID = 0

	_, err := run(t, New(), cat, spec())
	if err == nil {
		t.Fatal("expected a refusal, got a plan")
	}
	if !strings.Contains(err.Error(), "no index attached") {
		t.Fatalf("error = %v, want a message about the incomplete partitioned index", err)
	}
}

func TestCatalogFailurePropagates(t *testing.T) {
	cat := fixture(t, 3)
	cat.Err = errors.New("connection reset by peer")

	if _, err := run(t, New(), cat, spec()); err == nil {
		t.Fatal("expected the catalog error to propagate")
	}
}

// ---------------------------------------------------------------------------
// Notes
// ---------------------------------------------------------------------------

func TestNotesStateWhyThePlanIsPerLeaf(t *testing.T) {
	// This is the note that stops the next reader "simplifying" the operation
	// back to REINDEX INDEX CONCURRENTLY on the parent. If this test is deleted,
	// the reason goes with it.
	out := mustRun(t, New(), fixture(t, 12), spec())
	if len(out.Notes) == 0 {
		t.Fatal("the plan carries no notes")
	}
	first := out.Notes[0]
	for _, want := range []string{
		"one leaf at a time",
		"The parent form works",
		"14.23 and 17.10",
		"already fresh",
		"resume",
		"FR-PLAN-5",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("the leading note does not mention %q; got:\n%s", want, first)
		}
	}
}

func TestNotesReportTheLeafCountsAndTheLeftovers(t *testing.T) {
	cat := fixture(t, 12)
	addLeftover(cat, 3, "_ccnew1", 3001)
	addLeftover(cat, 7, "_ccold2", 3002)

	out := mustRun(t, New(), cat, spec())
	joined := strings.Join(out.Notes, "\n")
	for _, want := range []string{
		"11 of 12 leaf index(es) will be rebuilt; 1 skipped as already complete.",
		"2 REINDEX CONCURRENTLY leftover(s)",
		"1 _ccnew",
		"1 _ccold",
		"AC-19",
		"Peak additional storage",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes do not mention %q; got:\n%s", want, joined)
		}
	}
}

func TestNotesExplainTheWatermarkWhenOneIsSupplied(t *testing.T) {
	pl := Planner{ReindexSince: time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)}
	out := mustRun(t, pl, fixture(t, 3), spec())

	joined := strings.Join(out.Notes, "\n")
	if !strings.Contains(joined, "2026-08-05T00:00:00Z") {
		t.Errorf("notes do not state the watermark; got:\n%s", joined)
	}
	if !strings.Contains(joined, "FR-PLAN-5") {
		t.Errorf("notes do not cite FR-PLAN-5; got:\n%s", joined)
	}
}

// ---------------------------------------------------------------------------
// Invariants the operation depends on but does not own
// ---------------------------------------------------------------------------

func TestReindexNodeKindPropertiesAreTheOnesThisPlannerAssumes(t *testing.T) {
	k := protocol.KindIndexReindexConcurrently
	if !k.MustRunOutsideTransaction() {
		t.Error("REINDEX INDEX CONCURRENTLY must run outside a transaction block")
	}
	if k.AllowsStatementTimeout() {
		t.Error("a single leaf may be 10 TB, so no statement_timeout may be applied")
	}
	if !k.WaitsForConcurrentTransactions() {
		t.Error("the kind must use the long BuildLockTimeout, not the short LockTimeout")
	}
	if k.RetrySafe() {
		t.Error("a failed REINDEX CONCURRENTLY leaves a _ccnew behind; recovery is resume, not retry")
	}
	if got := k.LockLevel(); got != protocol.LockShareUpdateExclusive {
		t.Errorf("lock level = %v, want ShareUpdateExclusive", got)
	}
}

func TestPlanningIsConstantQueriesInTheNumberOfPartitions(t *testing.T) {
	// NFR-PERF-1. The ownership marker rides along with IndexesOnRelations, so
	// no per-leaf IndexComment round trip is made.
	small := fixture(t, 3)
	mustRun(t, New(), small, spec())
	large := fixture(t, 12)
	mustRun(t, New(), large, spec())

	for _, method := range []string{"IndexesOnRelations", "LookupIndex", "IndexComment"} {
		if small.Calls[method] != large.Calls[method] {
			t.Errorf("%s called %d times for 3 leaves and %d times for 12: planning must be O(1) queries "+
				"in the number of partitions (NFR-PERF-1)",
				method, small.Calls[method], large.Calls[method])
		}
	}
	if large.Calls["IndexComment"] != 0 {
		t.Errorf("IndexComment called %d times; the marker rides along with the batched index pass",
			large.Calls["IndexComment"])
	}
}

func TestPlanIsAPureFunctionOfItsInputs(t *testing.T) {
	a := mustRun(t, New(), fixture(t, 12), spec())
	b := mustRun(t, New(), fixture(t, 12), spec())
	if a.Plan.Digest != b.Plan.Digest {
		t.Fatalf("two plans over the same catalog have different digests:\n%s\n%s",
			a.Plan.Digest, b.Plan.Digest)
	}
}
