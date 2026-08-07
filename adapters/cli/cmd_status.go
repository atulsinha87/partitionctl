package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"sort"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
	"github.com/atulsinha/partitionctl/engine/state"
)

// StatusReport is the stable field schema `status --json` emits (FR-CLI-15,
// NFR-OBS-2).
//
// It is deliberately flat and additive. A consumer reads run_id, status and the
// counts and never has to branch on which command produced the document.
type StatusReport struct {
	RunID      state.RunID        `json:"run_id"`
	PlanID     protocol.PlanID    `json:"plan_id"`
	PlanDigest string             `json:"plan_digest"`
	Operation  protocol.Operation `json:"operation"`
	Status     state.RunStatus    `json:"status"`
	Actor      string             `json:"actor,omitempty"`

	Database string `json:"database,omitempty"`
	Table    string `json:"table"`
	Index    string `json:"index,omitempty"`

	Total     int `json:"total"`
	Done      int `json:"done"`
	Skipped   int `json:"skipped"`
	Failed    int `json:"failed"`
	Remaining int `json:"remaining"`
	InFlight  int `json:"in_flight"`

	// CurrentNode is the node in RUNNING or VERIFYING, if any.
	CurrentNode protocol.NodeID   `json:"current_node,omitempty"`
	CurrentKind protocol.NodeKind `json:"current_kind,omitempty"`

	StartedAt  string `json:"started_at"`
	UpdatedAt  string `json:"updated_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	ElapsedMS  int64  `json:"elapsed_ms"`

	// ETASeconds is derived from the plan's per-node estimates (FR-PLAN-9,
	// FR-ORD-5). It is present only when the plan artifact was supplied, because
	// the estimates live in the plan and the state store records node identity
	// rather than node cost.
	ETASeconds *int `json:"eta_seconds,omitempty"`

	CancelRequested bool   `json:"cancel_requested,omitempty"`
	LastError       string `json:"last_error,omitempty"`

	// Resumable reports whether `resume` may adopt this run (FR-CLI-11).
	Resumable bool `json:"resumable"`

	// Notes carry anything the core output degraded on, most often the absence
	// of live enrichment because the target was unreachable (FR-CLI-12, AC-25).
	Notes []string `json:"notes,omitempty"`
}

// cmdStatus implements `status [<run-id>]` (FR-CLI-12, FR-ORD-5, AC-25).
//
// # It answers from the state store alone
//
// No part of the core output requires a connection to the target database.
// That is the whole point: an operator checks progress precisely when the target
// is unreachable or saturated, and a status command that needs the sick database
// to report on the sick database is useless at the only moment it matters.
//
// Live index-build progress from pg_stat_progress_create_index is an
// enrichment. It is never a precondition, and its absence degrades the report
// with a note rather than failing it.
//
// # AC-25 holds for --state file, and cannot hold for --state sql
//
// The guarantee is a property of where execution state lives, not of this
// command. With the file store the state is outside the target's blast radius,
// so an unreachable target costs only the enrichment. With the SQL store the
// state lives in a dedicated schema *inside* the target (FR-STATE-3), so a
// target that cannot be reached is also a state store that cannot be read, and
// no amount of degradation recovers a report from it.
//
// --state sql is the default, so an operator who wants `status` to survive an
// unreachable target has to choose the file store deliberately. What this
// command owes them in the meantime is the reason and the remedy rather than a
// bare connection error, which is what the store-selection failure says.
func (a *App) cmdStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	setFlags := globalFlags(fs)

	asJSON := fs.Bool("json", false, "emit the stable field schema of NFR-OBS-2 (FR-CLI-15)")
	planPath := fs.String("plan", "", "plan artifact, for the ETA from its per-node estimates (FR-ORD-5)")
	limit := fs.Int("limit", 10, "how many runs to list when no run id is given")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := a.config(setFlags())
	if err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) > 1 {
		return protocol.ErrFailure.Detailf("expected at most one <run-id>, got %d", len(rest))
	}

	// The database handle is optional here and must stay that way. A file state
	// store needs none at all, and a SQL store's failure is reported as the
	// store failure it is rather than as a connection failure.
	db, note := a.optionalDB(ctx, cfg)
	if db != nil {
		defer func() { _ = db.Close() }()
	}
	store, err := a.openStore(ctx, cfg, db)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// A degraded report says so in the document, not only on stderr. A consumer
	// reading --json is usually a script, and a script that cannot tell a
	// complete report from one produced with the target unreachable will treat
	// a missing enrichment as a missing fact (FR-CLI-12, FR-CLI-15, AC-25).
	var notes []string
	if note != "" {
		notes = append(notes, note)
	}

	var plan *protocol.Plan
	if *planPath != "" {
		p, perr := loadPlan(*planPath)
		if perr != nil {
			return perr
		}
		plan = p
	}

	query := state.RunQuery{Limit: *limit}
	switch {
	case len(rest) == 1:
		query = state.RunQuery{RunID: state.RunID(rest[0])}
	case plan != nil:
		query.PlanDigest = plan.Digest
	}

	runs, err := store.FindRuns(ctx, query)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		if *asJSON {
			return json.NewEncoder(a.Stdout).Encode([]StatusReport{})
		}
		fmt.Fprintln(a.Stdout, "no runs recorded")
		return nil
	}
	sort.Slice(runs, func(i, j int) bool {
		return runs[i].StartedAt.Time.After(runs[j].StartedAt.Time)
	})

	reports := make([]StatusReport, 0, len(runs))
	for _, run := range runs {
		nodes, nerr := store.ListNodes(ctx, run.RunID)
		if nerr != nil {
			return nerr
		}
		reports = append(reports, a.report(run, nodes, plan, notes))
	}

	if *asJSON {
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		if query.RunID != "" && len(reports) == 1 {
			return enc.Encode(reports[0])
		}
		return enc.Encode(reports)
	}
	for i, r := range reports {
		if i > 0 {
			fmt.Fprintln(a.Stdout)
		}
		a.printStatus(r)
	}
	return nil
}

// optionalDB opens the target handle if it can, and shrugs if it cannot.
// A failed connection here is not a failed `status`: the core output comes from
// the state store (FR-CLI-12, AC-25).
//
// The second return value is the note describing what the report lost by not
// having a connection, empty when nothing was lost.
func (a *App) optionalDB(ctx context.Context, cfg Config) (*sql.DB, string) {
	if cfg.State == StateFile {
		// A file store needs no database at all, so do not even try. This is
		// the configuration AC-25 is tested against.
		return nil, ""
	}
	db, err := a.openDB(ctx, cfg)
	if err != nil {
		fmt.Fprintf(a.Stderr,
			"warning: the target is unreachable (%v); reporting from the state store alone (FR-CLI-12)\n", err)
		return nil, fmt.Sprintf(
			"the target database is unreachable (%v); this report comes from the state store alone "+
				"and carries no live index-build progress (FR-CLI-12)", err)
	}
	return db, ""
}

// report projects a run and its node records into the JSON schema.
func (a *App) report(run state.Run, nodes []state.NodeRecord, plan *protocol.Plan, notes []string) StatusReport {
	counts := state.CountNodes(nodes)

	r := StatusReport{
		RunID:           run.RunID,
		PlanID:          run.PlanID,
		PlanDigest:      run.PlanDigest,
		Operation:       run.Operation,
		Status:          run.Status,
		Actor:           run.Actor,
		Database:        run.Target.Database,
		Table:           run.Target.Table.String(),
		Total:           counts.Total,
		Failed:          counts.Failed,
		Remaining:       counts.Remaining,
		InFlight:        counts.InFlight,
		StartedAt:       run.StartedAt.String(),
		UpdatedAt:       run.UpdatedAt.String(),
		CancelRequested: run.CancelRequested,
		LastError:       run.LastError,
		Resumable:       run.Status.IsResumable(),
		Notes:           notes,
	}
	if run.Target.Index != nil {
		r.Index = run.Target.Index.String()
	}
	r.Done = counts.ByState[protocol.NodeDone]
	r.Skipped = counts.ByState[protocol.NodeSkipped]
	if r.Total == 0 {
		r.Total = run.NodeCount
		r.Remaining = run.NodeCount
	}

	end := a.Now()
	if run.FinishedAt != nil {
		r.FinishedAt = run.FinishedAt.String()
		end = run.FinishedAt.Time
	}
	if elapsed := end.Sub(run.StartedAt.Time); elapsed > 0 {
		r.ElapsedMS = elapsed.Milliseconds()
	}

	for _, n := range nodes {
		if n.State == protocol.NodeRunning || n.State == protocol.NodeVerifying {
			r.CurrentNode, r.CurrentKind = n.NodeID, n.Kind
			break
		}
	}

	// FR-ORD-5: the ETA comes from the plan's per-node estimates (FR-PLAN-9),
	// summed over the nodes that are not yet complete. It needs the plan,
	// because the state store records what a node is, not what it costs.
	if plan != nil && plan.Digest == run.PlanDigest {
		done := make(map[protocol.NodeID]bool, len(nodes))
		for _, n := range nodes {
			if n.State.IsComplete() {
				done[n.NodeID] = true
			}
		}
		eta := 0
		for i := range plan.Nodes {
			if !done[plan.Nodes[i].ID] {
				eta += plan.Nodes[i].EstimatedSeconds
			}
		}
		r.ETASeconds = &eta
	}
	return r
}

func (a *App) printStatus(r StatusReport) {
	fmt.Fprintf(a.Stdout, "run %s  %s\n", r.RunID, r.Status)
	fmt.Fprintf(a.Stdout, "  operation   %s on %s", r.Operation, r.Table)
	if r.Index != "" {
		fmt.Fprintf(a.Stdout, " index %s", r.Index)
	}
	fmt.Fprintln(a.Stdout)
	fmt.Fprintf(a.Stdout, "  plan        %s digest %s\n", r.PlanID, r.PlanDigest)
	fmt.Fprintf(a.Stdout, "  nodes       %d total: %d done, %d skipped, %d failed, %d remaining\n",
		r.Total, r.Done, r.Skipped, r.Failed, r.Remaining)
	if r.CurrentNode != "" {
		fmt.Fprintf(a.Stdout, "  current     %s (%s)\n", r.CurrentNode, r.CurrentKind)
	}
	fmt.Fprintf(a.Stdout, "  started     %s (elapsed %s)\n", r.StartedAt, time.Duration(r.ElapsedMS)*time.Millisecond)
	if r.FinishedAt != "" {
		fmt.Fprintf(a.Stdout, "  finished    %s\n", r.FinishedAt)
	}
	if r.ETASeconds != nil {
		fmt.Fprintf(a.Stdout, "  eta         %s of estimated work remains (FR-PLAN-9; advisory)\n",
			humanSeconds(*r.ETASeconds))
	} else {
		fmt.Fprintln(a.Stdout, "  eta         unavailable: pass --plan <file> for the per-node estimates")
	}
	if r.CancelRequested {
		fmt.Fprintln(a.Stdout, "  cancel      requested; the executor stops at its next node boundary (FR-CLI-10)")
	}
	if r.LastError != "" {
		fmt.Fprintf(a.Stdout, "  last error  %s\n", r.LastError)
	}
	if r.Resumable {
		fmt.Fprintln(a.Stdout, "  resumable   yes: partitionctl resume <plan>")
	}
	for _, n := range r.Notes {
		fmt.Fprintf(a.Stdout, "  note        %s\n", n)
	}
}
