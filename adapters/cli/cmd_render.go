package cli

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/atulsinha/partitionctl/engine/executor"
	"github.com/atulsinha/partitionctl/engine/protocol"
)

// cmdRender implements `render <plan>` (FR-CLI-4).
//
// It is offline. It reads the plan file and emits SQL, connecting to nothing,
// which is the whole difference from `execute --dry-run`: one answers "what
// would run", the other answers "would it still run cleanly right now"
// (TRD §7.2.12).
//
// The SQL is re-rendered from each node's structured parameters through
// [executor.Render]. The plan's own rendered_sql field is never echoed: it is a
// non-authoritative human preview, and re-rendering is what keeps it off the
// injection surface (FR-PLANFILE-7, T2). A runbook that disagreed with what the
// executor would send would be worse than no runbook.
func (a *App) cmdRender(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)

	rollback := fs.Bool("rollback", false, "emit the unwind runbook instead (TRD §13.2)")
	confirm := fs.Bool("confirm-exclusive-lock", false,
		"acknowledge that the unwind's final statement takes an AccessExclusiveLock on the parent "+
			"and every leaf; without it that statement is emitted commented out (TRD §13.2.1)")
	out := fs.String("o", "", "write to this file instead of stdout")
	lockTimeout := fs.Duration("lock-timeout", executor.DefaultLockTimeout,
		"lock_timeout for the runbook's ordinary DDL statements")
	buildLockTimeout := fs.Duration("build-lock-timeout", executor.DefaultBuildLockTimeout,
		"lock_timeout for the CONCURRENTLY statements, which wait on concurrent transactions "+
			"throughout and not merely to acquire (FR-EXEC-5)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	planPath, err := requirePositional(fs, "<plan>")
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

	var buf bytes.Buffer
	if *rollback {
		err = renderRollback(&buf, plan, *confirm, *lockTimeout)
	} else {
		err = renderForward(&buf, plan, *lockTimeout, *buildLockTimeout)
	}
	if err != nil {
		return err
	}

	if *out == "" {
		_, err = a.Stdout.Write(buf.Bytes())
		return err
	}
	if err := writeFileAtomic(*out, buf.Bytes()); err != nil {
		return protocol.ErrFailure.Detailf("writing %s: %v", *out, err)
	}
	fmt.Fprintf(a.Stderr, "wrote %s\n", *out)
	return nil
}

