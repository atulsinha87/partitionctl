package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/atulsinha/partitionctl/engine/executor"
	"github.com/atulsinha/partitionctl/engine/protocol"
	"github.com/atulsinha/partitionctl/engine/state"
)

// ErrRunIncomplete closes a command whose run stopped without finishing:
// cancelled at a node boundary, or halted with work left. It is exit 1, the
// generic failure: the run is resumable, nothing is broken, and CI still needs
// to know the job did not finish.
var ErrRunIncomplete = &protocol.Error{
	Kind: protocol.KindGeneric,
	Code: protocol.ExitFailure,
	Msg:  "the run did not complete",
}

// ErrPriorRunIncomplete is FR-CLI-9 and AC-23: `execute` refuses a plan with an
// incomplete or orphaned prior run, issues no DDL, and names `resume`.
//
// The two commands are deliberately separate (TRD §7.2.12). `resume` is where
// provenance-authorized cleanup happens, and it drops INVALID indexes left by a
// previous process. Doing that silently under `execute` would mean a routine
// re-run can destroy catalog objects, so continuing after an interruption is a
// decision rather than a default.
var ErrPriorRunIncomplete = &protocol.Error{
	Kind: protocol.KindGeneric,
	Code: protocol.ExitFailure,
	Msg:  "this plan has an incomplete prior run",
}

