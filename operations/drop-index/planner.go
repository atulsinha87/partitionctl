package dropindex

import (
	"context"
	"errors"
	"fmt"

	"github.com/atulsinha/partitionctl/engine/planner"
	"github.com/atulsinha/partitionctl/engine/protocol"
)

// Node IDs. They are deterministic functions of what they act on, so a re-plan
// over an unchanged catalog produces the same graph and therefore the same
// digest, and so `status` output names the object rather than an ordinal.
const (
	nodeAssert      protocol.NodeID = "assert.preconditions"
	nodeDrop        protocol.NodeID = "drop.partitioned"
	nodeFinalVerify protocol.NodeID = "verify.final"
)

func orphanNodeID(child protocol.ObjectName) protocol.NodeID {
	return protocol.NodeID("drop.orphan:" + child.String())
}

// catalogOnlySeconds is the estimate for a node whose work is a catalog update.
// The drop's real cost is waiting for the lock, which is unbounded from the
// planner's side and is bounded at run time by lock_timeout plus backoff
// (FR-DROP-6); pretending to estimate it would be a fabricated number in the
// artifact the operator reviews.
const catalogOnlySeconds = 1

// Planner implements [planner.OperationPlanner] for DropPartitionedIndex.
//
// The zero value is the whole configuration. Everything the operation needs
// arrives on [planner.Request], because the host has already discovered the
// tree and checked role membership and an operation that re-derived either
// could disagree with the plan's own fingerprint.
type Planner struct{}

// Operation names the operation this planner implements.
func (Planner) Operation() protocol.Operation { return protocol.OpDropIndex }

// DiscoverOptions is what a host must be configured with to run this planner.
//
// [planner.AllowNoPartitions] is required and is specific to drop: a
// partitioned table whose partitions have all been detached still has a
// partitioned index on it, and that index is exactly the kind of leftover an
// operator wants removed. Refusing to plan for it would leave the only tool
// that can drop it unable to run.
func DiscoverOptions() []planner.DiscoverOption {
	return []planner.DiscoverOption{planner.AllowNoPartitions()}
}

// Plan emits the drop graph for one specification against one discovered
// topology.
//
// It returns an error and no nodes at all rather than a graph it cannot
// justify. The refusals are, in order: the index does not exist, is not a
// partitioned index, or is not on the discovered root (FR-DROP-1); the index
// backs a constraint, in which case the message names the constraint and the
// statement the operator must run instead (FR-DROP-2, AC-14); the
// acknowledgement is missing (FR-DROP-3, AC-13).
func (p Planner) Plan(ctx context.Context, req planner.Request) (planner.Result, error) {
	if req.Catalog == nil {
		return planner.Result{}, protocol.ErrFailure.Detailf("drop-index: request carries no catalog reader")
	}

	parent := req.Topology.Root.Name
	target := req.Spec.Index
	if target.Schema == "" {
		// PostgreSQL puts an index in its table's schema, so the parent's
		// schema is the only one the target can be in.
		target.Schema = parent.Schema
	}

	idx, err := resolveTarget(ctx, req.Catalog, target, parent, req.Topology.Root.OID)
	if err != nil {
		return planner.Result{}, err
	}
	if err := refuseConstraintBacked(idx, parent); err != nil {
		return planner.Result{}, err
	}
	leafCount := req.Topology.LeafCount()
	if err := requireConfirmation(req.Spec, parent, idx.Name, leafCount); err != nil {
		return planner.Result{}, err
	}

	orphans, err := findOrphans(ctx, req, idx)
	if err != nil {
		return planner.Result{}, err
	}

	nodes := make([]protocol.Node, 0, 3+len(orphans.drop))
	nodes = append(nodes, assertNode(req.Role, parent, idx.Name, req.Topology.Relations()))

	deps := []protocol.NodeID{nodeAssert}
	if len(orphans.drop) > 0 {
		deps = deps[:0]
		for _, o := range orphans.drop {
			nodes = append(nodes, orphanDropNode(o))
			deps = append(deps, orphanNodeID(o.index))
		}
	}

	nodes = append(nodes, dropNode(parent, idx.Name, leafCount, deps))
	nodes = append(nodes, finalVerifyNode(idx.Name, orphans.expectAbsent))

	return planner.Result{
		Nodes: nodes,
		Notes: notes(parent, idx.Name, leafCount, orphans),
	}, nil
}

