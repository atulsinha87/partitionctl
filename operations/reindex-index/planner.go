package reindexindex

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/atulsinha/partitionctl/engine/planner"
	"github.com/atulsinha/partitionctl/engine/protocol"
)

// Node IDs are deterministic functions of the object they act on, so a re-plan
// over an unchanged catalog produces the same graph and a reviewer reading
// `status` output can tell which partition a node belongs to.
const (
	nodeAssert      protocol.NodeID = "assert.preconditions"
	nodeFinalVerify protocol.NodeID = "verify.final"
)

func nodeID(step string, obj protocol.ObjectName) protocol.NodeID {
	return protocol.NodeID(step + ":" + obj.String())
}

// Planner implements [planner.OperationPlanner] for ReindexPartitionedIndex and
// nothing else: no second catalog interface, no second specification, no plan
// sealing. The host owns discovery, the privilege check, the fingerprint and
// the digest.
//
// The zero value is usable and reindexes every leaf.
type Planner struct {
	// ReindexSince is the FR-PLAN-5 watermark. A leaf whose index carries a
	// PartitionCTL marker with `reindexed` at or after this instant emits no
	// reindex node, which is what makes a re-plan after an interruption cheap
	// and a 400-partition reindex resumable across days.
	//
	// The zero value reindexes every leaf, because an operator who asked for a
	// reindex asked for a reindex.
	//
	// It is set from [planner.Specification.ReindexSince], which the CLI
	// parses from the spec file's `reindex_since` as RFC 3339 and passes to
	// the constructor in the operation registry. The field is duplicated here
	// rather than read from the Request because a planner may carry per-run
	// configuration and the registry builds one planner per invocation.
	ReindexSince time.Time
}

// New returns the default planner: no watermark, every leaf rebuilt.
func New() Planner { return Planner{} }

// Operation names the operation this planner implements.
func (Planner) Operation() protocol.Operation { return protocol.OpReindexIndex }

// leafPlan is the per-leaf decision, resolved from the catalog before any node
// is built. Separating the decision from the node construction is what lets the
// Notes report the shape of the plan without re-deriving it.
type leafPlan struct {
	leaf      planner.Relation
	child     planner.Index
	leftovers []planner.Index

	// reindex reports whether this leaf still needs rebuilding.
	reindex bool
	// skipReason records why it does not, for the Notes.
	skipReason string
}

// Plan emits the graph for one reindex.
//
// It issues no DDL and opens no write transaction: the only database access is
// req.Catalog, which is read-only (FR-PLAN-8). It returns an error and no nodes
// at all rather than a graph it cannot justify.
func (pl Planner) Plan(ctx context.Context, req planner.Request) (planner.Result, error) {
	parent := req.Topology.Root.Name

	parentIndex, pidx, err := pl.resolveParentIndex(ctx, req, parent)
	if err != nil {
		return planner.Result{}, err
	}

	leaves := req.Topology.Leaves
	if len(leaves) == 0 {
		return planner.Result{}, protocol.ErrUnsupportedTopology.Detailf(
			"%s has no leaf partitions, so there are no leaf indexes under %s to rebuild",
			parent, parentIndex)
	}

	indexes, err := pl.readLeafIndexes(ctx, req, leaves)
	if err != nil {
		return planner.Result{}, err
	}

	plans, err := pl.classifyLeaves(leaves, indexes, pidx, parentIndex)
	if err != nil {
		return planner.Result{}, err
	}

	nodes := make([]protocol.Node, 0, 2+len(plans)*4)
	nodes = append(nodes, assertNode(req.Role, parent, parentIndex, req.Topology.Strategy, len(leaves)))

	var tails []protocol.NodeID
	for _, lp := range plans {
		chain := pl.leafChain(req, lp, parentIndex)
		if len(chain) == 0 {
			continue
		}
		nodes = append(nodes, chain...)
		tails = append(tails, chain[len(chain)-1].ID)
	}
	if len(tails) == 0 {
		// Every leaf was already fresh. The final verify still runs, so a
		// converged plan is a checked no-op rather than an empty file.
		tails = []protocol.NodeID{nodeAssert}
	}
	nodes = append(nodes, finalVerifyNode(parentIndex, leaves, tails))

	return planner.Result{Nodes: nodes, Notes: pl.notes(req, parentIndex, plans)}, nil
}

