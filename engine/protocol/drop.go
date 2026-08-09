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
//
// # KNOWN DEFECT (measured, tracked as issue #2)
//
// https://github.com/atulsinha87/partitionctl/issues/2 carries the measurements
// below and both candidate fixes.
//
// The trimmed name is only the real base when PostgreSQL did not have to
// truncate. It builds a leftover name with ChooseRelationName/makeObjectName,
// which truncates the BASE so that base + "_ccnew" fits in NAMEDATALEN-1 = 63
// bytes. Measured on PG 17.10 by forcing real REINDEX INDEX CONCURRENTLY
// cancellations:
//
//	base 57 B -> leftover <base>_ccnew   (63 B)  trim recovers the base
//	base 58 B -> leftover <56 B>_ccnew1  (63 B)  trim recovers 56 B, WRONG
//	base 63 B -> leftover <57 B>_ccnew   (63 B)  trim recovers 57 B, WRONG
//
// [ChildIndexName] emits names of exactly 63 bytes whenever parentIndex + "_" +
// partition exceeds 63, so this tool's own generator produces the exposed case.
// Three consequences, in increasing severity:
//
//  1. authorizeLeftover halts the whole plan reporting that a base index "does
//     not exist" under a name the tool truncated itself. The reindex operation
//     is then permanently non-convergent for that tree: nothing in the tool
//     clears the leftover (resume's cleanup and drop-index's findOrphans both
//     iterate generated child names only), while verify --end-state's
//     CheckNoLeftoverIndexes does see it and fails. Only a hand-run DROP INDEX
//     CONCURRENTLY breaks the loop.
//  2. A truncated base makes `base != child.Name` in the reindex planner always
//     true, so a leaf whose rebuild DID succeed is rebuilt again -- hours on a
//     large leaf.
//  3. The truncated name can collide with a DIFFERENT real index in the same
//     schema. [DecideLeftoverDrop] would then read that unrelated index's
//     marker and, if it carries one, return DropAuthorized: an ownership proof
//     taken from the wrong object, which is the forgery INV-7/AC-19 exist to
//     prevent.
//
// # Why it is not fixed here
//
// The resolution has to be "the trimmed stem is a prefix of exactly one index
// on the same relation", which needs the candidate set. The planner has it
// (operations/reindex-index/planner.go holds every index on the leaf), but the
// executor re-derives the base at dispatch through
// [executor.CatalogEvaluator.Marker], which takes one name and cannot list. So
// a planner-only fix moves the halt from plan time to dispatch time without
// restoring convergence, and leaves consequence 3 -- the dangerous one -- fully
// open on the executor path.
//
// Closing it properly requires one of two owner decisions:
//
//   - add an index-listing method to executor.CatalogEvaluator (an internal
//     port: two implementations, one new catalog query), or
//   - carry the resolved base on protocol.Authorization so the executor does
//     not have to re-derive it (additive, but it is a plan-schema change and
//     plan artifacts are committed and reviewed).
//
// Neither is an integration repair, so neither was taken unilaterally.
func LeftoverBase(leftover ObjectName) (ObjectName, bool) {
	kind, base := ClassifyLeftover(leftover.Name)
	if kind == LeftoverNone {
		return ObjectName{}, false
	}
	return NewObjectName(leftover.Schema, base), true
}
