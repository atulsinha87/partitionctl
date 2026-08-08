package createindex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/atulsinha/partitionctl/engine/planner"
	"github.com/atulsinha/partitionctl/engine/protocol"
)

// ---------------------------------------------------------------------------
// The graph of TRD §7.2.13
// ---------------------------------------------------------------------------

func TestPlanCleanBuildEmitsTheSpecifiedGraph(t *testing.T) {
	leaves := []string{"orders_2026_01", "orders_2026_02", "orders_2026_03"}
	cat := newCatalog(leaves...)
	p := mustPlan(t, cat, newSpec())

	// 1 assert + 1 parent + 4 per leaf + 1 final verify.
	if got, want := len(p.Nodes), 2+len(leaves)*4+1; got != want {
		t.Fatalf("plan has %d nodes, want %d: %v", got, want, nodeIDs(p))
	}

	if p.Nodes[0].ID != nodeAssert || p.Nodes[0].Kind != protocol.KindCatalogAssert {
		t.Fatalf("first node is %q/%q, want the single catalog.assert", p.Nodes[0].ID, p.Nodes[0].Kind)
	}
	if countKind(p, protocol.KindCatalogAssert) != 1 {
		t.Fatalf("plan has %d catalog.assert nodes, want exactly 1", countKind(p, protocol.KindCatalogAssert))
	}
	if countKind(p, protocol.KindIndexCreateParentInvalid) != 1 {
		t.Fatalf("plan has %d index.create_parent_invalid nodes, want exactly 1",
			countKind(p, protocol.KindIndexCreateParentInvalid))
	}

	depsEqual(t, p, nodeParentIndex, nodeAssert)

	// Per leaf: create -> verify -> attach -> wait, all rooted at the parent
	// index node.
	var tails []protocol.NodeID
	for _, l := range leaves {
		leaf := obj(l)
		create, verify := nodeID("create", leaf), nodeID("verify", leaf)
		attach, wait := nodeID("attach", leaf), nodeID("wait", leaf)
		depsEqual(t, p, create, nodeParentIndex)
		depsEqual(t, p, verify, create)
		depsEqual(t, p, attach, verify)
		depsEqual(t, p, wait, attach)
		tails = append(tails, wait)

		if hasNode(p, nodeID("drop", leaf)) {
			t.Fatalf("clean build emitted a drop node for %s", leaf)
		}
	}

	// The final verify is a node with N incoming edges; there is no barrier
	// kind (TRD §7.2.2).
	depsEqual(t, p, nodeFinalVerify, tails...)

	if _, err := p.TopologicalOrder(); err != nil {
		t.Fatalf("TopologicalOrder() error = %v", err)
	}
	if !hasWork(p) {
		t.Fatal("a clean build produced no DDL")
	}
}

func TestPlanNodeKindsAreOnlyTheSpecifiedSix(t *testing.T) {
	cat := newCatalog("p1", "p2")
	cat.indexes[child("p1")] = marked(invalidIndex("p1"))
	p := mustPlan(t, cat, newSpec())

	allowed := map[protocol.NodeKind]bool{
		protocol.KindCatalogAssert:            true,
		protocol.KindIndexCreateParentInvalid: true,
		protocol.KindIndexCreateConcurrently:  true,
		protocol.KindIndexVerify:              true,
		protocol.KindIndexAttach:              true,
		protocol.KindWait:                     true,
		protocol.KindIndexDropConcurrently:    true,
	}
	for _, k := range kindsOf(p) {
		if !allowed[k] {
			t.Fatalf("CreatePartitionedIndex emitted kind %q, which is not in its vocabulary", k)
		}
	}
	// Reindex and drop_partitioned belong to the other two operations.
	if countKind(p, protocol.KindIndexReindexConcurrently) != 0 ||
		countKind(p, protocol.KindIndexDropPartitioned) != 0 {
		t.Fatal("create-index emitted a node kind belonging to another operation")
	}
}

func TestPlanAssertNodeCarriesEveryPrecondition(t *testing.T) {
	leaves := []string{"p1", "p2"}
	cat := newCatalog(leaves...)
	p := mustPlan(t, cat, newSpec())

	params, ok := node(t, p, nodeAssert).Params.(*protocol.CatalogAssertParams)
	if !ok {
		t.Fatalf("assert node params are %T", node(t, p, nodeAssert).Params)
	}

	seen := map[protocol.AssertionKind]int{}
	for _, a := range params.Assertions {
		seen[a.Assertion]++
	}
	for _, want := range []protocol.AssertionKind{
		protocol.AssertRelationIsPartitioned,
		protocol.AssertPartitionStrategy,
		protocol.AssertPartitionDepth,
		protocol.AssertNoDefaultPartition,
		protocol.AssertRoleMembership,
		protocol.AssertIndexNameAvailable,
	} {
		if seen[want] == 0 {
			t.Errorf("assert node is missing precondition %q", want)
		}
	}
	// Role membership covers the parent and every leaf.
	if got, want := seen[protocol.AssertRoleMembership], 1+len(leaves); got != want {
		t.Errorf("role-membership assertions = %d, want %d (parent plus every leaf)", got, want)
	}

	// Failure codes carry the contract exit codes (TRD §7.2.12).
	for _, a := range params.Assertions {
		switch a.Assertion {
		case protocol.AssertRelationIsPartitioned, protocol.AssertPartitionStrategy,
			protocol.AssertPartitionDepth, protocol.AssertNoDefaultPartition:
			if a.FailureCode != protocol.ExitUnsupportedTopology {
				t.Errorf("assertion %q failure code = %d, want %d", a.Assertion, a.FailureCode, protocol.ExitUnsupportedTopology)
			}
		case protocol.AssertRoleMembership:
			if a.FailureCode != protocol.ExitInsufficientPrivilege {
				t.Errorf("assertion %q failure code = %d, want %d", a.Assertion, a.FailureCode, protocol.ExitInsufficientPrivilege)
			}
			if a.Role != testRole {
				t.Errorf("assertion %q role = %q, want %q", a.Assertion, a.Role, testRole)
			}
		}
	}

	if params.Assertions[0].Assertion != protocol.AssertRelationIsPartitioned {
		t.Errorf("first assertion is %q; the structural check should fail first", params.Assertions[0].Assertion)
	}
}

