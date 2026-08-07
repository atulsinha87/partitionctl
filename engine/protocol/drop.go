package protocol

// DropAction is the verdict of the destructive-action decision table
// (tech-lead directive A.5). It is the single implementation both the planner
// and the executor evaluate: the planner at plan time, the executor again
// against live state immediately before dispatch (FR-AUTH-5, INV-2). Two
// implementations of this table would be two places for the answers to differ,
// and the second one only ever runs against a production catalog.
type DropAction string

// The three verdicts.
const (
	// DropHalt refuses. The caller emits no plan, or halts the run.
	DropHalt DropAction = "halt"

	// DropAuthorized drops the object. Everything that reaches this verdict is
	// authorized by a catalog fact, so `execute` may perform it.
	DropAuthorized DropAction = "drop"

	// DropAdoptThenDrop writes the ownership marker onto the object and only
	// then drops it. It is reachable only while a live claim names the object,
	// and it is reserved to `resume` (FR-CLI-9).
	DropAdoptThenDrop DropAction = "adopt_then_drop"
)

func (a DropAction) String() string { return string(a) }

// Destructive reports whether the verdict ends in a DROP.
func (a DropAction) Destructive() bool {
	return a == DropAuthorized || a == DropAdoptThenDrop
}

// DropVerdict is one evaluation of the decision table.
type DropVerdict struct {
	// Action is the only field that decides anything.
	Action DropAction
	// Evidence is what made the verdict, recorded before the statement runs
	// (FR-AUTH-6). It is JSON-marshalled by the state store, so a map is safe:
	// encoding/json sorts keys.
	Evidence map[string]string
	// Reason explains a halt, in operator-facing terms.
	Reason string
}

// Satisfied reports whether the verdict authorizes the drop.
func (v DropVerdict) Satisfied() bool { return v.Action.Destructive() }

// ProvenanceDropInput is everything the [AuthProvenance] row of the decision
// table reads: the marker on the object itself, and whether any run holds a
// live claim on it.
type ProvenanceDropInput struct {
	// Object is what would be destroyed.
	Object ObjectName
	// Status classifies whatever comment the object carries.
	Status MarkerStatus
	// Marker is the parsed marker, meaningful only when Status is
	// [MarkerOurs].
	Marker Marker
	// ClaimRun is the run holding a live claim on Object, empty when none.
	ClaimRun string
}

// DecideProvenanceDrop evaluates the provenance row of the decision table for a
// non-usable leaf index occupying a generated child name.
//
//	marker      claim   verdict
//	ours        any     DROP, on the strength of the marker
//	absent      yes     ADOPT then DROP: write the marker, then drop
//	absent      no      HALT (ErrForeignInvalidIndex, exit 13)
//	foreign     any     HALT: never overwrite a human's comment, never drop under it
//	unreadable  any     HALT: a newer or corrupt marker owns this object
//
// The absent-with-claim row is what covers the one crash window the marker
// cannot: CREATE INDEX CONCURRENTLY returned or partly ran and the process died
// before the COMMENT. The claim is the node checkpoint the executor already
// writes before dispatch, and it expires by state transition rather than by
// time — when a run completes, every node is DONE and no claim survives. That
// is what closes the stale-record attack that defeated AC-6: a completed run
// leaves nothing behind that can authorize destroying a same-named index
// somebody else created later.
func DecideProvenanceDrop(in ProvenanceDropInput) DropVerdict {
	object := in.Object.String()
	switch in.Status {
	case MarkerOurs:
		return DropVerdict{
			Action: DropAuthorized,
			Evidence: map[string]string{
				"mode":        string(AuthProvenance),
				"object":      object,
				"source":      "marker",
				"marker_run":  in.Marker.Run,
				"marker_plan": in.Marker.Plan,
				"marker_at":   in.Marker.At,
				"marker_op":   in.Marker.Op,
			},
		}

	case MarkerAbsent:
		if in.ClaimRun == "" {
			return DropVerdict{
				Action: DropHalt,
				Reason: object + " carries no PartitionCTL ownership marker and no run holds a live " +
					"claim on it, so PartitionCTL cannot prove it created it (FR-AUTH-2, AC-6). " +
					"Review it and drop it by hand if it is yours",
			}
		}
		return DropVerdict{
			Action: DropAdoptThenDrop,
			Evidence: map[string]string{
				"mode":      string(AuthProvenance),
				"object":    object,
				"source":    "claim",
				"claim_run": in.ClaimRun,
			},
		}

	case MarkerForeign:
		return DropVerdict{
			Action: DropHalt,
			Reason: object + " carries a comment that is not a PartitionCTL marker. Somebody wrote it " +
				"deliberately, so PartitionCTL will neither overwrite it nor drop the object under it " +
				"(FR-AUTH-2, AC-6)",
		}
	}
	return DropVerdict{
		Action: DropHalt,
		Reason: object + " carries a marker with the " + markerPrefix + " prefix that this binary " +
			"cannot read: either a newer PartitionCTL owns this object, or the comment is corrupt. " +
			"Upgrade, or clear the comment by hand after reviewing it",
	}
}

