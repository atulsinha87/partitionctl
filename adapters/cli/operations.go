package cli

import (
	"bytes"
	"context"
	"time"

	"github.com/atulsinha87/partitionctl/engine/planner"
	"github.com/atulsinha87/partitionctl/engine/protocol"
	"github.com/atulsinha87/partitionctl/engine/verifier"
	createindex "github.com/atulsinha87/partitionctl/operations/create-index"
	dropindex "github.com/atulsinha87/partitionctl/operations/drop-index"
	reindexindex "github.com/atulsinha87/partitionctl/operations/reindex-index"
)

// operationRegistry is the whole of the CLI's knowledge of which operations
// exist (NFR-EXT-1, AC-21).
//
// What this map buys, stated as narrowly as it is true: an operation cannot
// silently inherit another operation's command behaviour. Every way an
// operation differs from its siblings is a field below, so `verify`, `render`,
// `execute`, `resume`, `status` and `cancel` contain no per-operation branch at
// all. TestEveryRegisteredOperationCarriesItsOwnCommandBehaviour
// (integration_test.go) is the evidence, and it is a real test of that claim.
//
// The wider claim this comment used to make -- "wiring an operation is: write
// operations/<op>/, add one entry here" -- is false, and it cited a test named
// TestEveryOperationIsReachableFromTheRegistry that has never existed. Wiring
// reindex-index needed a `ReindexSince` field on the shared
// planner.Specification (engine/planner/host.go), a `reindex_since` field and
// its RFC-3339 parsing on the shared SpecFile (adapters/cli/spec.go), and this
// entry. drop-index separately needed a schema-qualification fix inside
// planner.Host.Run. So operation #4, if it carries any parameter of its own,
// edits engine/planner/host.go, adapters/cli/spec.go and this file at minimum,
// plus engine/protocol/plan.go for the new Operation constant.
//
// The honest cost is: a sibling planner package, an entry here, and one field
// per operation-specific parameter on two shared structs. An operation needing
// a NEW NODE KIND costs far more than that -- ten switch statements in
// engine/protocol/nodekind.go plus render, params, executor and authorize --
// and, per nodekind.go, a PlanFormatVersion bump that invalidates every plan
// artifact already committed to a repository. The obvious next operations
// (ATTACH/DETACH PARTITION, ADD COLUMN, ADD CONSTRAINT NOT VALID + VALIDATE)
// reuse none of the nine index node kinds, so that is the cost that matters for
// NFR-EXT-1, and it is not what this comment used to describe.
//
// "One entry" is the honest unit, not "one line". An operation differs from its
// siblings in four ways and all four are fields below: which planner to build,
// which discovery relaxation it needs, which catalog gate answers "did it reach
// its end state", and whether it can be unwound. The first two were already
// here. The last two were not, and lived instead as flag-driven bodies inside
// `verify` and `render` that had been written against create-index's shape —
// which is how `verify --end-state` came to report FAIL on a successful drop
// and PASS on a reindex that left a leftover on every partition. Anything an
// operation can disagree with its siblings about belongs in this struct, or the
// next operation inherits another command's assumptions silently.
//
// The value is a constructor rather than an instance because a planner may
// legitimately carry per-run configuration (reindex-index carries the
// `--reindex-since` watermark), and sharing one instance across concurrent
// invocations of the library would share that configuration with it.
type operationEntry struct {
	// New builds the planner for one invocation.
	New func(spec planner.Specification) planner.OperationPlanner

	// Discover relaxes a discovery rejection this operation can cope with.
	// Only drop-index has one: a partitioned table whose partitions have all
	// been detached still carries the partitioned index an operator wants
	// removed, so refusing to plan for it would leave the only tool that can
	// drop it unable to run.
	Discover func() []planner.DiscoverOption

	// EndState is the gate `verify --end-state` runs: "did this operation reach
	// the state it exists to reach?", asked of the catalog rather than of the
	// plan's own verify nodes (FR-VER-1..4).
	//
	// It is per-operation because the answer is. Dispatching on the --end-state
	// flag alone ran VerifyPartitionedIndex for all three, which reports FAIL on
	// a drop that worked perfectly — the index is supposed to be gone — and, in
	// the dangerous direction, PASS on a reindex that left a _ccold on every
	// partition, because the leftover gate was never reached. Both were live:
	// `verify -end-state` on a completed reindex ran 26 checks, none of them
	// about leftovers.
	EndState func(ctx context.Context, v *verifier.Verifier, plan *protocol.Plan) (verifier.Report, error)

	// Rollback renders the `render --rollback` unwind, or refuses with a reason.
	//
	// Also per-operation, and for a sharper reason: the one body that existed
	// assumed a partially-completed create, so it offered `DROP INDEX <target>`
	// as the "unwind" of a reindex — a statement that destroys a pre-existing
	// production index the run never created — and as the unwind of a drop of
	// that same index. An operation that cannot be unwound must say so; a
	// nil here is that refusal, and it is what makes operation four safe by
	// construction rather than by review.
	Rollback func(w *bytes.Buffer, plan *protocol.Plan, confirmed bool, lockTimeout time.Duration) error
}

