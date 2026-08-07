package planner

import (
	"context"
	"fmt"
	"sort"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// Topology is a discovered and validated partition tree: a single-level RANGE
// or LIST tree with no DEFAULT partition, which is the whole of what v0.1
// supports (§3.1).
//
// Get one from [Discover]. A Topology that came from there has already
// satisfied FR-PLAN-1, FR-PLAN-2 and FR-PLAN-3, which is why the operation
// planners downstream never re-check any of it.
type Topology struct {
	// Root is the partitioned parent table.
	Root Relation
	// Strategy is the root's partitioning strategy: RANGE or LIST.
	Strategy protocol.PartitionStrategy
	// Leaves are the partitions, sorted by schema then name. The order is
	// deterministic so that two planning passes over the same catalog produce
	// byte-identical node sequences and therefore the same digest.
	Leaves []Relation
	// Depth is the deepest pg_partition_tree level found: 1 for a validated
	// tree, and 0 for the childless case [AllowNoPartitions] permits.
	Depth int
}

// LeafCount is how many partitions the tree has.
func (t Topology) LeafCount() int { return len(t.Leaves) }

// LeafNames returns the partitions' bare relation names, in leaf order.
//
// Bare names are not sufficient to generate child index names: two partitions
// of one parent may share a bare name in different schemas, which is legal and
// not a collision. Use [Topology.LeafObjectNames] with
// [protocol.ChildIndexNamesQualified] for that.
func (t Topology) LeafNames() []string {
	out := make([]string, len(t.Leaves))
	for i, l := range t.Leaves {
		out[i] = l.Name.Name
	}
	return out
}

// LeafObjectNames returns the partitions' schema-qualified names, in leaf
// order. It is the input to [protocol.ChildIndexNamesQualified].
func (t Topology) LeafObjectNames() []protocol.ObjectName {
	out := make([]protocol.ObjectName, len(t.Leaves))
	for i, l := range t.Leaves {
		out[i] = l.Name
	}
	return out
}

// LeafOIDs returns the partitions' OIDs, in leaf order.
func (t Topology) LeafOIDs() []uint32 {
	out := make([]uint32, len(t.Leaves))
	for i, l := range t.Leaves {
		out[i] = l.OID
	}
	return out
}

// Relations returns the root followed by every leaf: exactly the set whose
// ownership must be validated (FR-PLAN-10) and whose indexes must be inspected
// (FR-PLAN-4).
func (t Topology) Relations() []Relation {
	out := make([]Relation, 0, len(t.Leaves)+1)
	out = append(out, t.Root)
	return append(out, t.Leaves...)
}

// RelationOIDs returns the root's OID followed by every leaf's.
func (t Topology) RelationOIDs() []uint32 {
	out := make([]uint32, 0, len(t.Leaves)+1)
	out = append(out, t.Root.OID)
	return append(out, t.LeafOIDs()...)
}

// LeafByOID looks up a partition.
func (t Topology) LeafByOID(oid uint32) (Relation, bool) {
	for _, l := range t.Leaves {
		if l.OID == oid {
			return l, true
		}
	}
	return Relation{}, false
}

// TotalRelPages sums pg_class.relpages across the leaves, for a whole-run
// estimate (FR-PLAN-9).
func (t Topology) TotalRelPages() int64 {
	var total int64
	for _, l := range t.Leaves {
		total += l.RelPages
	}
	return total
}

// Input projects the topology into the fingerprint's input (FR-PLANFILE-4).
func (t Topology) Input() protocol.TopologyInput {
	parts := make([]protocol.RelationState, len(t.Leaves))
	for i, l := range t.Leaves {
		parts[i] = l.State()
	}
	return protocol.TopologyInput{
		Root:       t.Root.State(),
		Strategy:   t.Strategy,
		Partitions: parts,
	}
}

// Fingerprint computes the topology fingerprint (FR-PLANFILE-4).
func (t Topology) Fingerprint() (string, error) { return t.Input().Fingerprint() }

// DiscoverOption relaxes one of [Discover]'s rejections for an operation that
// can genuinely cope with the topology in question.
//
// There is deliberately no option for HASH, multi-level or DEFAULT: those are
// rejections of the whole v0.1 design (§3.2), not of one operation.
type DiscoverOption func(*discoverConfig)

type discoverConfig struct {
	allowNoPartitions bool
}

// AllowNoPartitions accepts a partitioned table that currently has no
// partitions.
//
// [Discover] rejects one by default because an index build cannot converge
// there: a parent index created ON ONLY is marked valid on the final attach,
// and with nothing to attach it stays invalid forever. Removing an index has no
// such problem, so DropPartitionedIndex passes this and create and reindex do
// not.
func AllowNoPartitions() DiscoverOption {
	return func(c *discoverConfig) { c.allowNoPartitions = true }
}

// Discover reads the partition tree for root and validates it against
// everything v0.1 supports.
//
// Discovery is via pg_partition_tree() and never by parsing DDL text
// (FR-PLAN-1). Every rejection is a hard plan-time error carrying a distinct
// [TopologyCode] (FR-PLAN-2, FR-PLAN-3, AC-11), because a topology this binary
// mishandles must never become a partial run.
func Discover(ctx context.Context, cr CatalogReader, root protocol.ObjectName, opts ...DiscoverOption) (Topology, error) {
	var cfg discoverConfig
	for _, o := range opts {
		o(&cfg)
	}

	rootRel, err := cr.LookupRelation(ctx, root)
	if err != nil {
		return Topology{}, err
	}
	if rootRel.Kind != RelKindPartitionedTable {
		return Topology{}, topologyErr(CodeNotPartitioned, rootRel.Name.String(),
			fmt.Sprintf("pg_class.relkind is %q, want %q; PartitionCTL plans against partitioned tables only",
				rootRel.Kind, RelKindPartitionedTable))
	}

	entries, err := cr.PartitionTree(ctx, rootRel.OID)
	if err != nil {
		return Topology{}, err
	}
	if len(entries) == 0 {
		return Topology{}, ErrCatalogUnavailable.Detailf(
			"pg_partition_tree(%s) returned no rows; it always returns at least the root",
			rootRel.Name.String())
	}

	strategy, err := cr.PartitionStrategy(ctx, rootRel.OID)
	if err != nil {
		return Topology{}, err
	}
	switch {
	case strategy == protocol.StrategyHash:
		return Topology{}, topologyErr(CodeHashStrategy, rootRel.Name.String(),
			"HASH partitioning is not supported in v0.1; every partition would be indexed "+
				"but the modulus is not part of the plan's identity")
	case !strategy.SupportedInV01():
		return Topology{}, topologyErr(CodeUnsupportedStrategy, rootRel.Name.String(),
			fmt.Sprintf("partition strategy %q is not one of %s or %s",
				strategy, protocol.StrategyRange, protocol.StrategyList))
	}

	// Classify parent versus leaf (FR-PLAN-2). The root is the level-0 entry
	// pg_partition_tree always returns; every other entry must sit at level 1
	// and must itself be unpartitioned.
	var (
		discovered Relation
		foundRoot  bool
		leaves     []Relation
		maxLevel   int
	)
	for _, e := range entries {
		if e.Level > maxLevel {
			maxLevel = e.Level
		}
		if e.Level == 0 {
			if foundRoot {
				return Topology{}, ErrCatalogUnavailable.Detailf(
					"pg_partition_tree(%s) returned two level-0 rows", rootRel.Name.String())
			}
			discovered, foundRoot = e.Relation, true
			continue
		}
		if e.Level > 1 {
			return Topology{}, topologyErr(CodeMultiLevel, e.Name.String(),
				fmt.Sprintf("partition sits at pg_partition_tree level %d; v0.1 supports depth 1 exactly "+
					"(FR-PLAN-2). Plan against the sub-partitioned relation directly instead", e.Level))
		}
		if e.Kind.IsPartitioned() || !e.IsLeaf {
			return Topology{}, topologyErr(CodeMultiLevel, e.Name.String(),
				"partition is itself partitioned; v0.1 supports depth 1 exactly (FR-PLAN-2). "+
					"Plan against the sub-partitioned relation directly instead")
		}
		if !e.Kind.CanCarryIndex() {
			return Topology{}, topologyErr(CodeUnsupportedPartitionKind, e.Name.String(),
				fmt.Sprintf("partition has pg_class.relkind %q; only ordinary tables (%q) can carry "+
					"an index built by CREATE INDEX CONCURRENTLY", e.Kind, RelKindTable))
		}
		leaf := e.Relation
		leaf.IsDefault = leaf.IsDefault || IsDefaultBound(leaf.PartitionBound)
		leaves = append(leaves, leaf)
	}
	if !foundRoot {
		return Topology{}, ErrCatalogUnavailable.Detailf(
			"pg_partition_tree(%s) returned no level-0 row", rootRel.Name.String())
	}

	// Sort before the DEFAULT scan so the relation named in the error is
	// deterministic regardless of the order the server returned rows in.
	sort.Slice(leaves, func(i, j int) bool {
		if leaves[i].Name.Schema != leaves[j].Name.Schema {
			return leaves[i].Name.Schema < leaves[j].Name.Schema
		}
		return leaves[i].Name.Name < leaves[j].Name.Name
	})

	for _, l := range leaves {
		if l.IsDefault {
			return Topology{}, topologyErr(CodeDefaultPartition, l.Name.String(),
				"a DEFAULT partition is present; v0.1 rejects it (FR-PLAN-3) because rows can "+
					"move into it between plan and execute, so the indexed set would not be the planned set")
		}
	}

	if len(leaves) == 0 && !cfg.allowNoPartitions {
		return Topology{}, topologyErr(CodeNoPartitions, discovered.Name.String(),
			"the table is partitioned but has no partitions; a parent index created ON ONLY is "+
				"marked valid on the final attach, and with nothing to attach it would stay invalid forever")
	}

	// The root state used for the fingerprint is the one pg_partition_tree
	// reported, not the one LookupRelation reported, so that both sides of the
	// fingerprint come from the same query and the same snapshot.
	discovered.IsDefault = discovered.IsDefault || IsDefaultBound(discovered.PartitionBound)
	if discovered.Owner == "" {
		discovered.Owner = rootRel.Owner
	}

	return Topology{
		Root:     discovered,
		Strategy: strategy,
		Leaves:   leaves,
		Depth:    maxLevel,
	}, nil
}
