package createindex

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// Node IDs. They are deterministic functions of the relation they act on, so a
// re-plan over an unchanged catalog produces the same graph, and so a reviewer
// reading `status` output can tell which partition a node belongs to.
const (
	nodeAssert      protocol.NodeID = "assert.preconditions"
	nodeParentIndex protocol.NodeID = "index.parent"
	nodeFinalVerify protocol.NodeID = "verify.final"
)

func nodeID(step string, leaf protocol.ObjectName) protocol.NodeID {
	return protocol.NodeID(step + ":" + leaf.String())
}

// Planner compiles a [Specification] into a [protocol.Plan].
//
// The zero value is usable and is deliberately the strictest configuration: it
// has no claim source, so the only thing that can authorize a drop is the
// ownership marker on the object itself (FR-PLAN-7).
type Planner struct {
	// Now supplies the plan's CreatedAt. Nil means [time.Now]. Injecting it is
	// what lets a test assert a byte-exact plan digest.
	Now func() time.Time

	// Claims reports whether a run still holds a live claim on an object
	// (FR-PLAN-6, FR-PLAN-7). Nil means [NoClaims].
	Claims ClaimReader
}

// Plan is the package-level convenience form of [Planner.Plan], using the
// default planner. Because the default has no claim source, it halts on an
// unmarked INVALID index; a caller that can resume must supply one.
func Plan(ctx context.Context, spec Specification, cat CatalogReader) (*protocol.Plan, error) {
	return Planner{}.Plan(ctx, spec, cat)
}

// HasWork reports whether a plan contains any node that issues DDL.
//
// A plan for a fully converged catalog carries only its precondition assert and
// its final verify, so it is a checked no-op rather than an empty file: running
// it re-proves the end state and exits zero (AC-7). HasWork is how a caller
// tells that case from a plan with work in it, without inspecting node kinds.
func HasWork(p *protocol.Plan) bool {
	if p == nil {
		return false
	}
	for i := range p.Nodes {
		if p.Nodes[i].Kind.IssuesDDL() {
			return true
		}
	}
	return false
}

// Plan reads the catalog and compiles spec into a sealed plan.
//
// It issues no DDL and opens no write transaction (FR-PLAN-8). It emits only
// the work that remains (FR-PLAN-5), and it returns an error and no plan at all
// rather than emitting a graph it cannot justify: an unsupported topology
// (exit 15), a role that is not a member of an owning role (exit 16), or an
// INVALID index without provenance (exit 13, FR-PLAN-7, AC-6).
func (pl Planner) Plan(ctx context.Context, spec Specification, cat CatalogReader) (*protocol.Plan, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if cat == nil {
		return nil, protocol.ErrFailure.Detailf("create-index: catalog reader is nil")
	}

	topo, err := cat.Topology(ctx, spec.Table)
	if err != nil {
		return nil, fmt.Errorf("create-index: discover topology of %s: %w", spec.Table, err)
	}
	if err := topo.Validate(); err != nil {
		return nil, err
	}

	parent := protocol.NewObjectName(topo.Root.Schema, topo.Root.Name)
	if err := checkResolution(spec.Table, parent); err != nil {
		return nil, err
	}
	if err := checkTopology(topo, parent); err != nil {
		return nil, err
	}

	leaves := sortedPartitions(topo.Partitions)

	parentIndex := spec.Index
	if parentIndex.Schema == "" {
		parentIndex.Schema = parent.Schema
	}

	children, err := childIndexNames(parentIndex, leaves)
	if err != nil {
		return nil, err
	}

	// FR-PLAN-10 / AC-12: the connected role must be a member of the owning
	// role of the parent and of every leaf. Checked now so an unprivileged run
	// fails at plan time with exit 16, and recorded as an assertion so the
	// executor re-checks it against live state.
	relations := append([]protocol.ObjectName{parent}, leaves...)
	if err := checkRoleMembership(ctx, cat, spec.Role, relations); err != nil {
		return nil, err
	}

	states, err := cat.Indexes(ctx, append([]protocol.ObjectName{parentIndex}, children...))
	if err != nil {
		return nil, fmt.Errorf("create-index: read index state: %w", err)
	}
	pages, err := cat.RelationPages(ctx, leaves)
	if err != nil {
		return nil, fmt.Errorf("create-index: read relation sizes: %w", err)
	}

	claims := pl.claims()

	needParent, err := classifyParentIndex(ctx, claims, states, parent, parentIndex)
	if err != nil {
		return nil, err
	}

	nodes := make([]protocol.Node, 0, 2+len(leaves)*5)
	nodes = append(nodes, assertNode(spec.Role, parent, parentIndex, topo.Strategy, relations))

	chainRoot := nodeAssert
	if needParent {
		nodes = append(nodes, parentIndexNode(parent, parentIndex, spec.Definition))
		chainRoot = nodeParentIndex
	}

	var tails []protocol.NodeID
	for i, leaf := range leaves {
		chain, err := pl.leafChain(ctx, claims, spec, leaf, children[i], parentIndex, states, pages[leaf], chainRoot)
		if err != nil {
			return nil, err
		}
		if len(chain) == 0 {
			continue
		}
		nodes = append(nodes, chain...)
		tails = append(tails, chain[len(chain)-1].ID)
	}
	if len(tails) == 0 {
		// Nothing remained to build. The final verify still runs, so a
		// converged plan is a checked no-op rather than an empty one (AC-7).
		tails = []protocol.NodeID{chainRoot}
	}
	nodes = append(nodes, finalVerifyNode(parentIndex, children, tails))

	fingerprint, err := topo.Fingerprint()
	if err != nil {
		return nil, err
	}
	createdAt := protocol.NewTimestamp(pl.now())

	plan := &protocol.Plan{
		FormatVersion: protocol.PlanFormatVersion,
		PlanID:        spec.PlanID,
		Operation:     protocol.OpCreateIndex,
		Target: protocol.Target{
			Database: spec.Database,
			Table:    parent,
			Index:    &parentIndex,
		},
		CreatedAt:           createdAt,
		Nodes:               nodes,
		TopologyFingerprint: fingerprint,
	}
	if plan.PlanID == "" {
		plan.PlanID = derivePlanID(spec.Database, parent, parentIndex, fingerprint, createdAt)
	}

	if err := plan.Validate(); err != nil {
		return nil, err
	}
	if err := plan.Seal(); err != nil {
		return nil, err
	}
	return plan, nil
}

