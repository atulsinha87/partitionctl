package planner

import (
	"context"
	"strings"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// MinServerVersionNum is the oldest server_version_num PartitionCTL plans
// against: PostgreSQL 14 (NFR-COMPAT-1).
const MinServerVersionNum = 140000

// RelKind is pg_class.relkind.
type RelKind string

// The relkinds the planner distinguishes. The rest of PostgreSQL's set exists
// but is never a valid target.
const (
	RelKindTable            RelKind = "r"
	RelKindIndex            RelKind = "i"
	RelKindSequence         RelKind = "S"
	RelKindToast            RelKind = "t"
	RelKindView             RelKind = "v"
	RelKindMatView          RelKind = "m"
	RelKindComposite        RelKind = "c"
	RelKindForeignTable     RelKind = "f"
	RelKindPartitionedTable RelKind = "p"
	RelKindPartitionedIndex RelKind = "I"
)

func (k RelKind) String() string { return string(k) }

// IsPartitioned reports whether k is a partitioned table or index, which is
// what pg_partition_tree reports as a non-leaf.
func (k RelKind) IsPartitioned() bool {
	return k == RelKindPartitionedTable || k == RelKindPartitionedIndex
}

// IsIndex reports whether k is an index, partitioned or not.
func (k RelKind) IsIndex() bool {
	return k == RelKindIndex || k == RelKindPartitionedIndex
}

// CanCarryIndex reports whether a partition of this kind can carry an index
// built by CREATE INDEX CONCURRENTLY. Only an ordinary table can; a foreign
// table partition is legal in PostgreSQL and is a hard planner error here.
func (k RelKind) CanCarryIndex() bool { return k == RelKindTable }

// DefaultPartitionBound is the exact text pg_get_expr returns for a DEFAULT
// partition's relpartbound.
const DefaultPartitionBound = "DEFAULT"

// IsDefaultBound reports whether a partition bound expression describes a
// DEFAULT partition (FR-PLAN-3).
func IsDefaultBound(bound string) bool {
	return strings.EqualFold(strings.TrimSpace(bound), DefaultPartitionBound)
}

// Relation is one relation's catalog state, read from pg_class joined to
// pg_namespace and pg_inherits.
type Relation struct {
	// OID is pg_class.oid. It is the identity used by the topology
	// fingerprint: a partition dropped and recreated under the same name is a
	// different relation.
	OID uint32
	// Name is the schema-qualified name.
	Name protocol.ObjectName
	// Kind is pg_class.relkind.
	Kind RelKind
	// OwnerOID is pg_class.relowner.
	OwnerOID uint32
	// Owner is the owning role's name, when it has been resolved.
	Owner string
	// RelPages is pg_class.relpages, the planner's only size signal
	// (FR-PLAN-9). It is a statistic, not a measurement, and it is
	// deliberately excluded from the topology fingerprint.
	RelPages int64
	// ParentOID is pg_inherits.inhparent, or 0 for a relation with no parent.
	ParentOID uint32
	// PartitionBound is pg_get_expr(relpartbound, oid), empty when the
	// relation is not a partition.
	PartitionBound string
	// IsDefault marks a DEFAULT partition.
	IsDefault bool
}

func (r Relation) String() string { return r.Name.String() }

// State projects the relation into the fingerprint's view of it. The
// projection is lossy on purpose: RelPages is a statistic that changes on every
// autovacuum, and including it would make drift detection fire constantly (R6).
func (r Relation) State() protocol.RelationState {
	return protocol.RelationState{
		OID:            r.OID,
		Schema:         r.Name.Schema,
		Name:           r.Name.Name,
		RelKind:        string(r.Kind),
		OwnerOID:       r.OwnerOID,
		ParentOID:      r.ParentOID,
		PartitionBound: r.PartitionBound,
		IsDefault:      r.IsDefault,
	}
}

// TreeEntry is one row of pg_partition_tree(), joined to the relation state it
// names.
type TreeEntry struct {
	Relation
	// Level is pg_partition_tree.level: 0 for the root, 1 for its direct
	// partitions.
	Level int
	// IsLeaf is pg_partition_tree.isleaf: false for a relation that is itself
	// partitioned.
	IsLeaf bool
}

// Index is one index's catalog state, read from pg_index joined to pg_class,
// pg_namespace, pg_inherits and pg_constraint.
type Index struct {
	// OID is pg_class.oid of the index.
	OID uint32
	// Name is the schema-qualified index name.
	Name protocol.ObjectName
	// Kind is pg_class.relkind: 'i' for an ordinary index, 'I' for a
	// partitioned one.
	Kind RelKind
	// OwnerOID is pg_class.relowner of the index.
	OwnerOID uint32
	// RelPages is pg_class.relpages of the index, used to estimate a
	// reindex's peak additional storage (FR-REIDX-7).
	RelPages int64
	// TableOID is pg_index.indrelid.
	TableOID uint32
	// Table is the schema-qualified name of the indexed relation.
	Table protocol.ObjectName
	// IsValid is pg_index.indisvalid (FR-PLAN-4).
	IsValid bool
	// IsReady is pg_index.indisready (FR-PLAN-4).
	IsReady bool
	// IsLive is pg_index.indislive (FR-PLAN-4).
	IsLive bool
	// IsUnique is pg_index.indisunique.
	IsUnique bool
	// IsPrimary is pg_index.indisprimary.
	IsPrimary bool
	// IsExclusion is pg_index.indisexclusion.
	IsExclusion bool
	// ParentIndexOID is pg_inherits.inhparent for the index, or 0 when the
	// index is not attached to a partitioned index.
	ParentIndexOID uint32
	// ConstraintName is pg_constraint.conname when the index backs a UNIQUE,
	// PRIMARY KEY or EXCLUDE constraint (FR-DROP-2, AC-14), else empty.
	ConstraintName string
	// ConstraintType is pg_constraint.contype: "p", "u" or "x", else empty.
	ConstraintType string
}

func (i Index) String() string { return i.Name.String() }

// Attached reports whether the index is attached to any partitioned index.
func (i Index) Attached() bool { return i.ParentIndexOID != 0 }

// AttachedTo reports whether the index is attached to the partitioned index
// with the given OID.
func (i Index) AttachedTo(parentOID uint32) bool {
	return parentOID != 0 && i.ParentIndexOID == parentOID
}

// ConstraintBacked reports whether dropping the index requires ALTER TABLE ...
// DROP CONSTRAINT instead (FR-DROP-2).
func (i Index) ConstraintBacked() bool { return i.ConstraintName != "" }

// Condition classifies the index's usability (FR-PLAN-4). See
// [IndexCondition].
func (i Index) Condition() IndexCondition {
	switch {
	case !i.IsLive:
		return IndexNotLive
	case !i.IsValid:
		return IndexInvalid
	case !i.IsReady:
		return IndexNotReady
	}
	return IndexValid
}

// RoleMembership is the answer to "is the connected role a member of this
// owning role?" for one role (FR-PLAN-10).
type RoleMembership struct {
	// OwnerOID is the role's pg_roles.oid.
	OwnerOID uint32
	// OwnerName is pg_roles.rolname.
	OwnerName string
	// IsMember reports whether the connected role has the privileges of the
	// owning role, which is the test PostgreSQL itself applies to an ownership
	// check.
	IsMember bool
}

// CatalogReader is the read-only catalog surface the planner needs (FR-PLAN-8).
//
// Every method is a SELECT. No method issues DDL, opens a write transaction, or
// mutates anything. Implementations are expected to hold one consistent
// snapshot for the life of a planning pass; [BeginReadOnly] does that with a
// REPEATABLE READ, READ ONLY transaction.
//
// The interface is small on purpose: it is the whole seam between the planner
// and PostgreSQL, and [FakeCatalog] implements all of it in memory.
type CatalogReader interface {
	// CurrentRole returns the connected role (current_user).
	CurrentRole(ctx context.Context) (string, error)

	// CurrentDatabase returns the connected database name, recorded in the
	// plan so it cannot be executed against an unintended target by accident
	// (T8).
	CurrentDatabase(ctx context.Context) (string, error)

	// ServerVersionNum returns server_version_num, for the NFR-COMPAT-1 gate.
	ServerVersionNum(ctx context.Context) (int, error)

	// LookupRelation resolves a name to catalog state. An unqualified name
	// resolves through search_path. It returns an error matching
	// [ErrRelationNotFound] when there is no such relation.
	LookupRelation(ctx context.Context, name protocol.ObjectName) (Relation, error)

	// PartitionTree returns every relation in pg_partition_tree(root),
	// including the root itself (FR-PLAN-1). The planner never parses DDL
	// text to discover partitions.
	PartitionTree(ctx context.Context, rootOID uint32) ([]TreeEntry, error)

	// PartitionStrategy returns pg_partitioned_table.partstrat for a
	// partitioned relation.
	PartitionStrategy(ctx context.Context, rootOID uint32) (protocol.PartitionStrategy, error)

	// IndexesOnRelations returns every index on the given relations, in one
	// pass. Discovery is O(1) queries in the number of partitions, which is
	// what keeps planning inside NFR-PERF-1 at 1,000 leaves.
	IndexesOnRelations(ctx context.Context, tableOIDs []uint32) ([]Index, error)

	// LookupIndex resolves an index name to catalog state. It returns an error
	// matching [ErrIndexNotFound] when there is no such index.
	LookupIndex(ctx context.Context, name protocol.ObjectName) (Index, error)

	// IndexComment returns obj_description(index, 'pg_class'), which is where
	// PartitionCTL records ownership of an object it created
	// ([protocol.Marker]). found is false when the index does not exist or
	// carries no comment; the two are deliberately not distinguished, because
	// every destructive decision treats them identically.
	IndexComment(ctx context.Context, index protocol.ObjectName) (comment string, found bool, err error)

	// RoleMemberships reports, for each owning role OID, whether role has that
	// role's privileges (FR-PLAN-10). The result is keyed by OID and contains
	// an entry for every OID that exists in pg_roles.
	RoleMemberships(ctx context.Context, role string, ownerOIDs []uint32) (map[uint32]RoleMembership, error)
}

// ReadOnlyAsserter is implemented by a [CatalogReader] that can prove it is
// running inside a read-only transaction. [Host.Run] calls it when present, so
// FR-PLAN-8 is enforced rather than merely intended.
type ReadOnlyAsserter interface {
	AssertReadOnly(ctx context.Context) error
}

// ClaimLookup answers whether some run still holds a live claim on an object
// (FR-PLAN-6, FR-PLAN-7).
//
// It covers exactly one window that the ownership marker cannot: the object
// exists because a statement ran, and the process died before the marker could
// be written onto it. Outside that window the marker is the answer, and it is
// read from the catalog rather than from here.
//
// The planner deliberately depends on this one-method view rather than on the
// whole StateStore: it reads, never writes, and coupling planning to the store's
// full surface would make the planner untestable without one. state.ClaimsObject
// satisfies it with a two-line adapter.
type ClaimLookup interface {
	// ClaimsObject reports the run holding a live claim on object, if any.
	ClaimsObject(ctx context.Context, object protocol.ObjectName) (string, bool, error)
}

// IndexMarker reads the ownership marker on an index and classifies it. It is
// the one place a comment becomes a [protocol.MarkerStatus] on this side, so no
// consumer can invent its own idea of what counts as ours.
func IndexMarker(ctx context.Context, cr CatalogReader, index protocol.ObjectName) (protocol.Marker, protocol.MarkerStatus, error) {
	comment, found, err := cr.IndexComment(ctx, index)
	if err != nil {
		return protocol.Marker{}, protocol.MarkerAbsent, err
	}
	if !found {
		return protocol.Marker{}, protocol.MarkerAbsent, nil
	}
	m, status := protocol.ParseMarker(comment)
	return m, status, nil
}

// strategyFromCode maps pg_partitioned_table.partstrat to the protocol's
// strategy names.
func strategyFromCode(code string) (protocol.PartitionStrategy, error) {
	switch code {
	case "r":
		return protocol.StrategyRange, nil
	case "l":
		return protocol.StrategyList, nil
	case "h":
		return protocol.StrategyHash, nil
	}
	return "", ErrCatalogUnavailable.Detailf(
		"pg_partitioned_table.partstrat is %q, which this binary does not know", code)
}