// ---------------------------------------------------------------------------
// Refusals (FR-DROP-1, FR-DROP-2, FR-DROP-3)
// ---------------------------------------------------------------------------

// resolveTarget reads the named index and proves it is the object this
// operation can act on: a partitioned index (relkind 'I') on the discovered
// root (FR-DROP-1).
func resolveTarget(
	ctx context.Context,
	cat planner.CatalogReader,
	target, parent protocol.ObjectName,
	rootOID uint32,
) (planner.Index, error) {
	idx, err := cat.LookupIndex(ctx, target)
	if err != nil {
		if errors.Is(err, planner.ErrIndexNotFound) {
			return planner.Index{}, planner.ErrIndexNotFound.Detailf(
				"drop-index: %s does not exist, so there is no partitioned index on %s to drop (FR-DROP-1)",
				target, parent)
		}
		return planner.Index{}, fmt.Errorf("drop-index: resolve index %s: %w", target, err)
	}
	if idx.Kind != planner.RelKindPartitionedIndex {
		return planner.Index{}, protocol.ErrUnsupportedTopology.Detailf(
			"drop-index: %s has relkind %q; DropPartitionedIndex acts on a partitioned index "+
				"(relkind 'I') only. An ordinary index is dropped online with "+
				"DROP INDEX CONCURRENTLY %s and needs no plan (FR-DROP-1)",
			idx.Name, idx.Kind, idx.Name.Quoted())
	}
	if idx.TableOID != rootOID {
		return planner.Index{}, protocol.ErrUnsupportedTopology.Detailf(
			"drop-index: %s is an index on %s (OID %d), not on %s (OID %d). "+
				"Halting: dropping it would destroy an index family the specification does not name (FR-DROP-1)",
			idx.Name, idx.Table, idx.TableOID, parent, rootOID)
	}
	return idx, nil
}

// refuseConstraintBacked halts on an index PostgreSQL will not let DROP INDEX
// touch, and names the statement that does work (FR-DROP-2, AC-14).
//
// The set covers contype 'x' as well as 'p' and 'u'. FR-DROP-2's text names
// only UNIQUE and PRIMARY KEY, and that is an under-specification rather than a
// decision: an EXCLUDE constraint owns its index exactly as a unique constraint
// does, DROP INDEX is rejected on it with the same message, and an operator who
// hit that path would get a raw PostgreSQL error out of the executor instead of
// the statement they need.
func refuseConstraintBacked(idx planner.Index, parent protocol.ObjectName) error {
	if !idx.ConstraintBacked() {
		return nil
	}
	return protocol.ErrFailure.Detailf(
		"drop-index: %s is the index behind the %s constraint %s on %s, and PostgreSQL does not "+
			"permit DROP INDEX on a constraint's index. Run this instead:\n"+
			"    ALTER TABLE %s DROP CONSTRAINT %s;\n"+
			"That statement takes the same AccessExclusiveLock this plan would have taken, so weigh it "+
			"the same way (FR-DROP-2, AC-14)",
		idx.Name, constraintKindName(idx.ConstraintType), idx.ConstraintName, parent,
		parent.Quoted(), protocol.QuoteIdentifier(idx.ConstraintName))
}

// constraintKindName spells pg_constraint.contype for a human.
func constraintKindName(contype string) string {
	switch contype {
	case "p":
		return "PRIMARY KEY"
	case "u":
		return "UNIQUE"
	case "x":
		return "EXCLUDE"
	case "":
		return "unknown"
	}
	return "contype " + contype
}