func (pl Planner) now() time.Time {
	if pl.Now != nil {
		return pl.Now()
	}
	return time.Now()
}

func (pl Planner) claims() ClaimReader {
	if pl.Claims != nil {
		return pl.Claims
	}
	return NoClaims()
}

// ownership evaluates the shared destructive-action decision table for one
// object: the marker on it, and the live claim consulted only where the marker
// is missing ([protocol.DecideProvenanceDrop]).
func ownership(ctx context.Context, claims ClaimReader, object protocol.ObjectName, st IndexState) (protocol.DropVerdict, error) {
	marker, status := st.Marker()
	in := protocol.ProvenanceDropInput{Object: object, Status: status, Marker: marker}
	if status == protocol.MarkerAbsent {
		run, found, err := claims.ClaimsObject(ctx, object)
		if err != nil {
			return protocol.DropVerdict{}, fmt.Errorf("create-index: read the claim on %s: %w", object, err)
		}
		if found {
			in.ClaimRun = run
		}
	}
	return protocol.DecideProvenanceDrop(in), nil
}

// ---------------------------------------------------------------------------
// Topology validation (FR-PLAN-2, FR-PLAN-3, AC-11)
// ---------------------------------------------------------------------------

// checkResolution guards against a CatalogReader that answered about a
// different relation than the one asked for.
func checkResolution(asked, resolved protocol.ObjectName) error {
	if asked.Name != resolved.Name || (asked.Schema != "" && asked.Schema != resolved.Schema) {
		return protocol.ErrFailure.Detailf(
			"create-index: asked for topology of %s but the catalog resolved %s", asked, resolved)
	}
	return nil
}

