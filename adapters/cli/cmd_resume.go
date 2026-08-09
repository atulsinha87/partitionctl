package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/atulsinha87/partitionctl/engine/executor"
	"github.com/atulsinha87/partitionctl/engine/planner"
	"github.com/atulsinha87/partitionctl/engine/protocol"
	"github.com/atulsinha87/partitionctl/engine/state"
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

	return a.walk(ctx, cfg, tgt, store, lock, run, plan, true)
}

// cleanup performs the ownership-authorized removal of half-built indexes,
// which is the step FR-CLI-9 reserves to `resume`.
//
// # What it removes, and what it refuses to
//
// An interrupted CREATE INDEX CONCURRENTLY leaves an index with
// indisvalid = false. Rebuilding over it is impossible, because the name is
// taken, so it must go first. Whether it may go is decided by the shared
// destructive-action table ([protocol.DecideProvenanceDrop]): the PartitionCTL
// ownership marker on the object (FR-PLAN-6, AC-5), or, where the process died
// before the marker could be written, a live claim naming it — in which case the
// marker is written first, and only then the drop. Anything else halts
// (FR-PLAN-7, AC-6, NFR-REL-3). An INVALID index this tool did not create
// belongs to somebody else's in-flight build, and dropping it would be the
// single most destructive thing this program could do.
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

	// Decide everything under the snapshot, then close it, then act.
	//
	// The two phases must not overlap. DROP INDEX CONCURRENTLY calls
	// WaitForOlderSnapshots, so it waits for every snapshot older than itself to
	// end — including this command's own REPEATABLE READ catalog snapshot, which
	// cannot end until the drop it is blocking returns. Deciding and dropping
	// inside one snapshot therefore deadlocks the process against itself:
	// observed on PostgreSQL 17.10 as the tool's own connection sitting `idle in
	// transaction` on obj_description while its second connection waited on that
	// connection's virtualxid, ending in `canceling statement due to lock
	// timeout` after the full build bound. It is not the adopt path only —
	// the ordinary AC-5 case takes the identical call — and FR-CLI-9 reserves
	// this cleanup to `resume`, so the deadlock removed the tool's only route to
	// it entirely.
	//
	// Releasing early is only safe because every decision is re-evaluated
	// against live state immediately before its statement, by [App.recheck]
	// below (FR-AUTH-5). It is NOT re-evaluated by RecordAuthorization, which
	// validates the record's shape and writes it; its own doc says so
	// ([state.AuthorizationRecord.Validate]: "It cannot decide whether the mode
	// is genuinely satisfied against live state"). This comment used to claim
	// otherwise, and the gap it papered over is real and long: each DROP INDEX
	// CONCURRENTLY calls WaitForOlderSnapshots and blocks on every transaction
	// that can see the index, minutes at a time on a busy table, so the last
	// item's verdict could be hours old by the time it was acted on — while the
	// halt message this command prints is telling the operator to go and fix
	// things by hand in exactly that window.
	type cleanupItem struct {
		child    planner.ChildIndexPlan
		decision planner.CleanupDecision
		verdict  protocol.DropVerdict
	}
	var todo []cleanupItem

	if err := func() error {
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

		claims := claimLookup{store: store, database: plan.Target.Database}
		for _, child := range inspection.Children {
			if !child.Exists() || child.Condition.Usable() {
				continue
			}
			decision, verdict, err := planner.DecideCleanup(ctx, read, claims, child)
			if err != nil {
				if decision == planner.CleanupHalt {
					return planner.ErrForeignInvalidIndex.Detailf(
						"%s on %s is %s: %s. Halting rather than dropping it: an index this tool cannot "+
							"prove it created is never destroyed (FR-PLAN-7, AC-6, NFR-REL-3). Resolve it "+
							"by hand, then resume again%s",
						child.ChildIndex, child.Leaf.Name, child.Condition, verdict.Reason,
						a.legacyProvenanceHint(cfg, plan, child.ChildIndex))
				}
				return err
			}
			if !decision.Destructive() {
				continue
			}
			todo = append(todo, cleanupItem{child: child, decision: decision, verdict: verdict})
		}
		return nil
	}(); err != nil {
		return err
	}

	// Snapshot closed. Now the DDL, each item re-decided against live state
	// immediately before its own statement.
	claims := claimLookup{store: store, database: plan.Target.Database}
	for _, item := range todo {
		decision, verdict, err := a.recheck(ctx, tgt, claims, item.child, item.decision)
		if err != nil {
			return err
		}
		if err := a.dropOwnedIndex(ctx, cfg, tgt, store, run, plan, item.child, decision, verdict); err != nil {
			return err
		}
	}
	return nil
}