func TestPlanFinalVerifyAssertsTheWhole(t *testing.T) {
	leaves := []string{"p1", "p2", "p3"}
	cat := newCatalog(leaves...)
	p := mustPlan(t, cat, newSpec())

	params := node(t, p, nodeFinalVerify).Params.(*protocol.VerifyParams)

	var sawParentValid bool
	var count *int
	attached := map[protocol.ObjectName]bool{}
	for _, c := range params.Checks {
		switch c.Check {
		case protocol.CheckParentIndexValid:
			sawParentValid = true
			if c.ParentIndex == nil || *c.ParentIndex != obj(testIndex) {
				t.Errorf("parent-valid check names %v, want %s", c.ParentIndex, obj(testIndex))
			}
		case protocol.CheckLeafIndexCount:
			count = c.ExpectedCount
		case protocol.CheckIndexAttached:
			attached[*c.Index] = true
		}
	}
	if !sawParentValid {
		t.Error("final verify does not assert the parent index is valid (FR-VER-3)")
	}
	if count == nil || *count != len(leaves) {
		t.Errorf("final verify leaf count = %v, want %d (FR-VER-4)", count, len(leaves))
	}
	for _, l := range leaves {
		if !attached[child(l)] {
			t.Errorf("final verify does not assert %s is attached (FR-VER-2)", child(l))
		}
	}
}

func TestPlanLeafVerifyChecksValidReadyLiveBeforeAttach(t *testing.T) {
	cat := newCatalog("p1")
	p := mustPlan(t, cat, newSpec())

	verify := node(t, p, nodeID("verify", obj("p1")))
	params := verify.Params.(*protocol.VerifyParams)
	if len(params.Checks) != 1 || params.Checks[0].Check != protocol.CheckIndexValid {
		t.Fatalf("leaf verify checks = %+v, want a single index_valid check (FR-VER-1)", params.Checks)
	}
	if *params.Checks[0].Index != child("p1") {
		t.Fatalf("leaf verify names %s, want %s", *params.Checks[0].Index, child("p1"))
	}
	// It must come before the attach, not after it.
	depsEqual(t, p, nodeID("attach", obj("p1")), nodeID("verify", obj("p1")))
}

// ---------------------------------------------------------------------------
// FR-PLAN-5: emit no node for work already complete
// ---------------------------------------------------------------------------

func TestPlanFullyCompleteEmitsNoWork(t *testing.T) {
	leaves := []string{"p1", "p2", "p3"}
	cat := newCatalog(leaves...)
	cat.indexes[obj(testIndex)] = parentIndexState(true)
	for _, l := range leaves {
		cat.indexes[child(l)] = attachedIndex(l)
	}

	p := mustPlan(t, cat, newSpec())

	if hasWork(p) {
		t.Fatalf("converged catalog still produced DDL; nodes: %v", nodeIDs(p))
	}
	// It is a checked no-op, not an empty file: assert plus final verify.
	if got := nodeIDs(p); len(got) != 2 || got[0] != nodeAssert || got[1] != nodeFinalVerify {
		t.Fatalf("converged plan nodes = %v, want [%s %s]", got, nodeAssert, nodeFinalVerify)
	}
	// With no leaf chains, the final verify hangs off the assert.
	depsEqual(t, p, nodeFinalVerify, nodeAssert)
	// And it still proves the whole end state.
	checks := node(t, p, nodeFinalVerify).Params.(*protocol.VerifyParams).Checks
	if len(checks) != 2+len(leaves) {
		t.Fatalf("converged plan final verify has %d checks, want %d", len(checks), 2+len(leaves))
	}
}

func TestPlanPartiallyCompleteEmitsOnlyTheRemainingLeaves(t *testing.T) {
	leaves := []string{"p1", "p2", "p3", "p4"}
	cat := newCatalog(leaves...)
	cat.indexes[obj(testIndex)] = marked(parentIndexState(false)) // mid-build: invalid parent
	cat.indexes[child("p1")] = attachedIndex("p1")                // done
	cat.indexes[child("p2")] = attachedIndex("p2")                // done
	cat.indexes[child("p3")] = builtIndex("p3")                   // built, not attached

	p := mustPlan(t, cat, newSpec())

	// The parent index already exists, so no create_parent_invalid.
	if hasNode(p, nodeParentIndex) {
		t.Error("re-planned an index.create_parent_invalid for an existing parent index")
	}
	// Completed leaves contribute nothing at all.
	for _, done := range []string{"p1", "p2"} {
		for _, step := range []string{"drop", "create", "verify", "attach", "wait"} {
			if hasNode(p, nodeID(step, obj(done))) {
				t.Errorf("completed leaf %s still has a %s node (FR-PLAN-5)", done, step)
			}
		}
	}
	// p3 is built but unattached: only the tail of the chain remains.
	if hasNode(p, nodeID("create", obj("p3"))) {
		t.Error("re-planned a CREATE INDEX CONCURRENTLY for a leaf whose index is already built")
	}
	depsEqual(t, p, nodeID("verify", obj("p3")), nodeAssert)
	depsEqual(t, p, nodeID("attach", obj("p3")), nodeID("verify", obj("p3")))
	depsEqual(t, p, nodeID("wait", obj("p3")), nodeID("attach", obj("p3")))

	// p4 is untouched: the full chain, rooted at the assert because the parent
	// index already exists.
	depsEqual(t, p, nodeID("create", obj("p4")), nodeAssert)
	depsEqual(t, p, nodeID("verify", obj("p4")), nodeID("create", obj("p4")))

	depsEqual(t, p, nodeFinalVerify, nodeID("wait", obj("p3")), nodeID("wait", obj("p4")))

	if got, want := countKind(p, protocol.KindIndexCreateConcurrently), 1; got != want {
		t.Errorf("create_concurrently nodes = %d, want %d", got, want)
	}
	if got, want := countKind(p, protocol.KindIndexAttach), 2; got != want {
		t.Errorf("attach nodes = %d, want %d", got, want)
	}
}

