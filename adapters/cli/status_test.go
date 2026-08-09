package cli

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// AC-25: "`status` returns full core output with the target database
// unreachable, degrading only the live index-build enrichment."
//
// cmd_status.go's own comment says the file store "is the configuration AC-25
// is tested against", and there was no such test anywhere. The only test citing
// AC-25 lived in engine/state and asserted that NewSQLStore issues no SQL at
// construction, which is necessary but nowhere near sufficient.

// TestStatusWithTheTargetUnreachable is AC-25 proper.
func TestStatusWithTheTargetUnreachable(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()
	if code := h.Run("execute", plan); code != int(protocol.ExitSuccess) {
		t.Fatalf("execute exited %d: %s", code, h.Out())
	}
	run := h.Runs()[0]

	// The target is now unreachable. The file state store needs no connection.
	h.App.OpenDB = func(context.Context, Config) (*sql.DB, error) {
		return nil, errors.New("dial tcp 10.0.0.1:5432: connect: connection refused")
	}
	h.App.NewTarget = func(context.Context, Config, *sql.DB) (*Target, error) {
		return nil, errors.New("no connection")
	}

	if code := h.Run("status", string(run.RunID)); code != int(protocol.ExitSuccess) {
		t.Fatalf("status exited %d with the target unreachable, want 0 (AC-25): %s", code, h.Out())
	}

	// "Full core output": the run id, its status and the node counts. These
	// come from the state store and owe nothing to the target.
	out := h.Out()
	for _, want := range []string{string(run.RunID), string(run.Status)} {
		if !strings.Contains(out, want) {
			t.Errorf("core output is missing %q:\n%s", want, out)
		}
	}
}

// TestStatusJSONWithTheTargetUnreachable: a script reading --json must be able
// to tell a complete report from a degraded one.
func TestStatusJSONWithTheTargetUnreachable(t *testing.T) {
	h := newHarness(t)
	plan := h.MustPlan()
	if code := h.Run("execute", plan); code != int(protocol.ExitSuccess) {
		t.Fatalf("execute exited %d: %s", code, h.Out())
	}
	run := h.Runs()[0]

	h.App.OpenDB = func(context.Context, Config) (*sql.DB, error) {
		return nil, errors.New("connection refused")
	}

	if code := h.Run("status", "--json", string(run.RunID)); code != int(protocol.ExitSuccess) {
		t.Fatalf("status --json exited %d, want 0 (AC-25): %s", code, h.Out())
	}
	if !strings.Contains(h.Stdout.String(), string(run.RunID)) {
		t.Errorf("the JSON report does not carry the run id:\n%s", h.Stdout.String())
	}
}

// TestStatusUnderTheSQLStoreExplainsWhyItCannotDegrade.
//
// AC-25 cannot hold for --state sql, and no amount of graceful degradation can
// change that: the execution state lives *in* the database that is unreachable,
// so there is nothing left to report from. What the command owes the operator
// is that explanation and the configuration that survives it, rather than a
// bare connection error.
func TestStatusUnderTheSQLStoreExplainsWhyItCannotDegrade(t *testing.T) {
	h := newHarness(t)
	// Use the real store selection, so this exercises the shipping path.
	h.App.NewStore = nil
	h.App.OpenDB = func(context.Context, Config) (*sql.DB, error) {
		return nil, errors.New("connection refused")
	}

	// --state sql is Defaults(), i.e. what an operator gets with no flags.
	code := h.App.Run(ctx(), []string{"status", "--state", "sql", "--actor", "tester"})
	if code == int(protocol.ExitSuccess) {
		t.Fatalf("status succeeded with no state store: %s", h.Out())
	}
	out := h.Out()
	if !strings.Contains(out, "--state file") {
		t.Errorf("the failure does not name the configuration that survives an unreachable "+
			"target (AC-25, FR-CLI-12):\n%s", out)
	}
}