// renderForward emits the runbook that reaches the plan's end state.
func renderForward(w *bytes.Buffer, plan *protocol.Plan, lockTimeout, buildLockTimeout time.Duration) error {
	order, err := plan.TopologicalOrder()
	if err != nil {
		return err
	}
	writeHeader(w, plan, "forward", lockTimeout)

	for i, id := range order {
		n, ok := plan.NodeByID(id)
		if !ok {
			return protocol.ErrInvalidPlan.Detailf("topological order names unknown node %q", id)
		}
		fmt.Fprintf(w, "\n-- [%d/%d] %s  %s\n", i+1, len(order), n.Kind, n.ID)
		if n.EstimatedSeconds > 0 {
			fmt.Fprintf(w, "--   estimate %s (FR-PLAN-9; advisory)\n", humanSeconds(n.EstimatedSeconds))
		}
		if lock := n.Kind.LockLevel(); lock != protocol.LockNone {
			fmt.Fprintf(w, "--   lock %s\n", lock)
		}
		if n.Kind.MustRunOutsideTransaction() {
			fmt.Fprintln(w, "--   MUST NOT run inside a transaction block; PostgreSQL rejects it (FR-EXEC-6)")
		}
		if !n.Kind.AllowsStatementTimeout() {
			fmt.Fprintln(w, "--   run with no finite statement_timeout: this legitimately takes hours (FR-EXEC-5)")
		}
		if n.Authorization != nil {
			fmt.Fprintf(w, "--   DESTRUCTIVE. Authorization mode %s on %s.\n",
				n.Authorization.Mode, n.Authorization.Object)
			fmt.Fprintln(w, "--   The engine re-evaluates this against live state before dispatch (FR-AUTH-5).")
			fmt.Fprintln(w, "--   Running it by hand skips that check. Satisfy yourself first.")
		}

		sql, err := executor.Render(n)
		if err != nil {
			return err
		}
		if sql == "" {
			fmt.Fprintf(w, "--   no statement: %s\n", describeNonDDL(n))
			continue
		}

		// A CONCURRENTLY statement needs the long lock_timeout, and it needs it
		// here rather than in the preamble. lock_timeout does not bound only the
		// initial acquisition: each of the wait-for-lockers phases *inside* the
		// statement is subject to it too, and each waits for every transaction
		// that touched the relation to finish. Under the preamble's 5s, one
		// six-second application transaction aborts a build that has already
		// scanned the whole partition, with 55P03, leaving a _ccnew behind.
		//
		// The executor learned this and switches on exactly this predicate
		// (engine/executor/executor.go, cfg.BuildLockTimeout). The runbook is
		// the documented offline path — the whole story for a shop that will not
		// run a Go binary against production — and it was still shipping the
		// short value over every REINDEX and CREATE INDEX CONCURRENTLY in the
		// file. Sharing the predicate is what stops the two drifting apart.
		concurrent := n.Kind.WaitsForConcurrentTransactions()
		if concurrent {
			fmt.Fprintf(w, "--   this statement waits on concurrent transactions throughout, not just to acquire:\n")
			fmt.Fprintf(w, "SET lock_timeout = %s;\n", sqlLiteral(pgDuration(buildLockTimeout)))
		}
		fmt.Fprintf(w, "%s;\n", sql)
		if concurrent {
			fmt.Fprintf(w, "SET lock_timeout = %s;  -- back to the ordinary value\n",
				sqlLiteral(pgDuration(lockTimeout)))
		}

		// TRD §14.2: the runbook must reach the same catalog state the engine
		// would. That includes the ownership marker, or an operator who ran the
		// runbook by hand would leave objects this tool can never prove are its
		// own, and every later plan would halt on them (FR-PLAN-7, AC-6).
		//
		// The marker records run "manual" because that is the truth: no run
		// executed it. The prior marker is unknown from here, so a rewrite kind
		// re-establishes rather than preserves; the runbook is a debugging and
		// audit aid, not a resume path.
		marker, ok, err := protocol.RenderMarkerStatement(n, manualMarker(plan), protocol.Marker{}, protocol.MarkerAbsent)
		if err != nil {
			return err
		}
		if ok {
			fmt.Fprintln(w, "--   ownership marker: what lets a later run prove this object is PartitionCTL's")
			fmt.Fprintf(w, "%s;\n", marker)
		}
	}

	fmt.Fprintln(w, "\n-- End of runbook.")
	fmt.Fprintln(w, "-- Verify the result with: partitionctl verify <plan>")
	return nil
}

// manualMarker is the ownership marker a hand-run runbook writes.
//
// run is the literal "manual", not a fabricated run id. A marker is evidence,
// and evidence that names a run which never existed is worse than evidence that
// says plainly it came from a human.
func manualMarker(plan *protocol.Plan) protocol.Marker {
	return protocol.Marker{
		Run:  "manual",
		Plan: plan.Digest,
		Op:   string(plan.Operation),
		At:   protocol.MarkerTime(plan.CreatedAt.Time),
	}
}

// renderRollback selects the unwind for the plan's operation, or refuses.
//
// The refusal is the feature. Before this dispatch existed there was one body,
// written for create-index, that every plan reached: `render --rollback` on a
// reindex plan emitted `DROP INDEX "public"."orders_created_at_idx";` as "the
// unwind", destroying a production index the run never created, and on a drop
// plan it emitted the same statement as the rollback of dropping that very
// index. Both then advised confirming success with `verify --expect-absent`.
func renderRollback(w *bytes.Buffer, plan *protocol.Plan, confirmed bool, lockTimeout time.Duration) error {
	entry, ok := operationRegistry[plan.Operation]
	if ok && entry.Rollback != nil {
		return entry.Rollback(w, plan, confirmed, lockTimeout)
	}
	if reason, known := rollbackUnsupported[plan.Operation]; known {
		return protocol.ErrFailure.Detailf(
			"%s has no unwind runbook: %s.\n"+
				"To see what this plan does, drop the --rollback flag", plan.Operation, reason)
	}
	return protocol.ErrFailure.Detailf(
		"this build has no unwind runbook for operation %q, and will not guess one: "+
			"an unwind emitted from the wrong operation's assumptions is a destructive statement "+
			"presented as a safety measure", plan.Operation)
}

