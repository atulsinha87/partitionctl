package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/atulsinha/partitionctl/engine/planner"
	"github.com/atulsinha/partitionctl/engine/protocol"
	createindex "github.com/atulsinha/partitionctl/operations/create-index"
)

// createIndexOperation presents [createindex.Planner] as the
// [planner.OperationPlanner] the host runs.
//
// # Why the adapter exists rather than the operation implementing the interface
//
// operations/create-index deliberately depends on nothing but engine/protocol.
// That is what lets it be tested with a hand-written catalog fake and no
// planner host, and it is what NFR-EXT-1 means by "adding an operation SHALL
// require a new planner": the operation is a self-contained compiler from
// specification to graph. The host, meanwhile, owns the checks that must never
// be skipped — the read-only proof (FR-PLAN-8), the server version gate
// (NFR-COMPAT-1), discovery and topology rejection (FR-PLAN-1..3), and role
// membership (FR-PLAN-10) — and it owns plan identity, the topology fingerprint
// and the digest, so that an operation cannot ship an unsealed plan.
//
// Running the operation *through* the host is therefore what buys the fixed
// check order of TRD §7.2.1, and this type is the twenty lines that connect
// them. Every future operation gets a sibling of this file and nothing else.
//
// The operation is asked for its graph and the host keeps the nodes; the plan
// the operation seals internally is discarded, because the host's is the one
// with the host's plan id, the host's timestamp and the fingerprint computed
// over the tree the host validated. Handing the operation
// [planner.Request.PlanID] keeps the two identities the same rather than
// merely compatible.
// It carries no state: everything it needs arrives in the [planner.Request].
type createIndexOperation struct{}

var _ planner.OperationPlanner = (*createIndexOperation)(nil)

// Operation implements [planner.OperationPlanner].
func (o *createIndexOperation) Operation() protocol.Operation { return protocol.OpCreateIndex }

// Plan implements [planner.OperationPlanner] (FR-PLAN-12).
func (o *createIndexOperation) Plan(ctx context.Context, req planner.Request) (planner.Result, error) {
	spec := createindex.Specification{
		Database:    req.Database,
		Table:       req.Topology.Root.Name,
		Index:       req.Spec.Index,
		Definition:  req.Spec.Definition,
		Role:        req.Role,
		PaceSeconds: req.Spec.PaceSeconds,
		PlanID:      req.PlanID,
	}

	cat := &operationCatalog{reader: req.Catalog, topo: req.Topology}

	inner := createindex.Planner{
		Now:    func() time.Time { return req.Now.Time },
		Claims: claimsFor(req.Claims),
	}
	plan, err := inner.Plan(ctx, spec, cat)
	if err != nil {
		return planner.Result{}, err
	}

	return planner.Result{
		Nodes: plan.Nodes,
		Notes: createIndexNotes(req, plan),
	}, nil
}

// claimsFor adapts the planner's one-method claim view to the operation's,
// which declares the identical method under its own name. A nil source means no
// claim, which leaves the ownership marker on the object as the only thing that
// can authorize a drop (FR-PLAN-7, NFR-REL-3).
func claimsFor(c planner.ClaimLookup) createindex.ClaimReader {
	if c == nil {
		return createindex.NoClaims()
	}
	return claimAdapter{c}
}

type claimAdapter struct{ inner planner.ClaimLookup }

func (a claimAdapter) ClaimsObject(ctx context.Context, object protocol.ObjectName) (string, bool, error) {
	return a.inner.ClaimsObject(ctx, object)
}

// createIndexNotes are the operator-facing lines `plan` prints for anything the
// artifact cannot state structurally (FR-PLAN-5's skip count, the estimate).
func createIndexNotes(req planner.Request, plan *protocol.Plan) []string {
	builds := 0
	drops := 0
	for i := range plan.Nodes {
		switch plan.Nodes[i].Kind {
		case protocol.KindIndexCreateConcurrently:
			builds++
		case protocol.KindIndexDropConcurrently:
			drops++
		}
	}
	leaves := req.Topology.LeafCount()
	notes := []string{
		fmt.Sprintf("%d leaf partition(s) discovered; %d index build(s) remain, %d already complete (FR-PLAN-5)",
			leaves, builds, leaves-builds),
		"the parent index is INVALID for the whole build; PostgreSQL marks it valid on the final attach",
	}
	if drops > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d INVALID index(es) will be dropped first, each authorized by the PartitionCTL ownership "+
				"marker on the object itself, re-read at dispatch (FR-PLAN-6, FR-AUTH-5, AC-5)", drops))
	}
	notes = append(notes,
		"every index this run creates is stamped with a PartitionCTL ownership marker "+
			"(COMMENT ON INDEX, ShareUpdateExclusiveLock, ~1ms), which is what lets a later run prove "+
			"the object is its own to clean up (AC-6)")
	if !createindex.HasWork(plan) {
		notes = append(notes,
			"no DDL remains: this plan is a checked no-op that re-proves the end state and exits zero (AC-7)")
	}
	return notes
}