// requireConfirmation enforces FR-DROP-3 at plan time, so an operator can never
// end up holding an AccessExclusiveLock on their largest table without having
// said the words first (AC-13, risk R11).
//
// protocol.Plan.Validate enforces the same thing on the artifact. Checking it
// here as well is not redundant: it produces a message that names the flag and
// the blast radius instead of an invalid-plan error, and it fails before any
// further catalog work.
func requireConfirmation(spec planner.Specification, parent, index protocol.ObjectName, leafCount int) error {
	if spec.Confirmed(protocol.ConfirmExclusiveLock) {
		return nil
	}
	return protocol.ErrAuthorizationUnsatisfied.Detailf(
		"drop-index: %s requires the %s acknowledgement, and the specification does not carry it. "+
			"DROP INDEX %s takes AccessExclusiveLock on %s and on all %d leaf partition(s) "+
			"simultaneously, blocking every read and write against the whole tree for as long as it "+
			"holds. Re-run `plan` with %s once that is acceptable (FR-DROP-3, AC-13)",
		index, protocol.ConfirmExclusiveLock, index.Quoted(), parent, leafCount,
		protocol.ConfirmExclusiveLock)
}

// ---------------------------------------------------------------------------
// Orphan discovery (TRD §7.2.13 step 1)
// ---------------------------------------------------------------------------

// orphan is one unattached leaf index that this plan will remove ahead of the
// parent drop.
type orphan struct {
	leaf    protocol.ObjectName
	index   protocol.ObjectName
	verdict protocol.DropVerdict
}

// orphanScan is the outcome of looking under every generated child name.
type orphanScan struct {
	// drop are the orphans this plan removes, in leaf order.
	drop []orphan
	// skipped explains, in leaf order, every generated name occupied by
	// something this plan will not touch.
	skipped []string
	// expectAbsent are the child names the final verify may assert are gone:
	// every generated name except the ones deliberately skipped.
	expectAbsent []protocol.ObjectName
	// attached counts the child names occupied by an index attached to the
	// target, which the parent's drop removes by cascade.
	attached int
}

// findOrphans looks under every generated child index name for the wreckage an
// abandoned CreatePartitionedIndex leaves behind.
//
// The cases under one generated name, and what each becomes:
//
//   - nothing there: nothing to do, and the final verify still asserts it,
//     which costs one catalog row and proves the name is clear;
//   - an index attached to the target: the parent's DROP INDEX cascades to it,
//     so no node is emitted and the final verify covers it;
//   - an unattached ordinary index PartitionCTL can prove it created: one
//     index.drop_concurrently under mode provenance;
//   - anything else — unattached but foreign, attached to a different index
//     family, or itself partitioned: skipped with a note.
//
// A skipped name is also dropped from the final verify's absent set. Asserting
// the absence of an object this plan deliberately declined to remove would
// guarantee the run reports failure after a drop that in fact succeeded, which
// is the same "a foreign index makes the drop impossible" outcome the skip
// exists to prevent, moved one node later.
func findOrphans(ctx context.Context, req planner.Request, target planner.Index) (orphanScan, error) {
	var scan orphanScan

	leaves := req.Topology.LeafObjectNames()
	if len(leaves) == 0 {
		return scan, nil
	}
	children, err := protocol.ChildIndexNamesQualified(target.Name.Name, leaves)
	if err != nil {
		return scan, err
	}

	// One batched pass, never one lookup per leaf: discovery has to stay O(1)
	// queries in the number of partitions (NFR-PERF-1), and Index.Comment rides
	// along with it so the ownership marker costs no extra round trip.
	all, err := req.Catalog.IndexesOnRelations(ctx, req.Topology.LeafOIDs())
	if err != nil {
		return scan, fmt.Errorf("drop-index: read indexes on the partitions of %s: %w",
			req.Topology.Root.Name, err)
	}
	byName := make(map[protocol.ObjectName]planner.Index, len(all))
	for _, ix := range all {
		byName[ix.Name] = ix
	}

	scan.expectAbsent = make([]protocol.ObjectName, 0, len(children))
	for i, child := range children {
		existing, present := byName[child]
		if !present {
			scan.expectAbsent = append(scan.expectAbsent, child)
			continue
		}
		if existing.AttachedTo(target.OID) {
			scan.attached++
			scan.expectAbsent = append(scan.expectAbsent, child)
			continue
		}
		if existing.Attached() {
			scan.skipped = append(scan.skipped, fmt.Sprintf(
				"skipped %s on %s: it is attached to a different partitioned index, so it is not this "+
					"index family's orphan and the drop does not cascade to it",
				child, leaves[i]))
			continue
		}
		if existing.Kind != planner.RelKindIndex {
			scan.skipped = append(scan.skipped, fmt.Sprintf(
				"skipped %s on %s: it has relkind %q, and DROP INDEX CONCURRENTLY is rejected on "+
					"anything but an ordinary index",
				child, leaves[i], existing.Kind))
			continue
		}

		verdict, err := orphanVerdict(ctx, req.Claims, child, existing)
		if err != nil {
			return scan, err
		}
		if !verdict.Satisfied() {
			// FR-AUTH-2 in the direction that matters here: not ours, so not
			// ours to drop — and not a reason to refuse the operator's own
			// drop either.
			scan.skipped = append(scan.skipped, fmt.Sprintf(
				"skipped %s on %s: %s. The parent index is still dropped; remove this one by hand "+
					"if it is yours",
				child, leaves[i], verdict.Reason))
			continue
		}
		scan.drop = append(scan.drop, orphan{leaf: leaves[i], index: child, verdict: verdict})
		scan.expectAbsent = append(scan.expectAbsent, child)
	}
	return scan, nil
}