// recheck re-evaluates one cleanup verdict against live state, outside the
// snapshot the decision was taken under, immediately before the statement it
// authorizes (FR-AUTH-5, INV-2).
//
// It is the same shared decision table, re-read: the object's condition and its
// ownership marker come from a fresh, unpinned catalog read, and the claim is
// re-asked of the store. A verdict that is no longer destructive, or that has
// changed row — adopt-then-drop becoming marker-authorized, or either becoming
// a halt — aborts the cleanup rather than proceeding on the strength of the
// older answer.
//
// The window this closes is not theoretical. DROP INDEX CONCURRENTLY waits for
// every transaction that can see the index, so a 30-item cleanup takes minutes
// per item, and the tool's own halt message invites a DBA to work on the same
// tree meanwhile ("Resolve it by hand, then resume again"). Two things a DBA
// plausibly does in that window are fatal to a stale verdict: rebuilding a
// wrecked leaf index under the same name with a plain CREATE INDEX, which makes
// it healthy and unmarked; and writing their own COMMENT on it, which makes it
// MarkerForeign. Acting on the old verdict destroys the first and overwrites the
// second.
func (a *App) recheck(
	ctx context.Context,
	tgt *Target,
	claims claimLookup,
	child planner.ChildIndexPlan,
	was planner.CleanupDecision,
) (planner.CleanupDecision, protocol.DropVerdict, error) {
	if tgt.Catalog == nil {
		return planner.CleanupHalt, protocol.DropVerdict{}, executor.ErrMissingPort.Detailf(
			"resume cannot re-read %s before dropping it, and will not drop it on the strength of "+
				"a decision taken under a snapshot that is now closed (FR-AUTH-5)", child.ChildIndex)
	}

	indexes, err := tgt.Catalog.IndexesOnRelations(ctx, []uint32{child.Leaf.OID})
	if err != nil {
		return planner.CleanupHalt, protocol.DropVerdict{}, err
	}
	fresh := planner.ChildIndexPlan{
		Leaf:       child.Leaf,
		ChildIndex: child.ChildIndex,
		Condition:  planner.IndexAbsent,
	}
	for i := range indexes {
		if indexes[i].Name == child.ChildIndex {
			fresh.Existing = &indexes[i]
			fresh.Condition = indexes[i].Condition()
			break
		}
	}

	decision, verdict, err := planner.DecideCleanup(ctx, tgt.Catalog, claims, fresh)
	if err != nil {
		return decision, verdict, planner.ErrForeignInvalidIndex.Detailf(
			"%s changed under the cleanup: it was %s and %s when the snapshot was read, and re-reading "+
				"it now says %s. Halting rather than acting on the older answer (FR-AUTH-5, AC-6). "+
				"Resolve it by hand, then resume again",
			child.ChildIndex, child.Condition, was, verdict.Reason)
	}
	if decision != was {
		if !decision.Destructive() {
			return decision, verdict, protocol.ErrFailure.Detailf(
				"%s was %s and %s when the snapshot was read; re-reading it now says %s, so it is no "+
					"longer this run's to drop. Halting: re-run `partitionctl resume` to plan the cleanup "+
					"against what is there now",
				child.ChildIndex, child.Condition, was, decision)
		}
		// Still destructive, but by different evidence than was recorded. The
		// fresh verdict is what gets written to the audit trail, so say so
		// rather than passing the old one along silently.
		fmt.Fprintf(a.Stdout, "cleanup: %s is now authorized by %s rather than %s; re-read at dispatch (FR-AUTH-5)\n",
			child.ChildIndex, verdict.Evidence["source"], was)
	}
	return decision, verdict, nil
}

