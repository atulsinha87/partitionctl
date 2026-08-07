package cli

import (
	"context"

	"github.com/atulsinha/partitionctl/engine/executor"
	"github.com/atulsinha/partitionctl/engine/planner"
	"github.com/atulsinha/partitionctl/engine/protocol"
	"github.com/atulsinha/partitionctl/engine/verifier"
	createindex "github.com/atulsinha/partitionctl/operations/create-index"
)

// operationCatalog adapts [planner.CatalogReader], plus the topology the host
// has already discovered, to the batched reader
// [createindex.CatalogReader] declares.
//
// The two interfaces answer the same questions in different shapes.
// planner.CatalogReader is organized around OIDs, because that is what
// pg_partition_tree and pg_index join on; createindex.CatalogReader is
// organized around names, because a planner reasons about the index names it
// generates. Translating is this type's job, and it does so without a single
// extra round trip for topology or page counts: the host read both before the
// operation was asked for a node, and re-reading them would risk a torn view
// across two snapshots as well as costing time NFR-PERF-1 does not have.
type operationCatalog struct {
	reader planner.CatalogReader
	topo   planner.Topology

	// indexes caches the one pg_index pass over the whole tree. Discovery must
	// be O(1) queries in the number of partitions to plan a thousand leaves
	// inside five seconds (NFR-PERF-1), so the per-name lookups the operation
	// makes are served from here rather than from the server.
	indexes map[protocol.ObjectName]planner.Index
	loaded  bool
}

var _ createindex.CatalogReader = (*operationCatalog)(nil)

// Topology implements [createindex.CatalogReader]. It returns the tree the host
// discovered and validated, so the operation cannot plan against a different
// one than the fingerprint describes.
func (c *operationCatalog) Topology(ctx context.Context, table protocol.ObjectName) (protocol.TopologyInput, error) {
	if table.Name != c.topo.Root.Name.Name ||
		(table.Schema != "" && table.Schema != c.topo.Root.Name.Schema) {
		return protocol.TopologyInput{}, protocol.ErrFailure.Detailf(
			"the operation asked for the topology of %s but the host discovered %s", table, c.topo.Root.Name)
	}
	return c.topo.Input(), nil
}

// Indexes implements [createindex.CatalogReader] (FR-PLAN-4).
//
// A name absent from the result does not exist, which is how the planner learns
// there is work to do. That makes the "absent" answer load-bearing, so the
// underlying read covers the whole tree in one pass rather than probing name by
// name: a probe that failed for a reason other than absence would look like
// absence.
func (c *operationCatalog) Indexes(ctx context.Context, names []protocol.ObjectName) (map[protocol.ObjectName]createindex.IndexState, error) {
	if err := c.load(ctx); err != nil {
		return nil, err
	}
	out := make(map[protocol.ObjectName]createindex.IndexState, len(names))
	for _, n := range names {
		idx, ok := c.indexes[n]
		if !ok {
			continue
		}
		out[n] = indexStateOf(idx, c.indexes)
	}
	return out, nil
}

// RelationPages implements [createindex.CatalogReader] (FR-PLAN-9). The page
// counts come from the discovery pass; a relation the host did not discover
// estimates as zero, which the interface documents as the correct answer,
// because an estimate is advisory and drives the ETA alone.
func (c *operationCatalog) RelationPages(ctx context.Context, names []protocol.ObjectName) (map[protocol.ObjectName]int64, error) {
	out := make(map[protocol.ObjectName]int64, len(names))
	byName := make(map[protocol.ObjectName]int64, c.topo.LeafCount()+1)
	for _, r := range c.topo.Relations() {
		byName[r.Name] = r.RelPages
	}
	for _, n := range names {
		if pages, ok := byName[n]; ok {
			out[n] = pages
		}
	}
	return out, nil
}

// OwnedByMemberRole implements [createindex.CatalogReader] (FR-PLAN-10, AC-12).
//
// A relation the host did not discover is reported as not owned, which is the
// direction that fails closed: the operation asks about the relations it will
// modify, and one that is not in the validated tree is one the host never
// checked.
func (c *operationCatalog) OwnedByMemberRole(ctx context.Context, role string, names []protocol.ObjectName) (map[protocol.ObjectName]bool, error) {
	owners := make(map[protocol.ObjectName]uint32, c.topo.LeafCount()+1)
	oids := make([]uint32, 0, c.topo.LeafCount()+1)
	seen := make(map[uint32]bool)
	for _, r := range c.topo.Relations() {
		owners[r.Name] = r.OwnerOID
		if !seen[r.OwnerOID] {
			seen[r.OwnerOID] = true
			oids = append(oids, r.OwnerOID)
		}
	}
	memberships, err := c.reader.RoleMemberships(ctx, role, oids)
	if err != nil {
		return nil, err
	}
	out := make(map[protocol.ObjectName]bool, len(names))
	for _, n := range names {
		owner, known := owners[n]
		if !known {
			out[n] = false
			continue
		}
		out[n] = memberships[owner].IsMember
	}
	return out, nil
}

// load performs the single pg_index pass over the tree.
func (c *operationCatalog) load(ctx context.Context) error {
	if c.loaded {
		return nil
	}
	idxs, err := c.reader.IndexesOnRelations(ctx, c.topo.RelationOIDs())
	if err != nil {
		return err
	}
	c.indexes = make(map[protocol.ObjectName]planner.Index, len(idxs))
	for _, i := range idxs {
		c.indexes[i.Name] = i
	}
	c.loaded = true
	return nil
}

// indexStateOf projects a planner.Index into the operation's view of it.
//
// AttachedTo is a name in one vocabulary and an OID in the other, so it is
// resolved against the same pass: an attachment to an index outside the tree
// cannot be named, and is reported as unattached-to-anything-we-know rather
// than guessed at. The operation then refuses to build over it, which is the
// safe outcome either way.
func indexStateOf(i planner.Index, all map[protocol.ObjectName]planner.Index) createindex.IndexState {
	st := createindex.IndexState{
		Index:         i.Name,
		Relation:      i.Table,
		IsPartitioned: i.Kind == planner.RelKindPartitionedIndex,
		Valid:         i.IsValid,
		Ready:         i.IsReady,
		Live:          i.IsLive,
		Comment:       i.Comment,
	}
	if i.ParentIndexOID != 0 {
		for name, cand := range all {
			if cand.OID == i.ParentIndexOID {
				n := name
				st.AttachedTo = &n
				break
			}
		}
		if st.AttachedTo == nil {
			// Attached to something outside the discovered tree. Naming it
			// would take another query, and every caller treats "attached to
			// not-my-parent" identically: halt.
			unknown := protocol.NewObjectName("", "?unknown-parent-index")
			st.AttachedTo = &unknown
		}
	}
	return st
}

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