func TestPlanIsIdempotentAcrossReplans(t *testing.T) {
	leaves := []string{"p1", "p2"}
	cat := newCatalog(leaves...)
	first := mustPlan(t, cat, newSpec())

	// Simulate the run having completed, then re-plan.
	cat.indexes[obj(testIndex)] = parentIndexState(true)
	for _, l := range leaves {
		cat.indexes[child(l)] = attachedIndex(l)
	}
	second := mustPlan(t, cat, newSpec())

	if !hasWork(first) {
		t.Fatal("first plan has no work")
	}
	if hasWork(second) {
		t.Fatalf("re-plan after convergence still has work: %v", nodeIDs(second))
	}
}

// ---------------------------------------------------------------------------
// FR-PLAN-6 / FR-PLAN-7: unusable-index handling
// ---------------------------------------------------------------------------

func TestPlanInvalidMarkedLeafEmitsDropBeforeRebuild(t *testing.T) {
	leaves := []string{"p1", "p2"}
	cat := newCatalog(leaves...)
	cat.indexes[obj(testIndex)] = marked(parentIndexState(false))
	cat.indexes[child("p1")] = marked(invalidIndex("p1"))

	p := mustPlan(t, cat, newSpec())

	drop := node(t, p, nodeID("drop", obj("p1")))
	if drop.Kind != protocol.KindIndexDropConcurrently {
		t.Fatalf("drop node kind = %q, want %q", drop.Kind, protocol.KindIndexDropConcurrently)
	}
	if drop.Authorization == nil {
		t.Fatal("drop node carries no authorization (FR-AUTH-1)")
	}
	if drop.Authorization.Mode != protocol.AuthProvenance {
		t.Errorf("drop authorization mode = %q, want %q (FR-PLAN-6)", drop.Authorization.Mode, protocol.AuthProvenance)
	}
	if drop.Authorization.Object != child("p1") {
		t.Errorf("drop authorization object = %s, want %s", drop.Authorization.Object, child("p1"))
	}
	if drop.Authorization.Relation == nil || *drop.Authorization.Relation != obj("p1") {
		t.Errorf("drop authorization relation = %v, want %s", drop.Authorization.Relation, obj("p1"))
	}
	params := drop.Params.(*protocol.DropConcurrentlyParams)
	if params.Reason != protocol.DropInvalidBuild {
		t.Errorf("drop reason = %q, want %q", params.Reason, protocol.DropInvalidBuild)
	}

	// It comes BEFORE the rebuild, in the same chain.
	depsEqual(t, p, nodeID("drop", obj("p1")), nodeAssert)
	depsEqual(t, p, nodeID("create", obj("p1")), nodeID("drop", obj("p1")))
	depsEqual(t, p, nodeID("verify", obj("p1")), nodeID("create", obj("p1")))

	// The healthy leaf gets no drop.
	if hasNode(p, nodeID("drop", obj("p2"))) {
		t.Error("emitted a drop for a leaf with no existing index")
	}
	if got, want := countKind(p, protocol.KindIndexDropConcurrently), 1; got != want {
		t.Errorf("drop nodes = %d, want %d", got, want)
	}

	// The topological order proves the drop precedes the create.
	order, err := p.TopologicalOrder()
	if err != nil {
		t.Fatalf("TopologicalOrder() error = %v", err)
	}
	if indexOf(order, nodeID("drop", obj("p1"))) > indexOf(order, nodeID("create", obj("p1"))) {
		t.Error("drop is ordered after the rebuild it must precede")
	}
}

func TestPlanUnmarkedInvalidLeafHalts(t *testing.T) {
	cat := newCatalog("p1", "p2")
	cat.indexes[obj(testIndex)] = marked(parentIndexState(false))
	// The parent index is ours; the leaf carries nothing.
	cat.indexes[child("p1")] = invalidIndex("p1")

	p, _, err := tryPlan(t, cat, newSpec())
	if p != nil {
		t.Fatalf("Plan() returned a plan of %d nodes; FR-PLAN-7 requires no plan at all", len(p.Nodes))
	}
	if got := protocol.ExitCodeFor(err); got != protocol.ExitAuthorizationUnsatisfied {
		t.Errorf("exit code = %d, want %d (%v)", got, protocol.ExitAuthorizationUnsatisfied, err)
	}
	if !strings.Contains(err.Error(), child("p1").String()) {
		t.Errorf("error does not name the offending index: %v", err)
	}
}

func TestPlanWithNoClaimSourceHalts(t *testing.T) {
	cat := newCatalog("p1")
	cat.indexes[child("p1")] = invalidIndex("p1")

	// No claim source, and the object carries no marker. The safe default is to
	// halt, never to drop.
	p, _, err := tryPlan(t, cat, newSpec())
	if p != nil || protocol.ExitCodeFor(err) != protocol.ExitAuthorizationUnsatisfied {
		t.Fatalf("Plan() = (%v, %v), want (nil, an authorization halt)", p, err)
	}
}

