package state

import (
	"context"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// GuardedAction is a side effect that a state store runs only after the record
// justifying it is durably committed.
//
// It is the shape that makes INV-2 structural. A caller cannot issue the
// destructive statement first, because the statement is an argument to the
// write, not something the caller sequences after it.
type GuardedAction func(ctx context.Context) error

// AuthorizationRecord is the justification for one destructive statement,
// written to the audit-bearing store before the statement runs (FR-AUTH-6,
// INV-2, AC-20).
//
// Its evidence is the map the decision table produced ([protocol.DropVerdict]),
// validated against a per-mode required-key list. An authorization that cites
// nothing is not an authorization, and [AuthorizationRecord.Validate] refuses
// it.
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

	// Confirmation names the acknowledgement flag the operator supplied, for
	// example [protocol.ConfirmExclusiveLock]. Required for
	// [protocol.AuthExplicit] (FR-AUTH-4).
	Confirmation string `json:"confirmation,omitempty"`

	// Evidence is what made the mode satisfied, as produced by the decision
	// table. It is checked against [RequiredEvidence] for the mode, so a record
	// that cites nothing cannot be written and therefore cannot gate a
	// statement.
	Evidence map[string]string `json:"evidence,omitempty"`

	GrantedAt protocol.Timestamp `json:"granted_at"`
}

// RequiredEvidence lists the evidence keys a record must carry for each mode.
// It is the check that replaced the typed ProvenanceID and ReindexRunID fields:
// the decision table is the only thing that can satisfy a mode, so the store
// asks for the keys the decision table produces rather than for ids it would
// have had to resolve a second time.
func RequiredEvidence(mode protocol.AuthorizationMode) []string {
	switch mode {
	case protocol.AuthProvenance:
		// Either the marker on the object or the live claim that stands in for
		// it while the marker is missing. "source" says which, and the
		// corresponding key is required alongside it.
		return []string{"mode", "object", "source"}
	case protocol.AuthLeftover:
		return []string{"mode", "object", "leftover_class", "base_index"}
	case protocol.AuthExplicit:
		return []string{"mode", "object", "confirmation"}
	}
	return nil
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
	if a.Mode == protocol.AuthLeftover {
		if a.Relation == nil {
			return protocol.ErrAuthorizationUnsatisfied.Detailf(
				"mode %q requires a relation, so the audit trail names the leaf the leftover sat on (FR-AUTH-3)", a.Mode)
		}
		if kind, _ := protocol.ClassifyLeftover(a.Object.Name); kind == protocol.LeftoverNone {
			return protocol.ErrAuthorizationUnsatisfied.Detailf(
				"mode %q requires the object name %q to match %s/%s (FR-AUTH-3, INV-7)",
				a.Mode, a.Object.Name, protocol.LeftoverNewPrefix, protocol.LeftoverOldPrefix)
		}
	}
	if a.Mode == protocol.AuthExplicit && a.Confirmation == "" {
		return protocol.ErrAuthorizationUnsatisfied.Detailf(
			"mode %q requires the confirmation flag the operator supplied (FR-AUTH-4)", a.Mode)
	}
	for _, k := range RequiredEvidence(a.Mode) {
		if a.Evidence[k] == "" {
			return protocol.ErrAuthorizationUnsatisfied.Detailf(
				"mode %q requires evidence %q; an authorization that cites nothing is not an "+
					"authorization (FR-AUTH-6, INV-2)", a.Mode, k)
		}
	}
	// The provenance mode is satisfied two ways and each names its own witness.
	if a.Mode == protocol.AuthProvenance {
		switch a.Evidence["source"] {
		case "marker":
			if a.Evidence["marker_run"] == "" {
				return protocol.ErrAuthorizationUnsatisfied.Detailf(
					"mode %q sourced from the object's marker must name the run that wrote it (FR-AUTH-2)", a.Mode)
			}
		case "claim":
			if a.Evidence["claim_run"] == "" {
				return protocol.ErrAuthorizationUnsatisfied.Detailf(
					"mode %q sourced from a live claim must name the claiming run (FR-AUTH-2)", a.Mode)
			}
		default:
			return protocol.ErrAuthorizationUnsatisfied.Detailf(
				"mode %q evidence source is %q; want \"marker\" or \"claim\" (FR-AUTH-2)",
				a.Mode, a.Evidence["source"])
		}
	}
	return nil
}

// RunReader is the read half of run records.
type RunReader interface {
	FindRuns(ctx context.Context, q RunQuery) ([]Run, error)
}
