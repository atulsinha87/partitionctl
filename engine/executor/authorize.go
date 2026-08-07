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
// plan asserts. The returned error is reserved for a store failure, which is
// "cannot decide" rather than "denied"; a denial is a nil error with
// Satisfied false.
//
// Every mode is additionally gated on the authorization naming exactly the
// object the node would destroy. Without that check a plan could authorize one
// index and drop another.
func Authorize(ctx context.Context, store AuthorityReader, plan *protocol.Plan, n *protocol.Node) (AuthorizationDecision, error) {
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
		return authorizeProvenance(ctx, store, d, auth)
	case protocol.AuthLeftover:
		return authorizeLeftover(ctx, store, d, auth)
	case protocol.AuthExplicit:
		return authorizeExplicit(d, plan, auth)
	}
	d.Reason = "unknown authorization mode " + string(auth.Mode)
	return d, nil
}

// authorizeProvenance is satisfied only by a committed provenance record
// proving PartitionCTL created the object (FR-AUTH-2). An INVALID index nobody
// recorded is somebody else's, and the run halts rather than dropping it
// (AC-6, NFR-REL-3).
func authorizeProvenance(ctx context.Context, store AuthorityReader, d AuthorizationDecision, auth *protocol.Authorization) (AuthorizationDecision, error) {
	rec, found, err := store.LookupProvenance(ctx, auth.Object)
	if err != nil {
		return d, err
	}
	if !found {
		d.Reason = "no committed provenance record proves PartitionCTL created " +
			auth.Object.String() + " (FR-AUTH-2, AC-6)"
		return d, nil
	}
	d.Satisfied = true
	d.Evidence = map[string]string{
		"mode":                string(protocol.AuthProvenance),
		"object":              auth.Object.String(),
		"provenance_run_id":   string(rec.RunID),
		"provenance_node_id":  string(rec.NodeID),
		"provenance_recorded": rec.CreatedAt.UTC().Format(timeLayout),
	}
	return d, nil
}

// authorizeLeftover requires two independent conditions: the name matches
// PostgreSQL's _ccnew/_ccold convention, and the relation has a recorded
// PartitionCTL reindex run (FR-AUTH-3, FR-AUTH-7, INV-7).
//
// Naming alone is forgeable and non-exclusive. An operator who ran REINDEX
// CONCURRENTLY by hand and left their own _ccnew behind must not have it
// deleted by this tool (AC-19).
func authorizeLeftover(ctx context.Context, store AuthorityReader, d AuthorizationDecision, auth *protocol.Authorization) (AuthorizationDecision, error) {
	kind, base := protocol.ClassifyLeftover(auth.Object.Name)
	if kind == protocol.LeftoverNone {
		d.Reason = auth.Object.String() + " does not match the " +
			protocol.LeftoverNewPrefix + "/" + protocol.LeftoverOldPrefix +
			" naming convention (FR-AUTH-3)"
		return d, nil
	}
	if auth.Relation == nil {
		d.Reason = "leftover authorization carries no relation, so reindex-run history cannot be resolved (FR-AUTH-3)"
		return d, nil
	}
	ran, err := store.HasReindexRun(ctx, *auth.Relation)
	if err != nil {
		return d, err
	}
	if !ran {
		d.Reason = "no PartitionCTL reindex run is recorded for " + auth.Relation.String() +
			", so the leftover is not ours to drop (FR-AUTH-3, INV-7, AC-19)"
		return d, nil
	}
	d.Satisfied = true
	d.Evidence = map[string]string{
		"mode":           string(protocol.AuthLeftover),
		"object":         auth.Object.String(),
		"leftover_class": string(kind),
		"base_index":     base,
		"relation":       auth.Relation.String(),
		"reindex_run":    "recorded",
	}
	return d, nil
}

// authorizeExplicit is satisfied only when the specification names the object
// directly and the operator supplied the operation's confirmation flag
// (FR-AUTH-4). Both halves are re-checked here against the plan artifact, which
// is where the acknowledgement was recorded (FR-DROP-3, AC-13).
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
