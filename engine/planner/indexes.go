package planner

import (
	"context"

	"github.com/atulsinha87/partitionctl/engine/protocol"
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

	// Attached is an index already attached to the target partitioned index on
	// this leaf under a name this tool did not generate, or nil.
	//
	// PostgreSQL creates and attaches one by itself: adding a partition to a
	// table that already carries a partitioned index makes the server build the
	// child index immediately, under its OWN generated name
	// (orders_2026_01_created_at_idx), and attach it. The partition needs no
	// work at all. Keying only on this tool's generated name made that index
	// invisible, so the planner emitted a full CREATE INDEX CONCURRENTLY — hours
	// on a large partition, plus its write amplification — followed by an ATTACH
	// that fails terminally with SQLSTATE 55000, because PostgreSQL permits only
	// one attached child index per (partition, partitioned index) and the slot
	// is taken. Neither `resume` nor a re-plan could get past it, and each
	// attempt left another stray marked duplicate index behind.
	Attached *Index
}

// Exists reports whether an index already occupies the generated name.
func (c ChildIndexPlan) Exists() bool { return c.Existing != nil }

// AttachedTo reports whether the existing index is attached to the partitioned
// index with the given OID.
func (c ChildIndexPlan) AttachedTo(parentIndexOID uint32) bool {
	return c.Existing != nil && c.Existing.AttachedTo(parentIndexOID)
}

// Complete reports whether this leaf needs no work at all: a usable index
// attached to the parent index, under the generated name or under the name
// PostgreSQL chose when it propagated the index itself. FR-PLAN-5 turns this
// into "emit no node", which is what makes a plan naturally idempotent and a
// re-execute a no-op (AC-7).
func (c ChildIndexPlan) Complete(parentIndexOID uint32) bool {
	if c.Condition.Usable() && c.AttachedTo(parentIndexOID) {
		return true
	}
	return c.Attached != nil && c.Attached.Condition().Usable() &&
		c.Attached.AttachedTo(parentIndexOID)
}

// ForeignAttached returns an index attached to the parent under a name this
// tool did not generate, when that index is NOT usable.
//
// It is separated from [ChildIndexPlan.Complete] because the two need opposite
// treatment. A usable propagated index means the leaf is done. An unusable one
// means the slot is occupied by something broken that this tool cannot repair:
// it cannot be dropped individually (an attached child index is a dependency of
// its parent, and PostgreSQL offers no ALTER INDEX ... DETACH PARTITION), and
// anything this planner emits for the leaf would fail 55000 on the attach. That
// has to be said, not walked into.
func (c ChildIndexPlan) ForeignAttached(parentIndexOID uint32) (*Index, bool) {
	if c.Attached == nil || !c.Attached.AttachedTo(parentIndexOID) {
		return nil, false
	}
	if c.Attached.Condition().Usable() {
		return nil, false
	}
	return c.Attached, true
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
		// A child index already attached to the target parent under any other
		// name. The data needed for this was always in `indexes` -- Index
		// carries ParentIndexOID -- it was simply never consulted.
		if parent != nil && (c.Existing == nil || !c.Existing.AttachedTo(parent.OID)) {
			for j := range indexes {
				idx := &indexes[j]
				if idx.TableOID == leaf.OID && idx.AttachedTo(parent.OID) {
					c.Attached = idx
					break
				}
			}
		}
		children[i] = c
	}

	return ChildIndexInspection{ParentIndex: parent, Children: children}, nil
}

// CleanupDecision is what to do about an index that occupies a leaf's generated
// name but is not usable.
type CleanupDecision string

