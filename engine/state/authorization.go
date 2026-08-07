package state

import (
	"context"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// AuthorizationRecord is the justification for one destructive statement,
// written to the audit-bearing store before the statement runs (FR-AUTH-6,
// INV-2, AC-20).
//
// It carries the evidence for its mode in typed fields rather than only in a
// free-form map, so the store can check that the mode cites something. An
// authorization that names no provenance record, no reindex run and no
// confirmation is not an authorization, and [AuthorizationRecord.Validate]
// refuses it.
type AuthorizationRecord struct {
	// AuthorizationID is assigned by the store and is stable.
	AuthorizationID string `json:"authorization_id"`

	RunID  RunID           `json:"run_id"`
	NodeID protocol.NodeID `json:"node_id"`

	// Mode is the single satisfied mode (FR-AUTH-1). Recording a record at all
	// asserts that the executor evaluated it against live state and found it
	// satisfied (FR-AUTH-5).
	Mode protocol.AuthorizationMode `json:"mode"`

	// Object is what will be destroyed.
	Object protocol.ObjectName `json:"object"`

	// Relation is the table the object belongs to. Required for
	// [protocol.AuthLeftover], whose run history is recorded per relation.
	Relation *protocol.ObjectName `json:"relation,omitempty"`

	// Database narrows the identity to one target.
	Database string `json:"database,omitempty"`

	// ProvenanceID cites the committed provenance record that satisfies
	// [protocol.AuthProvenance] (FR-AUTH-2). Required for that mode.
	ProvenanceID string `json:"provenance_id,omitempty"`

	// ReindexRunID cites the recorded PartitionCTL reindex run that satisfies
	// the second half of [protocol.AuthLeftover] (FR-AUTH-3, INV-7). Required
	// for that mode. The first half, the _ccnew/_ccold naming match, is checked
	// against [protocol.ClassifyLeftover] during validation, so neither
	// condition alone can produce a record.
	ReindexRunID RunID `json:"reindex_run_id,omitempty"`

	// Confirmation names the acknowledgement flag the operator supplied, for
	// example [protocol.ConfirmExclusiveLock]. Required for
	// [protocol.AuthExplicit] (FR-AUTH-4).
	Confirmation string `json:"confirmation,omitempty"`

	// Evidence is free-form supporting detail for the audit reader. It is
	// never the sole basis of a mode.
	Evidence map[string]string `json:"evidence,omitempty"`

	GrantedAt protocol.Timestamp `json:"granted_at"`
}

// Validate enforces FR-AUTH-1…4 and INV-7 on the record's shape.
//
// It cannot decide whether the mode is genuinely satisfied against live state:
// that is the executor's job at dispatch (FR-AUTH-5). What it can do, and does,
// is refuse to record a justification that cites nothing.
func (a *AuthorizationRecord) Validate() error {
	if a == nil {
		return protocol.ErrAuthorizationUnsatisfied.Detailf("authorization record is nil")
	}
	if a.RunID == "" {
		return protocol.ErrAuthorizationUnsatisfied.Detailf("authorization has an empty run id")
	}
	if a.NodeID == "" {
		return protocol.ErrAuthorizationUnsatisfied.Detailf("authorization has an empty node id")
	}
	if !a.Mode.Valid() {
		return protocol.ErrAuthorizationUnsatisfied.Detailf(
			"unknown authorization mode %q, want one of %v", a.Mode, protocol.AllAuthorizationModes())
	}
	if err := a.Object.Validate(); err != nil {
		return protocol.ErrAuthorizationUnsatisfied.Detailf("authorization object: %v", err)
	}
	if a.Relation != nil {
		if err := a.Relation.Validate(); err != nil {
			return protocol.ErrAuthorizationUnsatisfied.Detailf("authorization relation: %v", err)
		}
	}
	switch a.Mode {
	case protocol.AuthProvenance:
		if a.ProvenanceID == "" {
			return protocol.ErrAuthorizationUnsatisfied.Detailf(
				"mode %q requires the id of the committed provenance record it relies on (FR-AUTH-2)", a.Mode)
		}
	case protocol.AuthLeftover:
		if a.Relation == nil {
			return protocol.ErrAuthorizationUnsatisfied.Detailf(
				"mode %q requires a relation, because reindex-run history is recorded per relation (FR-AUTH-3)", a.Mode)
		}
		if kind, _ := protocol.ClassifyLeftover(a.Object.Name); kind == protocol.LeftoverNone {
			return protocol.ErrAuthorizationUnsatisfied.Detailf(
				"mode %q requires the object name %q to match %s/%s (FR-AUTH-3, INV-7)",
				a.Mode, a.Object.Name, protocol.LeftoverNewPrefix, protocol.LeftoverOldPrefix)
		}
		if a.ReindexRunID == "" {
			return protocol.ErrAuthorizationUnsatisfied.Detailf(
				"mode %q requires a recorded reindex run for %s; naming alone is forgeable (FR-AUTH-7, INV-7, AC-19)",
				a.Mode, a.Relation)
		}
	case protocol.AuthExplicit:
		if a.Confirmation == "" {
			return protocol.ErrAuthorizationUnsatisfied.Detailf(
				"mode %q requires the confirmation flag the operator supplied (FR-AUTH-4)", a.Mode)
		}
	}
	return nil
}

// ReindexHistoryQuery asks whether PartitionCTL has ever reindexed a relation.
//
// Relations takes more than one name on purpose. A _ccnew leftover lives on a
// leaf partition, while the reindex run is recorded against the partitioned
// parent it targeted, so answering FR-AUTH-3 for a leaf means asking about the
// leaf and its parent together. The caller supplies both; a match on either is
// a match.
type ReindexHistoryQuery struct {
	Database  string
	Relations []protocol.ObjectName

	// Since bounds the search to runs that finished at or after this instant.
	// The zero value matches any run, which is what FR-AUTH-3 wants; FR-LB-4's
	// gate sets it.
	Since time.Time

	// Statuses filters run status. Empty means any status, which is
	// deliberate for FR-AUTH-3: a reindex run that failed partway is still
	// what left the _ccnew behind.
	Statuses []RunStatus
}

// ReindexRunFor answers the second, non-forgeable half of
// [protocol.AuthLeftover]: does the store record a PartitionCTL reindex run for
// this relation (FR-AUTH-3, INV-7, AC-19)?
//
// It returns the most recently started matching run. A caller with no match
// must halt: a _ccnew or _ccold index on a relation PartitionCTL never
// reindexed belongs to whoever ran REINDEX CONCURRENTLY by hand, and is not
// PartitionCTL's to drop.
func ReindexRunFor(ctx context.Context, s RunReader, q ReindexHistoryQuery) (Run, bool, error) {
	var best Run
	found := false
	for i := range q.Relations {
		rel := q.Relations[i]
		runs, err := s.FindRuns(ctx, RunQuery{
			Database:      q.Database,
			Table:         &rel,
			Operation:     protocol.OpReindexIndex,
			Statuses:      q.Statuses,
			FinishedSince: q.Since,
		})
		if err != nil {
			return Run{}, false, err
		}
		for _, r := range runs {
			if !found || r.StartedAt.Time.After(best.StartedAt.Time) {
				best, found = r, true
			}
		}
	}
	return best, found, nil
}

// RunReader is the read half of run records.
type RunReader interface {
	FindRuns(ctx context.Context, q RunQuery) ([]Run, error)
}