// ---------------------------------------------------------------------------
// Catalog resolution
// ---------------------------------------------------------------------------

// resolveParentIndex refuses everything that is not a partitioned index on the
// discovered root.
//
// The INVALID check is not decoration: the plan's own terminal verification
// asserts CheckParentIndexValid, so a plan built over an INVALID parent index
// would be a plan that cannot pass its own final gate no matter how well every
// leaf rebuild goes. An INVALID parent is an unfinished CreatePartitionedIndex,
// and finishing it is that operation's job, not this one's.
func (pl Planner) resolveParentIndex(
	ctx context.Context,
	req planner.Request,
	parent protocol.ObjectName,
) (protocol.ObjectName, planner.Index, error) {
	want := req.Spec.Index
	if want.Schema == "" {
		want.Schema = parent.Schema
	}

	idx, err := req.Catalog.LookupIndex(ctx, want)
	if err != nil {
		return protocol.ObjectName{}, planner.Index{}, fmt.Errorf("reindex-index: look up %s: %w", want, err)
	}
	if idx.Kind != planner.RelKindPartitionedIndex {
		return protocol.ObjectName{}, planner.Index{}, protocol.ErrUnsupportedTopology.Detailf(
			"%s has relkind %q; ReindexPartitionedIndex requires a partitioned index (relkind 'I'). "+
				"To rebuild an ordinary index, REINDEX INDEX CONCURRENTLY it directly",
			idx.Name, idx.Kind)
	}
	if idx.TableOID != req.Topology.Root.OID {
		return protocol.ObjectName{}, planner.Index{}, protocol.ErrFailure.Detailf(
			"%s is an index on %s (OID %d), not on %s (OID %d)",
			idx.Name, idx.Table, idx.TableOID, parent, req.Topology.Root.OID)
	}
	if !idx.IsValid {
		return protocol.ObjectName{}, planner.Index{}, protocol.ErrFailure.Detailf(
			"%s is not valid, so it is an unfinished build rather than an index to rebuild. "+
				"This plan's final verification asserts the parent index is valid, so no leaf rebuild "+
				"could make it pass. Finish or abandon the create first, then re-plan", idx.Name)
	}
	return idx.Name, idx, nil
}

// readLeafIndexes fetches every index on every leaf in one pass.
//
// One pass, not one per leaf: planning has to stay O(1) queries in the number
// of partitions (NFR-PERF-1), and planner.Index.Comment rides along with this
// result, so the ownership marker each leftover authorization needs costs no
// extra round trip. CatalogReader.IndexComment exists for single lookups and is
// deliberately not used here.
func (pl Planner) readLeafIndexes(
	ctx context.Context,
	req planner.Request,
	leaves []planner.Relation,
) ([]planner.Index, error) {
	oids := make([]uint32, len(leaves))
	for i, l := range leaves {
		oids[i] = l.OID
	}
	indexes, err := req.Catalog.IndexesOnRelations(ctx, oids)
	if err != nil {
		return nil, fmt.Errorf("reindex-index: read indexes on the partitions of %s: %w",
			req.Topology.Root.Name, err)
	}
	return indexes, nil
}

// ---------------------------------------------------------------------------
// Per-leaf classification (FR-REIDX-3, FR-REIDX-4, FR-REIDX-5, FR-PLAN-5)
// ---------------------------------------------------------------------------