// orphanVerdict evaluates the A.5.1 provenance row for one unattached leaf
// index: the marker on the object, and a live claim consulted only where the
// marker is missing.
func orphanVerdict(
	ctx context.Context,
	claims planner.ClaimLookup,
	object protocol.ObjectName,
	existing planner.Index,
) (protocol.DropVerdict, error) {
	marker, status := existing.Marker()
	in := protocol.ProvenanceDropInput{Object: object, Status: status, Marker: marker}
	if status == protocol.MarkerAbsent && claims != nil {
		run, found, err := claims.ClaimsObject(ctx, object)
		if err != nil {
			return protocol.DropVerdict{}, fmt.Errorf("drop-index: read the claim on %s: %w", object, err)
		}
		if found {
			in.ClaimRun = run
		}
	}
	return protocol.DecideProvenanceDrop(in), nil
}

// ---------------------------------------------------------------------------
// Node construction
// ---------------------------------------------------------------------------

// assertNode carries the operation's preconditions in a single catalog.assert
// evaluated before anything else runs.
//
// Every kind here already exists and is already evaluated by the CLI's
// assertion evaluator, so this operation adds no evaluator work. The plan-time
// refusals above and these assertions are deliberately the same predicates: the
// planner refuses on a snapshot, and the executor re-proves them against live
// state, because a plan is reviewed and committed and may run hours later.
func assertNode(role string, parent, index protocol.ObjectName, relations []planner.Relation) protocol.Node {
	parentRef := parent
	indexRef := index

	assertions := []protocol.Assertion{
		{
			Assertion:   protocol.AssertRelationIsPartitioned,
			Relation:    &parentRef,
			FailureCode: protocol.ExitUnsupportedTopology,
			Message:     parent.String() + " must be a partitioned table (relkind 'p')",
		},
	}

	// Role membership over the parent and every leaf. The drop takes
	// AccessExclusiveLock on all of them at once, so a role that owns the
	// parent but not one leaf cannot run it at all, and finding that out at
	// lock-acquisition time would mean discovering it while holding the tree.
	for _, r := range relations {
		rel := r.Name
		assertions = append(assertions, protocol.Assertion{
			Assertion:   protocol.AssertRoleMembership,
			Relation:    &rel,
			Role:        role,
			FailureCode: protocol.ExitInsufficientPrivilege,
			Message:     "role " + role + " must be a member of the owning role of " + rel.String(),
		})
	}

	assertions = append(assertions,
		protocol.Assertion{
			Assertion:   protocol.AssertIndexExists,
			Index:       &indexRef,
			FailureCode: protocol.ExitFailure,
			Message:     index.String() + " must exist (FR-DROP-1)",
		},
		protocol.Assertion{
			Assertion:   protocol.AssertIndexIsPartitioned,
			Relation:    &parentRef,
			Index:       &indexRef,
			FailureCode: protocol.ExitUnsupportedTopology,
			Message: index.String() + " must be a partitioned index (relkind 'I') on " +
				parent.String() + " (FR-DROP-1)",
		},
		protocol.Assertion{
			Assertion:   protocol.AssertIndexNotConstraintBacked,
			Relation:    &parentRef,
			Index:       &indexRef,
			FailureCode: protocol.ExitFailure,
			Message: index.String() + " must not back a PRIMARY KEY, UNIQUE or EXCLUDE constraint; " +
				"such an index is removed with ALTER TABLE ... DROP CONSTRAINT (FR-DROP-2, AC-14)",
		},
	)

	return protocol.Node{
		ID:          nodeAssert,
		Kind:        protocol.KindCatalogAssert,
		Params:      &protocol.CatalogAssertParams{Assertions: assertions},
		RenderedSQL: renderAssertComment(len(assertions)),
	}
}

