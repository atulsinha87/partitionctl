package dropindex

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/atulsinha87/partitionctl/engine/planner"
	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func run(t *testing.T, f *fixture, claims planner.ClaimLookup, confirmed bool) (*planner.Outcome, error) {
	t.Helper()
	return f.host(claims).Run(context.Background(), Planner{}, f.spec(confirmed))
}

func mustPlan(t *testing.T, f *fixture, claims planner.ClaimLookup) *planner.Outcome {
	t.Helper()
	out, err := run(t, f, claims, true)
	if err != nil {
		t.Fatalf("plan: unexpected error: %v", err)
	}
	return out
}

func nodeIDs(p *protocol.Plan) []protocol.NodeID {
	out := make([]protocol.NodeID, len(p.Nodes))
	for i := range p.Nodes {
		out[i] = p.Nodes[i].ID
	}
	return out
}

func nodeByID(t *testing.T, p *protocol.Plan, id protocol.NodeID) *protocol.Node {
	t.Helper()
	for i := range p.Nodes {
		if p.Nodes[i].ID == id {
			return &p.Nodes[i]
		}
	}
	t.Fatalf("plan has no node %q; it has %v", id, nodeIDs(p))
	return nil
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

// ---------------------------------------------------------------------------
// The graph (FR-DROP-4, FR-DROP-5, FR-DROP-7)
// ---------------------------------------------------------------------------

func TestPlanEmitsAssertDropVerifyForACleanTree(t *testing.T) {
	f := newFixture(t, 12)
	out := mustPlan(t, f, nil)

	want := []protocol.NodeID{nodeAssert, nodeDrop, nodeFinalVerify}
	got := nodeIDs(out.Plan)
	if len(got) != len(want) {
		t.Fatalf("plan has %d nodes (%v), want exactly %v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("node %d is %q, want %q", i, got[i], want[i])
		}
	}

	if deps := nodeByID(t, out.Plan, nodeDrop).DependsOn; len(deps) != 1 || deps[0] != nodeAssert {
		t.Errorf("the drop depends on %v, want [%s]: with no orphans the assert is its only predecessor",
			deps, nodeAssert)
	}
	if deps := nodeByID(t, out.Plan, nodeFinalVerify).DependsOn; len(deps) != 1 || deps[0] != nodeDrop {
		t.Errorf("the final verify depends on %v, want [%s]", deps, nodeDrop)
	}
}

// The whole operation is one statement, so there is nothing to pace between.
func TestPlanEmitsNoWaitNodes(t *testing.T) {
	f := newFixture(t, 12)
	f.detachChild(3, ourMarker(t, "run-abandoned"))
	spec := f.spec(true)
	spec.PaceSeconds = 30 // even when the operator asks for pacing

	out, err := f.host(nil).Run(context.Background(), Planner{}, spec)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if n := countKind(out.Plan, protocol.KindWait); n != 0 {
		t.Errorf("plan contains %d wait node(s); pacing a single atomic statement is meaningless", n)
	}
}

func TestDropNodeCarriesExplicitAuthorizationAndTheBlastRadius(t *testing.T) {
	f := newFixture(t, 12)
	out := mustPlan(t, f, nil)
	n := nodeByID(t, out.Plan, nodeDrop)

	if n.Kind != protocol.KindIndexDropPartitioned {
		t.Fatalf("drop node kind is %q, want %q", n.Kind, protocol.KindIndexDropPartitioned)
	}
	if got := countKind(out.Plan, protocol.KindIndexDropPartitioned); got != 1 {
		t.Errorf("plan has %d index.drop_partitioned nodes, want exactly 1 (FR-DROP-4, INV-8)", got)
	}

	auth := n.Authorization
	if auth == nil {
		t.Fatal("the drop node carries no authorization")
	}
	if auth.Mode != protocol.AuthExplicit {
		t.Errorf("authorization mode is %q, want %q (FR-DROP-4)", auth.Mode, protocol.AuthExplicit)
	}
	if auth.RequiredConfirmation != protocol.ConfirmExclusiveLock {
		t.Errorf("required confirmation is %q, want %q (FR-DROP-3)",
			auth.RequiredConfirmation, protocol.ConfirmExclusiveLock)
	}
	if auth.Object != f.index {
		t.Errorf("authorization names %s, want %s", auth.Object, f.index)
	}

	params, ok := n.Params.(*protocol.DropPartitionedParams)
	if !ok {
		t.Fatalf("drop node params are %T, want *protocol.DropPartitionedParams", n.Params)
	}
	if params.LeafCount != 12 {
		t.Errorf("leaf_count is %d, want 12: rendered_sql and `plan` state the blast radius from it (FR-DROP-5)",
			params.LeafCount)
	}
	if params.Parent != f.root || params.Index != f.index {
		t.Errorf("drop params name %s / %s, want %s / %s", params.Parent, params.Index, f.root, f.index)
	}
	if !strings.Contains(n.RenderedSQL, "AccessExclusiveLock") || !strings.Contains(n.RenderedSQL, "12 leaf") {
		t.Errorf("rendered_sql does not state the lock and the leaf count (FR-DROP-5):\n%s", n.RenderedSQL)
	}
}

func TestFinalVerifyAssertsAbsenceOfTheParentAndEveryGeneratedChild(t *testing.T) {
	f := newFixture(t, 12)
	out := mustPlan(t, f, nil)

	params, ok := nodeByID(t, out.Plan, nodeFinalVerify).Params.(*protocol.VerifyParams)
	if !ok {
		t.Fatalf("final verify params are %T, want *protocol.VerifyParams", nodeByID(t, out.Plan, nodeFinalVerify).Params)
	}
	if len(params.Checks) != 13 {
		t.Fatalf("final verify has %d checks, want 13: the parent index and 12 generated child names (FR-DROP-7)",
			len(params.Checks))
	}
	seen := map[protocol.ObjectName]bool{}
	for _, c := range params.Checks {
		if c.Check != protocol.CheckIndexAbsent {
			t.Errorf("check on %v is %q, want %q: absence is the only end state a drop has",
				c.Index, c.Check, protocol.CheckIndexAbsent)
		}
		if c.Index == nil {
			t.Fatal("an index_absent check names no index")
		}
		seen[*c.Index] = true
	}
	if !seen[f.index] {
		t.Error("the final verify does not assert the parent index is absent (FR-DROP-7)")
	}
	for _, child := range f.children {
		if !seen[child] {
			t.Errorf("the final verify does not assert %s is absent (FR-DROP-7)", child)
		}
	}
}

func TestAssertNodeUsesOnlyExistingAssertionKinds(t *testing.T) {
	f := newFixture(t, 12)
	out := mustPlan(t, f, nil)

	params, ok := nodeByID(t, out.Plan, nodeAssert).Params.(*protocol.CatalogAssertParams)
	if !ok {
		t.Fatal("the assert node does not carry CatalogAssertParams")
	}
	counts := map[protocol.AssertionKind]int{}
	for _, a := range params.Assertions {
		if !a.Assertion.Valid() {
			t.Errorf("assertion %q is not a known kind; this operation adds no evaluator work", a.Assertion)
		}
		counts[a.Assertion]++
	}
	for _, want := range []protocol.AssertionKind{
		protocol.AssertRelationIsPartitioned,
		protocol.AssertIndexExists,
		protocol.AssertIndexIsPartitioned,
		protocol.AssertIndexNotConstraintBacked,
	} {
		if counts[want] != 1 {
			t.Errorf("assertion %q appears %d times, want 1", want, counts[want])
		}
	}
	// One per relation the drop locks: the parent and every leaf. A role that
	// owns the parent but not one leaf cannot run the statement at all, and
	// discovering that at lock-acquisition time means discovering it while
	// holding the whole tree.
	if counts[protocol.AssertRoleMembership] != 13 {
		t.Errorf("role membership is asserted %d times, want 13 (parent + 12 leaves)",
			counts[protocol.AssertRoleMembership])
	}
}

// A plan is reviewed, committed and executed later, so two passes over an
// unchanged catalog must produce the same artifact or the review is worthless.
func TestPlanIsDeterministic(t *testing.T) {
	a := mustPlan(t, newFixture(t, 12), nil)
	b := mustPlan(t, newFixture(t, 12), nil)
	if a.Plan.Digest != b.Plan.Digest {
		t.Errorf("two passes over the same catalog produced different digests:\n%s\n%s",
			a.Plan.Digest, b.Plan.Digest)
	}
}

// ---------------------------------------------------------------------------
// Orphans (TRD §7.2.13 step 1)
// ---------------------------------------------------------------------------

func TestOrphanWeCanProveWeCreatedIsDroppedConcurrentlyFirst(t *testing.T) {
	f := newFixture(t, 12)
	f.detachChild(3, ourMarker(t, "run-abandoned"))
	orphan := f.children[3]

	out := mustPlan(t, f, nil)

	id := orphanNodeID(orphan)
	n := nodeByID(t, out.Plan, id)
	if n.Kind != protocol.KindIndexDropConcurrently {
		t.Errorf("orphan node kind is %q, want %q", n.Kind, protocol.KindIndexDropConcurrently)
	}
	if n.Authorization == nil || n.Authorization.Mode != protocol.AuthProvenance {
		t.Errorf("orphan drop is not authorized under mode %q", protocol.AuthProvenance)
	}
	if deps := n.DependsOn; len(deps) != 1 || deps[0] != nodeAssert {
		t.Errorf("orphan drop depends on %v, want [%s]", deps, nodeAssert)
	}
	drop := nodeByID(t, out.Plan, nodeDrop)
	if len(drop.DependsOn) != 1 || drop.DependsOn[0] != id {
		t.Errorf("the parent drop depends on %v, want [%s]: the orphan must be gone first, because "+
			"after the parent is dropped its child names can no longer be derived", drop.DependsOn, id)
	}
	if !strings.Contains(n.RenderedSQL, "DROP INDEX CONCURRENTLY") {
		t.Errorf("orphan rendered_sql is %q; an unattached leaf index comes out online", n.RenderedSQL)
	}
}

func TestEveryOurOrphanBecomesItsOwnNodeAndTheDropWaitsForAllOfThem(t *testing.T) {
	f := newFixture(t, 12)
	for _, i := range []int{0, 5, 11} {
		f.detachChild(i, ourMarker(t, "run-abandoned"))
	}
	out := mustPlan(t, f, nil)

	if got := countKind(out.Plan, protocol.KindIndexDropConcurrently); got != 3 {
		t.Fatalf("plan has %d orphan drops, want 3", got)
	}
	drop := nodeByID(t, out.Plan, nodeDrop)
	if len(drop.DependsOn) != 3 {
		t.Fatalf("the parent drop depends on %v, want all three orphan drops", drop.DependsOn)
	}
	if err := out.Plan.Validate(); err != nil {
		// INV-8 as amended: index.drop_concurrently is the one destructive kind
		// permitted alongside index.drop_partitioned.
		t.Errorf("a plan with orphan drops alongside the partitioned drop is invalid: %v", err)
	}
}

// The rule that is easiest to get backwards: a foreign index under a generated
// name is skipped, never halted on. Somebody else's object must not make the
// operator's own drop impossible.
func TestForeignOrphanIsSkippedWithANoteAndNeverHaltsThePlan(t *testing.T) {
	cases := map[string]string{
		"no marker at all":            "",
		"a human's comment":           "do not touch, owned by the reporting team",
		"a marker we cannot read":     "partitionctl:v9:{\"run\":\"?\"}",
		"a marker naming another run": "", // filled below with a foreign-shaped comment
	}
	cases["a marker naming another run"] = "partitionctl:v1:not-json"

	for name, comment := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, 12)
			f.detachChild(7, comment)
			foreign := f.children[7]

			out := mustPlan(t, f, nil)

			if countKind(out.Plan, protocol.KindIndexDropConcurrently) != 0 {
				t.Errorf("plan drops an orphan it cannot prove it created")
			}
			if countKind(out.Plan, protocol.KindIndexDropPartitioned) != 1 {
				t.Fatal("the operator's own drop was not planned")
			}
			var noted bool
			for _, line := range out.Notes {
				if strings.Contains(line, foreign.String()) && strings.Contains(line, "skipped") {
					noted = true
				}
			}
			if !noted {
				t.Errorf("no note names the skipped index %s; the operator must be told:\n%v",
					foreign, out.Notes)
			}
			// Asserting the absence of an object the plan deliberately declined
			// to remove would guarantee a failed run after a successful drop.
			params := nodeByID(t, out.Plan, nodeFinalVerify).Params.(*protocol.VerifyParams)
			for _, c := range params.Checks {
				if c.Index != nil && *c.Index == foreign {
					t.Errorf("the final verify asserts %s is absent, but the plan does not remove it",
						foreign)
				}
			}
			if len(params.Checks) != 12 {
				t.Errorf("final verify has %d checks, want 12: the parent and 11 unskipped children",
					len(params.Checks))
			}
		})
	}
}