// renderRollbackCreate emits CreatePartitionedIndex's unwind (TRD §13.2, §13.2.1).
//
// Unwinding a partial create is only partially online, and the reason is two
// PostgreSQL restrictions with no workaround: DROP INDEX CONCURRENTLY cannot be
// used on a partitioned index, and an attached child index cannot be dropped
// individually because there is no ALTER INDEX ... DETACH PARTITION. So the
// unwind splits in two, and the split is the whole content of this runbook:
//
//   - leaf indexes that were built but never attached come out online, with
//     DROP INDEX CONCURRENTLY at ShareUpdateExclusiveLock;
//   - the parent index, and with it every attached child by cascade, comes out
//     with one DROP INDEX that takes AccessExclusiveLock on the parent and every
//     leaf simultaneously.
//
// The second statement is gated. Without --confirm-exclusive-lock it is emitted
// commented out, because an operator should never reach an AccessExclusiveLock
// on their largest table by copying a block of text (TRD §13.2.1).
func renderRollbackCreate(w *bytes.Buffer, plan *protocol.Plan, confirmed bool, lockTimeout time.Duration) error {
	if plan.Target.Index == nil {
		return protocol.ErrFailure.Detailf("this plan's target names no index, so there is nothing to unwind")
	}
	parentIndex := *plan.Target.Index
	writeHeader(w, plan, "rollback", lockTimeout)

	fmt.Fprintln(w, "--")
	fmt.Fprintln(w, "-- Roll forward is the default and is fully online: `partitionctl resume <plan>`")
	fmt.Fprintln(w, "-- converges without ever taking an AccessExclusiveLock. Unwinding does not.")
	fmt.Fprintln(w, "-- Use this only when the build must vacate the cluster (TRD §13.2).")
	fmt.Fprintln(w, "--")

	// Phase 1: the leaf indexes this plan would have created. Each is dropped
	// online, and each is skipped by the operator if it is already attached.
	children := plannedChildIndexes(plan)
	fmt.Fprintf(w, "\n-- Phase 1 of 2: unattached leaf indexes (%d), online, ShareUpdateExclusiveLock.\n", len(children))
	fmt.Fprintln(w, "-- Each of these is skipped if the index is already attached to the parent:")
	fmt.Fprintln(w, "-- an attached child is a dependency of its partitioned parent and cannot be")
	fmt.Fprintln(w, "-- dropped individually. It is removed by the cascade in phase 2 instead.")
	fmt.Fprintln(w, "--")
	fmt.Fprintln(w, "-- Check attachment first:")
	fmt.Fprintf(w, "--   SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid\n")
	fmt.Fprintf(w, "--    WHERE i.inhparent = %s::regclass;\n", sqlLiteral(parentIndex.Quoted()))
	for _, child := range children {
		fmt.Fprintf(w, "DROP INDEX CONCURRENTLY IF EXISTS %s;\n", child.Quoted())
	}
	if len(children) == 0 {
		fmt.Fprintln(w, "-- (this plan created no leaf indexes)")
	}

	// Phase 2: the parent, and everything attached to it.
	fmt.Fprintln(w, "\n-- Phase 2 of 2: the parent index.")
	fmt.Fprintln(w, "-- DROP INDEX CONCURRENTLY is rejected on a partitioned index, so this is the")
	fmt.Fprintln(w, "-- only statement available, and it takes AccessExclusiveLock on the parent AND")
	fmt.Fprintln(w, "-- on every leaf partition simultaneously. It blocks reads and writes on the")
	fmt.Fprintln(w, "-- whole tree for as long as it takes to acquire, which means queueing behind")
	fmt.Fprintln(w, "-- every open transaction touching any leaf. No data is rewritten.")
	line := fmt.Sprintf("DROP INDEX %s;", parentIndex.Quoted())
	if confirmed {
		fmt.Fprintf(w, "%s\n", line)
	} else {
		fmt.Fprintln(w, "--")
		fmt.Fprintln(w, "-- Emitted commented out. Re-run with --confirm-exclusive-lock to emit it")
		fmt.Fprintln(w, "-- as a live statement (TRD §13.2.1).")
		fmt.Fprintf(w, "-- %s\n", line)
	}

	fmt.Fprintln(w, "\n-- Confirm the catalog is back to its pre-run state:")
	fmt.Fprintln(w, "--   partitionctl verify --expect-absent <plan>")
	return nil
}