// classifyLeaves resolves, per leaf and from the catalog alone, which leaf index
// is ours, which leftovers sit beside it, and whether the leaf still needs
// rebuilding. Every halt in the operation is raised here, before a single node
// is constructed, so a refusal is never a partially built graph.
func (pl Planner) classifyLeaves(
	leaves []planner.Relation,
	indexes []planner.Index,
	pidx planner.Index,
	parentIndex protocol.ObjectName,
) ([]leafPlan, error) {
	byTable := make(map[uint32][]planner.Index, len(leaves))
	byName := make(map[protocol.ObjectName]planner.Index, len(indexes))
	for _, idx := range indexes {
		byTable[idx.TableOID] = append(byTable[idx.TableOID], idx)
		byName[idx.Name] = idx
	}

	out := make([]leafPlan, 0, len(leaves))
	for _, leaf := range leaves {
		on := byTable[leaf.OID]
		sort.Slice(on, func(i, j int) bool { return on[i].Name.Name < on[j].Name.Name })

		child, err := attachedChild(leaf, on, pidx, parentIndex)
		if err != nil {
			return nil, err
		}

		lp := leafPlan{leaf: leaf, child: child, reindex: true}

		for _, idx := range on {
			kind, _ := protocol.ClassifyLeftover(idx.Name.Name)
			if kind == protocol.LeftoverNone {
				continue
			}
			if err := authorizeLeftover(leaf, idx, byName); err != nil {
				return nil, err
			}
			lp.leftovers = append(lp.leftovers, idx)

			// Only a leftover of *this* leaf's index says anything about
			// whether this leaf still needs rebuilding. A _ccold belonging to
			// some unrelated index on the same partition is dropped, because
			// the terminal CheckNoLeftoverIndexes demands the leaf be clean,
			// but it is not evidence that our rebuild already succeeded.
			base, ok := protocol.LeftoverBase(idx.Name)
			if !ok || base != child.Name {
				continue
			}
			if kind == protocol.LeftoverOld {
				// The rebuild succeeded and only the old copy survived: the
				// live index under the base name is already the new one, so
				// rebuilding it again would be hours of wasted work
				// (FR-REIDX-4).
				lp.reindex = false
				lp.skipReason = "already rebuilt; " + idx.Name.String() +
					" is the surviving old copy (FR-REIDX-4)"
			}
			// A _ccnew means the rebuild failed and the original is intact, so
			// the drop is emitted and the leaf is still reindexed
			// (FR-REIDX-3). That is the default, so there is nothing to set.
		}

		if lp.reindex && pl.freshEnough(child) {
			lp.reindex = false
			m, _ := child.Marker()
			lp.skipReason = "reindexed at " + m.Reindexed + ", at or after the requested watermark " +
				protocol.MarkerTime(pl.ReindexSince) + " (FR-PLAN-5)"
		}

		out = append(out, lp)
	}
	return out, nil
}