// The one window the marker cannot cover: the statement ran and the process
// died before the COMMENT. The live claim is what closes it.
func TestUnmarkedOrphanWithALiveClaimIsAdoptedThenDropped(t *testing.T) {
	f := newFixture(t, 12)
	f.detachChild(2, "")
	orphan := f.children[2]

	out := mustPlan(t, f, planner.NewFakeClaims(orphan))

	n := nodeByID(t, out.Plan, orphanNodeID(orphan))
	if n.Authorization == nil || n.Authorization.Mode != protocol.AuthProvenance {
		t.Fatalf("the adopted orphan is not authorized under mode %q", protocol.AuthProvenance)
	}
	// Without the claim the same catalog halts on it.
	bare := newFixture(t, 12)
	bare.detachChild(2, "")
	if got := countKind(mustPlan(t, bare, nil).Plan, protocol.KindIndexDropConcurrently); got != 0 {
		t.Errorf("the same orphan is dropped with no claim and no marker; %d node(s) emitted", got)
	}
}

// An index at a generated name that belongs to a different index family is not
// this plan's orphan, and the parent drop does not cascade to it.
func TestIndexAttachedToAnotherFamilyIsSkipped(t *testing.T) {
	f := newFixture(t, 12)
	for j := range f.cat.Indexes {
		if f.cat.Indexes[j].Name == f.children[4] {
			f.cat.Indexes[j].ParentIndexOID = 9999
		}
	}
	out := mustPlan(t, f, nil)

	if countKind(out.Plan, protocol.KindIndexDropConcurrently) != 0 {
		t.Error("plan drops an index attached to another partitioned index")
	}
	params := nodeByID(t, out.Plan, nodeFinalVerify).Params.(*protocol.VerifyParams)
	for _, c := range params.Checks {
		if c.Index != nil && *c.Index == f.children[4] {
			t.Errorf("the final verify asserts %s is absent, but nothing in this plan removes it",
				f.children[4])
		}
	}
}

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

