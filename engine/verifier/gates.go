package verifier

import (
	"context"
	"sort"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// The entry points in this file build their check set from the live catalog
// rather than from a plan, which is what the Liquibase gates need: a gate
// asserts an end state and has no plan file to read (TRD §7.2.8). They funnel
// every assertion through [Verifier.Check], so there is exactly one
// implementation of each predicate no matter which consumer asked (TRD §7.2.7).

// VerifyPartitionedIndex asserts the complete end state of
// CreatePartitionedIndex: the parent index is valid (FR-VER-3), every discovered
// leaf partition carries an index that is attached (FR-VER-2) and valid, ready
// and live (FR-VER-1), and the attached leaf index count equals the discovered
// leaf partition count (FR-VER-4).
//
// It backs `verify` for a create plan and <partitionctlIndexGate> (FR-LB-2). It
// issues no DDL and mutates nothing.
//
// The leaf index names are discovered from pg_inherits rather than re-derived
// with [protocol.ChildIndexName], so the assertion holds for an index built by
// any means, not only one PartitionCTL created.
//
// It returns an error only when the leaf partitions could not be discovered at
// all; every other outcome, including a catalog read failing mid-way, is a
// result in the report.
func (v *Verifier) VerifyPartitionedIndex(ctx context.Context, table, parentIndex protocol.ObjectName) (Report, error) {
	if v == nil || v.cat == nil {
		return Report{}, protocol.ErrFailure.Detailf("verifier has no catalog")
	}
	leaves, err := v.cat.LeafPartitions(ctx, table)
	if err != nil {
		return Report{}, err
	}
	attached, err := v.cat.AttachedIndexes(ctx, parentIndex)
	if err != nil {
		return Report{}, err
	}

	var r Report
	r.Add(v.Check(ctx, protocol.VerifyCheck{
		Check:       protocol.CheckParentIndexValid,
		ParentIndex: &parentIndex,
	}))
	want := len(leaves)
	r.Add(v.Check(ctx, protocol.VerifyCheck{
		Check:         protocol.CheckLeafIndexCount,
		ParentIndex:   &parentIndex,
		ExpectedCount: &want,
	}))

	// Index the attached children by the relation they sit on, so the per-leaf
	// assertions iterate the *partitions* rather than the children. Iterating the
	// children would make "attached" trivially true and would say nothing about a
	// partition that has no index at all, which is the failure mode a partial
	// create actually produces.
	byRelation := make(map[string]IndexState, len(attached))
	for _, ix := range attached {
		byRelation[ix.Relation.String()] = ix
	}

	sorted := append([]protocol.ObjectName(nil), leaves...)
	sortNames(sorted)
	for i := range sorted {
		leaf := sorted[i]
		child, ok := byRelation[leaf.String()]
		if !ok {
			relation := leaf
			r.Add(Result{
				Check:       protocol.CheckIndexAttached,
				Status:      StatusFail,
				ParentIndex: &parentIndex,
				Relation:    &relation,
				Expected:    "an index attached to " + parentIndex.String(),
				Actual:      "none",
				Reason: "partition " + leaf.String() + " carries no index attached to " +
					parentIndex.String() + " in pg_inherits",
			})
			continue
		}
		name := child.Name
		r.Add(v.Check(ctx, protocol.VerifyCheck{
			Check:       protocol.CheckIndexAttached,
			Index:       &name,
			ParentIndex: &parentIndex,
			Relation:    &leaf,
		}))
		r.Add(v.Check(ctx, protocol.VerifyCheck{
			Check:    protocol.CheckIndexValid,
			Index:    &name,
			Relation: &leaf,
		}))
	}
	return r, nil
}

// VerifyReindexedIndex asserts the end state of ReindexPartitionedIndex
// (FR-REIDX-6): everything [Verifier.VerifyPartitionedIndex] asserts, since the
// parent must stay valid and every leaf must stay attached across the swap, plus
// the absence of any _ccnew/_ccold leftover on the tree.
//
// Since M2 this answers the whole of <partitionctlReindexGate> except the
// watermark comparison itself. "A PartitionCTL reindex ran at or after `since`"
// used to be a StateStore query the verifier did not own; the ownership marker
// records `reindexed` and `reindex_run` on the leaf index, so it is now a
// catalog fact read from obj_description like everything else here. The gate
// keeps its `since` parameter and loses its StateStore dependency, which makes
// it structurally identical to the other two (TRD §7.2.7, FR-LB-4, AC-22).
func (v *Verifier) VerifyReindexedIndex(ctx context.Context, table, parentIndex protocol.ObjectName) (Report, error) {
	r, err := v.VerifyPartitionedIndex(ctx, table, parentIndex)
	if err != nil {
		return Report{}, err
	}
	leftovers := v.VerifyNoLeftovers(ctx, table)
	r.Results = append(r.Results, leftovers.Results...)
	return r, nil
}

// VerifyNoLeftovers asserts that no _ccnew/_ccold index remains on the relation
// or any partition beneath it (FR-REIDX-6, AC-17).
func (v *Verifier) VerifyNoLeftovers(ctx context.Context, relation protocol.ObjectName) Report {
	var r Report
	r.Add(v.Check(ctx, protocol.VerifyCheck{
		Check:    protocol.CheckNoLeftoverIndexes,
		Relation: &relation,
	}))
	return r
}

// VerifyIndexAbsent asserts that the partitioned index and every leaf index it
// owned are gone from pg_index (FR-DROP-7). It backs
// <partitionctlIndexAbsentGate> (FR-LB-3).
//
// The parent is asserted absent directly. The leaf indexes cannot be, because
// once the parent is dropped pg_inherits holds nothing to enumerate them from,
// so their names are regenerated with [protocol.ChildIndexName] — the same
// deterministic function the planner used to create them (FR-PLAN-11), which is
// exactly the property FR-PLAN-11 exists to provide.
//
// The limitation that follows is worth stating: a leaf index created by
// PostgreSQL's own recursion, rather than by PartitionCTL, carries a
// server-generated name this cannot reconstruct. The parent assertion still
// holds, and DROP INDEX on the parent cascades to every attached child
// regardless, so the residue this could miss is an *unattached* orphan of a
// build PartitionCTL did not perform.
func (v *Verifier) VerifyIndexAbsent(ctx context.Context, table, parentIndex protocol.ObjectName) (Report, error) {
	if v == nil || v.cat == nil {
		return Report{}, protocol.ErrFailure.Detailf("verifier has no catalog")
	}
	leaves, err := v.cat.LeafPartitions(ctx, table)
	if err != nil {
		return Report{}, err
	}

	var r Report
	r.Add(v.Check(ctx, protocol.VerifyCheck{
		Check: protocol.CheckIndexAbsent,
		Index: &parentIndex,
	}))

	sorted := append([]protocol.ObjectName(nil), leaves...)
	sortNames(sorted)
	for i := range sorted {
		leaf := sorted[i]
		// An index lives in the same schema as the table it is on, so the child
		// index is looked for in the leaf partition's schema.
		child := protocol.NewObjectName(leaf.Schema, protocol.ChildIndexName(parentIndex.Name, leaf.Name))
		r.Add(v.Check(ctx, protocol.VerifyCheck{
			Check:    protocol.CheckIndexAbsent,
			Index:    &child,
			Relation: &leaf,
		}))
	}
	return r, nil
}

// sortNames orders names by schema then name, so that a report over 400
// partitions is byte-identical between runs.
func sortNames(names []protocol.ObjectName) {
	sort.Slice(names, func(i, j int) bool {
		if names[i].Schema != names[j].Schema {
			return names[i].Schema < names[j].Schema
		}
		return names[i].Name < names[j].Name
	})
}

// VerifyIndexAbsentForPlan is [VerifyIndexAbsent] driven by the names a plan
// recorded, in addition to those derivable from the live leaf set
// (FR-PLAN-13, FR-VER-3).
//
// # Why the plan's names are not optional
//
// FR-PLAN-13 makes the recorded name authoritative "rather than deriving it
// again at execution time", and the reason shows up here. Re-derivation asks
// the catalog what the leaves are called *now* and generates names from that,
// so a partition renamed since the build produces a different name and the
// index the plan actually created is never looked at. The check then reports
// PASS with that index still present as an unattached orphan, holding disk and
// write overhead, and the parent assertion passes too because the parent really
// is gone. The report is all green and the object survives.
//
// The two sources are unioned rather than swapped. The plan covers what this
// tool built; re-derivation covers leaves the plan emitted no node against,
// including ones added since. A name that only one source knows is still
// checked, so the result is strictly stronger than either alone.
//
// [VerifyIndexAbsent] remains for the plan-less path, where a gate is handed a
// table and an index name and nothing else (FR-LB-3).
func (v *Verifier) VerifyIndexAbsentForPlan(ctx context.Context, plan *protocol.Plan) (Report, error) {
	if v == nil || v.cat == nil {
		return Report{}, protocol.ErrFailure.Detailf("verifier has no catalog")
	}
	if plan == nil || plan.Target.Index == nil {
		return Report{}, protocol.ErrFailure.Detailf(
			"an absence check needs a plan whose target names an index")
	}
	parentIndex := *plan.Target.Index

	// Start from the live derivation, then add every name the plan recorded.
	r, err := v.VerifyIndexAbsent(ctx, plan.Target.Table, parentIndex)
	if err != nil {
		return Report{}, err
	}
	seen := map[protocol.ObjectName]bool{parentIndex: true}
	for _, res := range r.Results {
		if res.Index != nil {
			seen[*res.Index] = true
		}
	}

	recorded := recordedChildIndexes(plan)
	sortNames(recorded)
	for i := range recorded {
		child := recorded[i]
		if seen[child] {
			continue
		}
		seen[child] = true
		r.Add(v.Check(ctx, protocol.VerifyCheck{
			Check: protocol.CheckIndexAbsent,
			Index: &child,
		}))
	}
	return r, nil
}

// recordedChildIndexes collects every child index name a plan names, from the
// two node kinds that carry one.
func recordedChildIndexes(plan *protocol.Plan) []protocol.ObjectName {
	var out []protocol.ObjectName
	for i := range plan.Nodes {
		switch p := plan.Nodes[i].Params.(type) {
		case *protocol.CreateConcurrentlyParams:
			out = append(out, p.Index)
		case *protocol.DropConcurrentlyParams:
			out = append(out, p.Index)
		}
	}
	return out
}