// attachedChild finds the one leaf index attached to the parent index.
//
// A leaf with no attached child index means the partitioned index is not
// complete over the tree. Nothing this planner emits repairs that: REINDEX
// rebuilds an index that exists, and creating the missing one is
// CreatePartitionedIndex's job. Halting here rather than emitting a partial plan
// is what keeps the terminal CheckLeafIndexCount honest.
func attachedChild(
	leaf planner.Relation,
	on []planner.Index,
	pidx planner.Index,
	parentIndex protocol.ObjectName,
) (planner.Index, error) {
	var found []planner.Index
	for _, idx := range on {
		if idx.AttachedTo(pidx.OID) {
			found = append(found, idx)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return planner.Index{}, protocol.ErrFailure.Detailf(
			"partition %s has no index attached to %s, so the partitioned index is incomplete over the "+
				"tree. REINDEX rebuilds an index that exists; run CreatePartitionedIndex to finish the "+
				"build first, then re-plan the reindex", leaf.Name, parentIndex)
	default:
		return planner.Index{}, protocol.ErrFailure.Detailf(
			"partition %s has %d indexes attached to %s; PostgreSQL allows exactly one, so the catalog "+
				"is in a state this planner will not act on", leaf.Name, len(found), parentIndex)
	}
}

// authorizeLeftover applies the A.5.2 decision table to one _ccnew/_ccold index
// and halts the whole plan if it is unsatisfied (FR-REIDX-5, AC-19).
//
// The marker is read off the *base* index, never off the leftover itself:
// whether PostgreSQL's index_concurrently_swap copies the description onto
// _ccnew/_ccold in every failure path is not measured, and a rebuild that fails
// before the swap certainly leaves an unmarked _ccnew behind.
//
// Halting rather than skipping is the deliberate difference from
// DropPartitionedIndex's treatment of a foreign orphan. There, a foreign index
// must not make the operator's own drop impossible. Here, the terminal
// CheckNoLeftoverIndexes asserts the leaf is clean, so a leftover left in place
// would fail the run at the end instead of at the start — and the leftover
// might be an operator's own hand-run REINDEX in progress.
func authorizeLeftover(
	leaf planner.Relation,
	leftover planner.Index,
	byName map[protocol.ObjectName]planner.Index,
) error {
	in := protocol.LeftoverDropInput{Object: leftover.Name}
	if base, ok := protocol.LeftoverBase(leftover.Name); ok {
		baseIdx, exists := byName[base]
		in.BaseExists = exists
		if exists {
			in.BaseMarker, in.BaseStatus = baseIdx.Marker()
		}
	}
	v := protocol.DecideLeftoverDrop(in)
	if !v.Satisfied() {
		return protocol.ErrAuthorizationUnsatisfied.Detailf(
			"%s on partition %s is a REINDEX CONCURRENTLY leftover this run may not drop: %s. "+
				"Halting with no plan: resolve it by hand, then re-plan (FR-REIDX-5, AC-19)",
			leftover.Name, leaf.Name, v.Reason)
	}
	return nil
}

// freshEnough reports whether the leaf index's own marker already proves it was
// rebuilt at or after the watermark (FR-PLAN-5).
//
// The question is answered from the catalog, not from run history: the executor
// rewrites the leaf's comment with `reindexed` after each successful rebuild, so
// "was this reindexed since T?" is one obj_description away. That is what makes
// the M4 <partitionctlReindexGate> a pure catalog assertion with no StateStore
// dependency (FR-LB-4, AC-22).
//
// Every ambiguity resolves to "rebuild it": no watermark, no marker, a foreign
// or unreadable marker, an empty or unparseable timestamp. Rebuilding a fresh
// index wastes time; skipping a stale one silently fails to do the job.
func (pl Planner) freshEnough(child planner.Index) bool {
	if pl.ReindexSince.IsZero() {
		return false
	}
	m, status := child.Marker()
	if status != protocol.MarkerOurs || m.Reindexed == "" {
		return false
	}
	at, err := time.Parse(time.RFC3339, m.Reindexed)
	if err != nil {
		return false
	}
	return !at.Before(pl.ReindexSince)
}

// ---------------------------------------------------------------------------
// Node construction
// ---------------------------------------------------------------------------

// leafChain returns the nodes for one leaf, in order, or nil when the leaf is
// already complete and carries no leftovers.
//
// The shapes are:
//
//   - fresh, no leftovers: no nodes at all (FR-PLAN-5);
//   - stale: reindex → verify → wait;
//   - _ccnew present: drop → reindex → verify → wait (FR-REIDX-3);
//   - _ccold present: drop only, the leaf counts as done (FR-REIDX-4);
//   - a leftover that fails authorization: no plan at all (FR-REIDX-5, AC-19),
//     raised earlier in classifyLeaves.
func (pl Planner) leafChain(req planner.Request, lp leafPlan, parentIndex protocol.ObjectName) []protocol.Node {
	chain := make([]protocol.Node, 0, 4)
	prev := nodeAssert
	add := func(n protocol.Node) {
		n.DependsOn = []protocol.NodeID{prev}
		chain = append(chain, n)
		prev = n.ID
	}

	for _, lo := range lp.leftovers {
		add(dropLeftoverNode(req, lp.leaf, lo))
	}
	if !lp.reindex {
		return chain
	}
	add(reindexNode(req, lp, parentIndex))
	add(leafVerifyNode(lp.leaf.Name, lp.child.Name, parentIndex))
	if req.Spec.PaceSeconds > 0 {
		add(waitNode(req, lp.leaf.Name))
	}
	return chain
}

// assertNode carries every precondition in a single catalog.assert evaluated
// before anything else runs. Each assertion kind already exists and is already
// evaluated by adapters/cli/assert.go; this operation adds none.
//
// The assertions are ordered cheapest and most structural first, and each
// carries the exit code its failure class maps to: unsupported topology is 15,
// insufficient privilege is 16 (TRD §7.2.12).
func assertNode(
	role string,
	parent, parentIndex protocol.ObjectName,
	strategy protocol.PartitionStrategy,
	leaves int,
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
			Message: "the partition tree rooted at " + parent.String() +
				" must be exactly one level deep (FR-PLAN-2)",
		},
		{
			Assertion:   protocol.AssertNoDefaultPartition,
			Relation:    &parentRef,
			FailureCode: protocol.ExitUnsupportedTopology,
			Message:     parent.String() + " must have no DEFAULT partition (FR-PLAN-3)",
		},
		{
			Assertion:   protocol.AssertRoleMembership,
			Relation:    &parentRef,
			Role:        role,
			FailureCode: protocol.ExitInsufficientPrivilege,
			Message:     "role " + role + " must be a member of the owning role of " + parent.String(),
		},
		{
			Assertion:   protocol.AssertIndexExists,
			Relation:    &parentRef,
			Index:       &indexRef,
			FailureCode: protocol.ExitUnsupportedTopology,
			Message:     parentIndex.String() + " must exist; a reindex rebuilds an index that is already there",
		},
		{
			Assertion:   protocol.AssertIndexIsPartitioned,
			Relation:    &parentRef,
			Index:       &indexRef,
			FailureCode: protocol.ExitUnsupportedTopology,
			Message: parentIndex.String() + " must be a partitioned index (relkind 'I') on " +
				parent.String(),
		},
		{
			Assertion:   protocol.AssertLeavesAttached,
			Relation:    &parentRef,
			Index:       &indexRef,
			FailureCode: protocol.ExitUnsupportedTopology,
			Message: fmt.Sprintf(
				"all %d leaf index(es) must be attached to %s before any of them is rebuilt (FR-REIDX-6)",
				leaves, parentIndex),
		},
	}

	return protocol.Node{
		ID:          nodeAssert,
		Kind:        protocol.KindCatalogAssert,
		Params:      &protocol.CatalogAssertParams{Assertions: assertions},
		RenderedSQL: renderAssertComment(len(assertions)),
	}
}