func TestPlanRefusesWithoutTheExclusiveLockAcknowledgement(t *testing.T) {
	f := newFixture(t, 12)
	_, err := run(t, f, nil, false)
	if err == nil {
		t.Fatal("planned a drop with no acknowledgement (FR-DROP-3, AC-13)")
	}
	if !isErr(err, protocol.ErrAuthorizationUnsatisfied) {
		t.Errorf("error is %v, want the authorization-unsatisfied class (exit 13)", err)
	}
	for _, want := range []string{protocol.ConfirmExclusiveLock, "AccessExclusiveLock", "12 leaf"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}
}

func TestPlanRefusesAConstraintBackedIndexAndNamesTheStatementToRunInstead(t *testing.T) {
	for _, tc := range []struct {
		contype string
		name    string
		want    string
	}{
		{"p", "events_pkey", "PRIMARY KEY"},
		{"u", "events_ts_key", "UNIQUE"},
		// FR-DROP-2 names only UNIQUE and PRIMARY KEY. An exclusion
		// constraint's index is equally undroppable, so it is refused too
		// (AC-14).
		{"x", "events_no_overlap", "EXCLUDE"},
	} {
		t.Run(tc.contype, func(t *testing.T) {
			f := newFixture(t, 12)
			f.setTarget(func(i *planner.Index) {
				i.ConstraintName = tc.name
				i.ConstraintType = tc.contype
			})
			_, err := run(t, f, nil, true)
			if err == nil {
				t.Fatalf("planned a drop of a %s-backed index (FR-DROP-2, AC-14)", tc.want)
			}
			for _, want := range []string{tc.want, tc.name, "ALTER TABLE", "DROP CONSTRAINT"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q:\n%v", want, err)
				}
			}
		})
	}
}

