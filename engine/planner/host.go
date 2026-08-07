package planner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// Specification is the declarative statement of the desired end state: the
// planner's input (TRD §17.1).
//
// It carries no credentials and no connection string (NFR-SEC-2, NFR-SEC-3).
// The database name is recorded for the reviewer's benefit only.
type Specification struct {
	// Operation names the planner to run. It must match the
	// [OperationPlanner] the host is given.
	Operation protocol.Operation

	// Table is the partitioned parent table.
	Table protocol.ObjectName

	// Index is the partitioned index the operation acts on. All three v0.1
	// operations name one.
	Index protocol.ObjectName

	// Definition is the index shape. Required by create-index; ignored by the
	// operations that act on an index that already exists.
	Definition protocol.IndexDefinition

	// PaceSeconds is the pause the planner emits between leaves, as `wait`
	// nodes in the graph (FR-ORD-3). Zero emits none. Expressing pacing as
	// nodes rather than as executor configuration is what makes every pause
	// visible in the artifact the operator reviews.
	PaceSeconds int

	// PaceReason is the operator-facing explanation recorded on each wait
	// node.
	PaceReason string

	// Confirmations are the acknowledgements the operator supplied at plan
	// time. DropPartitionedIndex requires [protocol.ConfirmExclusiveLock]
	// (FR-DROP-3, AC-13).
	Confirmations []protocol.Confirmation

	// Actor is who ran `plan`, recorded for the audit trail.
	Actor string
}

// Confirmed reports whether the specification carries the named
// acknowledgement.
func (s Specification) Confirmed(flag string) bool {
	for _, c := range s.Confirmations {
		if c.Flag == flag {
			return true
		}
	}
	return false
}

// Validate checks the specification is structurally complete. It says nothing
// about the catalog: that is [Discover]'s and the operation's job.
func (s Specification) Validate() error {
	if !s.Operation.Valid() {
		return ErrInvalidSpecification.Detailf(
			"unknown operation %q, want one of %v", s.Operation, protocol.AllOperations())
	}
	if s.Table.IsZero() {
		return ErrInvalidSpecification.Detailf("table is required")
	}
	if err := s.Table.Validate(); err != nil {
		return ErrInvalidSpecification.Detailf("table: %v", err)
	}
	if s.Index.IsZero() {
		return ErrInvalidSpecification.Detailf(
			"index is required: every v0.1 operation names the partitioned index it acts on")
	}
	if err := s.Index.Validate(); err != nil {
		return ErrInvalidSpecification.Detailf("index: %v", err)
	}
	if s.Operation == protocol.OpCreateIndex {
		if err := s.Definition.Validate(); err != nil {
			return ErrInvalidSpecification.Detailf("definition: %v", err)
		}
	}
	if s.PaceSeconds < 0 {
		return ErrInvalidSpecification.Detailf("pace_seconds is negative: %d", s.PaceSeconds)
	}
	for i, c := range s.Confirmations {
		if c.Flag == "" {
			return ErrInvalidSpecification.Detailf("confirmation %d has an empty flag", i)
		}
	}
	return nil
}

// Request is the prepared input the host hands an [OperationPlanner].
//
// Every field is derived from a read-only catalog pass the host has already
// made, so an operation never repeats discovery, never re-runs the privilege
// check, and cannot skip either. That is the point of the split: the safety
// checks that must not be forgotten live in the host, and an operation is left
// with the one thing only it knows, which nodes to emit.
type Request struct {
	// Spec is the specification, already validated.
	Spec Specification

	// Catalog is the read-only catalog reader, for anything the operation
	// needs beyond discovery.
	Catalog CatalogReader

	// Topology is the discovered and validated partition tree
	// (FR-PLAN-1..3).
	Topology Topology

	// Role is the connected role, already validated as a member of the owning
	// role of every relation in Topology (FR-PLAN-10).
	Role string

	// Database is the connected database name.
	Database string

	// ServerVersionNum is the target server's server_version_num.
	ServerVersionNum int

	// Estimator converts relpages into per-node duration estimates
	// (FR-PLAN-9).
	Estimator Estimator

	// Provenance reads PartitionCTL's record of what it created (FR-PLAN-6,
	// FR-PLAN-7). It may be nil, which [DecideCleanup] treats as "no
	// provenance": the safe direction.
	Provenance ProvenanceLookup

	// PlanID is the identity the host assigned to the plan being built. An
	// operation may use it to derive node IDs.
	PlanID protocol.PlanID

	// Now is the single timestamp for this planning pass. Operations use it
	// rather than reading the clock, so a plan is a function of its inputs.
	Now protocol.Timestamp
}