// The crash window: the CREATE INDEX CONCURRENTLY ran and the process died
// before the COMMENT could be written. The object is unmarked but the run that
// died still claims it, so it is adoptable.
func TestPlanUnmarkedInvalidLeafWithALiveClaimIsDropped(t *testing.T) {
	cat := newCatalog("p1", "p2")
	cat.indexes[obj(testIndex)] = marked(parentIndexState(false))
	cat.indexes[child("p1")] = invalidIndex("p1")
	cat.claims = planner.NewFakeClaims(child("p1"))

	p := mustPlan(t, cat, newSpec())
	if !hasNode(p, nodeID("drop", obj("p1"))) {
		t.Fatal("a claimed, unmarked unusable index was not planned for cleanup (FR-PLAN-6)")
	}
}

// Never overwrite a human's comment, never drop the object under it, and a live
// claim does not change that.
func TestPlanInvalidLeafUnderAForeignCommentHalts(t *testing.T) {
	cat := newCatalog("p1", "p2")
	cat.indexes[obj(testIndex)] = marked(parentIndexState(false))
	cat.indexes[child("p1")] = commented(invalidIndex("p1"), "built by the DBA team, do not touch")
	cat.claims = planner.NewFakeClaims(child("p1"))

	p, _, err := tryPlan(t, cat, newSpec())
	if p != nil {
		t.Fatal("Plan() emitted a graph that would drop an index under somebody else's comment")
	}
	if got := protocol.ExitCodeFor(err); got != protocol.ExitAuthorizationUnsatisfied {
		t.Fatalf("exit code = %d, want %d (%v)", got, protocol.ExitAuthorizationUnsatisfied, err)
	}
}

func TestPlanUnmarkedInvalidParentIndexHalts(t *testing.T) {
	cat := newCatalog("p1")
	cat.indexes[obj(testIndex)] = parentIndexState(false)
	cat.claims = planner.NewFakeClaims()

	p, _, err := tryPlan(t, cat, newSpec())
	if p != nil {
		t.Fatal("Plan() adopted an in-progress parent index it cannot prove it created")
	}
	if got := protocol.ExitCodeFor(err); got != protocol.ExitAuthorizationUnsatisfied {
		t.Fatalf("exit code = %d, want %d (%v)", got, protocol.ExitAuthorizationUnsatisfied, err)
	}
}

func TestPlanMarkedInvalidParentIndexIsAdopted(t *testing.T) {
	cat := newCatalog("p1")
	cat.indexes[obj(testIndex)] = marked(parentIndexState(false))

	p := mustPlan(t, cat, newSpec())
	if hasNode(p, nodeParentIndex) {
		t.Error("re-created a parent index that already exists and is ours to adopt")
	}
	if !hasNode(p, nodeID("create", obj("p1"))) {
		t.Error("adopting the parent index dropped the leaf work with it")
	}
}

func TestPlanInvalidAttachedLeafHalts(t *testing.T) {
	cat := newCatalog("p1")
	cat.indexes[obj(testIndex)] = marked(parentIndexState(true))
	st := marked(invalidIndex("p1"))
	st.ParentIndexOID = parentIndexOID
	cat.indexes[child("p1")] = st

	// Even when it is provably ours: PostgreSQL cannot drop an attached child
	// index individually, so no graph fixes this (TRD §7.2.10).
	p, _, err := tryPlan(t, cat, newSpec())
	if p != nil {
		t.Fatal("Plan() emitted a graph for an attached, unusable leaf index")
	}
	if err == nil || !strings.Contains(err.Error(), "attached") {
		t.Fatalf("Plan() error = %v, want an explanation naming the attachment", err)
	}
}

