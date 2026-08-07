package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/atulsinha/partitionctl/engine/executor"
	"github.com/atulsinha/partitionctl/engine/planner"
	"github.com/atulsinha/partitionctl/engine/protocol"
	"github.com/atulsinha/partitionctl/engine/state"
)

// cmdResume implements `resume <plan>` (FR-CLI-9, TRD §7.3.2 diagram D3b).
//
// The sequence is the diagram's:
//
//  1. verify the plan's digest, so the artifact still describes the run;
//  2. take the advisory lock, which is the proof that no other process holds
//     this target and is what makes orphan detection sound (INV-4);
//  3. mark every abandoned run for this target ORPHANED (FR-LOCK-4);
//  4. adopt the run bound to this plan's digest, refusing one that was
//     terminally cancelled (FR-CLI-11);
//  5. re-read the catalog and perform provenance-authorized cleanup;
//  6. continue the remaining nodes.
//
// Step 5 is why `execute` and `resume` are separate commands. It drops INVALID
// indexes left behind by a dead process, and it is the only place in the CLI
// that does. Reaching it must be an operator's decision (TRD §7.2.12).
func (a *App) cmdResume(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	setFlags := globalFlags(fs)

	allowDrift := fs.Bool("allow-drift", false,
		"proceed even though the catalog has changed since planning (FR-PLANFILE-5)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	planPath, err := requirePositional(fs, "<plan>")
	if err != nil {
		return err
	}
	cfg, err := a.config(setFlags())
	if err != nil {
		return err
	}

	plan, err := loadPlan(planPath)
	if err != nil {
		return err
	}
	if err := plan.Validate(); err != nil {
		return err
	}

	db, err := a.openDB(ctx, cfg)
	if err != nil {
		return err
	}
	if db != nil {
		defer func() { _ = db.Close() }()
	}
	store, err := a.openStore(ctx, cfg, db)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	tgt, err := a.openTarget(ctx, cfg, db)
	if err != nil {
		return err
	}
	defer tgt.close()

	// T8, before the lock and before the provenance-gated cleanup: resume is
	// the command that issues destructive statements, so binding the plan's
	// claimed database to the live connection matters most here.
	if err := a.checkDatabaseIdentity(ctx, tgt, plan); err != nil {
		return err
	}

	lock, err := store.TryLock(ctx, lockKeyFor(plan))
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock(context.WithoutCancel(ctx)) }()

	// INV-4 and FR-LOCK-4. Holding the lock is what proves that a RUNNING run
	// with an expired lease is abandoned rather than merely slow, which is why
	// [state.RecoverOrphans] takes the lock as an argument rather than trusting
	// the caller to have one.
	orphaned, err := state.RecoverOrphans(ctx, store, lock, a.Now())
	if err != nil {
		return err
	}
	for _, r := range orphaned {
		fmt.Fprintf(a.Stdout, "marked run %s ORPHANED: its lease expired and its advisory lock was unheld (INV-4)\n", r.RunID)
	}

	prior, err := priorRuns(ctx, store, plan.Digest)
	if err != nil {
		return err
	}
	target, ok := prior.resumable()
	if !ok {
		if run, done := prior.completed(); done {
			fmt.Fprintf(a.Stdout,
				"nothing to resume: run %s already completed this plan at %s (AC-7)\n", run.RunID, runFinished(run))
			return nil
		}
		if len(prior.cancelled) > 0 {
			return protocol.ErrFailure.Detailf(
				"run %s was terminally cancelled and will not be adopted (FR-CLI-11); "+
					"re-plan against the live catalog", prior.cancelled[0].RunID)
		}
		return protocol.ErrFailure.Detailf(
			"no run of this plan exists to resume; start one with `partitionctl execute %s`", planPath)
	}

	// FR-PLANFILE-5. Drift is checked before the cleanup, because a tree that
	// has changed shape is a tree whose INVALID indexes may no longer mean what
	// the plan assumed.
	if err := a.checkTopology(ctx, tgt, plan, *allowDrift); err != nil {
		return err
	}

	run, err := state.AdoptOrphan(ctx, store, lock, target.RunID,
		state.DefaultHolder(), cfg.Actor, cfg.LeaseTTL, a.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "adopted run %s (was %s), bound to plan digest %s (INV-6)\n",
		run.RunID, target.Status, run.PlanDigest)

	if err := a.cleanup(ctx, cfg, tgt, store, run, plan); err != nil {
		// The cleanup halted. The run stays RUNNING and adopted, which is
		// wrong: it must be recorded as failed so `status` describes it and a
		// later resume can adopt it again.
		_, _ = store.SetRunStatus(context.WithoutCancel(ctx), state.RunStatusUpdate{
			RunID: run.RunID, From: state.RunRunning, To: state.RunFailed,
			Error: err.Error(), At: a.Now(),
		})
		return err
	}

	return a.walk(ctx, cfg, tgt, store, lock, run, plan)
}