// dropLeftoverNode removes one _ccnew/_ccold index. Online: DROP INDEX
// CONCURRENTLY takes ShareUpdateExclusiveLock, and both leftover classes are
// unattached ordinary indexes, so reindex recovery never needs an
// AccessExclusiveLock.
//
// The authorization it carries is a proposal only: the executor re-evaluates
// [protocol.DecideLeftoverDrop] against live state immediately before dispatch
// and halts if it is unsatisfied, whatever the plan asserts (FR-AUTH-5, INV-2).
func dropLeftoverNode(req planner.Request, leaf planner.Relation, leftover planner.Index) protocol.Node {
	relation := leaf.Name
	kind, _ := protocol.ClassifyLeftover(leftover.Name.Name)

	reason := protocol.DropCCNew
	note := "a _ccnew left by a REINDEX CONCURRENTLY that failed before the swap; the original index " +
		"is intact and the leaf is still rebuilt (FR-REIDX-3)"
	if kind == protocol.LeftoverOld {
		reason = protocol.DropCCOld
		note = "a _ccold left by a REINDEX CONCURRENTLY that swapped but could not drop the old copy; " +
			"the leaf is already rebuilt and is not rebuilt again (FR-REIDX-4)"
	}

	params := &protocol.DropConcurrentlyParams{
		Index:    leftover.Name,
		Relation: &relation,
		Reason:   reason,
	}
	return planner.Preview(protocol.Node{
		ID:     nodeID("drop", leftover.Name),
		Kind:   protocol.KindIndexDropConcurrently,
		Params: params,
		Authorization: &protocol.Authorization{
			Mode:     protocol.AuthLeftover,
			Object:   leftover.Name,
			Relation: &relation,
			Note:     note,
		},
		EstimatedSeconds: req.Estimator.CatalogNodeSeconds(),
	}, protocol.OpReindexIndex)
}