func TestPlanRefusesForeignObjectsOccupyingTargetNames(t *testing.T) {
	tests := []struct {
		name  string
		setup func(c *fakeCatalog)
		want  string
	}{
		{
			name: "parent index name taken by an ordinary index",
			setup: func(c *fakeCatalog) {
				st := parentIndexState(true)
				st.Kind = planner.RelKindIndex
				c.indexes[obj(testIndex)] = st
			},
			want: "ordinary index",
		},
		{
			name: "child index name is a partitioned index",
			setup: func(c *fakeCatalog) {
				st := builtIndex("p1")
				st.Kind = planner.RelKindPartitionedIndex
				c.indexes[child("p1")] = st
			},
			want: "partitioned index",
		},
		{
			name: "child index attached to a different parent",
			setup: func(c *fakeCatalog) {
				st := builtIndex("p1")
				st.ParentIndexOID = foreignOID
				c.indexes[child("p1")] = st
			},
			want: "already attached to",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cat := newCatalog("p1")
			tc.setup(cat)
			p, _, err := tryPlan(t, cat, newSpec())
			if p != nil {
				t.Fatalf("Plan() returned a plan; want a halt")
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Plan() error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// FR-PLAN-11 / FR-PLAN-13: child index naming
// ---------------------------------------------------------------------------

func TestPlanRecordsChildIndexNamesFromTheProtocolFunction(t *testing.T) {
	leaves := []string{"orders_2026_01", "orders_2026_02"}
	cat := newCatalog(leaves...)
	p := mustPlan(t, cat, newSpec())

	for _, l := range leaves {
		want := protocol.ChildIndexName(testIndex, l)
		create := node(t, p, nodeID("create", obj(l))).Params.(*protocol.CreateConcurrentlyParams)
		if create.Index.Name != want {
			t.Errorf("child index name = %q, want %q (FR-PLAN-11)", create.Index.Name, want)
		}
		if create.Index.Schema != testSchema {
			t.Errorf("child index schema = %q, want the partition's schema %q", create.Index.Schema, testSchema)
		}
		if create.ParentIndex == nil || *create.ParentIndex != obj(testIndex) {
			t.Errorf("create node parent index = %v, want %s", create.ParentIndex, obj(testIndex))
		}
		attach := node(t, p, nodeID("attach", obj(l))).Params.(*protocol.AttachParams)
		if attach.ChildIndex != create.Index {
			t.Errorf("attach child %s does not match the created index %s", attach.ChildIndex, create.Index)
		}
		if attach.ParentIndex != obj(testIndex) {
			t.Errorf("attach parent = %s, want %s", attach.ParentIndex, obj(testIndex))
		}
	}
}

func TestPlanTruncatesOverlongChildNamesDeterministically(t *testing.T) {
	longParent := strings.Repeat("a", 50)
	leaves := []string{strings.Repeat("b", 40), strings.Repeat("b", 39) + "c"}

	cat := newCatalog(leaves...)
	spec := newSpec()
	spec.Index = obj(longParent)
	p := mustPlan(t, cat, spec)

	seen := map[string]bool{}
	for _, l := range leaves {
		create := node(t, p, nodeID("create", obj(l))).Params.(*protocol.CreateConcurrentlyParams)
		if len(create.Index.Name) > protocol.MaxIdentifierBytes {
			t.Fatalf("child index name is %d bytes, over the %d-byte limit",
				len(create.Index.Name), protocol.MaxIdentifierBytes)
		}
		if seen[create.Index.Name] {
			t.Fatalf("two partitions share the child index name %q", create.Index.Name)
		}
		seen[create.Index.Name] = true
		if create.Index.Name != protocol.ChildIndexName(longParent, l) {
			t.Fatalf("child name %q is not the protocol function's output", create.Index.Name)
		}
	}
}

func TestPlanRejectsCollidingChildNames(t *testing.T) {
	// Two partitions whose names differ only past the truncation point still
	// get distinct names, so force a genuine duplicate instead: two relations,
	// same schema, same name.
	cat := newCatalog("p1", "p1")
	_, _, err := tryPlan(t, cat, newSpec())
	if !errors.Is(err, protocol.ErrNameCollision) {
		t.Fatalf("Plan() error = %v, want ErrNameCollision", err)
	}
}

func TestPlanNamesChildrenPerSchema(t *testing.T) {
	// Same partition name in two schemas is legal and must not be reported as
	// a collision: each index lives in its own table's schema.
	cat := newCatalog()
	cat.setLeaves(
		protocol.NewObjectName("a", "p1"),
		protocol.NewObjectName("b", "p1"),
	)
	p := mustPlan(t, cat, newSpec())

	for _, schema := range []string{"a", "b"} {
		leaf := protocol.NewObjectName(schema, "p1")
		create := node(t, p, nodeID("create", leaf)).Params.(*protocol.CreateConcurrentlyParams)
		if create.Index.Schema != schema {
			t.Errorf("child index for %s is in schema %q, want %q", leaf, create.Index.Schema, schema)
		}
	}
}

// ---------------------------------------------------------------------------
// FR-PLAN-9: estimates, and FR-ORD-3: pacing
// ---------------------------------------------------------------------------

func TestPlanEstimatesFromRelPages(t *testing.T) {
	cat := newCatalog("small", "large")
	cat.pages[obj("small")] = 128              // 1 MiB
	cat.pages[obj("large")] = 128 * 1024 * 100 // 100 GiB
	// 10 MiB/s, so the arithmetic is checkable, and a 1s floor.
	cat.estimator = planner.Estimator{BuildBytesPerSecond: 10 << 20, MinBuildSeconds: 1}

	p := mustPlan(t, cat, newSpec())

	small := node(t, p, nodeID("create", obj("small"))).EstimatedSeconds
	large := node(t, p, nodeID("create", obj("large"))).EstimatedSeconds
	if small != 1 {
		t.Errorf("small leaf estimate = %ds, want 1s (2 MiB of work at 10 MiB/s, floored at 1)", small)
	}
	if want := 100 * 1024 * 2 / 10; large != want {
		t.Errorf("large leaf estimate = %ds, want %ds", large, want)
	}
	if large <= small {
		t.Error("estimates do not scale with relpages (FR-PLAN-9)")
	}
	// A leaf with no relpages still estimates, at the default floor.
	p2 := mustPlan(t, newCatalog("unknown"), newSpec())
	if got := node(t, p2, nodeID("create", obj("unknown"))).EstimatedSeconds; got != planner.DefaultMinBuildSeconds {
		t.Errorf("unknown-size leaf estimate = %d, want the %ds floor", got, planner.DefaultMinBuildSeconds)
	}
}

func TestPlanTotalEstimateIncludesPacing(t *testing.T) {
	cat := newCatalog("p1", "p2")
	spec := newSpec()
	spec.PaceSeconds = 45

	p := mustPlan(t, cat, spec)
	for _, l := range []string{"p1", "p2"} {
		w := node(t, p, nodeID("wait", obj(l)))
		params := w.Params.(*protocol.WaitParams)
		if params.Seconds != 45 {
			t.Errorf("wait seconds = %d, want 45 (FR-ORD-3)", params.Seconds)
		}
		if w.EstimatedSeconds != 45 {
			t.Errorf("wait node estimate = %d, want 45", w.EstimatedSeconds)
		}
		if params.Reason == "" {
			t.Error("wait node has no reason; every pause must be legible in the artifact")
		}
	}
	if p.TotalEstimatedSeconds() < 90 {
		t.Errorf("total estimate %d does not include the two 45s pauses", p.TotalEstimatedSeconds())
	}
}

func TestPlanEmitsWaitNodesEvenWithZeroPacing(t *testing.T) {
	cat := newCatalog("p1")
	spec := newSpec()
	spec.PaceSeconds = 0

	p := mustPlan(t, cat, spec)
	w := node(t, p, nodeID("wait", obj("p1")))
	if got := w.Params.(*protocol.WaitParams).Seconds; got != 0 {
		t.Errorf("wait seconds = %d, want 0", got)
	}
	// The graph's shape must not depend on pacing.
	depsEqual(t, p, nodeFinalVerify, nodeID("wait", obj("p1")))
}

func TestPlanRecordsTheOperatorsPaceReason(t *testing.T) {
	cat := newCatalog("p1")
	spec := newSpec()
	spec.PaceSeconds = 30
	spec.PaceReason = "replica lag budget"

	p := mustPlan(t, cat, spec)
	if got := node(t, p, nodeID("wait", obj("p1"))).Params.(*protocol.WaitParams).Reason; got != spec.PaceReason {
		t.Errorf("wait reason = %q, want the operator's %q", got, spec.PaceReason)
	}
}

// ---------------------------------------------------------------------------
// Determinism and the artifact
// ---------------------------------------------------------------------------

func TestPlanIsDeterministic(t *testing.T) {
	first := mustPlan(t, newCatalog("p3", "p1", "p2"), newSpec())

	// The same partitions declared in a different order must produce the
	// identical artifact: the graph is a function of the catalog's content, not
	// of its physical layout. Leaf OIDs are derived from leaf names, so these
	// two catalogs are the same tree.
	second := mustPlan(t, newCatalog("p2", "p3", "p1"), newSpec())

	if first.Digest != second.Digest {
		t.Fatalf("digests differ across partition orderings:\n %s\n %s", first.Digest, second.Digest)
	}
	if first.TopologyFingerprint != second.TopologyFingerprint {
		t.Fatalf("fingerprints differ: %q vs %q", first.TopologyFingerprint, second.TopologyFingerprint)
	}
	a, err := protocol.EncodePlan(first)
	if err != nil {
		t.Fatalf("EncodePlan() error = %v", err)
	}
	b, err := protocol.EncodePlan(second)
	if err != nil {
		t.Fatalf("EncodePlan() error = %v", err)
	}
	if string(a) != string(b) {
		t.Fatal("encoded plans differ byte for byte")
	}
}

func TestPlanOrdersLeavesBySchemaThenName(t *testing.T) {
	cat := newCatalog()
	cat.setLeaves(
		protocol.NewObjectName("b", "a1"),
		protocol.NewObjectName("a", "z9"),
		protocol.NewObjectName("a", "a1"),
	)
	p := mustPlan(t, cat, newSpec())

	var creates []protocol.NodeID
	for i := range p.Nodes {
		if p.Nodes[i].Kind == protocol.KindIndexCreateConcurrently {
			creates = append(creates, p.Nodes[i].ID)
		}
	}
	want := []protocol.NodeID{"create:a.a1", "create:a.z9", "create:b.a1"}
	for i := range want {
		if creates[i] != want[i] {
			t.Fatalf("create order = %v, want %v", creates, want)
		}
	}
}

func TestPlanRoundTripsThroughTheArtifact(t *testing.T) {
	cat := newCatalog("p1", "p2")
	cat.indexes[child("p1")] = invalidIndex("p1")
	cat.claims = planner.NewFakeClaims(child("p1"))
	p := mustPlan(t, cat, newSpec())

	data, err := protocol.EncodePlan(p)
	if err != nil {
		t.Fatalf("EncodePlan() error = %v", err)
	}
	back, err := protocol.DecodePlan(data)
	if err != nil {
		t.Fatalf("DecodePlan() error = %v", err)
	}
	if err := back.VerifyDigest(); err != nil {
		t.Fatalf("decoded plan digest: %v", err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("decoded plan does not validate: %v", err)
	}
	if len(back.Nodes) != len(p.Nodes) {
		t.Fatalf("decoded %d nodes, wrote %d", len(back.Nodes), len(p.Nodes))
	}
	if back.Digest != p.Digest {
		t.Fatalf("digest changed across the artifact: %s -> %s", p.Digest, back.Digest)
	}
}

func TestPlanCarriesIdentityAndFingerprint(t *testing.T) {
	cat := newCatalog("p1", "p2")
	p := mustPlan(t, cat, newSpec())

	if p.FormatVersion != protocol.PlanFormatVersion {
		t.Errorf("format version = %d, want %d", p.FormatVersion, protocol.PlanFormatVersion)
	}
	if p.Operation != protocol.OpCreateIndex {
		t.Errorf("operation = %q, want %q", p.Operation, protocol.OpCreateIndex)
	}
	if p.Target.Database != testDB || p.Target.Table != obj(testTable) {
		t.Errorf("target = %+v, want database %s and table %s", p.Target, testDB, obj(testTable))
	}
	if p.Target.Index == nil || *p.Target.Index != obj(testIndex) {
		t.Errorf("target index = %v, want %s", p.Target.Index, obj(testIndex))
	}
	if p.TopologyFingerprint == "" {
		t.Error("plan carries no topology fingerprint (FR-PLANFILE-4)")
	}
	if p.Topology == nil || len(p.Topology.Partitions) != 2 {
		t.Errorf("plan does not carry the tree it was checked against: %+v", p.Topology)
	}
	if p.PlanID != testPlanID {
		t.Errorf("plan id = %q, want the host's %q", p.PlanID, testPlanID)
	}
	if !p.CreatedAt.Time.Equal(fixedNow()) {
		t.Errorf("created_at = %v, want %v", p.CreatedAt, fixedNow())
	}
	// Confirmations belong to DropPartitionedIndex, not to create.
	if len(p.Confirmations) != 0 {
		t.Errorf("create plan records confirmations %v", p.Confirmations)
	}
}

// ---------------------------------------------------------------------------
// Operator-facing notes
// ---------------------------------------------------------------------------

func TestNotesReportTheSkipCountAndTheMarker(t *testing.T) {
	leaves := []string{"p1", "p2", "p3", "p4"}
	cat := newCatalog(leaves...)
	cat.indexes[obj(testIndex)] = marked(parentIndexState(false))
	cat.indexes[child("p1")] = attachedIndex("p1")
	cat.indexes[child("p2")] = attachedIndex("p2")

	_, notes, err := tryPlan(t, cat, newSpec())
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "4 leaf partition(s) discovered; 2 index build(s) remain, 2 already complete") {
		t.Errorf("notes do not state the skip count (FR-PLAN-5):\n%s", joined)
	}
	if !strings.Contains(joined, "ownership marker") {
		t.Errorf("notes do not explain the ownership marker (AC-6):\n%s", joined)
	}
}

func TestNotesSayWhenNothingRemains(t *testing.T) {
	cat := newCatalog("p1")
	cat.indexes[obj(testIndex)] = parentIndexState(true)
	cat.indexes[child("p1")] = attachedIndex("p1")

	_, notes, err := tryPlan(t, cat, newSpec())
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "no DDL remains") {
		t.Errorf("a converged plan does not say so:\n%s", strings.Join(notes, "\n"))
	}
}

// ---------------------------------------------------------------------------
// rendered_sql
// ---------------------------------------------------------------------------

// The preview in the artifact is the executor's own renderer plus a terminator,
// so the two cannot drift (FR-PLANFILE-7, T2).
func TestRenderedSQLIsTheExecutorsRendererPlusATerminator(t *testing.T) {
	cat := newCatalog("p1")
	cat.indexes[child("p1")] = marked(invalidIndex("p1"))
	p := mustPlan(t, cat, newSpec())

	for i := range p.Nodes {
		n := &p.Nodes[i]
		if n.RenderedSQL == "" {
			t.Errorf("node %q has no rendered_sql", n.ID)
			continue
		}
		if !n.Kind.IssuesDDL() {
			if !strings.HasPrefix(n.RenderedSQL, "--") {
				t.Errorf("node %q issues no DDL but renders %q rather than a comment", n.ID, n.RenderedSQL)
			}
			continue
		}
		want, err := protocol.Render(n)
		if err != nil {
			t.Fatalf("protocol.Render(%q) error = %v", n.ID, err)
		}
		if !strings.HasPrefix(n.RenderedSQL, want+";") {
			t.Errorf("node %q previews\n  %q\nwhich does not start with the rendered statement\n  %q",
				n.ID, n.RenderedSQL, want+";")
		}
	}
}

// Every statement the run issues has to be in the artifact, including the
// ownership marker the executor writes after the DDL (G2).
func TestRenderedSQLShowsTheOwnershipMarkerStatement(t *testing.T) {
	p := mustPlan(t, newCatalog("p1"), newSpec())
	sql := node(t, p, nodeID("create", obj("p1"))).RenderedSQL
	if !strings.Contains(sql, "COMMENT ON INDEX") {
		t.Errorf("create node preview omits the marker statement:\n%s", sql)
	}
}

// ---------------------------------------------------------------------------
// Catalog access discipline
// ---------------------------------------------------------------------------

func TestPlanBatchesCatalogReads(t *testing.T) {
	leaves := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		leaves = append(leaves, fmt.Sprintf("p%03d", i))
	}
	cat := newCatalog(leaves...)
	fake := cat.build()
	host := &planner.Host{Catalog: fake, Now: fixedNow, NewPlanID: func() protocol.PlanID { return testPlanID }}

	out, err := host.Run(context.Background(), testPlanner, newSpec())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// NFR-PERF-1: discovery and inspection are O(1) queries in the number of
	// partitions. The only per-index read there could be is the marker
	// fallback, and a clean tree must not reach it at all.
	for method, want := range map[string]int{
		"PartitionTree":      1,
		"IndexesOnRelations": 1,
		"RoleMemberships":    1,
		"IndexComment":       0,
		"LookupIndex":        0,
		"PartitionStrategy":  1,
		"AssertReadOnly":     1,
		"ServerVersionNum":   1,
		"CurrentRole":        1,
		"CurrentDatabase":    1,
	} {
		if got := fake.Calls[method]; got != want {
			t.Errorf("%s called %d times, want %d (calls: %v)", method, got, want, fake.Calls)
		}
	}
	if got, want := len(out.Plan.Nodes), 2+len(leaves)*4+1; got != want {
		t.Errorf("plan has %d nodes, want %d", got, want)
	}
}

// indexReadFails is the operation's own catalog read failing. Discovery
// failures belong to the host and are covered by engine/planner's tests.
type indexReadFails struct {
	*planner.FakeCatalog
	err error
}

func (f indexReadFails) IndexesOnRelations(context.Context, []uint32) ([]planner.Index, error) {
	return nil, f.err
}

func TestPlanPropagatesCatalogErrors(t *testing.T) {
	boom := errors.New("connection reset")
	cat := newCatalog("p1")
	host := &planner.Host{
		Catalog:   indexReadFails{FakeCatalog: cat.build(), err: boom},
		Now:       fixedNow,
		NewPlanID: func() protocol.PlanID { return testPlanID },
	}
	out, err := host.Run(context.Background(), testPlanner, newSpec())
	if out != nil {
		t.Fatal("Run() returned an outcome despite a catalog error")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("Run() error = %v, want it to wrap %v", err, boom)
	}
}

func TestPlanPropagatesClaimErrors(t *testing.T) {
	boom := errors.New("state store unreachable")
	cat := newCatalog("p1")
	cat.indexes[child("p1")] = invalidIndex("p1")
	cat.claims = &planner.FakeClaims{Err: boom}

	p, _, err := tryPlan(t, cat, newSpec())
	if p != nil || !errors.Is(err, boom) {
		t.Fatalf("Plan() = (%v, %v), want (nil, wrapped %v)", p, err, boom)
	}
}

func TestPlanPropagatesClaimErrorsForTheParentIndex(t *testing.T) {
	boom := errors.New("state store unreachable")
	cat := newCatalog("p1")
	cat.indexes[obj(testIndex)] = parentIndexState(false)
	cat.claims = &planner.FakeClaims{Err: boom}

	p, _, err := tryPlan(t, cat, newSpec())
	if p != nil || !errors.Is(err, boom) {
		t.Fatalf("Plan() = (%v, %v), want (nil, wrapped %v)", p, err, boom)
	}
}

// TRD §14.2: child-index naming across 1,000 partitions with names forced past
// the identifier limit, asserting deterministic truncation and no collision.
func TestPlanAtScaleNamesEveryLeafWithoutCollision(t *testing.T) {
	const n = 1000
	prefix := strings.Repeat("orders_partition_", 3) // 51 bytes, so every child truncates
	leaves := make([]string, 0, n)
	for i := 0; i < n; i++ {
		leaves = append(leaves, fmt.Sprintf("%s%04d", prefix, i))
	}
	cat := newCatalog(leaves...)
	spec := newSpec()
	spec.Index = obj(strings.Repeat("x", 40))

	p := mustPlan(t, cat, spec)

	if got, want := len(p.Nodes), 2+n*4+1; got != want {
		t.Fatalf("plan has %d nodes, want %d", got, want)
	}
	seen := make(map[protocol.ObjectName]string, n)
	truncated := 0
	for i := range p.Nodes {
		params, ok := p.Nodes[i].Params.(*protocol.CreateConcurrentlyParams)
		if !ok {
			continue
		}
		if len(params.Index.Name) > protocol.MaxIdentifierBytes {
			t.Fatalf("child index name %q is %d bytes", params.Index.Name, len(params.Index.Name))
		}
		if prev, dup := seen[params.Index]; dup {
			t.Fatalf("partitions %s and %s collide on child index name %s",
				prev, params.Partition, params.Index)
		}
		seen[params.Index] = params.Partition.String()
		if params.Index.Name != spec.Index.Name+"_"+params.Partition.Name {
			truncated++
		}
	}
	if len(seen) != n {
		t.Fatalf("named %d children, want %d", len(seen), n)
	}
	if truncated != n {
		t.Fatalf("%d of %d names were truncated; the fixture was meant to force truncation on all", truncated, n)
	}
	if _, err := p.TopologicalOrder(); err != nil {
		t.Fatalf("TopologicalOrder() at scale: %v", err)
	}
}

// The digest is what a reviewer approves, so it must not move between runs of
// the same binary over the same inputs.
func TestPlanDigestIsStableAcrossRepeatedPlanning(t *testing.T) {
	var want string
	for i := 0; i < 20; i++ {
		p := mustPlan(t, newCatalog("p1", "p2", "p3"), newSpec())
		if i == 0 {
			want = p.Digest
			continue
		}
		if p.Digest != want {
			t.Fatalf("digest moved on run %d: %s -> %s", i, want, p.Digest)
		}
	}
}

// FR-PLAN-5 for the partition PostgreSQL indexed by itself.
//
// This is demo.sh's drift step, executed rather than merely planned. Adding a
// partition to a table that already carries the partitioned index makes the
// server build the child index immediately, under its own name, and attach it.
// The partition is done.
//
// The planner used to look only under the name it generates, see nothing, and
// emit create + verify + attach. The CREATE INDEX CONCURRENTLY is hours of
// wasted work on a real partition; the ATTACH then fails terminally with
// SQLSTATE 55000, because PostgreSQL permits one attached child index per
// partition per partitioned index and the slot is already taken. `resume`
// re-adopted the failed run and hit the same 55000; re-planning emitted a bare
// attach that failed the same way for ever. Only a human running DROP INDEX
// cleared the stray duplicate the run left behind.
func TestPlanEmitsNothingForAPartitionPostgreSQLAlreadyIndexed(t *testing.T) {
	cat := newCatalog("p1", "p2")
	cat.indexes[obj(testIndex)] = parentIndexState(true)
	cat.indexes[child("p1")] = attachedIndex("p1")

	// p2 was added after the parent index existed, so the server named and
	// attached its child index itself.
	propagated := attachedIndex("p2")
	propagated.Name = obj("p2_created_at_idx")
	propagated.Table = obj("p2")
	cat.indexes[obj("p2_created_at_idx")] = propagated

	p := mustPlan(t, cat, newSpec())

	for _, id := range []protocol.NodeID{
		nodeID("create", obj("p2")),
		nodeID("attach", obj("p2")),
		nodeID("verify", obj("p2")),
	} {
		if hasNode(p, id) {
			t.Errorf("plan emits %q for a partition PostgreSQL has already indexed and attached; "+
				"the create is wasted and the attach fails 55000", id)
		}
	}
	for _, n := range p.Nodes {
		if n.Kind.IssuesDDL() {
			t.Errorf("plan issues DDL (%s, %s) for a fully converged tree", n.ID, n.Kind)
		}
	}
}

// The same propagated index, but INVALID. It cannot be dropped individually and
// PostgreSQL has no DETACH, so nothing this planner emits can converge. It must
// refuse and say why rather than emit a graph that dies on the attach.
func TestPlanRefusesAnUnusableIndexAttachedUnderAnotherName(t *testing.T) {
	cat := newCatalog("p1")
	// The parent is valid: indisvalid on a partitioned index tracks whether
	// every partition has an attached child index, not whether those children
	// are usable. So this state is reachable and the parent check passes.
	cat.indexes[obj(testIndex)] = parentIndexState(true)

	broken := attachedIndex("p1")
	broken.Name = obj("p1_created_at_idx")
	broken.Table = obj("p1")
	broken.IsValid = false
	cat.indexes[obj("p1_created_at_idx")] = broken

	_, _, err := tryPlan(t, cat, newSpec())
	if err == nil {
		t.Fatal("planning succeeded against a partition whose attached child index is unusable")
	}
	for _, want := range []string{"p1_created_at_idx", "DETACH PARTITION"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}
