package createindex

import (
	"context"
	"fmt"

	"github.com/atulsinha/partitionctl/engine/planner"
	"github.com/atulsinha/partitionctl/engine/protocol"
)

// Node IDs. They are deterministic functions of the relation they act on, so a
// re-plan over an unchanged catalog produces the same graph, and so a reviewer
// reading `status` output can tell which partition a node belongs to.
const (
	nodeAssert      protocol.NodeID = "assert.preconditions"
	nodeParentIndex protocol.NodeID = "index.parent"
	nodeFinalVerify protocol.NodeID = "verify.final"
)

func nodeID(step string, leaf protocol.ObjectName) protocol.NodeID {
	return protocol.NodeID(step + ":" + leaf.String())
}

// Planner implements [planner.OperationPlanner] for CreatePartitionedIndex.
//
// The zero value is the whole configuration. Everything the operation needs
// arrives on [planner.Request]: the host has already proved the session is
// read-only (FR-PLAN-8), checked the server version (NFR-COMPAT-1), discovered
// and validated the tree (FR-PLAN-1..3) and checked role membership
// (FR-PLAN-10), and it owns plan identity, the topology fingerprint and the
// digest. What is left here is the one thing only this operation knows: which
// nodes to emit.
type Planner struct{}

var _ planner.OperationPlanner = Planner{}

// Operation names the operation this planner implements.
func (Planner) Operation() protocol.Operation { return protocol.OpCreateIndex }

// Plan emits the build graph for one specification against one discovered
// topology (FR-PLAN-12).
//
// It issues no DDL and opens no write transaction: the only database access
// available to it is [planner.Request.Catalog], which is read-only. It emits
// only the work that remains (FR-PLAN-5), and it returns an error and no nodes
// at all rather than a graph it cannot justify: a name occupied by something
// that is not ours, or an unusable index this tool cannot prove it created
// (exit 13, FR-PLAN-7, AC-6).
func (Planner) Plan(ctx context.Context, req planner.Request) (planner.Result, error) {
	parent := req.Topology.Root.Name

	parentIndex := req.Spec.Index
	if parentIndex.Schema == "" {
		parentIndex.Schema = parent.Schema
	}

	// One pg_index pass over the whole tree, which generates every child index
	// name and proves the set collision-free (FR-PLAN-4, FR-PLAN-11,
	// FR-PLAN-13). `resume` calls the identical function, which is what keeps
	// the two paths from disagreeing about whether a tree is legal (AC-4).
	insp, err := planner.InspectChildren(ctx, req.Catalog, req.Topology, parentIndex)
	if err != nil {
		return planner.Result{}, err
	}
	if err := checkChildNames(insp, parentIndex); err != nil {
		return planner.Result{}, err
	}

	needParent, err := classifyParentIndex(ctx, req, insp, parent, parentIndex)
	if err != nil {
		return planner.Result{}, err
	}

	relations := append([]protocol.ObjectName{parent}, req.Topology.LeafObjectNames()...)

	nodes := make([]protocol.Node, 0, 2+len(insp.Children)*5)
	nodes = append(nodes, assertNode(req.Role, parent, parentIndex, req.Topology.Strategy, relations))

	chainRoot := nodeAssert
	if needParent {
		nodes = append(nodes, parentIndexNode(req, parent, parentIndex))
		chainRoot = nodeParentIndex
	}

	var tails []protocol.NodeID
	drops, builds := 0, 0
	for _, c := range insp.Children {
		chain, err := leafChain(ctx, req, insp, c, parentIndex, chainRoot)
		if err != nil {
			return planner.Result{}, err
		}
		if len(chain) == 0 {
			continue
		}
		for i := range chain {
			switch chain[i].Kind {
			case protocol.KindIndexDropConcurrently:
				drops++
			case protocol.KindIndexCreateConcurrently:
				builds++
			}
		}
		nodes = append(nodes, chain...)
		tails = append(tails, chain[len(chain)-1].ID)
	}
	if len(tails) == 0 {
		// Nothing remained to build. The final verify still runs, so a
		// converged plan is a checked no-op rather than an empty one (AC-7).
		tails = []protocol.NodeID{chainRoot}
	}

	children := make([]protocol.ObjectName, len(insp.Children))
	for i, c := range insp.Children {
		children[i] = c.ChildIndex
	}
	nodes = append(nodes, finalVerifyNode(parentIndex, children, tails))

	hasDDL := false
	for i := range nodes {
		if nodes[i].Kind.IssuesDDL() {
			hasDDL = true
			break
		}
	}
	return planner.Result{Nodes: nodes, Notes: notes(len(children), builds, drops, hasDDL)}, nil
}