// checkTopology rejects every topology v0.1 cannot plan, each with its own
// message, and all of them with exit code 15 (AC-11).
func checkTopology(topo protocol.TopologyInput, parent protocol.ObjectName) error {
	if topo.Root.RelKind != RelKindPartitionedTable {
		return protocol.ErrUnsupportedTopology.Detailf(
			"%s has relkind %q; CreatePartitionedIndex requires a partitioned table (relkind 'p')",
			parent, topo.Root.RelKind)
	}
	if !topo.Strategy.SupportedInV01() {
		return protocol.ErrUnsupportedTopology.Detailf(
			"%s is %s partitioned; v0.1 supports RANGE and LIST only (FR-PLAN-3). "+
				"A HASH-partitioned tree has no ordering that makes per-partition pacing meaningful",
			parent, topo.Strategy)
	}
	if len(topo.Partitions) == 0 {
		return protocol.ErrUnsupportedTopology.Detailf(
			"%s has no leaf partitions; there is nothing to index, and the parent index "+
				"created by CREATE INDEX ON ONLY would have no attach to make it valid", parent)
	}
	for _, p := range topo.Partitions {
		if p.IsDefault {
			return protocol.ErrUnsupportedTopology.Detailf(
				"%s has a DEFAULT partition, %s; v0.1 rejects it (FR-PLAN-3). "+
					"A DEFAULT partition can absorb rows for a range that is later added as its own "+
					"partition, so the leaf set is not stable across a long run", parent, p)
		}
	}
	for _, p := range topo.Partitions {
		if p.RelKind == RelKindPartitionedTable {
			return protocol.ErrUnsupportedTopology.Detailf(
				"%s is itself partitioned, so the tree rooted at %s has depth > 1; "+
					"v0.1 requires exactly 1 (FR-PLAN-2)", p, parent)
		}
		if p.RelKind != RelKindTable {
			return protocol.ErrUnsupportedTopology.Detailf(
				"partition %s has relkind %q; v0.1 supports ordinary table partitions (relkind 'r') only",
				p, p.RelKind)
		}
		if p.ParentOID != topo.Root.OID {
			return protocol.ErrUnsupportedTopology.Detailf(
				"partition %s has parent OID %d, not %s's OID %d, so the tree has depth > 1; "+
					"v0.1 requires exactly 1 (FR-PLAN-2)", p, p.ParentOID, parent, topo.Root.OID)
		}
	}
	return nil
}