// reindexNode rebuilds one leaf index. This is the node that runs for hours: it
// builds a full second copy of the index, so peak storage is reported
// (FR-REIDX-7). It runs outside a transaction block and takes no statement
// timeout, because a single leaf may be 10 TB.
func reindexNode(req planner.Request, lp leafPlan, parentIndex protocol.ObjectName) protocol.Node {
	relation := lp.leaf.Name
	pi := parentIndex
	params := &protocol.ReindexConcurrentlyParams{
		Index:              lp.child.Name,
		Relation:           &relation,
		ParentIndex:        &pi,
		EstimatedPeakBytes: req.Estimator.ReindexPeakBytes(lp.child.RelPages),
	}
	return planner.Preview(protocol.Node{
		ID:               nodeID("reindex", lp.leaf.Name),
		Kind:             protocol.KindIndexReindexConcurrently,
		Params:           params,
		EstimatedSeconds: req.Estimator.ReindexSeconds(lp.leaf.RelPages),
	}, protocol.OpReindexIndex)
}

// leafVerifyNode checks the rebuilt leaf index immediately, before the run moves
// on: valid, ready and live, and still attached to the parent.
//
// The attachment check is not redundant with the assert. REINDEX CONCURRENTLY
// swaps the index's storage under its name, and that the pg_inherits attachment
// survives the swap is precisely the property the v0.0 spike had to establish
// (FR-REIDX-6). Checking it per leaf is what turns that measurement into a
// standing guarantee rather than a one-off observation.
func leafVerifyNode(leaf, child, parentIndex protocol.ObjectName) protocol.Node {
	idx := child
	pi := parentIndex
	return protocol.Node{
		ID:   nodeID("verify", leaf),
		Kind: protocol.KindIndexVerify,
		Params: &protocol.VerifyParams{Checks: []protocol.VerifyCheck{
			{
				Check:   protocol.CheckIndexValid,
				Index:   &idx,
				Message: child.String() + " must be valid, ready and live after the rebuild (FR-VER-1)",
			},
			{
				Check:       protocol.CheckIndexAttached,
				Index:       &idx,
				ParentIndex: &pi,
				Message: child.String() + " must still be attached to " + parentIndex.String() +
					" after the rebuild swapped its storage (FR-REIDX-6)",
			},
		}},
		RenderedSQL: renderLeafVerifyComment(child, parentIndex),
	}
}

// waitNode is the planner-emitted pause between leaves (FR-ORD-3). It is a node
// so that every pause is visible in the plan the operator reviews; the executor
// introduces no delays of its own.
//
// Pacing is one of the four things per-leaf reindexing buys that the parent-level
// statement cannot offer, so it is not an optional nicety here.
func waitNode(req planner.Request, leaf protocol.ObjectName) protocol.Node {
	seconds := req.Estimator.WaitSeconds(req.Spec.PaceSeconds)
	reason := req.Spec.PaceReason
	if reason == "" {
		reason = "pacing after " + leaf.String() + " was rebuilt (FR-ORD-3)"
	}
	return protocol.Node{
		ID:               nodeID("wait", leaf),
		Kind:             protocol.KindWait,
		Params:           &protocol.WaitParams{Seconds: seconds, Reason: reason},
		RenderedSQL:      renderWaitComment(seconds),
		EstimatedSeconds: seconds,
	}
}

