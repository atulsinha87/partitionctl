package verifier

import (
	"context"
	"fmt"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// IndexState is the catalog state of one index: the columns of pg_index and
// pg_class the verifier's assertions are about, and nothing else.
//
// It deliberately carries no OID. Two reads separated in time can see the same
// name backed by different OIDs (a REINDEX CONCURRENTLY swap does exactly that),
// and every assertion here is about the object reachable by name now. Identity
// over time is the topology fingerprint's job, not the verifier's.
type IndexState struct {
	// Name is the index, schema-qualified.
	Name protocol.ObjectName `json:"name"`
	// Relation is the table the index is on, schema-qualified.
	Relation protocol.ObjectName `json:"relation"`
	// Valid is pg_index.indisvalid.
	Valid bool `json:"indisvalid"`
	// Ready is pg_index.indisready.
	Ready bool `json:"indisready"`
	// Live is pg_index.indislive.
	Live bool `json:"indislive"`
	// Partitioned is true for pg_class.relkind = 'I', a partitioned index.
	Partitioned bool `json:"partitioned,omitempty"`
}

// Usable reports whether all three of indisvalid, indisready and indislive are
// true, which is the whole of FR-VER-1.
func (s IndexState) Usable() bool { return s.Valid && s.Ready && s.Live }

// Flags renders the three pg_index booleans for an operator-facing reason.
func (s IndexState) Flags() string {
	return fmt.Sprintf("indisvalid=%t indisready=%t indislive=%t", s.Valid, s.Ready, s.Live)
}

// usableFlags is what Flags returns for a healthy index; it is the "expected"
// side of an FR-VER-1 failure report.
const usableFlags = "indisvalid=true indisready=true indislive=true"

// Catalog is the read-only catalog surface the verifier needs. Every method is
// a query; there is no write method and there never will be (FR-VER-5).
//
// Implementations SHALL return schema-qualified names. An argument may be
// unqualified, in which case the implementation resolves it through the
// connection's search_path exactly as PostgreSQL would.
//
// A method that finds nothing returns an empty result and a nil error. An error
// means the catalog could not be read, which the verifier reports as
// [StatusError] rather than as a failed assertion.
type Catalog interface {
	// Index returns the state of one index. found is false when no index of
	// that name exists, which is a normal answer and not an error: it is what
	// CheckIndexAbsent is asking for.
	Index(ctx context.Context, name protocol.ObjectName) (state IndexState, found bool, err error)

	// IndexParent returns the partitioned index that child is attached to, per
	// pg_inherits (FR-VER-2). attached is false when child is not attached to
	// anything, including when child does not exist.
	IndexParent(ctx context.Context, child protocol.ObjectName) (parent protocol.ObjectName, attached bool, err error)

	// AttachedIndexes returns every index attached to parentIndex in
	// pg_inherits, with its state, ordered by schema then name. It is empty when
	// parentIndex does not exist or has no children.
	AttachedIndexes(ctx context.Context, parentIndex protocol.ObjectName) ([]IndexState, error)

	// LeafPartitions returns the leaf partitions of table, ordered by schema
	// then name. It is empty when table is not partitioned. This is the
	// "discovered leaf partition count" of FR-VER-4.
	LeafPartitions(ctx context.Context, table protocol.ObjectName) ([]protocol.ObjectName, error)

	// TreeIndexes returns every index on table and on every partition beneath
	// it, ordered by schema then name. It backs the leftover scan of FR-REIDX-6,
	// which must look at the whole tree because a failed REINDEX CONCURRENTLY
	// leaves its transient index on the leaf, not on the parent.
	TreeIndexes(ctx context.Context, table protocol.ObjectName) ([]IndexState, error)

	// IndexComment returns obj_description(index, 'pg_class'), which is where
	// PartitionCTL records ownership of an object it created
	// ([protocol.Marker]). found is false when the index does not exist or
	// carries no comment; the two are deliberately not distinguished, because
	// every destructive decision treats them identically.
	IndexComment(ctx context.Context, index protocol.ObjectName) (comment string, found bool, err error)
}

// IndexMarker reads the ownership marker on an index and classifies it. It is
// the one place the comment is turned into a [protocol.MarkerStatus], so no
// consumer can invent its own idea of what counts as ours.
func IndexMarker(ctx context.Context, c Catalog, index protocol.ObjectName) (protocol.Marker, protocol.MarkerStatus, error) {
	comment, found, err := c.IndexComment(ctx, index)
	if err != nil {
		return protocol.Marker{}, protocol.MarkerAbsent, err
	}
	if !found {
		return protocol.Marker{}, protocol.MarkerAbsent, nil
	}
	m, status := protocol.ParseMarker(comment)
	return m, status, nil
}

// sameObject compares an expected name against one the catalog returned.
//
// The expected side comes from a plan or a CLI flag and may legitimately be
// unqualified; the catalog side is always qualified. An unqualified expectation
// therefore matches on the bare name. That is not a loosening of the assertion:
// the object's identity was already fixed by the catalog lookup, which resolved
// the unqualified name through search_path. This comparison only decides whether
// the object the catalog found is the one that was named.
func sameObject(want, got protocol.ObjectName) bool {
	if want.Name != got.Name {
		return false
	}
	if want.Schema == "" || got.Schema == "" {
		return true
	}
	return want.Schema == got.Schema
}

// cloneName copies an optional name so a Result never aliases the caller's
// check, which would let a caller mutate a report after the fact.
func cloneName(o *protocol.ObjectName) *protocol.ObjectName {
	if o == nil {
		return nil
	}
	c := *o
	return &c
}