// orphanDropNode removes one unattached leaf index left behind by an abandoned
// build, so the drop leaves no garbage under a name the tool generates.
//
// It runs before the parent drop rather than after, because after the parent is
// gone there is no partitioned index to derive the child names from and the
// orphan becomes indistinguishable from any other index on the leaf.
//
// The authorization is a proposal. The executor re-evaluates the same decision
// table against live state immediately before dispatch and halts if it no
// longer holds, whatever this plan asserts (FR-AUTH-5, INV-2).
func orphanDropNode(o orphan) protocol.Node {
	relation := o.leaf
	params := &protocol.DropConcurrentlyParams{
		Index:    o.index,
		Relation: &relation,
		Reason:   protocol.DropUnattachedOrphan,
	}
	return planner.Preview(protocol.Node{
		ID:        orphanNodeID(o.index),
		Kind:      protocol.KindIndexDropConcurrently,
		Params:    params,
		DependsOn: []protocol.NodeID{nodeAssert},
		Authorization: &protocol.Authorization{
			Mode:     protocol.AuthProvenance,
			Object:   o.index,
			Relation: &relation,
			Note: "an unattached leaf index left behind by an abandoned CreatePartitionedIndex; " +
				o.verdict.Reason,
		},
		EstimatedSeconds: catalogOnlySeconds,
	}, protocol.OpDropIndex)
}

// dropNode is the statement the whole operation exists to gate.
//
// Mode is explicit and nothing else: the operator named the index and supplied
// the acknowledgement, and that is the authorization. This node deliberately
// does not consult the ownership marker. An index PartitionCTL did not create
// is still the operator's to drop, and making provenance a precondition here
// would mean the tool could destroy only what it had built, which is not what
// anybody wants from a drop command.
func dropNode(parent, index protocol.ObjectName, leafCount int, deps []protocol.NodeID) protocol.Node {
	relation := parent
	params := &protocol.DropPartitionedParams{
		Parent:    parent,
		Index:     index,
		LeafCount: leafCount,
	}
	n := planner.Preview(protocol.Node{
		ID:        nodeDrop,
		Kind:      protocol.KindIndexDropPartitioned,
		Params:    params,
		DependsOn: deps,
		Authorization: &protocol.Authorization{
			Mode:                 protocol.AuthExplicit,
			Object:               index,
			Relation:             &relation,
			RequiredConfirmation: protocol.ConfirmExclusiveLock,
			Note: fmt.Sprintf(
				"the specification names %s and the operator supplied %s at plan time; the statement "+
					"takes AccessExclusiveLock on %s and all %d leaf partition(s), one relation at a "+
					"time and held cumulatively, so blocking begins at the first acquisition and "+
					"continues through every later wait (FR-DROP-3, FR-DROP-4, AC-13)",
				index, protocol.ConfirmExclusiveLock, parent, leafCount),
		},
		EstimatedSeconds: catalogOnlySeconds,
	}, protocol.OpDropIndex)
	n.RenderedSQL = dropPartitionedPreamble(params) + "\n" + n.RenderedSQL
	return n
}