// dropOwnedIndex records the justification and only then issues the drop
// (INV-2, FR-AUTH-6, AC-20).
//
// Under [planner.CleanupAdoptThenDrop] the object carries no ownership marker
// and is ours only because a run that died still names it. The marker is
// written onto it first, so the drop that follows is auditable after the claim
// is gone. That write is the one adoption path in the tool, and this command is
// the only place it happens (FR-CLI-9).
func (a *App) dropOwnedIndex(
	ctx context.Context,
	cfg Config,
	tgt *Target,
	store state.StateStore,
	run state.Run,
	plan *protocol.Plan,
	child planner.ChildIndexPlan,
	decision planner.CleanupDecision,
	verdict protocol.DropVerdict,
) error {
	object := child.ChildIndex
	relation := child.Leaf.Name

	rel := relation
	nodeID := protocol.NodeID("resume.cleanup:" + object.String())
	evidence := map[string]string{"command": "resume"}
	for k, v := range verdict.Evidence {
		evidence[k] = v
	}
	evidence["reason"] = string(protocol.DropInvalidBuild)
	evidence["condition"] = string(child.Condition)

	rec := state.AuthorizationRecord{
		RunID:     run.RunID,
		NodeID:    nodeID,
		Mode:      protocol.AuthProvenance,
		Object:    object,
		Relation:  &rel,
		Database:  plan.Target.Database,
		Evidence:  evidence,
		GrantedAt: protocol.NewTimestamp(a.Now()),
	}

	sqlText := "DROP INDEX CONCURRENTLY " + object.Quoted()
	source := "carries a PartitionCTL ownership marker"
	if decision == planner.CleanupAdoptThenDrop {
		source = "is claimed by the interrupted run " + evidence["claim_run"] +
			" and is being adopted before it is dropped"
	}
	fmt.Fprintf(a.Stdout, "cleanup: %s is %s and %s; dropping it before the rebuild (AC-5)\n",
		object, child.Condition, source)

	_, err := store.RecordAuthorization(ctx, rec, func(ctx context.Context) error {
		if decision == planner.CleanupAdoptThenDrop {
			if err := a.adopt(ctx, cfg, tgt, run, plan, nodeID, object); err != nil {
				return err
			}
		}
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
			"source":   evidence["source"],
			"sql":      sqlText,
		},
		At: a.Now(),
	}); err != nil {
		return err
	}
	return nil
}

// adopt stamps the PartitionCTL ownership marker onto an object that has none,
// immediately before it is dropped. It is a catalog-only statement taking
// ShareUpdateExclusiveLock for about a millisecond (spike question 2).
func (a *App) adopt(
	ctx context.Context,
	cfg Config,
	tgt *Target,
	run state.Run,
	plan *protocol.Plan,
	nodeID protocol.NodeID,
	object protocol.ObjectName,
) error {
	text, err := protocol.FormatMarker(protocol.Marker{
		Run:  string(run.RunID),
		Plan: plan.Digest,
		Op:   string(plan.Operation),
		Role: protocol.MarkerRoleLeaf,
		At:   protocol.MarkerTime(a.Now()),
	})
	if err != nil {
		return err
	}
	return tgt.SQL.Exec(ctx, executor.Statement{
		RunID:    executor.RunID(run.RunID),
		NodeID:   nodeID,
		Kind:     protocol.KindIndexDropConcurrently,
		SQL:      protocol.RenderComment(object, text),
		Settings: executor.SessionSettings{LockTimeout: cfg.LockTimeout},
	})
}

// legacyProvenanceHint appends an explanation when the halt above is caused by
// the version-1 -> version-2 provenance change rather than by a genuinely
// foreign index.
//
// It returns "" whenever there is nothing to say, so the halt message is
// unchanged for every operator who never ran a version-1 binary.
//
// The sentence it adds is the difference between "go and fix this by hand" and
// "the tool changed how it proves ownership, here is the record it can see and
// no longer honours". The record is NOT treated as authorization: a side-table
// record outlives the object it names, so honouring one would authorize
// destroying a same-named index somebody else created later, which is the hole
// (AC-6, NFR-REL-3) that moving the marker onto the object was made to close.
func (a *App) legacyProvenanceHint(cfg Config, plan *protocol.Plan, object protocol.ObjectName) string {
	if cfg.State != StateFile {
		return ""
	}
	runID, ok := state.LegacyProvenanceRun(cfg.StateDir, plan.Target.Database, object)
	if !ok {
		return ""
	}
	return fmt.Sprintf(
		". NOTE: run %s holds a schema-version-1 provenance record naming this exact index, in %s. "+
			"This build no longer reads those records: ownership is proved by the PartitionCTL marker "+
			"on the object (COMMENT ON INDEX), or by a live claim from a node record naming it, and a "+
			"version-1 binary wrote neither. If that record is the proof you expected this command to "+
			"use, the index is almost certainly yours to drop -- but this build will not decide that "+
			"for you. Drop it by hand, then resume again",
		runID, cfg.StateDir)
}