// finalVerifyNode is the barrier with N incoming edges. It proves the whole
// tree, including the leaves this plan emitted no work for: the parent index is
// valid, the leaf index count still equals the partition count, and no leaf
// carries a _ccnew or _ccold.
//
// The leftover check is what makes the operation converge rather than merely
// progress. A run that rebuilt every leaf but left one transient index behind
// has not finished, and the next plan would have to rediscover that.
func finalVerifyNode(parentIndex protocol.ObjectName, leaves []planner.Relation, deps []protocol.NodeID) protocol.Node {
	pi := parentIndex
	count := len(leaves)
	checks := make([]protocol.VerifyCheck, 0, 2+len(leaves))
	checks = append(checks,
		protocol.VerifyCheck{
			Check:       protocol.CheckParentIndexValid,
			ParentIndex: &pi,
			Message:     parentIndex.String() + " must still be valid after every leaf was rebuilt (FR-VER-3)",
		},
		protocol.VerifyCheck{
			Check:         protocol.CheckLeafIndexCount,
			ParentIndex:   &pi,
			ExpectedCount: &count,
			Message: fmt.Sprintf("%s must still have exactly %d leaf index(es), one per partition (FR-VER-4)",
				parentIndex, count),
		},
	)
	for i := range leaves {
		rel := leaves[i].Name
		checks = append(checks, protocol.VerifyCheck{
			Check:    protocol.CheckNoLeftoverIndexes,
			Relation: &rel,
			Message: "no _ccnew or _ccold index may remain on " + rel.String() +
				" (TRD §7.2.11, FR-REIDX-3, FR-REIDX-4)",
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

// notes are the lines `plan` prints for anything the artifact cannot state
// structurally.
//
// The first note is mandatory and is the reason this function exists: the graph
// alone makes per-leaf reindexing look like an over-complication of a statement
// PostgreSQL already loops for us. Without the explanation on the page, the next
// reader "simplifies" it back to the parent form and silently deletes resume,
// the ETA, pacing and FR-PLAN-5.
func (pl Planner) notes(req planner.Request, parentIndex protocol.ObjectName, plans []leafPlan) []string {
	var rebuild, skipped, leftovers, ccnew, ccold int
	var peak int64
	for _, lp := range plans {
		if lp.reindex {
			rebuild++
			if b := req.Estimator.ReindexPeakBytes(lp.child.RelPages); b > peak {
				peak = b
			}
		} else {
			skipped++
		}
		for _, lo := range lp.leftovers {
			leftovers++
			if k, _ := protocol.ClassifyLeftover(lo.Name.Name); k == protocol.LeftoverOld {
				ccold++
			} else {
				ccnew++
			}
		}
	}

	notes := []string{
		fmt.Sprintf("%s is rebuilt one leaf at a time, not with REINDEX INDEX CONCURRENTLY on the parent. "+
			"The parent form works — the v0.0 spike measured it succeeding on 14.23 and 17.10, with "+
			"PostgreSQL looping the partitions itself — so FR-REIDX-2's claim that PostgreSQL rejects it "+
			"is wrong. We decline to use it anyway: it has no \"already fresh\" concept, so a re-run after "+
			"an interruption rebuilds every partition from the start. Per-leaf is what buys resume, the "+
			"ETA, pacing and FR-PLAN-5, and those four things are the whole of what this tool adds here.",
			parentIndex),
		fmt.Sprintf("%d of %d leaf index(es) will be rebuilt; %d skipped as already complete.",
			rebuild, len(plans), skipped),
	}

	if leftovers > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d REINDEX CONCURRENTLY leftover(s) will be dropped online: %d _ccnew (rebuild failed, the "+
				"original is intact, the leaf is still rebuilt — FR-REIDX-3) and %d _ccold (rebuild "+
				"succeeded, the old copy survived, the leaf counts as done — FR-REIDX-4). Each was "+
				"authorized from the ownership marker on its base index, never from its name alone "+
				"(FR-REIDX-5, AC-19).", leftovers, ccnew, ccold))
	}
	if peak > 0 {
		notes = append(notes, fmt.Sprintf(
			"Peak additional storage is about %d bytes, the size of the largest single leaf index: "+
				"REINDEX CONCURRENTLY holds the old and new copies at once, one leaf at a time "+
				"(FR-REIDX-7).", peak))
	}
	if !pl.ReindexSince.IsZero() {
		notes = append(notes, fmt.Sprintf(
			"A leaf whose ownership marker records a rebuild at or after %s was skipped (FR-PLAN-5). "+
				"The marker is written by the executor after each successful rebuild, so this is a "+
				"catalog question and needs no run history.", protocol.MarkerTime(pl.ReindexSince)))
	}
	if rebuild > 0 && req.Spec.PaceSeconds == 0 {
		notes = append(notes, "No pacing was requested, so the rebuilds run back to back. "+
			"REINDEX CONCURRENTLY is I/O-heavy; consider --pace-seconds on a busy cluster (FR-ORD-3).")
	}
	return notes
}
