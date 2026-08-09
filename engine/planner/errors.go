package planner

import (
	"errors"
	"strconv"
	"strings"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// ---------------------------------------------------------------------------
// Topology rejections (FR-PLAN-2, FR-PLAN-3, AC-11)
// ---------------------------------------------------------------------------

// TopologyCode is the distinct machine-readable code for one topology
// rejection.
//
// FR-PLAN-3 requires HASH and DEFAULT to be distinguishable, but TRD §7.2.12
// gives the whole unsupported-topology class a single process exit status, 15,
// because CI branches on the class rather than on the reason. The code is what
// makes the reasons distinguishable: in the operator-facing message, in
// structured logs, and to errors.Is.
type TopologyCode string

// The topology rejections. Every one of them is a hard error at plan time and
// never a silent partial run (§4.2: unsupported topologies fail loudly).
const (
	// CodeNotPartitioned: the target is not a partitioned table
	// (pg_class.relkind != 'p').
	CodeNotPartitioned TopologyCode = "TOPO_NOT_PARTITIONED"

	// CodeMultiLevel: the partition tree is deeper than one level, or a
	// level-1 relation is itself partitioned. v0.1 supports depth 1 exactly
	// (FR-PLAN-2).
	CodeMultiLevel TopologyCode = "TOPO_MULTI_LEVEL"

	// CodeHashStrategy: the root is HASH partitioned (FR-PLAN-3).
	CodeHashStrategy TopologyCode = "TOPO_HASH_STRATEGY"

	// CodeUnsupportedStrategy: the root's strategy is neither RANGE, LIST nor
	// HASH, so it postdates this binary.
	CodeUnsupportedStrategy TopologyCode = "TOPO_UNSUPPORTED_STRATEGY"

	// CodeDefaultPartition: the tree contains a DEFAULT partition
	// (FR-PLAN-3).
	CodeDefaultPartition TopologyCode = "TOPO_DEFAULT_PARTITION"

	// CodeNoPartitions: the root is partitioned but has no partitions. A
	// parent index created ON ONLY such a table can never be marked valid,
	// because validity is granted on the final attach and there is nothing to
	// attach (TRD §7.2.13). Planning it would produce an artifact that cannot
	// converge.
	CodeNoPartitions TopologyCode = "TOPO_NO_PARTITIONS"

	// CodeUnsupportedPartitionKind: a partition is not an ordinary table, for
	// example a foreign table, so CREATE INDEX cannot run on it.
	CodeUnsupportedPartitionKind TopologyCode = "TOPO_UNSUPPORTED_PARTITION_KIND"
)

// Valid reports whether c is a known rejection code.
func (c TopologyCode) Valid() bool {
	switch c {
	case CodeNotPartitioned,
		CodeMultiLevel,
		CodeHashStrategy,
		CodeUnsupportedStrategy,
		CodeDefaultPartition,
		CodeNoPartitions,
		CodeUnsupportedPartitionKind:
		return true
	}
	return false
}

func (c TopologyCode) String() string { return string(c) }

// AllTopologyCodes returns every rejection code, in declaration order. The
// returned slice is a copy.
func AllTopologyCodes() []TopologyCode {
	return []TopologyCode{
		CodeNotPartitioned,
		CodeMultiLevel,
		CodeHashStrategy,
		CodeUnsupportedStrategy,
		CodeDefaultPartition,
		CodeNoPartitions,
		CodeUnsupportedPartitionKind,
	}
}

// TopologyError is a plan-time refusal to plan against a topology v0.1 does not
// support.
//
// It satisfies errors.Is in two directions on purpose. Matching
// [protocol.ErrUnsupportedTopology] gets the class, which is what the CLI's
// exit-code mapping wants; matching a &TopologyError{Code: ...} gets the exact
// reason, which is what a test or a targeted recovery wants.
type TopologyError struct {
	// Code is the distinct reason (FR-PLAN-3).
	Code TopologyCode
	// Relation names the offending relation, where one relation is at fault.
	Relation string
	// Detail is the operator-facing explanation, including what to do instead.
	Detail string
}

func (e *TopologyError) Error() string {
	var b strings.Builder
	b.WriteString("unsupported topology [")
	b.WriteString(string(e.Code))
	b.WriteString("]")
	if e.Relation != "" {
		b.WriteString(" on ")
		b.WriteString(e.Relation)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	return b.String()
}

// ExitCode implements [protocol.ExitCoder]. Every topology rejection exits 15.
func (e *TopologyError) ExitCode() protocol.ExitCode { return protocol.ExitUnsupportedTopology }

// Is matches a *TopologyError with the same Code, or the class sentinel
// [protocol.ErrUnsupportedTopology]. A target with an empty Code matches any
// topology rejection.
//
// The class match is written here rather than exposed through Unwrap on
// purpose. [protocol.Error.Is] resolves its target with errors.As, which walks
// the *target's* chain, so a *TopologyError that unwrapped to the class
// sentinel would report itself equal to every other topology rejection: a HASH
// error would satisfy errors.Is against a DEFAULT error, which is exactly the
// distinction FR-PLAN-3 requires.
func (e *TopologyError) Is(target error) bool {
	switch t := target.(type) {
	case *TopologyError:
		return t.Code == "" || t.Code == e.Code
	case *protocol.Error:
		return t.Kind == protocol.KindUnsupportedTopology
	}
	return false
}

// As reports the rejection as the class sentinel, so [protocol.KindOf] can
// classify it for structured logging (NFR-OBS-2) without an Unwrap edge.
func (e *TopologyError) As(target any) bool {
	if pe, ok := target.(**protocol.Error); ok {
		*pe = protocol.ErrUnsupportedTopology
		return true
	}
	return false
}

// TopologyCodeOf extracts the distinct rejection code from an error chain.
func TopologyCodeOf(err error) (TopologyCode, bool) {
	var te *TopologyError
	if errors.As(err, &te) {
		return te.Code, true
	}
	return "", false
}

func topologyErr(code TopologyCode, relation, detail string) error {
	return &TopologyError{Code: code, Relation: relation, Detail: detail}
}

// ---------------------------------------------------------------------------
// Privilege rejection (FR-PLAN-10, AC-12)
// ---------------------------------------------------------------------------

// OwnershipViolation is one relation whose owning role the connected role is
// not a member of.
type OwnershipViolation struct {
	// Relation is the relation, in schema.name form.
	Relation string
	// Owner is the owning role's name, empty if it could not be resolved.
	Owner string
	// OwnerOID is pg_class.relowner.
	OwnerOID uint32
}

// PrivilegeError is a plan-time refusal because the connected role is not a
// member of the owning role of every relation the plan would modify.
//
// This fails at plan time deliberately (FR-PLAN-10). CREATE INDEX CONCURRENTLY
// requires table ownership and superuser is unavailable on managed PostgreSQL,
// so discovering this at leaf 300 of 400 would waste hours.
type PrivilegeError struct {
	// Role is the connected role.
	Role string
	// Violations lists every relation that failed, in discovery order.
	Violations []OwnershipViolation
}

func (e *PrivilegeError) Error() string {
	var b strings.Builder
	b.WriteString("insufficient privilege: role ")
	b.WriteString(strconv.Quote(e.Role))
	switch len(e.Violations) {
	case 0:
		b.WriteString(" is not a member of the owning role of a relation to be modified")
	case 1:
		v := e.Violations[0]
		b.WriteString(" is not a member of role ")
		b.WriteString(strconv.Quote(v.Owner))
		b.WriteString(", which owns ")
		b.WriteString(v.Relation)
	default:
		v := e.Violations[0]
		b.WriteString(" is not a member of the owning role of ")
		b.WriteString(strconv.Itoa(len(e.Violations)))
		b.WriteString(" relations, starting with ")
		b.WriteString(v.Relation)
		b.WriteString(" (owner ")
		b.WriteString(strconv.Quote(v.Owner))
		b.WriteString(")")
	}
	b.WriteString("; grant the owning role to it, or run as a role that already has it")
	return b.String()
}

// ExitCode implements [protocol.ExitCoder]. Insufficient privilege exits 16.
func (e *PrivilegeError) ExitCode() protocol.ExitCode { return protocol.ExitInsufficientPrivilege }

// Is matches any other *PrivilegeError, or the class sentinel
// [protocol.ErrInsufficientPrivilege]. See [TopologyError.Is] for why the class
// match is not an Unwrap edge.
func (e *PrivilegeError) Is(target error) bool {
	switch t := target.(type) {
	case *PrivilegeError:
		return true
	case *protocol.Error:
		return t.Kind == protocol.KindInsufficientPrivilege
	}
	return false
}

// As reports the refusal as the class sentinel, for [protocol.KindOf].
func (e *PrivilegeError) As(target any) bool {
	if pe, ok := target.(**protocol.Error); ok {
		*pe = protocol.ErrInsufficientPrivilege
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Sentinels
// ---------------------------------------------------------------------------

// The planner's error kinds. They extend [protocol.ErrorKind]; each maps to a
// contract exit code through the sentinel below it.
const (
	KindRelationNotFound         protocol.ErrorKind = "relation_not_found"
	KindIndexNotFound            protocol.ErrorKind = "index_not_found"
	KindAmbiguousRelation        protocol.ErrorKind = "ambiguous_relation"
	KindCatalogUnavailable       protocol.ErrorKind = "catalog_unavailable"
	KindUnsupportedServerVersion protocol.ErrorKind = "unsupported_server_version"
	KindInvalidSpecification     protocol.ErrorKind = "invalid_specification"
	KindNotReadOnly              protocol.ErrorKind = "not_read_only"
	KindForeignInvalidIndex      protocol.ErrorKind = "foreign_invalid_index"
)

// The planner's sentinel errors. Match them with errors.Is; derive richer
// values with Detailf or Wrap, exactly as in the protocol package.
var (
	// ErrRelationNotFound means the named relation does not exist, or is not
	// visible to the connected role. Exit 1.
	ErrRelationNotFound = &protocol.Error{
		Kind: KindRelationNotFound, Code: protocol.ExitFailure,
		Msg: "relation not found",
	}

	// ErrIndexNotFound means the named index does not exist (FR-DROP-1).
	// Exit 1.
	ErrIndexNotFound = &protocol.Error{
		Kind: KindIndexNotFound, Code: protocol.ExitFailure,
		Msg: "index not found",
	}

	// ErrAmbiguousRelation means an unqualified name resolved to more than one
	// relation. Exit 1.
	ErrAmbiguousRelation = &protocol.Error{
		Kind: KindAmbiguousRelation, Code: protocol.ExitFailure,
		Msg: "ambiguous relation name",
	}

	// ErrCatalogUnavailable means a catalog query failed or returned something
	// this binary cannot interpret. Exit 1.
	ErrCatalogUnavailable = &protocol.Error{
		Kind: KindCatalogUnavailable, Code: protocol.ExitFailure,
		Msg: "catalog read failed",
	}

	// ErrUnsupportedServerVersion means the server predates the oldest release
	// PartitionCTL supports (NFR-COMPAT-1). Exit 1.
	ErrUnsupportedServerVersion = &protocol.Error{
		Kind: KindUnsupportedServerVersion, Code: protocol.ExitFailure,
		Msg: "unsupported PostgreSQL server version",
	}

	// ErrInvalidSpecification means the specification is structurally
	// incomplete. Exit 1.
	ErrInvalidSpecification = &protocol.Error{
		Kind: KindInvalidSpecification, Code: protocol.ExitFailure,
		Msg: "invalid specification",
	}

	// ErrNotReadOnly means the catalog reader is not inside a read-only
	// transaction, which FR-PLAN-8 requires. Exit 1.
	ErrNotReadOnly = &protocol.Error{
		Kind: KindNotReadOnly, Code: protocol.ExitFailure,
		Msg: "planner is not running in a read-only transaction (FR-PLAN-8)",
	}

	// ErrForeignInvalidIndex means an INVALID index exists that PartitionCTL
	// cannot prove it created, so the run halts rather than dropping it
	// (FR-PLAN-7, NFR-REL-3, AC-6). Exit 13, the destructive-action-halted
	// code, because the halt is exactly the authorization gate refusing.
	ErrForeignInvalidIndex = &protocol.Error{
		Kind: KindForeignInvalidIndex, Code: protocol.ExitAuthorizationUnsatisfied,
		Msg: "an unusable index exists that PartitionCTL cannot prove it created",
	}
)