func TestPlanRefusesAnythingThatIsNotAPartitionedIndexOnTheNamedTable(t *testing.T) {
	t.Run("an ordinary index", func(t *testing.T) {
		f := newFixture(t, 12)
		f.setTarget(func(i *planner.Index) { i.Kind = planner.RelKindIndex })
		_, err := run(t, f, nil, true)
		if err == nil {
			t.Fatal("planned a drop of an ordinary index (FR-DROP-1)")
		}
		if !strings.Contains(err.Error(), "DROP INDEX CONCURRENTLY") {
			t.Errorf("the refusal does not point at the online statement that does work:\n%v", err)
		}
	})

	t.Run("an index on another table", func(t *testing.T) {
		f := newFixture(t, 12)
		f.setTarget(func(i *planner.Index) {
			i.TableOID = 7777
			i.Table = protocol.NewObjectName("public", "other")
		})
		_, err := run(t, f, nil, true)
		if err == nil {
			t.Fatal("planned a drop of an index on a table the specification does not name (FR-DROP-1)")
		}
		if !isErr(err, protocol.ErrUnsupportedTopology) {
			t.Errorf("error is %v, want the unsupported-topology class", err)
		}
	})

	t.Run("no such index", func(t *testing.T) {
		f := newFixture(t, 12)
		f.cat.Indexes = nil
		_, err := run(t, f, nil, true)
		if err == nil {
			t.Fatal("planned a drop of an index that does not exist (FR-DROP-1)")
		}
		if !isErr(err, planner.ErrIndexNotFound) {
			t.Errorf("error is %v, want the index-not-found class", err)
		}
	})
}