// The cleanup decisions. They are the rows of the destructive-action decision
// table ([protocol.DecideProvenanceDrop]) named from the planner's side.
const (
	// CleanupNone: nothing to remove.
	CleanupNone CleanupDecision = "none"

	// CleanupDropWithProvenance: emit index.drop_concurrently with
	// authorization mode provenance, ahead of the rebuild. The object carries
	// PartitionCTL's ownership marker, which is a catalog fact, so `execute`
	// may perform it (FR-PLAN-6, AC-5).
	CleanupDropWithProvenance CleanupDecision = "drop_with_provenance"

	// CleanupAdoptThenDrop: the object is unmarked but a run still holds a live
	// claim on it, so it is ours by a crash rather than by a marker. The
	// executor writes the marker onto it and only then drops it. This is the
	// one path reserved to `resume` (FR-CLI-9).
	CleanupAdoptThenDrop CleanupDecision = "adopt_then_drop"

	// CleanupHalt: refuse to plan. Accompanied by an error matching
	// [ErrForeignInvalidIndex] (FR-PLAN-7, AC-6).
	CleanupHalt CleanupDecision = "halt"
)

func (d CleanupDecision) String() string { return string(d) }

// Destructive reports whether the decision ends in a drop.
func (d CleanupDecision) Destructive() bool {
	return d == CleanupDropWithProvenance || d == CleanupAdoptThenDrop
}

// DecideCleanup decides what to do about whatever already occupies a leaf's
// generated child index name.
//
// The rule this enforces is NFR-REL-3: PartitionCTL never issues a destructive
// statement against an object it cannot prove it created. Ownership is read off
// the object itself, as a [protocol.Marker] in its comment, so a stale record
// can no longer authorize destroying a same-named object somebody else made
// (AC-6). The full table is [protocol.DecideProvenanceDrop], and the executor
// re-evaluates the identical function at dispatch (FR-AUTH-5).
//
// A nil ClaimLookup means "no claim", which is the safe direction: with
// execution state unavailable the only thing that can authorize a drop is the
// marker, which is exactly the property that makes this design survive a PITR
// restore (§7.2.5, R3).
//
// An index with indislive = false is treated the same way as an INVALID one,
// and for the same reason. It looks like a drop in flight, but it is equally
// the residue of one that died: a killed process, a reset connection or a
// statement_timeout leaves exactly that state with nothing running, and
// PostgreSQL's documented recovery is to reissue the DROP INDEX CONCURRENTLY.
// The advisory lock (FR-LOCK-1) already excludes a second PartitionCTL run
// against this target.
func DecideCleanup(ctx context.Context, cr CatalogReader, claims ClaimLookup, c ChildIndexPlan) (CleanupDecision, protocol.DropVerdict, error) {
	if !c.Exists() || !c.Condition.NeedsCleanup() {
		return CleanupNone, protocol.DropVerdict{}, nil
	}
	if cr == nil {
		return CleanupHalt, protocol.DropVerdict{}, ErrForeignInvalidIndex.Detailf(
			"index %s on %s is %s and no catalog reader is available, so its ownership marker "+
				"cannot be read (FR-PLAN-7)", c.ChildIndex.String(), c.Leaf.Name.String(), c.Condition)
	}

	// The comment came back with the rest of the index state in the discovery
	// pass, so the common path costs no extra query at all. The per-index read
	// is the fallback for a caller that inspected the tree some other way.
	marker, status := c.Existing.Marker()
	if c.Existing.Comment == "" {
		var err error
		marker, status, err = IndexMarker(ctx, cr, c.ChildIndex)
		if err != nil {
			return CleanupHalt, protocol.DropVerdict{}, err
		}
	}
	in := protocol.ProvenanceDropInput{Object: c.ChildIndex, Status: status, Marker: marker}
	// The claim is only consulted where it can change the answer, so a marked
	// object costs no state-store read at all.
	if status == protocol.MarkerAbsent && claims != nil {
		run, found, err := claims.ClaimsObject(ctx, c.ChildIndex)
		if err != nil {
			return CleanupHalt, protocol.DropVerdict{}, err
		}
		if found {
			in.ClaimRun = run
		}
	}

	v := protocol.DecideProvenanceDrop(in)
	switch v.Action {
	case protocol.DropAuthorized:
		return CleanupDropWithProvenance, v, nil
	case protocol.DropAdoptThenDrop:
		return CleanupAdoptThenDrop, v, nil
	}
	return CleanupHalt, v, ErrForeignInvalidIndex.Detailf(
		"index %s on %s is %s: %s", c.ChildIndex.String(), c.Leaf.Name.String(), c.Condition, v.Reason)
}
