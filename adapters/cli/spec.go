package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/atulsinha87/partitionctl/engine/planner"
	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// SpecFile is the on-disk form of a specification: the declarative statement of
// the desired end state that `plan --spec` compiles (TRD §17.1, FR-CLI-2).
//
// # Why JSON
//
// FR-CLI-7 names YAML for the *configuration* file, and that file is flat, so
// the restricted reader in config.go covers it honestly. A specification is not
// flat: an index definition nests columns, each with a collation, an operator
// class and a sort direction. Hand-rolling a parser for nested YAML would put a
// bug-prone hand-written parser in the path of a file that decides what DDL
// runs against a production database, and a misparsed opclass is a silently
// wrong index.
//
// So the specification is JSON, parsed by encoding/json with
// DisallowUnknownFields. The trade is deliberate: JSON is less pleasant to
// write than YAML, and in exchange a typo is a named error rather than a
// silently dropped field, and the format matches the plan artifact the operator
// reviews next. When a YAML parser is available the file gains a second
// accepted syntax; it does not need a different schema.
//
// # Example
//
//	{
//	  "operation": "create-index",
//	  "table": "public.orders",
//	  "index": "orders_created_at_idx",
//	  "definition": {
//	    "method": "btree",
//	    "columns": [{"name": "created_at", "descending": true}],
//	    "where": "status <> 'deleted'"
//	  },
//	  "pace_seconds": 5,
//	  "pace_reason": "let autovacuum catch up between partitions"
//	}
//
// The file carries no credentials and no connection string (NFR-SEC-3). The
// database is a connection concern and belongs in the configuration, not in a
// document that is committed to a repository.
type SpecFile struct {
	// Operation names the planner to run: create-index, reindex-index or
	// drop-index. Required.
	Operation string `json:"operation"`

	// Table is the partitioned parent table, as `schema.name` or `name`.
	// Required.
	Table string `json:"table"`

	// Index is the partitioned index the operation acts on. Required: all
	// three v0.1 operations name one.
	Index string `json:"index"`

	// Definition is the index shape. Required by create-index; ignored by the
	// operations that act on an index that already exists.
	Definition protocol.IndexDefinition `json:"definition,omitempty"`

	// PaceSeconds is the pause the planner emits after each leaf, as a `wait`
	// node in the graph (FR-ORD-3). Every pause is a node so that it is visible
	// in the artifact the operator reviews.
	PaceSeconds int `json:"pace_seconds,omitempty"`

	// PaceReason is the operator-facing explanation recorded on each wait node.
	PaceReason string `json:"pace_reason,omitempty"`

	// ReindexSince is reindex-index's watermark, as an RFC 3339 instant. A leaf
	// whose index records a PartitionCTL rebuild at or after it is skipped
	// (FR-PLAN-5), which is what makes a 400-partition reindex resumable across
	// days. Empty rebuilds every leaf.
	ReindexSince string `json:"reindex_since,omitempty"`

	// ConfirmExclusiveLock acknowledges that the operation takes an
	// AccessExclusiveLock on the parent and every leaf. DropPartitionedIndex
	// requires it and the plan records that it was given (FR-DROP-3, AC-13).
	ConfirmExclusiveLock bool `json:"confirm_exclusive_lock,omitempty"`

	// Note is free-form operator context recorded alongside a confirmation.
	Note string `json:"note,omitempty"`
}

// LoadSpecFile reads and parses a specification file.
func LoadSpecFile(path string) (SpecFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SpecFile{}, err
	}
	return ParseSpec(path, data)
}

// ParseSpec parses a specification document.
//
// Unknown fields are rejected. A specification that names `colums` instead of
// `columns` should not plan an index over nothing.
func ParseSpec(name string, data []byte) (SpecFile, error) {
	var s SpecFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return SpecFile{}, protocol.ErrFailure.Detailf("%s: %v", name, err)
	}
	// A second value in the document is a sign the file is not what its author
	// thinks it is.
	if dec.More() {
		return SpecFile{}, protocol.ErrFailure.Detailf(
			"%s: trailing content after the specification object", name)
	}
	return s, nil
}

// Specification converts the file into the planner host's input, resolving
// names and applying the actor and the confirmations.
func (s SpecFile) Specification(actor string, now protocol.Timestamp) (planner.Specification, error) {
	op := protocol.Operation(strings.TrimSpace(s.Operation))
	if !op.Valid() {
		return planner.Specification{}, protocol.ErrFailure.Detailf(
			"specification: unknown operation %q, want one of %v", s.Operation, protocol.AllOperations())
	}
	table, err := protocol.ParseObjectName(strings.TrimSpace(s.Table))
	if err != nil {
		return planner.Specification{}, protocol.ErrFailure.Detailf("specification: table: %v", err)
	}
	index, err := protocol.ParseObjectName(strings.TrimSpace(s.Index))
	if err != nil {
		return planner.Specification{}, protocol.ErrFailure.Detailf("specification: index: %v", err)
	}

	spec := planner.Specification{
		Operation:   op,
		Table:       table,
		Index:       index,
		Definition:  s.Definition,
		PaceSeconds: s.PaceSeconds,
		PaceReason:  s.PaceReason,
		Actor:       actor,
	}
	if since := strings.TrimSpace(s.ReindexSince); since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return planner.Specification{}, protocol.ErrFailure.Detailf(
				"specification: reindex_since %q is not an RFC 3339 instant: %v", since, err)
		}
		spec.ReindexSince = t
	}
	if s.ConfirmExclusiveLock {
		spec.Confirmations = append(spec.Confirmations, protocol.Confirmation{
			Flag:  protocol.ConfirmExclusiveLock,
			Actor: actor,
			At:    now,
			Note:  s.Note,
		})
	}
	return spec, spec.Validate()
}