// finalVerifyNode proves the end state: the parent index and every leaf index
// under a generated name are gone from pg_index (FR-DROP-7).
//
// Absence is the only thing there is to check. The statement is atomic, so
// there is no partial state to describe and no progress to report: it committed
// or it did not.
func finalVerifyNode(index protocol.ObjectName, children []protocol.ObjectName) protocol.Node {
	parentIdx := index
	checks := make([]protocol.VerifyCheck, 0, 1+len(children))
	checks = append(checks, protocol.VerifyCheck{
		Check:   protocol.CheckIndexAbsent,
		Index:   &parentIdx,
		Message: index.String() + " must be absent from pg_index (FR-DROP-7)",
	})
	for i := range children {
		child := children[i]
		checks = append(checks, protocol.VerifyCheck{
			Check: protocol.CheckIndexAbsent,
			Index: &child,
			Message: child.String() + " must be absent from pg_index: the drop cascades to every " +
				"attached leaf index, and this plan removed the unattached ones first (FR-DROP-7)",
		})
	}
	return protocol.Node{
		ID:          nodeFinalVerify,
		Kind:        protocol.KindIndexVerify,
		Params:      &protocol.VerifyParams{Checks: checks},
		DependsOn:   []protocol.NodeID{nodeDrop},
		RenderedSQL: renderFinalVerifyComment(index, len(children)),
	}
}

// ---------------------------------------------------------------------------
// Operator notes
// ---------------------------------------------------------------------------

// notes are the lines the `plan` command prints. They carry what the artifact
// cannot state structurally: the blast radius (FR-DROP-5), and every generated
// name this plan declined to touch.
func notes(parent, index protocol.ObjectName, leafCount int, scan orphanScan) []string {
	out := []string{
		fmt.Sprintf(
			"DROP INDEX %s takes AccessExclusiveLock on %s and on all %d leaf partition(s) "+
				"simultaneously. Every read and write against the whole tree blocks until it commits "+
				"(FR-DROP-5).",
			index.Quoted(), parent, leafCount),
		"The statement is atomic and irreversible in practice: rebuilding the index means running " +
			"CreatePartitionedIndex, which is hours of work, not a rollback (TRD §13.2.2).",
	}
	if leafCount == 0 {
		out = append(out,
			parent.String()+" has no partitions, so the drop locks the parent alone. The index is "+
				"planned anyway: a detached tree still leaves a partitioned index behind, and this is "+
				"the only operation that removes it.")
	}
	if scan.attached > 0 {
		out = append(out, fmt.Sprintf(
			"%d leaf index(es) are attached to %s and are removed by the same statement, by cascade. "+
				"PostgreSQL offers no ALTER INDEX ... DETACH PARTITION, so they cannot be dropped "+
				"individually first (FR-DROP-8, TRD §7.2.10).",
			scan.attached, index))
	}
	if n := len(scan.drop); n > 0 {
		out = append(out, fmt.Sprintf(
			"%d unattached orphan leaf index(es) are dropped online with DROP INDEX CONCURRENTLY "+
				"before the parent drop. An unattached index is not a dependency of the partitioned "+
				"parent, so the cascade would leave it behind (TRD §7.2.13).", n))
	}
	out = append(out, scan.skipped...)
	return out
}
