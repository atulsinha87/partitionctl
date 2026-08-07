package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"

	"github.com/atulsinha/partitionctl/engine/protocol"
	"github.com/atulsinha/partitionctl/engine/verifier"
)

// cmdVerify implements `verify <plan>` (FR-VER-5, FR-CLI-14, FR-CLI-15).
//
// It evaluates the verifier's assertions and reports pass or fail per
// assertion. It issues no DDL, and it cannot: the verifier's catalog holds a
// [verifier.Queryer], an interface carrying QueryContext and nothing else, so
// the refusal is a compile-time property rather than a review comment.
func (a *App) cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	setFlags := globalFlags(fs)

	asJSON := fs.Bool("json", false, "emit the stable field schema of NFR-OBS-2 (FR-CLI-15)")
	expectAbsent := fs.Bool("expect-absent", false,
		"assert the parent index and every leaf index are gone, rather than present; "+
			"this is what confirms an unwind reached the pre-run catalog state (TRD §13.2)")
	endState := fs.Bool("end-state", false,
		"assert the operation's complete end state from the catalog rather than from the plan's "+
			"index.verify nodes: parent valid, every discovered leaf attached, counts equal (FR-VER-1..4)")
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

	db, err := a.openDB(ctx, cfg)
	if err != nil {
		return err
	}
	if db != nil {
		defer func() { _ = db.Close() }()
	}
	tgt, err := a.openTarget(ctx, cfg, db)
	if err != nil {
		return err
	}
	defer tgt.close()

	v := verifierFor(tgt)
	if v == nil {
		return protocol.ErrFailure.Detailf("no catalog to verify against")
	}

	var report verifier.Report
	switch {
	case *expectAbsent:
		if plan.Target.Index == nil {
			return protocol.ErrFailure.Detailf("--expect-absent needs a plan whose target names an index")
		}
		// A plan is in hand, so the child index names it recorded are
		// authoritative (FR-PLAN-13). Deriving them again from the live leaf
		// set would miss an index whose partition has since been renamed, and
		// report PASS with that index still present as an unattached orphan.
		report, err = v.VerifyIndexAbsentForPlan(ctx, plan)
	case *endState:
		if plan.Target.Index == nil {
			return protocol.ErrFailure.Detailf("--end-state needs a plan whose target names an index")
		}
		report, err = v.VerifyPartitionedIndex(ctx, plan.Target.Table, *plan.Target.Index)
	default:
		// The plan's own index.verify nodes, in the order the executor would
		// have run them, so a `verify` transcript reads in the same sequence as
		// the run it is checking.
		report, err = v.VerifyPlan(ctx, plan)
	}
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(a.Stdout)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(report); encErr != nil {
			return encErr
		}
	} else {
		a.printReport(report)
	}

	// Report.Err distinguishes "the assertion is false" (exit 14) from "the
	// catalog could not be read", which is not a verdict about the database and
	// keeps its own code.
	return report.Err()
}

// printReport writes one line per assertion, pass and fail alike. The pass
// lines are what make the transcript checkable against the plan.
func (a *App) printReport(r verifier.Report) {
	for _, res := range r.Results {
		mark := "PASS"
		if res.Status == verifier.StatusFail {
			mark = "FAIL"
		} else if res.Status == verifier.StatusError {
			mark = "ERR "
		}
		fmt.Fprintf(a.Stdout, "%s  %-20s %s\n", mark, res.Check, res.Reason)
		if res.Status != verifier.StatusPass && res.Message != "" {
			fmt.Fprintf(a.Stdout, "      %s\n", res.Message)
		}
	}
	fmt.Fprintf(a.Stdout, "\n%s\n", r.Summary())
}

// verifierFor builds the one verifier the CLI, the executor and the Liquibase
// gates all share (TRD §7.2.7). One implementation, several consumers, which is
// why the adapter layer can be thin.
func verifierFor(tgt *Target) *verifier.Verifier {
	if tgt == nil || tgt.Verify == nil {
		return nil
	}
	return verifier.New(tgt.Verify)
}