// Result is what an [OperationPlanner] produces: the graph, and notes for the
// operator.
//
// It deliberately cannot carry a digest, a fingerprint or a format version. The
// host owns those, so an operation cannot ship an unsealed plan or one whose
// fingerprint describes a different tree than the one it planned against.
type Result struct {
	// Nodes is the graph. Edges are [protocol.Node.DependsOn].
	Nodes []protocol.Node

	// Notes are operator-facing lines the `plan` command prints, for anything
	// the artifact cannot state structurally: the blast radius of a drop
	// (FR-DROP-5), how many leaves were skipped as already complete
	// (FR-PLAN-5).
	Notes []string
}

// OperationPlanner is what an operation implements (TRD §7.2.1, NFR-EXT-1).
//
// Adding an operation means adding one of these. It does not mean touching the
// executor's dispatch loop, the state store, or the CLI. That is the whole
// claim NFR-EXT-1 makes and AC-21 measures.
//
// An implementation must issue no DDL and must open no write transaction: the
// only database access available to it is [Request.Catalog], which is read-only
// (FR-PLAN-8).
type OperationPlanner interface {
	// Operation names the operation this planner implements.
	Operation() protocol.Operation

	// Plan emits the graph for one specification against one discovered
	// topology (FR-PLAN-12).
	Plan(ctx context.Context, req Request) (Result, error)
}

// Outcome is what [Host.Run] returns: the sealed plan plus the plan-time
// context the `plan` command reports.
type Outcome struct {
	// Plan is sealed: its digest and topology fingerprint are set and
	// verified.
	Plan *protocol.Plan
	// Topology is the tree the plan was built against.
	Topology Topology
	// Role is the connected role.
	Role string
	// Notes are the operation's operator-facing notes.
	Notes []string
}

// Host runs one [OperationPlanner] and assembles the artifact around it.
//
// The sequence is fixed, and the order is the requirement: the checks that must
// fail before a multi-hour run starts all run before the operation is asked for
// a single node.
//
//  1. Validate the specification.
//  2. Prove the reader is read-only, where it can prove it (FR-PLAN-8).
//  3. Check the server version (NFR-COMPAT-1).
//  4. Discover and validate the partition tree (FR-PLAN-1..3, AC-11).
//  5. Validate role membership against every relation to be modified
//     (FR-PLAN-10, AC-12).
//  6. Ask the operation for the graph.
//  7. Compute the topology fingerprint (FR-PLANFILE-4), validate the whole
//     plan, and seal the digest (FR-PLANFILE-2).
//
// The host issues no DDL, and neither may anything it calls.
type Host struct {
	// Catalog is the read-only catalog reader. Required.
	Catalog CatalogReader

	// Estimator converts relpages into duration estimates. The zero value is
	// [DefaultEstimator].
	Estimator Estimator

	// Provenance reads PartitionCTL's record of what it created. Optional; nil
	// means no provenance is available, which halts rather than dropping
	// (FR-PLAN-7).
	Provenance ProvenanceLookup

	// Now returns the planning timestamp. Nil means time.Now. Tests set it to
	// make a plan a pure function of its inputs.
	Now func() time.Time

	// NewPlanID mints the plan identity. Nil means a random one. Tests set it
	// for the same reason as Now.
	NewPlanID func() protocol.PlanID

	// DiscoverOptions relaxes a discovery rejection for an operation that can
	// cope with it. The only one in v0.1 is [AllowNoPartitions], which
	// DropPartitionedIndex passes and the other two do not. Leaving it nil is
	// the strict, correct setting for create and reindex.
	DiscoverOptions []DiscoverOption

	// SkipReadOnlyCheck disables step 2. It exists for a caller that has
	// established read-only access some other way, for example by setting
	// default_transaction_read_only on the role. Leaving it false is the
	// supported configuration.
	SkipReadOnlyCheck bool
}

