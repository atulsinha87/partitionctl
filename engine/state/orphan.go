package state

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// FindOrphans implements the detection half of INV-4 and FR-LOCK-4: a run in
// RUNNING whose lease has expired and whose advisory lock is unheld is
// ORPHANED and resumable.
//
// The advisory-lock half is supplied structurally. lock must be a held [Lock]
// covering the same target, and the caller cannot produce one without having
// taken the lock, which is the order FR-LOCK-1 mandates anyway: `execute` and
// `resume` acquire it before any node runs. Holding it is what proves no other
// session holds it, so every RUNNING run for this target with an expired lease
// is by definition abandoned.
//
// It transitions nothing. [AdoptOrphan] does that, and separating the two keeps
// `status` able to report an orphan without changing it.
//
// Runs are returned oldest first.
func FindOrphans(ctx context.Context, s StateStore, lock Lock, now time.Time) ([]Run, error) {
	if lock == nil {
		return nil, protocol.ErrLockHeld.Detailf(
			"orphan detection requires the advisory lock to be held first (FR-LOCK-1, INV-4)")
	}
	key := lock.Key()
	if err := checkLock(ctx, lock, key); err != nil {
		return nil, err
	}

	candidates, err := s.FindRuns(ctx, RunQuery{
		Database: key.Database,
		Table:    &key.Table,
		Statuses: []RunStatus{RunRunning},
	})
	if err != nil {
		return nil, err
	}

	var orphans []Run
	for _, r := range candidates {
		lease, err := s.GetLease(ctx, r.RunID)
		switch {
		case errors.Is(err, ErrNotFound):
			// RUNNING with no lease at all. The process died between opening
			// the run and taking the lease, or the lease was released without
			// the status being settled. Either way nothing is alive.
			orphans = append(orphans, r)
			continue
		case err != nil:
			return nil, err
		}
		if lease.Expired(now) {
			orphans = append(orphans, r)
		}
	}
	sort.Slice(orphans, func(i, j int) bool {
		if orphans[i].StartedAt.Time.Equal(orphans[j].StartedAt.Time) {
			return orphans[i].RunID < orphans[j].RunID
		}
		return orphans[i].StartedAt.Time.Before(orphans[j].StartedAt.Time)
	})
	return orphans, nil
}