// LeftoverDropInput is everything the [AuthLeftover] row of the decision table
// reads. Authorization is taken off the *base* index, never off the leftover
// itself: whether PostgreSQL's index_concurrently_swap copies the description
// onto _ccnew/_ccold in every failure path is not measured, and a rebuild that
// fails before the swap certainly leaves an unmarked _ccnew.
type LeftoverDropInput struct {
	// Object is the leftover index that would be destroyed.
	Object ObjectName
	// BaseExists reports whether the base index — Object's name with the
	// _ccnew/_ccold suffix trimmed, in the same schema — is in the catalog. It
	// only refines the refusal message: a base that does not exist and a base
	// that exists unmarked are both halts. A caller that cannot distinguish the
	// two passes true and gets the generic message.
	BaseExists bool
	// BaseStatus classifies the base index's comment.
	BaseStatus MarkerStatus
	// BaseMarker is the base index's parsed marker, meaningful only when
	// BaseStatus is [MarkerOurs].
	BaseMarker Marker
}

// DecideLeftoverDrop evaluates the leftover row of the decision table for a
// _ccnew / _ccold index (INV-7, FR-AUTH-3 as amended).
//
// Two independent conditions, both required, because naming alone is forgeable:
// an operator may have run REINDEX CONCURRENTLY by hand and left their own
// _ccnew behind (AC-19).
//
//  1. the name matches [ClassifyLeftover]; and
//  2. the base index exists and carries a PartitionCTL marker.
//
// FR-AUTH-3's second condition was "the relation has a recorded PartitionCTL
// reindex run", which needed run history and therefore a state store that
// survives a PITR restore. Reading it off the base index instead makes the
// question a catalog question. The behavioural difference is one case: an
// operator who hand-ran REINDEX CONCURRENTLY on a *PartitionCTL-created* index
// and left a _ccnew behind will now have it dropped. That is correct — it is an
// unattached invalid index on our object, dropping it is PostgreSQL's own
// documented recovery, and nothing usable is destroyed.
func DecideLeftoverDrop(in LeftoverDropInput) DropVerdict {
	object := in.Object.String()
	kind, base := ClassifyLeftover(in.Object.Name)
	if kind == LeftoverNone {
		return DropVerdict{
			Action: DropHalt,
			Reason: object + " does not match the " + LeftoverNewPrefix + "/" + LeftoverOldPrefix +
				" naming convention (FR-AUTH-3, INV-7)",
		}
	}
	baseName := NewObjectName(in.Object.Schema, base)
	if in.BaseStatus != MarkerOurs {
		if !in.BaseExists {
			return DropVerdict{
				Action: DropHalt,
				Reason: object + " looks like a REINDEX CONCURRENTLY leftover but its base index " +
					baseName.String() + " does not exist, so there is nothing that proves the rebuild " +
					"was PartitionCTL's (FR-AUTH-3, INV-7, AC-19)",
			}
		}
		return DropVerdict{
			Action: DropHalt,
			Reason: "the base index " + baseName.String() + " carries no PartitionCTL ownership marker (" +
				in.BaseStatus.String() + "), so the leftover " + object +
				" is not PartitionCTL's to drop (FR-AUTH-3, INV-7, AC-19)",
		}
	}
	return DropVerdict{
		Action: DropAuthorized,
		Evidence: map[string]string{
			"mode":             string(AuthLeftover),
			"object":           object,
			"source":           "base_marker",
			"leftover_class":   string(kind),
			"base_index":       baseName.String(),
			"base_marker_run":  in.BaseMarker.Run,
			"base_marker_plan": in.BaseMarker.Plan,
			"base_marker_at":   in.BaseMarker.At,
		},
	}
}

// LeftoverBase returns the base index a leftover was a rebuild of: the same
// schema, the name with the _ccnew/_ccold suffix trimmed. ok is false when the
// name is not a leftover.
func LeftoverBase(leftover ObjectName) (ObjectName, bool) {
	kind, base := ClassifyLeftover(leftover.Name)
	if kind == LeftoverNone {
		return ObjectName{}, false
	}
	return NewObjectName(leftover.Schema, base), true
}