// Run executes the planning sequence and returns a sealed plan.
func (h *Host) Run(ctx context.Context, op OperationPlanner, spec Specification) (*Outcome, error) {
	if h.Catalog == nil {
		return nil, ErrInvalidSpecification.Detailf("planner host has no catalog reader")
	}
	if op == nil {
		return nil, ErrInvalidSpecification.Detailf("planner host was given no operation planner")
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if op.Operation() != spec.Operation {
		return nil, ErrInvalidSpecification.Detailf(
			"specification names operation %q but the planner implements %q",
			spec.Operation, op.Operation())
	}

	// FR-PLAN-8. A planner that has silently been handed a writable session is
	// a planner that could issue DDL, so refuse rather than trust.
	if !h.SkipReadOnlyCheck {
		if a, ok := h.Catalog.(ReadOnlyAsserter); ok {
			if err := a.AssertReadOnly(ctx); err != nil {
				return nil, err
			}
		}
	}

	version, err := h.Catalog.ServerVersionNum(ctx)
	if err != nil {
		return nil, err
	}
	if version < MinServerVersionNum {
		return nil, ErrUnsupportedServerVersion.Detailf(
			"server_version_num is %d; PartitionCTL supports %d and later (NFR-COMPAT-1)",
			version, MinServerVersionNum)
	}

	role, err := h.Catalog.CurrentRole(ctx)
	if err != nil {
		return nil, err
	}
	database, err := h.Catalog.CurrentDatabase(ctx)
	if err != nil {
		return nil, err
	}

	topo, err := Discover(ctx, h.Catalog, spec.Table, h.DiscoverOptions...)
	if err != nil {
		return nil, err
	}

	if err := ValidateRoleMembership(ctx, h.Catalog, role, topo.Relations()); err != nil {
		return nil, err
	}

	now := protocol.NewTimestamp(h.now())
	planID := h.newPlanID()

	result, err := op.Plan(ctx, Request{
		Spec:             spec,
		Catalog:          h.Catalog,
		Topology:         topo,
		Role:             role,
		Database:         database,
		ServerVersionNum: version,
		Estimator:        h.Estimator.withDefaults(),
		Provenance:       h.Provenance,
		PlanID:           planID,
		Now:              now,
	})
	if err != nil {
		return nil, err
	}
	if len(result.Nodes) == 0 {
		return nil, protocol.ErrInvalidPlan.Detailf(
			"operation %q emitted no nodes; a converged target still needs a plan that verifies it",
			spec.Operation)
	}

	index := spec.Index
	plan := &protocol.Plan{
		FormatVersion: protocol.PlanFormatVersion,
		PlanID:        planID,
		Operation:     spec.Operation,
		Target: protocol.Target{
			Database: database,
			Table:    topo.Root.Name,
			Index:    &index,
		},
		CreatedAt:     now,
		Confirmations: spec.Confirmations,
		Nodes:         result.Nodes,
	}

	// The tree and its fingerprint are recorded together, from one snapshot, so
	// the artifact carries both what was checked and what it was checked
	// against (FR-PLANFILE-4, AC-3).
	input := topo.Input()
	fingerprint, err := input.Fingerprint()
	if err != nil {
		return nil, err
	}
	plan.TopologyFingerprint = fingerprint
	plan.Topology = &input

	if err := plan.Validate(); err != nil {
		return nil, err
	}
	if err := plan.Seal(); err != nil {
		return nil, err
	}

	return &Outcome{
		Plan:     plan,
		Topology: topo,
		Role:     role,
		Notes:    result.Notes,
	}, nil
}

func (h *Host) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Host) newPlanID() protocol.PlanID {
	if h.NewPlanID != nil {
		return h.NewPlanID()
	}
	return NewPlanID()
}

// NewPlanID mints a random plan identity.
//
// It is not the digest and must not be derived from the plan's content: the
// digest changes when the content changes and the PlanID does not, which is
// what lets a run record stay bound to one plan while the operator re-reads it
// (INV-6).
func NewPlanID() protocol.PlanID {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any supported platform. If it somehow
		// does, a monotonic fallback is still unique enough for an identity
		// that is scoped to one artifact.
		return protocol.PlanID("plan-" + strconv.FormatInt(time.Now().UnixNano(), 16))
	}
	return protocol.PlanID("plan-" + hex.EncodeToString(b[:]))
}
