package cli

import (
	"context"

	"github.com/atulsinha/partitionctl/engine/executor"
	"github.com/atulsinha/partitionctl/engine/protocol"
	"github.com/atulsinha/partitionctl/engine/verifier"
)

// ---------------------------------------------------------------------------
// The executor's catalog evaluator
// ---------------------------------------------------------------------------

// catalogEvaluator answers the executor's two read-only node kinds.
//
// It is one type over two sources because the split is real and deliberate:
// engine/verifier owns end-state assertions (index.verify) and says so, while
// catalog.assert carries plan-time *preconditions* about topology, strategy,
// depth and role membership that the verifier explicitly does not own
// (verifier package doc, "What this package does not own"). Those preconditions
// are re-evaluated here against the live catalog, which is what makes a plan
// that was valid an hour ago fail at exit 15 or 16 rather than half-run.
type catalogEvaluator struct {
	assert *assertEvaluator
	verify *verifier.Verifier
	// marker is the read surface the ownership marker comes off. It is the same
	// catalog the verifier holds; it is named separately because reading a
	// marker is an authorization question, not a verification one.
	marker verifier.Catalog
}

var _ executor.CatalogEvaluator = (*catalogEvaluator)(nil)

// Assert implements [executor.CatalogEvaluator]. It returns exactly one result
// per assertion, in order, which is the contract the executor pairs on.
func (e *catalogEvaluator) Assert(ctx context.Context, assertions []protocol.Assertion) ([]executor.CheckResult, error) {
	if e.assert == nil {
		return nil, executor.ErrMissingPort.Detailf(
			"the plan contains %s nodes but no catalog reader is configured", protocol.KindCatalogAssert)
	}
	return e.assert.Evaluate(ctx, assertions)
}

// Verify implements [executor.CatalogEvaluator] by delegating to the one
// verifier the CLI, the executor and the Liquibase gates all share (TRD
// §7.2.7).
//
// A check the verifier could not evaluate is returned as an error rather than
// as a failed check. The distinction is the verifier's own (StatusError is not
// a verdict), and it is what lets the executor's classifier retry a dropped
// connection while refusing to retry a false assertion.
func (e *catalogEvaluator) Verify(ctx context.Context, checks []protocol.VerifyCheck) ([]executor.CheckResult, error) {
	if e.verify == nil {
		return nil, executor.ErrMissingPort.Detailf(
			"the plan contains %s nodes but no verifier catalog is configured", protocol.KindIndexVerify)
	}
	out := make([]executor.CheckResult, len(checks))
	for i, c := range checks {
		r := e.verify.Check(ctx, c)
		if r.Status == verifier.StatusError {
			if err := r.Err(); err != nil {
				return nil, err
			}
			return nil, protocol.ErrVerificationFailed.Detailf(
				"check %q could not be evaluated: %s", c.Check, r.Reason)
		}
		out[i] = executor.CheckResult{
			Name:        string(r.Check),
			Passed:      r.Passed(),
			Detail:      r.Reason,
			FailureCode: protocol.ExitVerificationFailed,
		}
	}
	return out, nil
}

// Marker implements [executor.CatalogEvaluator] (FR-AUTH-2 as amended).
//
// It reads the ownership marker off the object itself, through the same
// verifier catalog the index.verify checks use. An index that does not exist,
// or that carries no comment, is [protocol.MarkerAbsent] and a nil error:
// absence is an answer, and it is the answer that halts a destructive decision
// rather than one that hides an outage.
func (e *catalogEvaluator) Marker(ctx context.Context, object protocol.ObjectName) (protocol.Marker, protocol.MarkerStatus, error) {
	if e.marker == nil {
		return protocol.Marker{}, protocol.MarkerAbsent, executor.ErrMissingPort.Detailf(
			"the ownership marker on %s cannot be read: no verifier catalog is configured", object)
	}
	return verifier.IndexMarker(ctx, e.marker, object)
}