// cleanup performs the provenance-authorized removal of half-built indexes,
// which is the step FR-CLI-9 reserves to `resume`.
//
// # What it removes, and what it refuses to
//
// An interrupted CREATE INDEX CONCURRENTLY leaves an index with
// indisvalid = false. Rebuilding over it is impossible, because the name is
// taken, so it must go first. Whether it may go is decided entirely by
// provenance: a committed record proving PartitionCTL created it (FR-PLAN-6,
// AC-5), or no drop at all and a halt (FR-PLAN-7, AC-6, NFR-REL-3). An INVALID
// index this tool did not create belongs to somebody else's in-flight build, and
// dropping it would be the single most destructive thing this program could do.
//
// # Why the cleanup is not a plan node
//
// The plan is bound to the run for the run's lifetime (INV-6), so a node that
// did not exist when the plan was sealed cannot be added to it. More
// importantly it should not be: the wreckage a resume finds is a property of how
// the previous process died, not of what the operator approved. Recording the
// authorization through [state.AuthorizationStore.RecordAuthorization], whose
// signature takes the destructive statement as a callback, is what makes INV-2
// structural here: the statement cannot run before its justification is
// committed, because the statement is an argument to the write.
func (a *App) cleanup(
	ctx context.Context,
	cfg Config,
	tgt *Target,
	store state.StateStore,
	run state.Run,
	plan *protocol.Plan,
) error {
	if plan.Target.Index == nil {
		return nil
	}
	if tgt.SQL == nil {
		return executor.ErrMissingPort.Detailf("resume needs a connection to the target to clean up")
	}

	read, release, err := tgt.snapshot(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = release() }()

	topo, err := planner.Discover(ctx, read, plan.Target.Table)
	if err != nil {
		return err
	}
	inspection, err := planner.InspectChildren(ctx, read, topo, *plan.Target.Index)
	if err != nil {
		return err
	}

	prov := provenanceLookup{store: store, database: plan.Target.Database}
	for _, child := range inspection.Children {
		if !child.Exists() || child.Condition.Usable() {
			continue
		}
		decision, err := planner.DecideCleanup(ctx, prov, child)
		if err != nil {
			return err
		}
		switch decision {
		case planner.CleanupNone:
			continue
		case planner.CleanupHalt:
			return planner.ErrForeignInvalidIndex.Detailf(
				"%s on %s is %s and PartitionCTL has no provenance record proving it created it. "+
					"Halting rather than dropping it: an index this tool did not create is never destroyed "+
					"(FR-PLAN-7, AC-6, NFR-REL-3). Resolve it by hand, then resume again",
				child.ChildIndex, child.Leaf.Name, child.Condition)
		case planner.CleanupDropWithProvenance:
			if err := a.dropWithProvenance(ctx, cfg, tgt, store, run, plan, child); err != nil {
				return err
			}
		}
	}
	return nil
}

// dropWithProvenance records the justification and only then issues the drop
// (INV-2, FR-AUTH-6, AC-20).
func (a *App) dropWithProvenance(
	ctx context.Context,
	cfg Config,
	tgt *Target,
	store state.StateStore,
	run state.Run,
	plan *protocol.Plan,
	child planner.ChildIndexPlan,
) error {
	object := child.ChildIndex
	relation := child.Leaf.Name

	has, provID, err := state.HasProvenance(ctx, store, state.ProvenanceQuery{
		Object:   object,
		Database: plan.Target.Database,
	})
	if err != nil {
		return err
	}
	if !has {
		// DecideCleanup already asked, so reaching here means the record
		// vanished between the two reads. Halt: an authorization that cites
		// nothing is not an authorization.
		return protocol.ErrAuthorizationUnsatisfied.Detailf(
			"no committed provenance record proves PartitionCTL created %s (FR-AUTH-2, AC-6)", object)
	}

	rel := relation
	nodeID := protocol.NodeID("resume.cleanup:" + object.String())
	rec := state.AuthorizationRecord{
		RunID:        run.RunID,
		NodeID:       nodeID,
		Mode:         protocol.AuthProvenance,
		Object:       object,
		Relation:     &rel,
		Database:     plan.Target.Database,
		ProvenanceID: provID,
		Evidence: map[string]string{
			"reason":        string(protocol.DropInvalidBuild),
			"condition":     string(child.Condition),
			"provenance_id": provID,
			"command":       "resume",
		},
		GrantedAt: protocol.NewTimestamp(a.Now()),
	}

	sqlText := "DROP INDEX CONCURRENTLY " + object.Quoted()
	fmt.Fprintf(a.Stdout, "cleanup: %s is %s with PartitionCTL provenance; dropping it before the rebuild (AC-5)\n",
		object, child.Condition)

	_, err = store.RecordAuthorization(ctx, rec, func(ctx context.Context) error {
		return tgt.SQL.Exec(ctx, executor.Statement{
			RunID:  executor.RunID(run.RunID),
			NodeID: nodeID,
			Kind:   protocol.KindIndexDropConcurrently,
			SQL:    sqlText,
			Settings: executor.SessionSettings{
				// lock_timeout always; no finite statement_timeout, because a
				// concurrent drop waits for every transaction that can see the
				// index and that wait is legitimately long (FR-EXEC-5).
				//
				// The build bound is the right one for the same reason: that
				// wait is taken through the lock manager, so the short bound
				// meant for lock queueing would abort the drop and leave the
				// index indislive = false.
				LockTimeout: cfg.BuildLockTimeout,
			},
			MustRunOutsideTransaction: true,
		})
	})
	if err != nil {
		return protocol.ErrFailure.Detailf("dropping %s: %v", object, err)
	}

	if _, err := store.AppendAudit(ctx, state.AuditEvent{
		RunID:  run.RunID,
		NodeID: nodeID,
		Type:   state.EventDestructiveExecuted,
		Detail: map[string]string{
			"object":   object.String(),
			"relation": relation.String(),
			"mode":     string(protocol.AuthProvenance),
			"sql":      sqlText,
		},
		At: a.Now(),
	}); err != nil {
		return err
	}
	return nil
}
