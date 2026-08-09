package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/atulsinha87/partitionctl/engine/protocol"
	"github.com/atulsinha87/partitionctl/engine/state"
)

// cmdCancel implements `cancel <run-id>` (FR-CLI-10, FR-CLI-11, AC-24).
//
// # Why it works through the state store rather than through signals
//
// The host model gives a run to a CLI process that another terminal cannot
// signal portably (TRD §7.2.12), so `cancel` sets a flag the executor polls at
// node boundaries. It never interrupts an in-flight statement, because a
// half-killed CREATE INDEX CONCURRENTLY is exactly the state this tool exists to
// avoid, and the run stays resumable to completion.
//
// # One command, two effects
//
// Against a live run the flag is set and the run stops at its next node
// boundary, remaining resumable (FR-CLI-10, AC-24). Against a run that is
// already abandoned — orphaned, failed, interrupted, or RUNNING with an expired
// lease — the same command terminally cancels it so that `resume` will not
// adopt it (FR-CLI-11). Which one happens is the state store's decision, taken
// from the run's status and its lease, and not this command's.
//
// It touches only the state store. No connection to the target is required or
// made.
func (a *App) cmdCancel(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	setFlags := globalFlags(fs)

	note := fs.String("note", "", "why, recorded in the audit trail")
	if err := fs.Parse(args); err != nil {
		return err
	}
	runID, err := requirePositional(fs, "<run-id>")
	if err != nil {
		return err
	}
	cfg, err := a.config(setFlags())
	if err != nil {
		return err
	}

	db, _ := a.optionalDB(ctx, cfg)
	if db != nil {
		defer func() { _ = db.Close() }()
	}
	store, err := a.openStore(ctx, cfg, db)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	before, err := store.GetRun(ctx, state.RunID(runID))
	if err != nil {
		if isNotFound(err) {
			// The operator has a run id that does not resolve, and the usual
			// cause is that this process is looking at a different state store
			// than the run was recorded in. Name the store, because "not found"
			// alone sends them looking for the wrong problem.
			return protocol.ErrFailure.Detailf(
				"no run %q in the %s state store%s; `partitionctl status` lists the runs it can see",
				runID, cfg.State, stateLocation(cfg))
		}
		return err
	}
	after, err := store.RequestCancel(ctx, state.RunID(runID), cfg.Actor, *note)
	if err != nil {
		return err
	}

	switch {
	case after.Status == state.RunCancelled && before.Status != state.RunCancelled:
		fmt.Fprintf(a.Stdout,
			"run %s was %s and is now terminally CANCELLED; `resume` will not adopt it (FR-CLI-11)\n",
			after.RunID, before.Status)
	case after.CancelRequested && after.Status == state.RunRunning:
		fmt.Fprintf(a.Stdout,
			"cancellation requested for run %s. The executor observes the flag at its next node\n"+
				"boundary and never mid-statement, so an in-flight index build finishes first\n"+
				"(FR-CLI-10, AC-24). The run stays resumable: partitionctl resume <plan>\n",
			after.RunID)
	case before.Status.IsTerminal():
		fmt.Fprintf(a.Stdout, "run %s is already %s; nothing to cancel\n", after.RunID, before.Status)
	default:
		fmt.Fprintf(a.Stdout, "run %s is %s; cancellation recorded\n", after.RunID, after.Status)
	}
	return nil
}
