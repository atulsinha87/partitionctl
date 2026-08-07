package executor

import (
	"context"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// AuthorizationDecision is the verdict on one destructive node, reached against
// live state at dispatch time.
type AuthorizationDecision struct {
	// Mode is the mode the plan claimed.
	Mode protocol.AuthorizationMode
	// Object is what would be destroyed.
	Object protocol.ObjectName
	// Satisfied is the only field that decides anything.
	Satisfied bool
	// Adopt reports that the object must be marked as PartitionCTL's *before*
	// it is dropped, because the only thing proving ownership is a live claim
	// and the marker is missing. It is the [protocol.DropAdoptThenDrop] row of
	// the decision table, and it is reserved to `resume` (FR-CLI-9).
	Adopt bool
	// Evidence is what made the mode satisfied, recorded before the statement
	// runs (FR-AUTH-6). It is JSON-marshalled by the state store, so a map is
	// safe: encoding/json sorts keys.
	Evidence map[string]string
	// Reason explains an unsatisfied verdict, in operator-facing terms.
	Reason string
}

// Authorize re-evaluates a destructive node's authorization against live state
// (FR-AUTH-5, INV-2).
//
// A plan is a proposal, never a permission: the mode the planner wrote is
// re-checked here, and the run halts if it is unsatisfied, no matter what the
// plan asserts. The returned error is reserved for a store or catalog failure,
// which is "cannot decide" rather than "denied"; a denial is a nil error with
// Satisfied false.
//
// The decision table itself lives in [protocol.DecideProvenanceDrop] and
// [protocol.DecideLeftoverDrop] and is shared with the planner. There is
// deliberately one implementation: two would be two places for the answers to
// diverge, and the second one only ever runs against a production catalog.
//
// Every mode is additionally gated on the authorization naming exactly the
// object the node would destroy. Without that check a plan could authorize one
// index and drop another.
func Authorize(ctx context.Context, store AuthorityReader, cat CatalogEvaluator, plan *protocol.Plan, n *protocol.Node) (AuthorizationDecision, error) {
	if !n.Kind.IsDestructive() {
		return AuthorizationDecision{}, protocol.ErrInvalidPlan.Detailf(
			"node %q of kind %q is not destructive and needs no authorization", n.ID, n.Kind)
	}
	target, err := destructiveObject(n)
	if err != nil {
		return AuthorizationDecision{}, err
	}
	d := AuthorizationDecision{Object: target}

	auth := n.Authorization
	if auth == nil {
		d.Reason = "node carries no authorization (FR-AUTH-1)"
		return d, nil
	}
	d.Mode = auth.Mode
	if err := auth.Validate(); err != nil {
		d.Reason = "authorization is malformed: " + err.Error()
		return d, nil
	}
	if auth.Object != target {
		d.Reason = "authorization names " + auth.Object.String() +
			" but the node would destroy " + target.String()
		return d, nil
	}

	switch auth.Mode {
	case protocol.AuthProvenance:
		return authorizeProvenance(ctx, store, cat, d, auth)
	case protocol.AuthLeftover:
		return authorizeLeftover(ctx, cat, d, auth)
	case protocol.AuthExplicit:
		return authorizeExplicit(d, plan, auth)
	}
	d.Reason = "unknown authorization mode " + string(auth.Mode)
	return d, nil
}

// authorizeProvenance reads ownership off the object itself and falls back to a
// live claim only where the marker is missing (directive A.5.1, FR-AUTH-2).
func authorizeProvenance(ctx context.Context, store AuthorityReader, cat CatalogEvaluator, d AuthorizationDecision, auth *protocol.Authorization) (AuthorizationDecision, error) {
	if cat == nil {
		return d, ErrMissingPort.Detailf(
			"a CatalogEvaluator is required to read the ownership marker on %s (FR-AUTH-2)", auth.Object)
	}
	marker, status, err := cat.Marker(ctx, auth.Object)
	if err != nil {
		return d, err
	}

	in := protocol.ProvenanceDropInput{Object: auth.Object, Status: status, Marker: marker}
	// The claim is only consulted where it can change the answer, so a healthy
	// marked object costs no state-store read at all.
	if status == protocol.MarkerAbsent && store != nil {
		run, found, err := store.ClaimsObject(ctx, auth.Object)
		if err != nil {
			return d, err
		}
		if found {
			in.ClaimRun = string(run)
		}
	}

	v := protocol.DecideProvenanceDrop(in)
	d.Reason = v.Reason
	d.Evidence = v.Evidence
	d.Satisfied = v.Satisfied()
	d.Adopt = v.Action == protocol.DropAdoptThenDrop
	return d, nil
}

// authorizeLeftover requires two independent conditions: the name matches
// PostgreSQL's _ccnew/_ccold convention, and the *base* index carries a
// PartitionCTL marker (FR-AUTH-3 as amended, FR-AUTH-7, INV-7).
//
// Naming alone is forgeable and non-exclusive. An operator who ran REINDEX
// CONCURRENTLY by hand on an index this tool never built must not have the
// wreckage deleted by it (AC-19).
func authorizeLeftover(ctx context.Context, cat CatalogEvaluator, d AuthorizationDecision, auth *protocol.Authorization) (AuthorizationDecision, error) {
	base, ok := protocol.LeftoverBase(auth.Object)
	if !ok {
		v := protocol.DecideLeftoverDrop(protocol.LeftoverDropInput{Object: auth.Object})
		d.Reason = v.Reason
		return d, nil
	}
	if cat == nil {
		return d, ErrMissingPort.Detailf(
			"a CatalogEvaluator is required to read the ownership marker on %s (FR-AUTH-3)", base)
	}
	marker, status, err := cat.Marker(ctx, base)
	if err != nil {
		return d, err
	}
	v := protocol.DecideLeftoverDrop(protocol.LeftoverDropInput{
		Object: auth.Object,
		// The marker port cannot distinguish "no such index" from "an index
		// with no comment", and it does not need to: both halt. BaseExists is
		// true so the refusal reports the fact this side actually observed.
		BaseExists: true,
		BaseStatus: status,
		BaseMarker: marker,
	})
	d.Reason = v.Reason
	d.Evidence = v.Evidence
	d.Satisfied = v.Satisfied()
	if d.Satisfied && auth.Relation != nil {
		d.Evidence["relation"] = auth.Relation.String()
	}
	return d, nil
}

// authorizeExplicit is satisfied only when the specification names the object
// directly and the operator supplied the operation's confirmation flag
// (FR-AUTH-4). Both halves are re-checked here against the plan artifact, which
// is where the acknowledgement was recorded (FR-DROP-3, AC-13).
//
// The ownership marker plays no part, and must not: `drop-index` has to work on
// an index PartitionCTL never created, which is the entire reason this mode
// exists.
func authorizeExplicit(d AuthorizationDecision, plan *protocol.Plan, auth *protocol.Authorization) (AuthorizationDecision, error) {
	if plan == nil {
		d.Reason = "explicit authorization cannot be evaluated without the plan artifact"
		return d, nil
	}
	if !plan.Confirmed(auth.RequiredConfirmation) {
		d.Reason = "the plan records no " + auth.RequiredConfirmation +
			" acknowledgement (FR-AUTH-4, FR-DROP-3)"
		return d, nil
	}
	if plan.Target.Index == nil || *plan.Target.Index != auth.Object {
		d.Reason = "the plan's target does not name " + auth.Object.String() +
			", so the operator's stated intent does not cover it (FR-AUTH-4)"
		return d, nil
	}
	d.Satisfied = true
	d.Evidence = map[string]string{
		"mode":         string(protocol.AuthExplicit),
		"object":       auth.Object.String(),
		"confirmation": auth.RequiredConfirmation,
		"plan_id":      string(plan.PlanID),
	}
	for _, c := range plan.Confirmations {
		if c.Flag == auth.RequiredConfirmation {
			d.Evidence["confirmed_by"] = c.Actor
			d.Evidence["confirmed_at"] = c.At.Canonical()
			break
		}
	}
	return d, nil
}

// destructiveObject reports what a destructive node would actually destroy,
// read from its parameters rather than from its authorization. The two are then
// required to agree.
func destructiveObject(n *protocol.Node) (protocol.ObjectName, error) {
	switch n.Kind {
	case protocol.KindIndexDropConcurrently:
		p, err := paramsOf[*protocol.DropConcurrentlyParams](n)
		if err != nil {
			return protocol.ObjectName{}, err
		}
		return p.Index, nil
	case protocol.KindIndexDropPartitioned:
		p, err := paramsOf[*protocol.DropPartitionedParams](n)
		if err != nil {
			return protocol.ObjectName{}, err
		}
		return p.Index, nil
	}
	return protocol.ObjectName{}, protocol.ErrInvalidPlan.Detailf(
		"node %q of kind %q has no destructive target", n.ID, n.Kind)
}
