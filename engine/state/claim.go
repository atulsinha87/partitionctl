package state

import (
	"context"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// ClaimReader is the slice of a store [ClaimsObject] needs. It is separated so
// the claim question can be asked of anything that can list runs and nodes,
// including a fake, without dragging in leases, locks or the audit trail.
type ClaimReader interface {
	FindRuns(ctx context.Context, q RunQuery) ([]Run, error)
	ListNodes(ctx context.Context, runID RunID) ([]NodeRecord, error)
}

// claimingNode reports whether a node record still claims its object: the
// statement may have run, may be running, or may be about to be re-attempted.
//
// READY and PENDING are included only when the node has been dispatched at
// least once, and the distinction is load-bearing in both directions.
//
// A node that has never dispatched claims nothing, even though its record
// already names the object. Otherwise a plan that merely *intends* to create
// public.orders_idx_p1 would authorize destroying whatever unmarked index is
// already sitting under that name, which is exactly the confusion AC-6 forbids.
//
// The attempt counter is what marks the boundary, and it marks it in the right
// place: the executor increments it on the READY -> RUNNING transition, which
// is checkpointed *before* the statement is sent, so the claim goes live one
// durable write ahead of any DDL. READY used to claim unconditionally, which
// made this paragraph false — a process killed in the PENDING -> READY -> RUNNING
// gap left a durably READY node with attempts = 0 that had issued nothing, and
// `resume` would then stamp its marker onto whatever unmarked index a DBA had
// since left at that name and drop it.
//
// A node that has dispatched and is back in PENDING is the crash window itself:
// orphan recovery is the one non-monotonic edge in D7 (RUNNING -> PENDING,
// INV-5), and it fires before the resume walk. Excluding it would drop the claim
// at precisely the moment `resume` needs to adopt the half-built object, and
// would make the answer depend on whether recovery had run yet.
//
// A node in DONE or SKIPPED has finished with the object and claims nothing.
//
// Only a kind that would make the object PartitionCTL's can claim it
// ([protocol.NodeKind.ClaimsOwnership]). A node record names the object its node
// acts on for every kind that acts on one, including the two destructive kinds,
// because the audit trail is unreadable without it. Counting those as claims
// would be circular: a plan node saying "drop X" would itself prove X is ours to
// drop.
func claimingNode(n NodeRecord) bool {
	if !n.Kind.ClaimsOwnership() {
		return false
	}
	switch n.State {
	case protocol.NodeRunning, protocol.NodeRetryWait, protocol.NodeVerifying:
		// Every one of these implies a dispatch happened.
		return true
	case protocol.NodeReady, protocol.NodePending:
		return n.Attempts > 0
	}
	return false
}

// claimingRunStatuses are the run statuses whose node records can still hold a
// claim. A COMPLETED run has every node in a complete state, so it would match
// nothing anyway; a CANCELLED run is one `resume` will not adopt, so its claims
// are dead by decree (FR-CLI-11).
var claimingRunStatuses = []RunStatus{RunRunning, RunOrphaned, RunFailed, RunInterrupted}

// ClaimsObject reports whether some run holds a live claim on object: a node
// record naming it, in a state that still claims it ([claimingNode]), in a run
// whose status is RUNNING, ORPHANED, FAILED or INTERRUPTED.
//
// # What a claim is for, and what it deliberately is not
//
// It covers exactly one window: the object exists because a statement ran, and
// the process died before the ownership marker could be written onto it. In
// that window the object is unmarked and nothing else can prove it is ours.
//
// A claim expires by state transition, not by time. When a run completes, every
// node is DONE, so no claim survives it. That is what makes this strictly
// stronger than the provenance record it replaced: a completed run leaves
// nothing behind that can authorize destroying a same-named index somebody else
// created afterwards (AC-6, NFR-REL-3). See TestCompletedRunLeavesNoClaim.
//
// It is unscoped by database. Prefer [ClaimsObjectIn] wherever the target
// database is known: a [FileStore] deliberately holds state for several
// targets, and an unscoped answer could adopt an index in one database on the
// strength of a run against another.
func ClaimsObject(ctx context.Context, s ClaimReader, object protocol.ObjectName) (RunID, bool, error) {
	return ClaimsObjectIn(ctx, s, "", object)
}

// ClaimsObjectIn is [ClaimsObject] narrowed to one target database. An empty
// database matches any, which is what [ClaimsObject] passes.
func ClaimsObjectIn(ctx context.Context, s ClaimReader, database string, object protocol.ObjectName) (RunID, bool, error) {
	if s == nil {
		return "", false, nil
	}
	if object.IsZero() {
		return "", false, nil
	}
	runs, err := s.FindRuns(ctx, RunQuery{Database: database, Statuses: claimingRunStatuses})
	if err != nil {
		return "", false, err
	}
	var (
		best  Run
		found bool
	)
	for _, r := range runs {
		nodes, err := s.ListNodes(ctx, r.RunID)
		if err != nil {
			return "", false, err
		}
		for _, n := range nodes {
			if !claimingNode(n) {
				continue
			}
			if n.Object.Schema != object.Schema || n.Object.Name != object.Name {
				continue
			}
			if !found || r.StartedAt.Time.After(best.StartedAt.Time) {
				best, found = r, true
			}
			break
		}
	}
	if !found {
		return "", false, nil
	}
	return best.RunID, true, nil
}