// checkChildNames applies the two checks that are specific to emitting a plan,
// on top of the collision proof [planner.InspectChildren] has already made: a
// generated name must be a legal identifier, and it must not equal the parent
// index name.
func checkChildNames(insp planner.ChildIndexInspection, parentIndex protocol.ObjectName) error {
	for _, c := range insp.Children {
		if err := c.ChildIndex.Validate(); err != nil {
			return protocol.ErrInvalidIdentifier.Detailf(
				"generated child index name for %s: %v", c.Leaf.Name, err)
		}
		if c.ChildIndex == parentIndex {
			return protocol.ErrNameCollision.Detailf(
				"the child index name generated for %s equals the parent index name %s",
				c.Leaf.Name, parentIndex)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Existing-state classification (FR-PLAN-4, FR-PLAN-5, FR-PLAN-6, FR-PLAN-7)
// ---------------------------------------------------------------------------

// classifyParentIndex decides whether index.create_parent_invalid is still
// needed, and refuses to adopt a parent index that is not ours.
//
// A parent index that exists and is INVALID is the normal mid-build state, so
// adopting it is exactly what resume must do. Adopting one PartitionCTL cannot
// prove it created is not: the precondition in TRD §7.2.13 is that the target
// index name is free *or* matches an in-progress build carrying PartitionCTL's
// ownership marker.
func classifyParentIndex(
	ctx context.Context,
	req planner.Request,
	insp planner.ChildIndexInspection,
	parent, parentIndex protocol.ObjectName,
) (needCreate bool, err error) {
	existing := insp.ParentIndex
	if existing == nil {
		return true, nil
	}
	if existing.Kind != planner.RelKindPartitionedIndex {
		return false, protocol.ErrFailure.Detailf(
			"the index name %s is already taken by an ordinary index; "+
				"CreatePartitionedIndex needs the name for a partitioned index on %s", parentIndex, parent)
	}
	if existing.Condition().Usable() {
		return false, nil
	}

	// Unusable, and the name is the one we want. The shared decision table
	// answers whether it is ours to adopt; a plan that adopted somebody else's
	// half-built index would attach leaves to it (FR-PLAN-7, AC-6).
	_, verdict, err := planner.DecideCleanup(ctx, req.Catalog, req.Claims, planner.ChildIndexPlan{
		Leaf:       req.Topology.Root,
		ChildIndex: parentIndex,
		Existing:   existing,
		Condition:  existing.Condition(),
	})
	if err != nil {
		if verdict.Reason == "" {
			return false, err
		}
		return false, planner.ErrForeignInvalidIndex.Detailf(
			"%s already exists and is %s: %s. This is an in-progress build belonging to something "+
				"else. Halting with no plan: resolve it by hand, then re-plan (FR-PLAN-7, AC-6)",
			parentIndex, existing.Condition(), verdict.Reason)
	}
	return false, nil
}

// leafChain returns the nodes still needed for one leaf partition, in order, or
// nil when that leaf is already done (FR-PLAN-5).
//
// The five shapes it can return are the whole of the operation's idempotence:
//
//   - already built, valid and attached: no nodes at all;
//   - built and valid but unattached: verify → attach → wait;
//   - absent: create → verify → attach → wait;
//   - unusable and ours: drop → create → verify → attach → wait
//     (FR-PLAN-6, AC-5);
//   - unusable and not provably ours: no plan at all (FR-PLAN-7, AC-6).
func leafChain(
	ctx context.Context,
	req planner.Request,
	insp planner.ChildIndexInspection,
	c planner.ChildIndexPlan,
	parentIndex protocol.ObjectName,
	root protocol.NodeID,
) ([]protocol.Node, error) {
	parentOID := insp.ParentIndexOID()
	leaf, child := c.Leaf.Name, c.ChildIndex

	needDrop := false
	needCreate := true

	if c.Exists() {
		st := c.Existing
		if st.Kind == planner.RelKindPartitionedIndex {
			return nil, protocol.ErrFailure.Detailf(
				"the child index name %s generated for partition %s is a partitioned index, "+
					"not a leaf index. Halting", child, leaf)
		}
		if st.Attached() && !st.AttachedTo(parentOID) {
			return nil, protocol.ErrFailure.Detailf(
				"%s is already attached to another partitioned index, not to %s. Halting: PostgreSQL has no "+
					"ALTER INDEX ... DETACH PARTITION, so this cannot be undone by the tool (TRD §7.2.10)",
				child, parentIndex)
		}
		switch {
		case c.Complete(parentOID):
			// Built, verified and attached. Nothing remains (FR-PLAN-5).
			return nil, nil
		case st.Condition().Usable():
			// Built but not yet attached: the interruption landed between
			// CREATE INDEX CONCURRENTLY and ATTACH PARTITION. Rebuilding would
			// be hours of wasted work, so only the tail of the chain is
			// emitted.
			needCreate = false
		case st.Attached():
			// Unusable and attached. An attached child index is a dependency of
			// its partitioned parent and cannot be dropped individually, and
			// there is no DETACH (TRD §7.2.10), so no graph this planner can
			// emit will fix it.
			return nil, protocol.ErrFailure.Detailf(
				"%s is attached to %s but is %s. PostgreSQL cannot drop an attached child "+
					"index individually and offers no ALTER INDEX ... DETACH PARTITION (TRD §7.2.10), "+
					"so this needs manual repair: DROP INDEX %s takes AccessExclusiveLock on the whole tree",
				child, parentIndex, st.Condition(), parentIndex.Quoted())
		default:
			// Unattached wreckage of an interrupted CREATE INDEX CONCURRENTLY.
			// Droppable online, but only with proof we created it.
			decision, _, err := planner.DecideCleanup(ctx, req.Catalog, req.Claims, c)
			if err != nil {
				return nil, err
			}
			if !decision.Destructive() {
				return nil, planner.ErrForeignInvalidIndex.Detailf(
					"%s on %s is %s and is not provably ours. Halting with no plan (FR-PLAN-7, AC-6)",
					child, leaf, st.Condition())
			}
			needDrop = true
		}
	}

	chain := make([]protocol.Node, 0, 5)
	prev := root
	add := func(n protocol.Node) {
		n.DependsOn = []protocol.NodeID{prev}
		chain = append(chain, n)
		prev = n.ID
	}

	if needDrop {
		add(dropNode(req, leaf, child))
	}
	if needCreate {
		add(createNode(req, leaf, child, parentIndex))
	}
	add(leafVerifyNode(leaf, child))
	add(attachNode(req, leaf, child, parentIndex))
	add(waitNode(req, leaf))
	return chain, nil
}

// ---------------------------------------------------------------------------
// Node construction
// ---------------------------------------------------------------------------

// assertNode carries every precondition from TRD §7.2.13 in a single
// catalog.assert evaluated before anything else runs.
//
// The host has already checked all of these against the catalog it planned
// from; recording them as assertions is what makes the executor re-check them
// against live state, so a plan that was valid an hour ago fails at exit 15 or
// 16 rather than half-running.
func assertNode(
	role string,
	parent, parentIndex protocol.ObjectName,
	strategy protocol.PartitionStrategy,
	relations []protocol.ObjectName,
) protocol.Node {
	parentRef := parent
	indexRef := parentIndex

	assertions := []protocol.Assertion{
		{
			Assertion:   protocol.AssertRelationIsPartitioned,
			Relation:    &parentRef,
			FailureCode: protocol.ExitUnsupportedTopology,
			Message:     parent.String() + " must be a partitioned table (relkind 'p')",
		},
		{
			Assertion:   protocol.AssertPartitionStrategy,
			Relation:    &parentRef,
			Expected:    []string{string(protocol.StrategyRange), string(protocol.StrategyList)},
			FailureCode: protocol.ExitUnsupportedTopology,
			Message: fmt.Sprintf("%s must be RANGE or LIST partitioned; planned against %s (FR-PLAN-3)",
				parent, strategy),
		},
		{
			Assertion:   protocol.AssertPartitionDepth,
			Relation:    &parentRef,
			Expected:    []string{"1"},
			FailureCode: protocol.ExitUnsupportedTopology,
			Message:     "the partition tree rooted at " + parent.String() + " must be exactly one level deep (FR-PLAN-2)",
		},
		{
			Assertion:   protocol.AssertNoDefaultPartition,
			Relation:    &parentRef,
			FailureCode: protocol.ExitUnsupportedTopology,
			Message:     parent.String() + " must have no DEFAULT partition (FR-PLAN-3)",
		},
	}

	// Role membership, over the parent and every leaf. It is one precondition
	// expressed as one assertion per relation, because ownership is per
	// relation and a run that can write to the parent but not to a leaf fails
	// halfway through (FR-PLAN-10, AC-12).
	for _, r := range relations {
		rel := r
		assertions = append(assertions, protocol.Assertion{
			Assertion:   protocol.AssertRoleMembership,
			Relation:    &rel,
			Role:        role,
			FailureCode: protocol.ExitInsufficientPrivilege,
			Message:     "role " + role + " must be a member of the owning role of " + rel.String(),
		})
	}

	assertions = append(assertions, protocol.Assertion{
		Assertion:   protocol.AssertIndexNameAvailable,
		Relation:    &parentRef,
		Index:       &indexRef,
		FailureCode: protocol.ExitAuthorizationUnsatisfied,
		Message: parentIndex.String() + " must be free, or be an in-progress build with PartitionCTL " +
			"provenance (FR-PLAN-7)",
	})

	return protocol.Node{
		ID:          nodeAssert,
		Kind:        protocol.KindCatalogAssert,
		Params:      &protocol.CatalogAssertParams{Assertions: assertions},
		RenderedSQL: renderAssertComment(len(assertions)),
	}
}

// parentIndexNode issues CREATE INDEX ON ONLY <parent>: catalog-only, no
// partition data scanned, and ONLY is what stops PostgreSQL recursing into the
// leaves. The resulting index is deliberately INVALID for the whole build.
func parentIndexNode(req planner.Request, parent, parentIndex protocol.ObjectName) protocol.Node {
	return preview(protocol.Node{
		ID:   nodeParentIndex,
		Kind: protocol.KindIndexCreateParentInvalid,
		Params: &protocol.CreateParentInvalidParams{
			Parent:     parent,
			Index:      parentIndex,
			Definition: req.Spec.Definition,
		},
		DependsOn:        []protocol.NodeID{nodeAssert},
		EstimatedSeconds: req.Estimator.CatalogNodeSeconds(),
	})
}

// dropNode removes the unusable leaf index left by an interrupted CREATE INDEX
// CONCURRENTLY, so the rebuild has a free name (FR-PLAN-6, AC-5).
//
// The authorization it carries is [protocol.AuthProvenance] and it is a
// proposal only: the executor re-evaluates it against live state immediately
// before dispatch and halts if it is unsatisfied, whatever the plan asserts
// (FR-AUTH-5, INV-2).
func dropNode(req planner.Request, leaf, child protocol.ObjectName) protocol.Node {
	relation := leaf
	n := preview(protocol.Node{
		ID:   nodeID("drop", leaf),
		Kind: protocol.KindIndexDropConcurrently,
		Params: &protocol.DropConcurrentlyParams{
			Index:    child,
			Relation: &relation,
			Reason:   protocol.DropInvalidBuild,
		},
		EstimatedSeconds: req.Estimator.CatalogNodeSeconds(),
	})
	n.Authorization = &protocol.Authorization{
		Mode:     protocol.AuthProvenance,
		Object:   child,
		Relation: &relation,
		Note: "an unusable index left by an interrupted CREATE INDEX CONCURRENTLY, " +
			"carrying PartitionCTL's ownership marker (FR-PLAN-6, AC-5)",
	}
	return n
}

// createNode builds one leaf index. This is the node that runs for hours: two
// table scans, and it waits for every transaction that could see the index.
func createNode(req planner.Request, leaf, child, parentIndex protocol.ObjectName) protocol.Node {
	pi := parentIndex
	pages := int64(0)
	if r, ok := leafRelation(req, leaf); ok {
		pages = r.RelPages
	}
	return preview(protocol.Node{
		ID:   nodeID("create", leaf),
		Kind: protocol.KindIndexCreateConcurrently,
		Params: &protocol.CreateConcurrentlyParams{
			Partition:   leaf,
			Index:       child,
			Definition:  req.Spec.Definition,
			ParentIndex: &pi,
		},
		EstimatedSeconds: req.Estimator.BuildSeconds(pages),
	})
}

func leafRelation(req planner.Request, leaf protocol.ObjectName) (planner.Relation, bool) {
	for _, r := range req.Topology.Leaves {
		if r.Name == leaf {
			return r, true
		}
	}
	return planner.Relation{}, false
}

// leafVerifyNode checks the child before it is attached: indisvalid, indisready
// and indislive (FR-VER-1). Attaching an index that is not valid would put the
// tree into a state PostgreSQL gives no online way out of.
func leafVerifyNode(leaf, child protocol.ObjectName) protocol.Node {
	idx := child
	return protocol.Node{
		ID:   nodeID("verify", leaf),
		Kind: protocol.KindIndexVerify,
		Params: &protocol.VerifyParams{Checks: []protocol.VerifyCheck{{
			Check:   protocol.CheckIndexValid,
			Index:   &idx,
			Message: child.String() + " must be valid, ready and live before it is attached (FR-VER-1)",
		}}},
		RenderedSQL: renderLeafVerifyComment(child),
	}
}

// attachNode issues ALTER INDEX <parent> ATTACH PARTITION <child>. Catalog
// only. On the final attach PostgreSQL marks the parent index valid by itself;
// no statement is issued for that.
func attachNode(req planner.Request, leaf, child, parentIndex protocol.ObjectName) protocol.Node {
	return preview(protocol.Node{
		ID:               nodeID("attach", leaf),
		Kind:             protocol.KindIndexAttach,
		Params:           &protocol.AttachParams{ParentIndex: parentIndex, ChildIndex: child},
		EstimatedSeconds: req.Estimator.CatalogNodeSeconds(),
	})
}

// waitNode is the planner-emitted pause between leaves (FR-ORD-3). It is a node
// so that every pause is visible in the plan the operator reviews; the executor
// introduces no delays of its own.
func waitNode(req planner.Request, leaf protocol.ObjectName) protocol.Node {
	reason := req.Spec.PaceReason
	if reason == "" {
		reason = "pacing after " + leaf.String() + " attached (FR-ORD-3)"
	}
	seconds := req.Spec.PaceSeconds
	return protocol.Node{
		ID:               nodeID("wait", leaf),
		Kind:             protocol.KindWait,
		Params:           &protocol.WaitParams{Seconds: seconds, Reason: reason},
		RenderedSQL:      renderWaitComment(seconds),
		EstimatedSeconds: req.Estimator.WaitSeconds(seconds),
	}
}

// finalVerifyNode is the node with N incoming edges that TRD §7.2.2 says a
// barrier already is. It proves the whole build: the parent index is valid,
// the leaf index count equals the partition count (FR-VER-3, FR-VER-4), and
// every leaf index is attached in pg_inherits (FR-VER-2).
//
// The attachment checks cover every leaf, including the ones this plan emitted
// no work for. That is what makes a converged plan a proof of the end state
// rather than a proof that nothing happened.
func finalVerifyNode(parentIndex protocol.ObjectName, children []protocol.ObjectName, deps []protocol.NodeID) protocol.Node {
	pi := parentIndex
	count := len(children)
	checks := make([]protocol.VerifyCheck, 0, 2+len(children))
	checks = append(checks,
		protocol.VerifyCheck{
			Check:       protocol.CheckParentIndexValid,
			ParentIndex: &pi,
			Message: parentIndex.String() + " must be valid; PostgreSQL marks it so on the final attach " +
				"(FR-VER-3)",
		},
		protocol.VerifyCheck{
			Check:         protocol.CheckLeafIndexCount,
			ParentIndex:   &pi,
			ExpectedCount: &count,
			Message: fmt.Sprintf("%s must have exactly %d leaf index(es), one per partition (FR-VER-4)",
				parentIndex, count),
		},
	)
	for i := range children {
		child := children[i]
		checks = append(checks, protocol.VerifyCheck{
			Check:       protocol.CheckIndexAttached,
			Index:       &child,
			ParentIndex: &pi,
			Message:     child.String() + " must be attached to " + parentIndex.String() + " (FR-VER-2)",
		})
	}
	return protocol.Node{
		ID:          nodeFinalVerify,
		Kind:        protocol.KindIndexVerify,
		Params:      &protocol.VerifyParams{Checks: checks},
		DependsOn:   deps,
		RenderedSQL: renderFinalVerifyComment(parentIndex, count),
	}
}

// ---------------------------------------------------------------------------
// Operator-facing notes
// ---------------------------------------------------------------------------

// notes are the lines `plan` prints for what the artifact cannot state
// structurally: FR-PLAN-5's skip count, and what the ownership marker buys.
func notes(leaves, builds, drops int, hasDDL bool) []string {
	out := []string{
		fmt.Sprintf("%d leaf partition(s) discovered; %d index build(s) remain, %d already complete (FR-PLAN-5)",
			leaves, builds, leaves-builds),
		"the parent index is INVALID for the whole build; PostgreSQL marks it valid on the final attach",
	}
	if drops > 0 {
		out = append(out, fmt.Sprintf(
			"%d unusable index(es) will be dropped first, each authorized by the PartitionCTL ownership "+
				"marker on the object itself, re-read at dispatch (FR-PLAN-6, FR-AUTH-5, AC-5)", drops))
	}
	out = append(out,
		"every index this run creates is stamped with a PartitionCTL ownership marker "+
			"(COMMENT ON INDEX, ShareUpdateExclusiveLock, ~1ms), which is what lets a later run prove "+
			"the object is its own to clean up (AC-6)")
	if !hasDDL {
		out = append(out,
			"no DDL remains: this plan is a checked no-op that re-proves the end state and exits zero (AC-7)")
	}
	return out
}