var operationRegistry = map[protocol.Operation]operationEntry{
	protocol.OpCreateIndex: {
		New: func(planner.Specification) planner.OperationPlanner { return createindex.Planner{} },
		EndState: func(ctx context.Context, v *verifier.Verifier, plan *protocol.Plan) (verifier.Report, error) {
			return v.VerifyPartitionedIndex(ctx, plan.Target.Table, *plan.Target.Index)
		},
		Rollback: renderRollbackCreate,
	},
	protocol.OpDropIndex: {
		New:      func(planner.Specification) planner.OperationPlanner { return dropindex.Planner{} },
		Discover: dropindex.DiscoverOptions,
		// The drop's end state is absence, and the plan's recorded child names
		// are what to check it against (FR-PLAN-13): re-deriving them from the
		// live leaf set would miss an index whose partition was renamed since
		// planning and report PASS with that index still sitting there.
		EndState: func(ctx context.Context, v *verifier.Verifier, plan *protocol.Plan) (verifier.Report, error) {
			return v.VerifyIndexAbsentForPlan(ctx, plan)
		},
		Rollback: nil, // refuses; see rollbackUnsupported
	},
	protocol.OpReindexIndex: {
		New: func(spec planner.Specification) planner.OperationPlanner {
			return reindexindex.Planner{ReindexSince: spec.ReindexSince}
		},
		// VerifyPartitionedIndex plus VerifyNoLeftovers. The leftover half is
		// the entire point: a reindex whose leaves all rebuilt but left a
		// _ccold behind on each one is the FR-REIDX-4 wreckage this operation
		// exists to clean up, and without this gate it reported PASS.
		EndState: func(ctx context.Context, v *verifier.Verifier, plan *protocol.Plan) (verifier.Report, error) {
			return v.VerifyReindexedIndex(ctx, plan.Target.Table, *plan.Target.Index)
		},
		Rollback: nil, // refuses; see rollbackUnsupported
	},
}

// rollbackUnsupported explains why an operation has no unwind runbook.
//
// Kept beside the registry so the refusal and the entry that triggers it are
// read together. An operation missing from this map with a nil Rollback gets a
// generic refusal, which is still a refusal — the failure mode being avoided is
// emitting somebody else's DROP INDEX, not emitting imperfect prose.
var rollbackUnsupported = map[protocol.Operation]string{
	protocol.OpReindexIndex: "a reindex creates no object, so there is nothing to unwind: " +
		"REINDEX CONCURRENTLY either leaves the index rebuilt or leaves it untouched beside a " +
		"_ccnew/_ccold leftover, and the leftover is cleaned up by re-planning, not by a runbook. " +
		"To roll the catalog back to a pre-reindex physical state you would rebuild it again, " +
		"which is the forward operation",
	protocol.OpDropIndex: "a drop cannot be unwound: the index and every attached child are gone, " +
		"and PostgreSQL keeps nothing to restore them from. The way back is to build it again with " +
		"CreatePartitionedIndex, which is hours of online work rather than a rollback (TRD §13.2.2)",
}

// plannerFor returns the operation planner and the host options for a
// specification.
//
// An operation in the plan format's vocabulary with no entry here is a build
// that predates it, and the message says so rather than pretending the
// operation is invalid.
func plannerFor(spec planner.Specification) (planner.OperationPlanner, []planner.DiscoverOption, error) {
	entry, ok := operationRegistry[spec.Operation]
	if !ok {
		return nil, nil, protocol.ErrFailure.Detailf(
			"operation %q is in the plan format's vocabulary but has no planner in this build; "+
				"this build ships %v", spec.Operation, registeredOperations())
	}
	var opts []planner.DiscoverOption
	if entry.Discover != nil {
		opts = entry.Discover()
	}
	return entry.New(spec), opts, nil
}

// registeredOperations lists what this build can plan, in the plan format's
// declared order so the message is stable.
func registeredOperations() []protocol.Operation {
	var out []protocol.Operation
	for _, op := range protocol.AllOperations() {
		if _, ok := operationRegistry[op]; ok {
			out = append(out, op)
		}
	}
	return out
}
