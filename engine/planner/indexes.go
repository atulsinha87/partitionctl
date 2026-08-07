package planner

import (
	"context"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// IndexCondition classifies an existing index's usability from the three
// pg_index flags the planner reads (FR-PLAN-4).
//
// The four non-absent conditions are ordered by severity, and the classifier
// reports the most severe one that applies, because that is the one that
// decides what the planner must do about it.
type IndexCondition string

// The index conditions.
const (
	// IndexAbsent: no index of that name exists on the relation. The planner
	// emits a build.
	IndexAbsent IndexCondition = "absent"

	// IndexValid: indisvalid AND indisready AND indislive. The index is usable
	// and, if attached, the work is complete: FR-PLAN-5 says emit no node.
	IndexValid IndexCondition = "valid"

	// IndexInvalid: indisvalid is false. The signature of an interrupted
	// CREATE INDEX CONCURRENTLY. It must be dropped before a rebuild, and
	// dropping it requires provenance (FR-PLAN-6, FR-PLAN-7, AC-5, AC-6).
	IndexInvalid IndexCondition = "invalid"

	// IndexNotReady: indisready is false while indisvalid is true. The index
	// is not yet accepting inserts, so a build is still in flight.
	IndexNotReady IndexCondition = "not_ready"

	// IndexNotLive: indislive is false. A DROP INDEX CONCURRENTLY is in
	// flight against it, by this tool or by someone else. Nothing should be
	// planned against it in either case.
	IndexNotLive IndexCondition = "not_live"
)

// Usable reports whether the index can serve queries and satisfy verification
// (FR-VER-1).
func (c IndexCondition) Usable() bool { return c == IndexValid }

// NeedsCleanup reports whether an index in this condition must be removed
// before the leaf can be rebuilt.
func (c IndexCondition) NeedsCleanup() bool {
	return c == IndexInvalid || c == IndexNotReady || c == IndexNotLive
}

func (c IndexCondition) String() string { return string(c) }

// AllIndexConditions returns every condition, most usable first. The returned
// slice is a copy.
func AllIndexConditions() []IndexCondition {
	return []IndexCondition{IndexAbsent, IndexValid, IndexInvalid, IndexNotReady, IndexNotLive}
}

// ChildIndexPlan is one leaf's state with respect to a target parent index: the
// name the child index will have, and whatever is already there under that name
// (FR-PLAN-4).
type ChildIndexPlan struct {
	// Leaf is the partition.
	Leaf Relation
	// ChildIndex is the name the planner generated for this leaf's index. It
	// is a deterministic function of the parent index name and the partition
	// name, and it is recorded in the plan rather than re-derived at execution
	// time (FR-PLAN-11, FR-PLAN-13).
	ChildIndex protocol.ObjectName
	// Existing is the index already present under that name, or nil.
	Existing *Index
	// Condition classifies Existing. It is [IndexAbsent] when Existing is nil.
	Condition IndexCondition
}

// Exists reports whether an index already occupies the generated name.
func (c ChildIndexPlan) Exists() bool { return c.Existing != nil }

// AttachedTo reports whether the existing index is attached to the partitioned
// index with the given OID.
func (c ChildIndexPlan) AttachedTo(parentIndexOID uint32) bool {
	return c.Existing != nil && c.Existing.AttachedTo(parentIndexOID)
}

// Complete reports whether this leaf needs no work at all: a valid index under
// the generated name, already attached to the parent index. FR-PLAN-5 turns
// this into "emit no node", which is what makes a plan naturally idempotent and
// a re-execute a no-op (AC-7).
func (c ChildIndexPlan) Complete(parentIndexOID uint32) bool {
	return c.Condition.Usable() && c.AttachedTo(parentIndexOID)
}

// ChildIndexInspection is the whole-tree answer to "what already exists?".
type ChildIndexInspection struct {
	// ParentIndex is the partitioned index, or nil when it does not exist yet.
	ParentIndex *Index
	// Children is one entry per leaf, in [Topology] leaf order.
	Children []ChildIndexPlan
}

// ParentIndexOID is the partitioned index's OID, or 0 when it does not exist.
func (in ChildIndexInspection) ParentIndexOID() uint32 {
	if in.ParentIndex == nil {
		return 0
	}
	return in.ParentIndex.OID
}

// Remaining returns the leaves that still need work (FR-PLAN-5).
func (in ChildIndexInspection) Remaining() []ChildIndexPlan {
	parent := in.ParentIndexOID()
	var out []ChildIndexPlan
	for _, c := range in.Children {
		if !c.Complete(parent) {
			out = append(out, c)
		}
	}
	return out
}

// CompleteCount is how many leaves are already done.
func (in ChildIndexInspection) CompleteCount() int {
	parent := in.ParentIndexOID()
	n := 0
	for _, c := range in.Children {
		if c.Complete(parent) {
			n++
		}
	}
	return n
}

// InspectChildren generates the child index name for every leaf and reports
// what is already there (FR-PLAN-4, FR-PLAN-11, FR-PLAN-13).
//
// Names are generated once, here, for the whole tree, and the generation proves
// the set is collision-free before any of it reaches the plan
// ([protocol.ChildIndexNamesQualified]). A collision is an error, never a
// silent overwrite.
//
// The child index lives in its leaf's schema, because CREATE INDEX
// CONCURRENTLY <name> ON <leaf> resolves the new name in the table's schema.
// Collision-freedom is therefore proved per schema, by the same generator the
// operation planners use, so `resume` and `plan` cannot disagree about whether
// a tree is legal (AC-4).
func InspectChildren(ctx context.Context, cr CatalogReader, topo Topology, parentIndex protocol.ObjectName) (ChildIndexInspection, error) {
	if err := parentIndex.Validate(); err != nil {
		return ChildIndexInspection{}, ErrInvalidSpecification.Detailf("parent index: %v", err)
	}

	qualified, err := protocol.ChildIndexNamesQualified(parentIndex.Name, topo.LeafObjectNames())
	if err != nil {
		return ChildIndexInspection{}, err
	}

	indexes, err := cr.IndexesOnRelations(ctx, topo.RelationOIDs())
	if err != nil {
		return ChildIndexInspection{}, err
	}

	// Key by (table OID, index name): a name is unique per schema, and each
	// leaf's index lives in that leaf's schema.
	type key struct {
		table uint32
		name  string
	}
	byName := make(map[key]*Index, len(indexes))
	var parent *Index
	for i := range indexes {
		idx := &indexes[i]
		byName[key{idx.TableOID, idx.Name.Name}] = idx
		if idx.TableOID == topo.Root.OID && idx.Name.Name == parentIndex.Name {
			if parentIndex.Schema == "" || idx.Name.Schema == parentIndex.Schema {
				parent = idx
			}
		}
	}

	children := make([]ChildIndexPlan, len(topo.Leaves))
	for i, leaf := range topo.Leaves {
		c := ChildIndexPlan{
			Leaf:       leaf,
			ChildIndex: qualified[i],
			Condition:  IndexAbsent,
		}
		if existing, ok := byName[key{leaf.OID, qualified[i].Name}]; ok {
			c.Existing = existing
			c.Condition = existing.Condition()
		}
		children[i] = c
	}

	return ChildIndexInspection{ParentIndex: parent, Children: children}, nil
}

// CleanupDecision is what to do about an index that occupies a leaf's generated
// name but is not usable.
type CleanupDecision string

// The cleanup decisions.
const (
	// CleanupNone: nothing to remove.
	CleanupNone CleanupDecision = "none"

	// CleanupDropWithProvenance: emit index.drop_concurrently with
	// authorization mode provenance, ahead of the rebuild (FR-PLAN-6, AC-5).
	CleanupDropWithProvenance CleanupDecision = "drop_with_provenance"

	// CleanupHalt: refuse to plan. Accompanied by an error matching
	// [ErrForeignInvalidIndex] (FR-PLAN-7, AC-6).
	CleanupHalt CleanupDecision = "halt"
)

func (d CleanupDecision) String() string { return string(d) }

// DecideCleanup decides what to do about whatever already occupies a leaf's
// generated child index name.
//
// The rule this enforces is NFR-REL-3: PartitionCTL never issues a destructive
// statement against an object it cannot prove it created. An INVALID index with
// a committed provenance record is its own half-built work and is dropped
// (FR-PLAN-6). An INVALID index without one belongs to somebody else, and the
// planner halts and emits no plan rather than cleaning it up (FR-PLAN-7, AC-6).
// A nil ProvenanceLookup therefore means "no provenance", which halts: the safe
// direction when execution state has been lost (§7.2.5, R3).
//
// An index with indislive = false is treated the same way, and for the same
// reason. It looks like a drop in flight, but it is equally the residue of one
// that died: a killed process, a reset connection or a statement_timeout leaves
// exactly that state with nothing running, and PostgreSQL's documented recovery
// is to reissue the DROP INDEX CONCURRENTLY. Refusing unconditionally made this
// state recoverable by `plan` plus `execute`, whose leafChain checks provenance
// and emits a drop, but not by `resume`, which is the command the tool itself
// names after an interruption. The advisory lock (FR-LOCK-1) already excludes a
// second PartitionCTL run against this target, so provenance is the right
// question here too.
func DecideCleanup(ctx context.Context, prov ProvenanceLookup, c ChildIndexPlan) (CleanupDecision, error) {
	if !c.Exists() || !c.Condition.NeedsCleanup() {
		return CleanupNone, nil
	}

	if prov == nil {
		return CleanupHalt, ErrForeignInvalidIndex.Detailf(
			"index %s on %s is %s and no provenance source is available, so PartitionCTL cannot "+
				"prove it created it (FR-PLAN-7). Review it and drop it by hand if it is yours",
			c.ChildIndex.String(), c.Leaf.Name.String(), c.Condition)
	}

	owned, err := prov.HasProvenance(ctx, c.ChildIndex)
	if err != nil {
		return CleanupHalt, err
	}
	if !owned {
		return CleanupHalt, ErrForeignInvalidIndex.Detailf(
			"index %s on %s is %s and has no PartitionCTL provenance record, so it is not "+
				"PartitionCTL's to drop (FR-PLAN-7, AC-6). Review it and drop it by hand if it is yours",
			c.ChildIndex.String(), c.Leaf.Name.String(), c.Condition)
	}
	return CleanupDropWithProvenance, nil
}
