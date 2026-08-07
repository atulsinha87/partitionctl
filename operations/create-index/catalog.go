package createindex

import (
	"context"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// PostgreSQL pg_class.relkind values this planner reasons about.
const (
	// RelKindPartitionedTable is pg_class.relkind 'p'.
	RelKindPartitionedTable = "p"
	// RelKindTable is pg_class.relkind 'r', an ordinary leaf partition.
	RelKindTable = "r"
)

// CatalogReader is the planner's read-only view of the target catalog.
//
// Every method is a read. Implementations run inside a read-only transaction
// and issue no DDL (FR-PLAN-8), and they never parse DDL text (FR-PLAN-1). The
// interface is batched rather than per-relation because planning must complete
// in under five seconds at a thousand leaf partitions (NFR-PERF-1): a planner
// that asked one question per partition would spend its whole budget on round
// trips.
//
// The interface is expressed entirely in [protocol] types plus [IndexState], so
// that a fake with no database behind it is a complete implementation. That is
// what lets the whole planner be unit-tested without PostgreSQL.
type CatalogReader interface {
	// Topology discovers the partition tree rooted at table via
	// pg_partition_tree, pg_class and pg_partitioned_table (FR-PLAN-1). The
	// returned Root is the authoritative resolution of table, so an
	// unqualified name in the specification is resolved here and nowhere else.
	// Partitions holds the discovered children; a child that is itself
	// partitioned is reported as such rather than flattened, so that the
	// planner can reject depth > 1 (FR-PLAN-2).
	Topology(ctx context.Context, table protocol.ObjectName) (protocol.TopologyInput, error)

	// Indexes returns the pg_index and pg_inherits state of every index named,
	// keyed by name. A name absent from the result does not exist in the
	// catalog, which is how the planner learns there is work to do
	// (FR-PLAN-4).
	Indexes(ctx context.Context, names []protocol.ObjectName) (map[protocol.ObjectName]IndexState, error)

	// RelationPages returns pg_class.relpages for every relation named. It is
	// the input to the per-node duration estimate (FR-PLAN-9). A relation
	// absent from the result estimates as zero pages rather than failing,
	// because an estimate is advisory: it drives the ETA in `status` and
	// nothing else.
	RelationPages(ctx context.Context, names []protocol.ObjectName) (map[protocol.ObjectName]int64, error)

	// OwnedByMemberRole reports, per relation, whether role is a member of that
	// relation's owning role (FR-PLAN-10, AC-12). A relation absent from the
	// result is treated as not a member: the planner fails closed.
	OwnedByMemberRole(ctx context.Context, role string, names []protocol.ObjectName) (map[protocol.ObjectName]bool, error)
}

// IndexState is the catalog state of one index: what pg_class, pg_index and
// pg_inherits say about it.
//
// The three boolean flags are pg_index.indisvalid, indisready and indislive.
// All three must hold for an index to be usable, which is why [IndexState.Healthy]
// exists rather than a bare check of indisvalid (FR-VER-1).
type IndexState struct {
	// Index is the index's own schema-qualified name.
	Index protocol.ObjectName
	// Relation is the table the index is on.
	Relation protocol.ObjectName
	// IsPartitioned is true for pg_class.relkind 'I', a partitioned index.
	IsPartitioned bool
	// Valid is pg_index.indisvalid. False on an index left behind by an
	// interrupted CREATE INDEX CONCURRENTLY.
	Valid bool
	// Ready is pg_index.indisready.
	Ready bool
	// Live is pg_index.indislive.
	Live bool
	// AttachedTo is the partitioned index this one is attached to, from
	// pg_inherits, or nil when the index is unattached. An unattached leaf
	// index can be dropped concurrently; an attached one cannot be dropped
	// individually at all (TRD §7.2.10).
	AttachedTo *protocol.ObjectName

	// Comment is obj_description(index, 'pg_class'): where PartitionCTL records
	// that it created the object ([protocol.Marker]). It is read in the same
	// pass as the rest of the index state, because ownership is a per-index
	// question and asking it one index at a time would cost a round trip per
	// leaf (NFR-PERF-1).
	Comment string
}

// Marker classifies whatever comment the index carries.
func (s IndexState) Marker() (protocol.Marker, protocol.MarkerStatus) {
	return protocol.ParseMarker(s.Comment)
}

// Healthy reports whether the index is valid, ready and live (FR-VER-1).
func (s IndexState) Healthy() bool { return s.Valid && s.Ready && s.Live }

// ClaimReader answers the second, narrower half of "is this object ours?".
//
// The first half is the ownership marker on the object itself, which arrives
// through [IndexState.Comment] and is a catalog fact. This covers the one
// window the marker cannot: the object exists because a statement ran, and the
// process died before the marker could be written onto it. The claim is the
// node checkpoint the state store already holds, and it expires when the node
// reaches a complete state.
type ClaimReader interface {
	// ClaimsObject reports the run holding a live claim on object, if any.
	ClaimsObject(ctx context.Context, object protocol.ObjectName) (string, bool, error)
}

// NoClaims returns a [ClaimReader] that holds none.
//
// It is the planner's default, and the default is the safe one: with no claim
// source the only thing that can authorize a drop is the marker on the object,
// which is exactly the property that makes ownership survive a PITR restore
// (FR-PLAN-7, NFR-REL-3).
func NoClaims() ClaimReader { return noClaims{} }

type noClaims struct{}

func (noClaims) ClaimsObject(context.Context, protocol.ObjectName) (string, bool, error) {
	return "", false, nil
}