// MarkOrphaned moves a run from RUNNING to ORPHANED and rewinds the nodes that
// were in flight when the process died.
//
// The rewind is the RUNNING -> PENDING edge, which diagram D7 permits only
// under [protocol.ReasonOrphanRecovery] (INV-5). A node left in RUNNING would
// otherwise be undispatchable forever, because READY -> RUNNING is the only
// edge into it.
//
// It does not adopt the run. `resume` adopts by moving ORPHANED -> RUNNING
// through [AdoptOrphan], and `cancel` may instead terminally cancel it
// (FR-CLI-11), which is precisely the choice that must stay the operator's.
func MarkOrphaned(ctx context.Context, s StateStore, lock Lock, runID RunID, now time.Time) (Run, error) {
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	if err := checkLock(ctx, lock, run.LockKey()); err != nil {
		return Run{}, err
	}
	if run.Status != RunRunning {
		return Run{}, ErrConflict.Detailf(
			"run %s is %s, not %s; only a RUNNING run can be orphaned (INV-4)", runID, run.Status, RunRunning)
	}

	lease, err := s.GetLease(ctx, runID)
	switch {
	case errors.Is(err, ErrNotFound):
		// No lease: nothing is alive. Proceed.
	case err != nil:
		return Run{}, err
	default:
		if !lease.Expired(now) {
			return Run{}, ErrConflict.Detailf(
				"run %s holds a live lease until %s; it is not orphaned (INV-4)",
				runID, lease.ExpiresAt().UTC().Format(time.RFC3339))
		}
	}

	run, err = s.SetRunStatus(ctx, RunStatusUpdate{
		RunID: runID,
		From:  RunRunning,
		To:    RunOrphaned,
		At:    now,
	})
	if err != nil {
		return Run{}, err
	}

	nodes, err := s.ListNodes(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	rewound := 0
	for _, n := range nodes {
		if n.State != protocol.NodeRunning {
			continue
		}
		if _, err := s.TransitionNode(ctx, NodeTransition{
			RunID:  runID,
			NodeID: n.NodeID,
			From:   protocol.NodeRunning,
			To:     protocol.NodePending,
			Reason: protocol.ReasonOrphanRecovery,
			At:     now,
		}); err != nil {
			return Run{}, err
		}
		rewound++
	}

	if _, err := s.AppendAudit(ctx, AuditEvent{
		RunID: runID,
		Type:  EventRunOrphaned,
		At:    now,
		Detail: auditDetail(
			"reason", "lease expired, advisory lock unheld (INV-4)",
			"rewound_nodes", strconv.Itoa(rewound),
		),
	}); err != nil {
		return Run{}, err
	}
	return run, nil
}

// AdoptOrphan is `resume` taking over an abandoned run (FR-CLI-9, D3b).
//
// It moves ORPHANED, FAILED or INTERRUPTED back to RUNNING, takes the lease for
// the adopting holder, and records the adoption in the audit trail. A
// terminally cancelled run is refused, which is exactly what FR-CLI-11 buys the
// operator.
//
// Adoption keeps the same run and therefore the same plan digest (INV-6). It
// does not open a new run, because INV-5's RUNNING -> PENDING edge is scoped to
// a run's own node records and would be meaningless across two of them.
func AdoptOrphan(ctx context.Context, s StateStore, lock Lock, runID RunID, holder, actor string, ttl time.Duration, now time.Time) (Run, error) {
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	if err := checkLock(ctx, lock, run.LockKey()); err != nil {
		return Run{}, err
	}
	if run.Status == RunCancelled {
		return Run{}, ErrConflict.Detailf(
			"run %s was terminally cancelled and will not be adopted (FR-CLI-11)", runID)
	}
	if !run.Status.IsResumable() {
		return Run{}, ErrConflict.Detailf(
			"run %s is %s, which is not resumable", runID, run.Status)
	}

	// Adoption revokes any outstanding stop request. `cancel` sets a flag the
	// executor polls at every node boundary and nothing else clears it, so an
	// adopted run that kept the flag would stop again on its first boundary
	// with nothing dispatched, every time, and the run would advertise itself
	// as resumable while being impossible to resume (AC-24).
	adopted, err := s.SetRunStatus(ctx, RunStatusUpdate{
		RunID:       runID,
		From:        run.Status,
		To:          RunRunning,
		ClearCancel: true,
		At:          now,
	})
	if err != nil {
		return Run{}, err
	}
	if _, err := s.AcquireLease(ctx, runID, holder, ttl); err != nil {
		return Run{}, err
	}

	// Rewind the nodes that stopped the previous run, or the adopted run would
	// halt on the first of them with nothing dispatched.
	//
	// FAILED is terminal within a run, which is what stops a live executor
	// looping on a node that keeps failing. Across runs it cannot be, because
	// RunFailed is resumable and `execute` refuses a failed run and names
	// `resume`: if adoption could not rewind, neither command could make
	// progress. Resume's provenance-gated cleanup runs before the walk and
	// removes the INVALID index a failed build left behind, so the rebuilt node
	// starts from a clean leaf (FR-PLAN-6, AC-5).
	rewound, err := rewindFailedNodes(ctx, s, runID, now)
	if err != nil {
		return Run{}, err
	}

	if _, err := s.AppendAudit(ctx, AuditEvent{
		RunID: runID,
		Type:  EventRunAdopted,
		At:    now,
		Detail: auditDetail(
			"from_status", string(run.Status),
			"holder", holder,
			"actor", actor,
			"rewound_failed_nodes", strconv.Itoa(rewound),
		),
	}); err != nil {
		return Run{}, err
	}
	return adopted, nil
}

// rewindFailedNodes moves every FAILED node back to PENDING under
// [protocol.ReasonResumeRetry], and reports how many it moved.
func rewindFailedNodes(ctx context.Context, s StateStore, runID RunID, now time.Time) (int, error) {
	nodes, err := s.ListNodes(ctx, runID)
	if err != nil {
		return 0, err
	}
	rewound := 0
	for _, n := range nodes {
		if n.State != protocol.NodeFailed {
			continue
		}
		if _, err := s.TransitionNode(ctx, NodeTransition{
			RunID:  runID,
			NodeID: n.NodeID,
			From:   protocol.NodeFailed,
			To:     protocol.NodePending,
			Reason: protocol.ReasonResumeRetry,
			At:     now,
		}); err != nil {
			return 0, err
		}
		rewound++
	}
	return rewound, nil
}

// RecoverOrphans is the whole of INV-4 in one call, for the `resume` path: find
// every abandoned run for the locked target and mark it ORPHANED.
//
// It returns the runs it transitioned, oldest first. Adoption stays a separate,
// explicit step, because `cancel` must be able to reach an ORPHANED run before
// `resume` does (FR-CLI-11).
func RecoverOrphans(ctx context.Context, s StateStore, lock Lock, now time.Time) ([]Run, error) {
	orphans, err := FindOrphans(ctx, s, lock, now)
	if err != nil {
		return nil, err
	}
	out := make([]Run, 0, len(orphans))
	for _, o := range orphans {
		r, err := MarkOrphaned(ctx, s, lock, o.RunID, now)
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}

// IncompleteRunsForPlan finds runs bound to a plan digest that left work
// undone. It is the read behind FR-CLI-9 and AC-23: `execute` refuses a plan
// with an incomplete prior run, issues no DDL, and names `resume`.
func IncompleteRunsForPlan(ctx context.Context, s RunReader, digest string) ([]Run, error) {
	return s.FindRuns(ctx, RunQuery{
		PlanDigest: digest,
		Statuses:   []RunStatus{RunRunning, RunFailed, RunOrphaned, RunInterrupted},
	})
}
