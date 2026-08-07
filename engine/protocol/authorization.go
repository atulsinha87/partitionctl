package protocol

import "fmt"

// AuthorizationMode is one of the three ways a destructive action can be
// authorized (TRD §7.2.9). Every destructive node carries exactly one
// (FR-AUTH-1).
//
// The invariant preserved is not "PartitionCTL created it" — that would forbid
// DropPartitionedIndex entirely — but that every destructive act traces to a
// specific, recorded justification written before the statement ran (INV-2,
// AC-20).
type AuthorizationMode string

// The three authorization modes.
const (
	// AuthProvenance is satisfied by a [Marker] on the object proving
	// PartitionCTL created it, or, only while a claim is live, by an in-flight
	// node checkpoint naming it (FR-AUTH-2 as amended). Used by
	// CreatePartitionedIndex to clean up an INVALID leaf index left by a
	// failed CREATE INDEX CONCURRENTLY. See [DecideProvenanceDrop] for the
	// decision table, which is the single implementation.
	AuthProvenance AuthorizationMode = "provenance"

	// AuthLeftover is satisfied only when the object matches PostgreSQL's
	// _ccnew/_ccold naming convention AND the *base* index carries a
	// PartitionCTL [Marker] (FR-AUTH-3 as amended, INV-7). Both conditions are
	// required: naming alone is forgeable, since an operator may have run
	// REINDEX CONCURRENTLY by hand and left their own _ccnew behind (AC-19).
	// See [DecideLeftoverDrop].
	AuthLeftover AuthorizationMode = "leftover"

	// AuthExplicit is satisfied only when the specification names the object
	// directly AND the operator supplied the operation's required confirmation
	// flag (FR-AUTH-4). Used by DropPartitionedIndex, where the operator's
	// stated intent is the authorization.
	AuthExplicit AuthorizationMode = "explicit"
)

var allAuthorizationModes = []AuthorizationMode{AuthProvenance, AuthLeftover, AuthExplicit}

// AllAuthorizationModes returns the three modes. The returned slice is a copy.
func AllAuthorizationModes() []AuthorizationMode {
	out := make([]AuthorizationMode, len(allAuthorizationModes))
	copy(out, allAuthorizationModes)
	return out
}

// Valid reports whether m is one of the three modes.
func (m AuthorizationMode) Valid() bool {
	switch m {
	case AuthProvenance, AuthLeftover, AuthExplicit:
		return true
	}
	return false
}

func (m AuthorizationMode) String() string { return string(m) }

// ConfirmExclusiveLock is the acknowledgement DropPartitionedIndex requires at
// plan time and records in the plan artifact (FR-DROP-3, AC-13).
const ConfirmExclusiveLock = "--confirm-exclusive-lock"

// Authorization is the destructive-action justification the planner attaches to
// a node of a destructive [NodeKind].
//
// It is a *proposal*, never a permission. The executor re-evaluates Mode
// against live state immediately before dispatch and halts the run if it is
// unsatisfied, regardless of what the plan asserts (FR-AUTH-5). The satisfied
// mode and its evidence are written to the audit trail before the statement
// executes (FR-AUTH-6).
type Authorization struct {
	// Mode is the single mode claimed for this node (FR-AUTH-1).
	Mode AuthorizationMode `json:"mode"`

	// Object identifies exactly what will be destroyed. It is the object whose
	// ownership marker the executor reads, and the identity recorded in the
	// audit trail.
	Object ObjectName `json:"object"`

	// Relation is the table the object belongs to. It is required for
	// AuthLeftover and optional elsewhere: a leftover is always reported
	// against the leaf it sits on, and the audit trail is unreadable without
	// it.
	Relation *ObjectName `json:"relation,omitempty"`

	// RequiredConfirmation names the CLI flag whose acknowledgement authorizes
	// this node, for example [ConfirmExclusiveLock]. Required for
	// [AuthExplicit] and meaningless otherwise; the executor checks that the
	// plan's [Plan.Confirmations] records it.
	RequiredConfirmation string `json:"required_confirmation,omitempty"`

	// Note is a human-readable statement of why the planner believes the mode
	// is satisfiable. It carries no authority.
	Note string `json:"note,omitempty"`
}

// Validate checks the authorization's internal consistency. It does not and
// cannot decide whether the mode is *satisfied*: that requires live state and
// happens at dispatch (FR-AUTH-5).
func (a *Authorization) Validate() error {
	if a == nil {
		return fmt.Errorf("authorization is nil")
	}
	if !a.Mode.Valid() {
		return fmt.Errorf("unknown authorization mode %q, want one of %v", a.Mode, allAuthorizationModes)
	}
	if err := a.Object.Validate(); err != nil {
		return fmt.Errorf("authorization object: %w", err)
	}
	if a.Relation != nil {
		if err := a.Relation.Validate(); err != nil {
			return fmt.Errorf("authorization relation: %w", err)
		}
	}
	switch a.Mode {
	case AuthExplicit:
		if a.RequiredConfirmation == "" {
			return fmt.Errorf("authorization mode %q requires required_confirmation (FR-AUTH-4)", a.Mode)
		}
	case AuthLeftover:
		if a.Relation == nil {
			return fmt.Errorf("authorization mode %q requires relation, so the audit trail names the leaf the leftover sat on (FR-AUTH-3)", a.Mode)
		}
	}
	return nil
}