// cmdExecute implements `execute <plan>` (FR-CLI-3).
//
// The order of the checks is the contract: digest, then topology fingerprint,
// then the advisory lock, then the graph. Each one is cheaper than the next and
// each refuses before anything irreversible happens.
func (a *App) cmdExecute(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("execute", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	setFlags := globalFlags(fs)

	dryRun := fs.Bool("dry-run", false,
		"live pre-flight: verify the digest, recompute the fingerprint, test the advisory lock, "+
			"then print the dispatch sequence without issuing DDL (FR-CLI-5)")
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

	// FR-PLANFILE-3, AC-2: the digest is verified before anything else looks at
	// the plan.
	plan, err := loadPlan(planPath)
	if err != nil {
		return err
	}
	if err := plan.Validate(); err != nil {
		return err
	}

	// FR-CLI-9, before the database is even opened: `resume` is the only
	// command permitted to perform provenance-authorized cleanup.
	if err := refuseProvenanceCleanup(plan, planPath); err != nil {
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

	// FR-CLI-9 / AC-23, before the lock and before any DDL.
	prior, err := priorRuns(ctx, store, plan.Digest)
	if err != nil {
		return err
	}
	if msg, refuse := prior.refuseExecute(); refuse {
		return ErrPriorRunIncomplete.Detailf("%s", msg)
	}
	if run, done := prior.completed(); done {
		fmt.Fprintf(a.Stdout,
			"already complete: run %s finished at %s against this plan; nothing to do (AC-7)\n",
			run.RunID, runFinished(run))
		return nil
	}

	tgt, err := a.openTarget(ctx, cfg, db)
	if err != nil {
		return err
	}
	defer tgt.close()

	// T8, before the lock: the lock key, the provenance scope and every run
	// record are keyed on the plan's database name, so that name must be proven
	// to be this connection's before any of them is used.
	if err := a.checkDatabaseIdentity(ctx, tgt, plan); err != nil {
		return err
	}

	// FR-LOCK-1: the lock is taken before any node runs, and before the
	// fingerprint is recomputed, so the tree cannot change underneath the check
	// that just approved it.
	lock, err := store.TryLock(ctx, lockKeyFor(plan))
	if err != nil {
		return err
	}
	defer func() { _ = lock.Unlock(context.WithoutCancel(ctx)) }()

	// FR-PLANFILE-5, AC-3.
	if err := a.checkTopology(ctx, tgt, plan, *allowDrift); err != nil {
		return err
	}

	if *dryRun {
		// The pre-flight above is the whole difference from `render`: the
		// digest, the fingerprint and the lock have all been tested against the
		// live database. What follows issues nothing (FR-CLI-5).
		return a.printDispatch(ctx, cfg, tgt, plan)
	}

	now := a.Now()
	run, err := store.CreateRun(ctx, state.NewRun{
		Plan:      plan,
		RunID:     state.NewRunID(now),
		Actor:     cfg.Actor,
		StartedAt: now,
	})
	if err != nil {
		return err
	}
	return a.walk(ctx, cfg, tgt, store, lock, run, plan)
}

// refuseProvenanceCleanup enforces FR-CLI-9's second sentence: `resume` is the
// only command permitted to perform provenance-authorized cleanup.
//
// # Why the prior-run check is not enough
//
// [priorRunSet.refuseExecute] keys on *this plan's digest*, and a re-plan
// defeats it: CreatedAt alone gives a fresh artifact a new digest, so no prior
// run matches. The sequence that gets through is the ordinary one after any
// interruption. Execute is SIGKILLed mid-build, leaving an INVALID child index
// with a committed provenance record. The operator re-plans, which is the
// documented way to pick up live catalog state, and per FR-PLAN-6 the new plan
// now contains an `index.drop_concurrently` node authorized by that provenance.
// `execute` on the new artifact finds no prior run for the new digest and
// dispatches the drop.
//
// That is the outcome TRD §7.2.12 says the execute/resume split exists to
// prevent: "a routine re-run can destroy catalog objects". A CI pipeline that
// runs `plan` then `execute` unattended would silently destroy catalog objects
// after any interrupted run.
//
// The check is a scan of the artifact rather than a runtime gate, so it refuses
// before the database is opened, before the advisory lock, before a run record
// exists and before any DDL.
func refuseProvenanceCleanup(plan *protocol.Plan, planPath string) error {
	var offenders []string
	for i := range plan.Nodes {
		n := &plan.Nodes[i]
		if !n.Kind.IsDestructive() || n.Authorization == nil {
			continue
		}
		if n.Authorization.Mode != protocol.AuthProvenance {
			continue
		}
		offenders = append(offenders, fmt.Sprintf("  %s  %s  %s", n.ID, n.Kind, n.Authorization.Object))
	}
	if len(offenders) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "this plan contains %d provenance-authorized destructive node(s):\n", len(offenders))
	for _, o := range offenders {
		b.WriteString(o)
		b.WriteString("\n")
	}
	b.WriteString("no DDL was issued. Continue with:\n")
	fmt.Fprintf(&b, "  partitionctl resume %s\n", planPath)
	b.WriteString("`resume` is the only command permitted to perform provenance-authorized cleanup " +
		"(FR-CLI-9). These nodes drop INVALID indexes left behind by a previous process, so " +
		"performing them under `execute` would mean a routine re-run can destroy catalog objects")
	return ErrPriorRunIncomplete.Detailf("%s", b.String())
}

// ---------------------------------------------------------------------------
// The shared walk, used by execute and resume
// ---------------------------------------------------------------------------

// walk takes the lease, runs the graph, and records the run's terminal status.
//
// It is shared by `execute` and `resume` on purpose: the two differ in how they
// reach a run, never in how the run is executed. If they diverged here, AC-4's
// "resume converges to the same final catalog state as an uninterrupted run"
// would rest on two code paths staying accidentally identical.
func (a *App) walk(
	ctx context.Context,
	cfg Config,
	tgt *Target,
	store state.StateStore,
	lock state.Lock,
	run state.Run,
	plan *protocol.Plan,
) error {
	holder := state.DefaultHolder()
	if _, err := store.AcquireLease(ctx, run.RunID, holder, cfg.LeaseTTL); err != nil {
		return err
	}
	// Releasing runs under an uncancellable context: a signal must not stop the
	// executor from recording where it stopped.
	defer func() { _ = store.ReleaseLease(context.WithoutCancel(ctx), run.RunID, holder) }()

	// FR-EXEC-8: SIGINT and SIGTERM stop the scheduling of new nodes. They never
	// interrupt an in-flight statement, because a half-killed CREATE INDEX
	// CONCURRENTLY is the exact state this tool exists to avoid.
	sigCtx, stopSignals := a.signals(ctx)
	defer stopSignals()

	bridge := newExecutorStore(store, plan.Target.Database)
	exec, err := executor.New(executor.Config{
		Store:             bridge,
		SQL:               tgt.SQL,
		Catalog:           a.evaluator(tgt, store, plan),
		Logger:            a.log,
		Retry:             retryPolicyFrom(cfg),
		LockTimeout:       cfg.LockTimeout,
		BuildLockTimeout:  cfg.BuildLockTimeout,
		StatementTimeout:  cfg.StatementTimeout,
		Heartbeat:         &leaseHeartbeat{store: store, lock: lock, holder: holder},
		HeartbeatInterval: cfg.HeartbeatInterval,
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(a.Stdout, "run %s: %d node(s), estimate %s\n",
		run.RunID, len(plan.Nodes), humanSeconds(plan.TotalEstimatedSeconds()))

	res, runErr := exec.Run(sigCtx, executor.RunID(run.RunID), plan)
	return a.finish(ctx, store, run, res, runErr)
}

// finish records the run's terminal status and maps the outcome to an exit code.
//
// The status write runs under an uncancellable context. A run whose final state
// was never recorded is a run `status` cannot describe and `resume` cannot
// adopt, which is exactly the unrecoverable intermediate NFR-REL-2 forbids.
func (a *App) finish(ctx context.Context, store state.StateStore, run state.Run, res *executor.Result, runErr error) error {
	bg := context.WithoutCancel(ctx)

	set := func(to state.RunStatus, msg string) {
		if _, err := store.SetRunStatus(bg, state.RunStatusUpdate{
			RunID: run.RunID,
			From:  state.RunRunning,
			To:    to,
			Error: msg,
			At:    a.Now(),
		}); err != nil {
			fmt.Fprintf(a.Stderr, "warning: could not record run %s as %s: %v\n", run.RunID, to, err)
		}
	}

	switch {
	case runErr != nil:
		set(state.RunFailed, runErr.Error())
		a.reportResult(run, res)
		if res != nil && res.HaltedAt != "" {
			return haltedAt(runErr, res.HaltedAt)
		}
		return runErr

	case res != nil && res.Cancelled:
		set(state.RunInterrupted, "stopped at a node boundary: "+res.CancelReason)
		a.reportResult(run, res)
		fmt.Fprintf(a.Stdout, "stopped at a node boundary (%s); the run is resumable (AC-24)\n", res.CancelReason)
		return ErrRunIncomplete.Detailf(
			"run %s stopped at a node boundary with %d node(s) remaining; continue with `partitionctl resume`",
			run.RunID, res.Remaining)

	case res != nil && res.Complete():
		set(state.RunCompleted, "")
		a.reportResult(run, res)
		fmt.Fprintf(a.Stdout, "run %s complete\n", run.RunID)
		return nil
	}

	set(state.RunFailed, "the run ended with work remaining and no error")
	a.reportResult(run, res)
	return ErrRunIncomplete.Detailf("run %s ended with work remaining", run.RunID)
}

// haltedAt names the node a run stopped at without changing the run error's
// failure class (FR-CLI-13, AC-26).
//
// Re-typing the error here would be a silent contract break. [protocol.Error.Wrap]
// preserves the *receiver's* Kind and Code, and [protocol.ExitCodeFor] resolves
// the outermost [protocol.ExitCoder], so wrapping a node failure in ErrFailure
// turns every one of exit 13, 14, 15 and 16 into exit 1. Codes 13 and 14 can
// only arise during the walk, so that would make them unreachable by any CI
// consumer.
//
// A run error that carries no class of its own still gets the generic wrapper,
// because "the run stopped here" is worth saying either way.
func haltedAt(runErr error, node protocol.NodeID) error {
	var typed *protocol.Error
	if errors.As(runErr, &typed) {
		return typed.Detailf("halted at node %q", node)
	}
	return protocol.ErrFailure.Detailf("halted at node %q", node).Wrap(runErr)
}

func (a *App) reportResult(run state.Run, res *executor.Result) {
	if res == nil {
		return
	}
	fmt.Fprintf(a.Stdout, "  run %s: %d done, %d skipped, %d failed, %d remaining (of %d)\n",
		run.RunID, res.Done, res.Skipped, res.Failed, res.Remaining, res.Total)
}

// evaluator builds the executor's catalog port: assertions over the planner's
// reader, verifications over the verifier (TRD §7.2.7).
func (a *App) evaluator(tgt *Target, store state.StateStore, plan *protocol.Plan) executor.CatalogEvaluator {
	ev := &catalogEvaluator{}
	if tgt.Catalog != nil {
		ev.assert = newAssertEvaluator(tgt.Catalog, provenanceLookup{
			store:    store,
			database: plan.Target.Database,
		})
	}
	if tgt.Verify != nil {
		ev.verify = verifierFor(tgt)
	}
	return ev
}

// ---------------------------------------------------------------------------
// Prior runs
// ---------------------------------------------------------------------------

// priorRunSet is what the state store knows about a plan digest (INV-6).
type priorRunSet struct {
	incomplete []state.Run
	done       []state.Run
	cancelled  []state.Run
}

func priorRuns(ctx context.Context, store state.RunReader, digest string) (priorRunSet, error) {
	runs, err := store.FindRuns(ctx, state.RunQuery{PlanDigest: digest})
	if err != nil {
		return priorRunSet{}, err
	}
	var set priorRunSet
	for _, r := range runs {
		switch {
		case r.Status.IsIncomplete():
			set.incomplete = append(set.incomplete, r)
		case r.Status == state.RunCompleted:
			set.done = append(set.done, r)
		case r.Status == state.RunCancelled:
			set.cancelled = append(set.cancelled, r)
		}
	}
	return set, nil
}

// refuseExecute reports whether `execute` must stop, and why (FR-CLI-9, AC-23).
func (s priorRunSet) refuseExecute() (string, bool) {
	if len(s.incomplete) > 0 {
		var b strings.Builder
		b.WriteString("this plan has ")
		b.WriteString(strconv.Itoa(len(s.incomplete)))
		b.WriteString(" prior run(s) that left work undone:\n")
		for _, r := range s.incomplete {
			fmt.Fprintf(&b, "  %s  %s  started %s\n", r.RunID, r.Status, r.StartedAt)
		}
		b.WriteString("no DDL was issued. Continue with:\n")
		b.WriteString("  partitionctl resume <plan>\n")
		b.WriteString("`resume` is the only command permitted to perform provenance-authorized cleanup, " +
			"so continuing after an interruption is a decision rather than a default (FR-CLI-9)")
		return b.String(), true
	}
	if len(s.cancelled) > 0 && len(s.done) == 0 {
		var b strings.Builder
		b.WriteString("this plan's run was terminally cancelled and will not be adopted (FR-CLI-11):\n")
		for _, r := range s.cancelled {
			fmt.Fprintf(&b, "  %s  cancelled by %s: %s\n", r.RunID, r.CancelActor, r.CancelNote)
		}
		b.WriteString("re-plan against the live catalog and execute the new artifact; " +
			"re-running this one would issue statements against objects that may already exist")
		return b.String(), true
	}
	return "", false
}

// completed returns a run that already finished this plan (AC-7).
func (s priorRunSet) completed() (state.Run, bool) {
	if len(s.done) == 0 {
		return state.Run{}, false
	}
	sort.Slice(s.done, func(i, j int) bool {
		return s.done[i].StartedAt.Time.Before(s.done[j].StartedAt.Time)
	})
	return s.done[len(s.done)-1], true
}

// resumable returns the newest run `resume` may adopt (FR-CLI-9, FR-CLI-11).
func (s priorRunSet) resumable() (state.Run, bool) {
	var best state.Run
	found := false
	for _, r := range s.incomplete {
		if !r.Status.IsResumable() {
			continue
		}
		if !found || r.StartedAt.Time.After(best.StartedAt.Time) {
			best, found = r, true
		}
	}
	return best, found
}

func runFinished(r state.Run) string {
	if r.FinishedAt != nil {
		return r.FinishedAt.String()
	}
	return r.UpdatedAt.String()
}

// ---------------------------------------------------------------------------
// --dry-run
// ---------------------------------------------------------------------------

// printDispatch prints the dispatch sequence without issuing DDL (FR-CLI-5).
//
// It walks the plan through the executor itself, in dry-run mode, rather than
// re-deriving the order here. That is what makes the output a statement about
// what the executor would do: the same topological walk, the same pre-flight
// refusal of a node kind this build cannot dispatch, and the same renderer,
// which re-renders from structured parameters and never echoes the plan's
// rendered_sql (FR-PLANFILE-7).
func (a *App) printDispatch(ctx context.Context, cfg Config, tgt *Target, plan *protocol.Plan) error {
	collector := &collectingLogger{}
	exec, err := executor.New(executor.Config{
		DryRun:      true,
		Logger:      collector,
		LockTimeout: cfg.LockTimeout,
		Retry:       retryPolicyFrom(cfg),
	})
	if err != nil {
		return err
	}
	res, err := exec.Run(ctx, executor.RunID("dry-run"), plan)
	if err != nil {
		return err
	}

	fmt.Fprintf(a.Stdout, "dry run: %d node(s) would be dispatched in this order.\n", res.Total)
	fmt.Fprintln(a.Stdout, "The digest, the topology fingerprint and the advisory lock have all been")
	fmt.Fprintln(a.Stdout, "checked against the live database. No DDL was issued.")
	fmt.Fprintln(a.Stdout)

	for i, ev := range collector.events {
		n, ok := plan.NodeByID(ev.NodeID)
		if !ok {
			continue
		}
		fmt.Fprintf(a.Stdout, "%4d. %-24s %s\n", i+1, n.Kind, n.ID)
		if lock := n.Kind.LockLevel(); lock != protocol.LockNone {
			lockTimeout := cfg.LockTimeout
			if n.Kind.WaitsForConcurrentTransactions() {
				lockTimeout = cfg.BuildLockTimeout
			}
			settings := "lock_timeout=" + lockTimeout.String()
			if n.Kind.AllowsStatementTimeout() && cfg.StatementTimeout > 0 {
				settings += ", statement_timeout=" + cfg.StatementTimeout.String()
			} else {
				settings += ", no finite statement_timeout"
			}
			if n.Kind.MustRunOutsideTransaction() {
				settings += ", outside any transaction block"
			}
			fmt.Fprintf(a.Stdout, "       lock %s; %s\n", lock, settings)
		}
		if n.Authorization != nil {
			fmt.Fprintf(a.Stdout, "       authorization %s on %s, re-evaluated at dispatch (FR-AUTH-5)\n",
				n.Authorization.Mode, n.Authorization.Object)
		}
		if ev.Message != "" {
			for _, line := range strings.Split(strings.TrimRight(ev.Message, "\n"), "\n") {
				fmt.Fprintf(a.Stdout, "       %s\n", line)
			}
		}
	}
	fmt.Fprintf(a.Stdout, "\nestimate %s\n", humanSeconds(plan.TotalEstimatedSeconds()))
	return nil
}

// collectingLogger captures the executor's dry-run records so the CLI can
// present them, rather than emitting them as log lines an operator has to read
// as JSON.
type collectingLogger struct{ events []executor.LogEvent }

func (l *collectingLogger) Log(ev executor.LogEvent) { l.events = append(l.events, ev) }
