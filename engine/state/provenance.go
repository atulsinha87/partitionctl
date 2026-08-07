package state

import (
	"context"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// ObjectKind names what a provenance record describes. v0.1 creates exactly one
// kind of object, but recording it keeps the record self-describing for the
// operations that follow.
type ObjectKind string

// The object kinds PartitionCTL can create.
const (
	// ObjectIndex is an ordinary or partitioned index.
	ObjectIndex ObjectKind = "index"
)

// Valid reports whether k is a known object kind.
func (k ObjectKind) Valid() bool { return k == ObjectIndex }

func (k ObjectKind) String() string { return string(k) }

// Provenance is recorded proof that PartitionCTL created a given object
// (FR-STATE-6, TRD §17.1).
//
// It is immutable once written. There is no update path, which is what makes
// "the record existed before the DDL ran" (INV-1) a property of the store
// rather than of the caller's discipline.
//
// A record is written before the DDL and is deliberately NOT removed when the
// DDL fails. A failed CREATE INDEX CONCURRENTLY leaves an indisvalid = false
// index behind, and the provenance record is the only thing that lets `resume`
// prove the wreckage is its own and clean it up (FR-PLAN-6, AC-5). An INVALID
// index with no record halts the run instead (FR-PLAN-7, AC-6).
type Provenance struct {
	// ProvenanceID is assigned by the store and is stable. It is the value an
	// [AuthorizationRecord] in mode provenance cites as its evidence.
	ProvenanceID string `json:"provenance_id"`

	RunID  RunID           `json:"run_id"`
	NodeID protocol.NodeID `json:"node_id,omitempty"`

	// PlanDigest is copied from the run, so a provenance record identifies the
	// exact reviewed artifact that authorized the creation.
	PlanDigest string `json:"plan_digest,omitempty"`

	// Database is the database the object lives in. It is part of the identity
	// because a file store can hold state for more than one target.
	Database string `json:"database,omitempty"`

	// Object is what was created. Its [protocol.ObjectName.String] form is the
	// lookup key.
	Object     protocol.ObjectName `json:"object"`
	ObjectKind ObjectKind          `json:"object_kind"`

	// Relation is the table the object belongs to, where it has one. It lets
	// the planner ask "what did we create on this leaf?" without knowing the
	// generated index name.
	Relation *protocol.ObjectName `json:"relation,omitempty"`

	Actor      string             `json:"actor,omitempty"`
	RecordedAt protocol.Timestamp `json:"recorded_at"`
}

// Validate checks the record's internal consistency.
func (p *Provenance) Validate() error {
	if p == nil {
		return protocol.ErrInvalidPlan.Detailf("provenance record is nil")
	}
	if p.RunID == "" {
		return protocol.ErrInvalidPlan.Detailf("provenance has an empty run id")
	}
	if err := p.Object.Validate(); err != nil {
		return protocol.ErrInvalidPlan.Detailf("provenance object: %v", err)
	}
	if !p.ObjectKind.Valid() {
		return protocol.ErrInvalidPlan.Detailf("provenance object kind %q is unknown", p.ObjectKind)
	}
	if p.Relation != nil {
		if err := p.Relation.Validate(); err != nil {
			return protocol.ErrInvalidPlan.Detailf("provenance relation: %v", err)
		}
	}
	return nil
}

// ProvenanceQuery selects provenance records. Object is required; the rest
// narrow.
type ProvenanceQuery struct {
	// Object is the object whose provenance is being proven. Required.
	Object protocol.ObjectName

	// Database narrows to one target database.
	Database string

	// Relation narrows to records naming a specific parent relation.
	Relation *protocol.ObjectName

	// RunID narrows to one run. Leaving it empty is the normal case: `resume`
	// opens a new run and must still find the previous run's provenance.
	RunID RunID
}

func (q ProvenanceQuery) validate() error {
	if q.Object.IsZero() {
		return protocol.ErrInvalidPlan.Detailf("provenance query requires an object")
	}
	return q.Object.Validate()
}

func (q ProvenanceQuery) matches(p Provenance) bool {
	if p.Object.Schema != q.Object.Schema || p.Object.Name != q.Object.Name {
		return false
	}
	if q.Database != "" && p.Database != q.Database {
		return false
	}
	if q.RunID != "" && p.RunID != q.RunID {
		return false
	}
	if q.Relation != nil {
		if p.Relation == nil {
			return false
		}
		if p.Relation.Schema != q.Relation.Schema || p.Relation.Name != q.Relation.Name {
			return false
		}
	}
	return true
}

// GuardedAction is a side effect that a state store runs only after the record
// justifying it is durably committed.
//
// It is the shape that makes INV-1 and INV-2 structural. A caller cannot issue
// the DDL first, because the DDL is an argument to the write, not a statement
// the caller sequences after it.
type GuardedAction func(ctx context.Context) error

// HasProvenance reports whether the store holds a committed provenance record
// for the queried object. It is the whole of FR-AUTH-2: the provenance
// authorization mode is satisfied by a committed record and by nothing else.
func HasProvenance(ctx context.Context, s ProvenanceReader, q ProvenanceQuery) (bool, string, error) {
	recs, err := s.FindProvenance(ctx, q)
	if err != nil {
		return false, "", err
	}
	if len(recs) == 0 {
		return false, "", nil
	}
	return true, recs[0].ProvenanceID, nil
}

// ProvenanceReader is the read half of provenance, which is all the planner and
// the authorization evaluator need.
type ProvenanceReader interface {
	FindProvenance(ctx context.Context, q ProvenanceQuery) ([]Provenance, error)
}