// sortedPartitions orders the leaves by schema then name.
//
// The catalog returns partitions in whatever order the scan produced. Ordering
// them here is what makes the graph, and therefore the plan digest, a function
// of the catalog's content rather than of its physical layout. For a RANGE tree
// with conventional names it also happens to put the leaves in bound order,
// which is the order an operator expects to see them paced in.
func sortedPartitions(parts []protocol.RelationState) []protocol.ObjectName {
	out := make([]protocol.ObjectName, len(parts))
	for i, p := range parts {
		out[i] = protocol.NewObjectName(p.Schema, p.Name)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Schema != out[j].Schema {
			return out[i].Schema < out[j].Schema
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// childIndexNames generates the leaf index name for every partition
// (FR-PLAN-11) and proves the set is collision-free (FR-PLAN-13).
//
// The collision proof lives in [protocol.ChildIndexNamesQualified] and is
// applied per schema, which is where a collision can actually happen:
// PostgreSQL puts an index in its table's schema, so two partitions in
// different schemas cannot collide even when their names generate the same
// index name. Sharing that one generator with planner.InspectChildren is what
// keeps the plan path and the resume path agreeing on which trees are legal
// (AC-4).
//
// The two checks layered on top are specific to emitting a plan: the generated
// name must be a legal identifier, and it must not equal the parent index name.
func childIndexNames(parentIndex protocol.ObjectName, leaves []protocol.ObjectName) ([]protocol.ObjectName, error) {
	out, err := protocol.ChildIndexNamesQualified(parentIndex.Name, leaves)
	if err != nil {
		return nil, err
	}
	for i, child := range out {
		if err := child.Validate(); err != nil {
			return nil, protocol.ErrInvalidIdentifier.Detailf(
				"generated child index name for %s: %v", leaves[i], err)
		}
		if child == parentIndex {
			return nil, protocol.ErrNameCollision.Detailf(
				"the child index name generated for %s equals the parent index name %s",
				leaves[i], parentIndex)
		}
	}
	return out, nil
}

// checkRoleMembership fails the plan at exit 16 if the connected role is not a
// member of any owning role it would need (FR-PLAN-10, AC-12).
func checkRoleMembership(ctx context.Context, cat CatalogReader, role string, relations []protocol.ObjectName) error {
	member, err := cat.OwnedByMemberRole(ctx, role, relations)
	if err != nil {
		return fmt.Errorf("create-index: check role membership: %w", err)
	}
	for _, r := range relations {
		if !member[r] {
			return protocol.ErrInsufficientPrivilege.Detailf(
				"role %q is not a member of the owning role of %s; "+
					"every relation the run modifies needs it (FR-PLAN-10, AC-12)", role, r)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Existing-state classification (FR-PLAN-4, FR-PLAN-5, FR-PLAN-6, FR-PLAN-7)
// ---------------------------------------------------------------------------

// classifyParentIndex decides whether index.create_parent_invalid is still
// needed, and refuses to adopt a parent index that is not ours.
//
// A parent index that exists and is INVALID is the normal mid-build state, so
// adopting it is exactly what resume must do. Adopting one PartitionCTL cannot
// prove it created is not: the precondition in TRD §7.2.13 is that the target
// index name is free *or* matches an in-progress build with provenance.
func classifyParentIndex(
	ctx context.Context,
	claims ClaimReader,
	states map[protocol.ObjectName]IndexState,
	parent, parentIndex protocol.ObjectName,
) (needCreate bool, err error) {
	st, exists := states[parentIndex]
	if !exists {
		return true, nil
	}
	if !st.IsPartitioned {
		return false, protocol.ErrFailure.Detailf(
			"the index name %s is already taken by an ordinary index; "+
				"CreatePartitionedIndex needs the name for a partitioned index on %s", parentIndex, parent)
	}
	if st.Relation != parent {
		return false, protocol.ErrFailure.Detailf(
			"the index name %s already exists on %s, not on %s", parentIndex, st.Relation, parent)
	}
	if st.Healthy() {
		return false, nil
	}
	v, err := ownership(ctx, claims, parentIndex, st)
	if err != nil {
		return false, err
	}
	if !v.Satisfied() {
		return false, protocol.ErrAuthorizationUnsatisfied.Detailf(
			"%s already exists and is INVALID: %s. This is an in-progress build belonging to something "+
				"else. Halting with no plan: resolve it by hand, then re-plan (FR-PLAN-7, AC-6)",
			parentIndex, v.Reason)
	}
	return false, nil
}

// leafChain returns the nodes still needed for one leaf partition, in order, or
// nil when that leaf is already done (FR-PLAN-5).
//
// The five shapes it can return are the whole of the operation's idempotence:
//
//   - already built, valid and attached: no nodes at all;
//   - built and valid but unattached: verify → attach → wait;
//   - absent: create → verify → attach → wait;
//   - INVALID and ours: drop → create → verify → attach → wait
//     (FR-PLAN-6, AC-5);
//   - INVALID and not provably ours: no plan at all (FR-PLAN-7, AC-6).
func (pl Planner) leafChain(
	ctx context.Context,
	claims ClaimReader,
	spec Specification,
	leaf, child, parentIndex protocol.ObjectName,
	states map[protocol.ObjectName]IndexState,
	pages int64,
	root protocol.NodeID,
) ([]protocol.Node, error) {
	st, exists := states[child]

	needDrop := false
	needCreate := true

	if exists {
		if st.Relation != leaf {
			return nil, protocol.ErrFailure.Detailf(
				"the child index name %s generated for partition %s already exists on %s. "+
					"Halting: building over it would corrupt an unrelated object", child, leaf, st.Relation)
		}
		if st.IsPartitioned {
			return nil, protocol.ErrFailure.Detailf(
				"the child index name %s generated for partition %s is a partitioned index, "+
					"not a leaf index. Halting", child, leaf)
		}
		if st.AttachedTo != nil && *st.AttachedTo != parentIndex {
			return nil, protocol.ErrFailure.Detailf(
				"%s is already attached to %s, not to %s. Halting: PostgreSQL has no "+
					"ALTER INDEX ... DETACH PARTITION, so this cannot be undone by the tool (TRD §7.2.10)",
				child, *st.AttachedTo, parentIndex)
		}
		switch {
		case st.Healthy() && st.AttachedTo != nil:
			// Built, verified and attached. Nothing remains (FR-PLAN-5).
			return nil, nil
		case st.Healthy():
			// Built but not yet attached: the interruption landed between
			// CREATE INDEX CONCURRENTLY and ATTACH PARTITION. Rebuilding would
			// be hours of wasted work, so only the tail of the chain is
			// emitted.
			needCreate = false
		case st.AttachedTo != nil:
			// INVALID and attached. An attached child index is a dependency of
			// its partitioned parent and cannot be dropped individually, and
			// there is no DETACH (TRD §7.2.10), so no graph this planner can
			// emit will fix it.
			return nil, protocol.ErrFailure.Detailf(
				"%s is attached to %s but is not valid. PostgreSQL cannot drop an attached child "+
					"index individually and offers no ALTER INDEX ... DETACH PARTITION (TRD §7.2.10), "+
					"so this needs manual repair: DROP INDEX %s takes AccessExclusiveLock on the whole tree",
				child, parentIndex, parentIndex.Quoted())
		default:
			// INVALID and unattached: the wreckage of an interrupted CREATE
			// INDEX CONCURRENTLY. Droppable online, but only with proof we
			// created it.
			v, err := ownership(ctx, claims, child, st)
			if err != nil {
				return nil, err
			}
			if !v.Satisfied() {
				return nil, protocol.ErrAuthorizationUnsatisfied.Detailf(
					"%s on %s is INVALID: %s. Halting with no plan: an INVALID index this tool cannot "+
						"prove it created is never dropped (FR-PLAN-7, AC-6, NFR-REL-3)", child, leaf, v.Reason)
			}
			needDrop = true
		}
	}

	chain := make([]protocol.Node, 0, 5)
	prev := root
	add := func(n protocol.Node) {
		n.DependsOn = []protocol.NodeID{prev}
		chain = append(chain, n)
		prev = n.ID
	}

	if needDrop {
		add(dropNode(leaf, child))
	}
	if needCreate {
		add(createNode(leaf, child, parentIndex, spec.Definition, estimateBuildSeconds(pages, spec.buildRate())))
	}
	add(leafVerifyNode(leaf, child))
	add(attachNode(leaf, child, parentIndex))
	add(waitNode(leaf, spec.PaceSeconds))
	return chain, nil
}

// ---------------------------------------------------------------------------
// Node construction
// ---------------------------------------------------------------------------

// assertNode carries every precondition from TRD §7.2.13 in a single
// catalog.assert evaluated before anything else runs.
//
// The assertions are ordered so the cheapest and most structural failures
// surface first, and each carries the exit code its failure class maps to:
// unsupported topology is 15, insufficient privilege is 16 (TRD §7.2.12).
func assertNode(
	role string,
	parent, parentIndex protocol.ObjectName,
	strategy protocol.PartitionStrategy,
	relations []protocol.ObjectName,
) protocol.Node {
	parentRef := parent
	indexRef := parentIndex

	assertions := []protocol.Assertion{
		{
			Assertion:   protocol.AssertRelationIsPartitioned,
			Relation:    &parentRef,
			FailureCode: protocol.ExitUnsupportedTopology,
			Message:     parent.String() + " must be a partitioned table (relkind 'p')",
		},
		{
			Assertion:   protocol.AssertPartitionStrategy,
			Relation:    &parentRef,
			Expected:    []string{string(protocol.StrategyRange), string(protocol.StrategyList)},
			FailureCode: protocol.ExitUnsupportedTopology,
			Message: fmt.Sprintf("%s must be RANGE or LIST partitioned; planned against %s (FR-PLAN-3)",
				parent, strategy),
		},
		{
			Assertion:   protocol.AssertPartitionDepth,
			Relation:    &parentRef,
			Expected:    []string{"1"},
			FailureCode: protocol.ExitUnsupportedTopology,
			Message:     "the partition tree rooted at " + parent.String() + " must be exactly one level deep (FR-PLAN-2)",
		},
		{
			Assertion:   protocol.AssertNoDefaultPartition,
			Relation:    &parentRef,
			FailureCode: protocol.ExitUnsupportedTopology,
			Message:     parent.String() + " must have no DEFAULT partition (FR-PLAN-3)",
		},
	}

	// Role membership, over the parent and every leaf. It is one precondition
	// expressed as one assertion per relation, because ownership is per
	// relation and a run that can write to the parent but not to a leaf fails
	// halfway through (FR-PLAN-10, AC-12).
	for _, r := range relations {
		rel := r
		assertions = append(assertions, protocol.Assertion{
			Assertion:   protocol.AssertRoleMembership,
			Relation:    &rel,
			Role:        role,
			FailureCode: protocol.ExitInsufficientPrivilege,
			Message:     "role " + role + " must be a member of the owning role of " + rel.String(),
		})
	}

	assertions = append(assertions, protocol.Assertion{
		Assertion:   protocol.AssertIndexNameAvailable,
		Relation:    &parentRef,
		Index:       &indexRef,
		FailureCode: protocol.ExitAuthorizationUnsatisfied,
		Message: parentIndex.String() + " must be free, or be an in-progress build with PartitionCTL " +
			"provenance (FR-PLAN-7)",
	})

	return protocol.Node{
		ID:          nodeAssert,
		Kind:        protocol.KindCatalogAssert,
		Params:      &protocol.CatalogAssertParams{Assertions: assertions},
		RenderedSQL: renderAssertComment(len(assertions)),
	}
}

// parentIndexNode issues CREATE INDEX ON ONLY <parent>: catalog-only, no
// partition data scanned, and ONLY is what stops PostgreSQL recursing into the
// leaves. The resulting index is deliberately INVALID for the whole build.
func parentIndexNode(parent, parentIndex protocol.ObjectName, def protocol.IndexDefinition) protocol.Node {
	params := &protocol.CreateParentInvalidParams{
		Parent:     parent,
		Index:      parentIndex,
		Definition: def,
	}
	return withMarkerPreview(protocol.Node{
		ID:               nodeParentIndex,
		Kind:             protocol.KindIndexCreateParentInvalid,
		Params:           params,
		DependsOn:        []protocol.NodeID{nodeAssert},
		RenderedSQL:      renderCreateParentInvalid(params),
		EstimatedSeconds: catalogOnlySeconds,
	})
}

// dropNode removes the INVALID leaf index left by an interrupted CREATE INDEX
// CONCURRENTLY, so the rebuild has a free name (FR-PLAN-6, AC-5).
//
// The authorization it carries is [protocol.AuthProvenance] and it is a
// proposal only: the executor re-evaluates it against live state immediately
// before dispatch and halts if it is unsatisfied, whatever the plan asserts
// (FR-AUTH-5, INV-2).
func dropNode(leaf, child protocol.ObjectName) protocol.Node {
	relation := leaf
	params := &protocol.DropConcurrentlyParams{
		Index:    child,
		Relation: &relation,
		Reason:   protocol.DropInvalidBuild,
	}
	return protocol.Node{
		ID:          nodeID("drop", leaf),
		Kind:        protocol.KindIndexDropConcurrently,
		Params:      params,
		RenderedSQL: renderDropConcurrently(params),
		Authorization: &protocol.Authorization{
			Mode:     protocol.AuthProvenance,
			Object:   child,
			Relation: &relation,
			Note: "an INVALID index left by an interrupted CREATE INDEX CONCURRENTLY, " +
				"with a committed PartitionCTL provenance record (FR-PLAN-6, AC-5)",
		},
		EstimatedSeconds: catalogOnlySeconds,
	}
}

// createNode builds one leaf index. This is the node that runs for hours: two
// table scans, and it waits for every transaction that could see the index.
func createNode(
	leaf, child, parentIndex protocol.ObjectName,
	def protocol.IndexDefinition,
	seconds int,
) protocol.Node {
	pi := parentIndex
	params := &protocol.CreateConcurrentlyParams{
		Partition:   leaf,
		Index:       child,
		Definition:  def,
		ParentIndex: &pi,
	}
	return withMarkerPreview(protocol.Node{
		ID:               nodeID("create", leaf),
		Kind:             protocol.KindIndexCreateConcurrently,
		Params:           params,
		RenderedSQL:      renderCreateConcurrently(params),
		EstimatedSeconds: seconds,
	})
}

// leafVerifyNode checks the child before it is attached: indisvalid, indisready
// and indislive (FR-VER-1). Attaching an index that is not valid would put the
// tree into a state PostgreSQL gives no online way out of.
func leafVerifyNode(leaf, child protocol.ObjectName) protocol.Node {
	idx := child
	return protocol.Node{
		ID:   nodeID("verify", leaf),
		Kind: protocol.KindIndexVerify,
		Params: &protocol.VerifyParams{Checks: []protocol.VerifyCheck{{
			Check:   protocol.CheckIndexValid,
			Index:   &idx,
			Message: child.String() + " must be valid, ready and live before it is attached (FR-VER-1)",
		}}},
		RenderedSQL: renderLeafVerifyComment(child),
	}
}

// attachNode issues ALTER INDEX <parent> ATTACH PARTITION <child>. Catalog
// only. On the final attach PostgreSQL marks the parent index valid by itself;
// no statement is issued for that.
func attachNode(leaf, child, parentIndex protocol.ObjectName) protocol.Node {
	params := &protocol.AttachParams{ParentIndex: parentIndex, ChildIndex: child}
	return withMarkerPreview(protocol.Node{
		ID:               nodeID("attach", leaf),
		Kind:             protocol.KindIndexAttach,
		Params:           params,
		RenderedSQL:      renderAttach(params),
		EstimatedSeconds: catalogOnlySeconds,
	})
}

// waitNode is the planner-emitted pause between leaves (FR-ORD-3). It is a node
// so that every pause is visible in the plan the operator reviews; the executor
// introduces no delays of its own.
func waitNode(leaf protocol.ObjectName, seconds int) protocol.Node {
	return protocol.Node{
		ID:   nodeID("wait", leaf),
		Kind: protocol.KindWait,
		Params: &protocol.WaitParams{
			Seconds: seconds,
			Reason:  "pacing after " + leaf.String() + " attached (FR-ORD-3)",
		},
		RenderedSQL:      renderWaitComment(seconds),
		EstimatedSeconds: seconds,
	}
}

// finalVerifyNode is the node with N incoming edges that TRD §7.2.2 says a
// barrier already is. It proves the whole build: the parent index is valid,
// the leaf index count equals the partition count (FR-VER-3, FR-VER-4), and
// every leaf index is attached in pg_inherits (FR-VER-2).
//
// The attachment checks cover every leaf, including the ones this plan emitted
// no work for. That is what makes a converged plan a proof of the end state
// rather than a proof that nothing happened.
func finalVerifyNode(parentIndex protocol.ObjectName, children []protocol.ObjectName, deps []protocol.NodeID) protocol.Node {
	pi := parentIndex
	count := len(children)
	checks := make([]protocol.VerifyCheck, 0, 2+len(children))
	checks = append(checks,
		protocol.VerifyCheck{
			Check:       protocol.CheckParentIndexValid,
			ParentIndex: &pi,
			Message: parentIndex.String() + " must be valid; PostgreSQL marks it so on the final attach " +
				"(FR-VER-3)",
		},
		protocol.VerifyCheck{
			Check:         protocol.CheckLeafIndexCount,
			ParentIndex:   &pi,
			ExpectedCount: &count,
			Message: fmt.Sprintf("%s must have exactly %d leaf index(es), one per partition (FR-VER-4)",
				parentIndex, count),
		},
	)
	for i := range children {
		child := children[i]
		checks = append(checks, protocol.VerifyCheck{
			Check:       protocol.CheckIndexAttached,
			Index:       &child,
			ParentIndex: &pi,
			Message:     child.String() + " must be attached to " + parentIndex.String() + " (FR-VER-2)",
		})
	}
	return protocol.Node{
		ID:          nodeFinalVerify,
		Kind:        protocol.KindIndexVerify,
		Params:      &protocol.VerifyParams{Checks: checks},
		DependsOn:   deps,
		RenderedSQL: renderFinalVerifyComment(parentIndex, count),
	}
}

// ---------------------------------------------------------------------------
// Plan identity
// ---------------------------------------------------------------------------

// planIDDomain separates this hash from every other SHA-256 in the system.
const planIDDomain = "partitionctl.planid.create-index.v1"

// derivePlanID produces a stable, collision-resistant plan identity from the
// plan's own identity fields, so that planning is a pure function of the
// catalog and the specification. Drawing randomness here would make two
// otherwise identical plan files differ, which would defeat reviewing a plan as
// a diff.
func derivePlanID(database string, table, index protocol.ObjectName, fingerprint string, createdAt protocol.Timestamp) protocol.PlanID {
	h := sha256.New()
	for _, field := range []string{
		planIDDomain,
		string(protocol.OpCreateIndex),
		database,
		table.String(),
		index.String(),
		fingerprint,
		createdAt.Canonical(),
	} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(field)))
		h.Write(n[:])
		h.Write([]byte(field))
	}
	return protocol.PlanID(string(protocol.OpCreateIndex) + "-" + hex.EncodeToString(h.Sum(nil))[:16])
}