// writeHeader emits the runbook preamble: what this is, what it came from, and
// the session settings every statement below assumes.
func writeHeader(w *bytes.Buffer, plan *protocol.Plan, kind string, lockTimeout time.Duration) {
	fmt.Fprintf(w, "-- partitionctl %s runbook\n", kind)
	fmt.Fprintf(w, "-- plan       %s\n", plan.PlanID)
	fmt.Fprintf(w, "-- operation  %s\n", plan.Operation)
	fmt.Fprintf(w, "-- target     %s", plan.Target.Table)
	if plan.Target.Index != nil {
		fmt.Fprintf(w, " index %s", plan.Target.Index)
	}
	if plan.Target.Database != "" {
		fmt.Fprintf(w, " in database %s", plan.Target.Database)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "-- digest     %s\n", plan.Digest)
	fmt.Fprintf(w, "-- planned    %s\n", plan.CreatedAt)
	fmt.Fprintln(w, "--")
	fmt.Fprintln(w, "-- Generated offline from the plan artifact. No database was contacted, so nothing")
	fmt.Fprintln(w, "-- here reflects the catalog's current state. For a live pre-flight, use:")
	fmt.Fprintln(w, "--   partitionctl execute --dry-run <plan>")
	fmt.Fprintln(w, "--")
	fmt.Fprintln(w, "-- Every statement below assumes this session preamble. lock_timeout is not")
	fmt.Fprintln(w, "-- optional: without it a statement queues behind a long transaction forever")
	fmt.Fprintln(w, "-- and blocks everything behind it (FR-EXEC-5). This is the ordinary value;")
	fmt.Fprintln(w, "-- the CONCURRENTLY statements raise it around themselves and restore it after.")
	fmt.Fprintf(w, "SET lock_timeout = %s;\n", sqlLiteral(pgDuration(lockTimeout)))
	fmt.Fprintln(w, "SET statement_timeout = 0;  -- index builds legitimately run for hours")
}

// pgDuration renders a duration as a PostgreSQL interval literal.
//
// time.Duration.String() is not one. It renders 15 minutes as "15m0s", which
// PostgreSQL rejects outright, and its minute unit is "m" where PostgreSQL's is
// "min" — so the runbook's own preamble would have failed to parse the moment
// anyone passed --lock-timeout 90s. Falling back to milliseconds keeps every
// value expressible, since ms is lock_timeout's own default unit.
func pgDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "0"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dmin", d/time.Minute)
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", int64(d/time.Second))
	default:
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
}

// plannedChildIndexes lists the leaf indexes the plan creates, in plan order.
func plannedChildIndexes(plan *protocol.Plan) []protocol.ObjectName {
	var out []protocol.ObjectName
	for i := range plan.Nodes {
		n := &plan.Nodes[i]
		if n.Kind != protocol.KindIndexCreateConcurrently {
			continue
		}
		p, ok := n.Params.(*protocol.CreateConcurrentlyParams)
		if !ok {
			continue
		}
		out = append(out, p.Index)
	}
	return out
}

// describeNonDDL explains a node that sends no statement.
func describeNonDDL(n *protocol.Node) string {
	switch n.Kind {
	case protocol.KindCatalogAssert:
		if p, ok := n.Params.(*protocol.CatalogAssertParams); ok {
			return fmt.Sprintf("%d catalog precondition(s), evaluated before anything runs", len(p.Assertions))
		}
		return "catalog preconditions"
	case protocol.KindIndexVerify:
		if p, ok := n.Params.(*protocol.VerifyParams); ok {
			return fmt.Sprintf("%d catalog assertion(s) over pg_index and pg_inherits", len(p.Checks))
		}
		return "catalog assertions"
	case protocol.KindWait:
		if p, ok := n.Params.(*protocol.WaitParams); ok {
			return fmt.Sprintf("pause %ds: %s", p.Seconds, p.Reason)
		}
		return "pause"
	}
	return string(n.Kind)
}

// sqlLiteral renders a single-quoted SQL string literal.
func sqlLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