// ---------------------------------------------------------------------------
// The two rules the directive says are easiest to get wrong
// ---------------------------------------------------------------------------

// drop-index sets AuthExplicit and does not consult the marker. An index this
// tool did not create is still the operator's to drop; requiring provenance
// here would mean the tool could destroy only what it had built.
func TestTheTargetsMarkerIsNeverConsulted(t *testing.T) {
	for name, comment := range map[string]string{
		"no marker":        "",
		"a human comment":  "created by hand in 2019, do not remove",
		"a foreign marker": "partitionctl:v1:not-json",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, 12)
			f.setTarget(func(i *planner.Index) { i.Comment = comment })
			out := mustPlan(t, f, nil)
			if countKind(out.Plan, protocol.KindIndexDropPartitioned) != 1 {
				t.Fatal("the marker on the target changed whether the drop was planned")
			}
			if n := nodeByID(t, out.Plan, nodeDrop); n.Authorization.Mode != protocol.AuthExplicit {
				t.Errorf("authorization mode is %q, want %q", n.Authorization.Mode, protocol.AuthExplicit)
			}
		})
	}
}

// AllowNoPartitions is specific to this operation: a tree whose partitions were
// all detached still has a partitioned index, and this is the only operation
// that removes it.
func TestPlanRunsAgainstATreeWithNoPartitions(t *testing.T) {
	f := newFixture(t, 0)
	out := mustPlan(t, f, nil)

	params := nodeByID(t, out.Plan, nodeDrop).Params.(*protocol.DropPartitionedParams)
	if params.LeafCount != 0 {
		t.Errorf("leaf_count is %d, want 0", params.LeafCount)
	}
	verify := nodeByID(t, out.Plan, nodeFinalVerify).Params.(*protocol.VerifyParams)
	if len(verify.Checks) != 1 {
		t.Errorf("final verify has %d checks, want 1: the parent index alone", len(verify.Checks))
	}
	if f.cat.Calls["IndexesOnRelations"] != 0 {
		t.Errorf("the orphan scan queried the catalog for a tree with no leaves")
	}
}

// NFR-PERF-1: the orphan scan is one query, not one per partition.
func TestOrphanScanIsOneQueryRegardlessOfPartitionCount(t *testing.T) {
	f := newFixture(t, 12)
	mustPlan(t, f, nil)
	if got := f.cat.Calls["IndexesOnRelations"]; got != 1 {
		t.Errorf("IndexesOnRelations was called %d times, want 1", got)
	}
	if got := f.cat.Calls["IndexComment"]; got != 0 {
		t.Errorf("IndexComment was called %d times; the comment rides along with the batched pass", got)
	}
}

func TestOperationName(t *testing.T) {
	if got := (Planner{}).Operation(); got != protocol.OpDropIndex {
		t.Errorf("Operation() is %q, want %q", got, protocol.OpDropIndex)
	}
}

func TestNotesStateTheBlastRadius(t *testing.T) {
	f := newFixture(t, 12)
	out := mustPlan(t, f, nil)
	joined := strings.Join(out.Notes, "\n")
	for _, want := range []string{"AccessExclusiveLock", "12 leaf partition(s)", "FR-DROP-5"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the operator notes do not mention %q:\n%s", want, joined)
		}
	}
}

// isErr reports whether err belongs to the given protocol error class.
func isErr(err error, class *protocol.Error) bool {
	return err != nil && class != nil && errors.Is(err, class)
}
