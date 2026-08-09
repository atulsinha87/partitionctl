package planner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// TestDiscoverSupportedTrees covers the two topologies v0.1 supports
// (FR-PLAN-1, FR-PLAN-2, §3.1).
func TestDiscoverSupportedTrees(t *testing.T) {
	tests := []struct {
		name     string
		strategy protocol.PartitionStrategy
		leaves   []string
	}{
		{"range", protocol.StrategyRange, []string{"orders_2026_01", "orders_2026_02", "orders_2026_03"}},
		{"list", protocol.StrategyList, []string{"orders_eu", "orders_us"}},
		{"single leaf range", protocol.StrategyRange, []string{"orders_only"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := tree(tc.strategy, tc.leaves...)
			topo, err := Discover(ctx(), f, name("public", "orders"))
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if topo.Strategy != tc.strategy {
				t.Errorf("Strategy = %q, want %q", topo.Strategy, tc.strategy)
			}
			if topo.Depth != 1 {
				t.Errorf("Depth = %d, want 1", topo.Depth)
			}
			if topo.LeafCount() != len(tc.leaves) {
				t.Fatalf("LeafCount = %d, want %d", topo.LeafCount(), len(tc.leaves))
			}
			if topo.Root.OID != rootOID {
				t.Errorf("Root.OID = %d, want %d", topo.Root.OID, rootOID)
			}
			if topo.Root.Kind != RelKindPartitionedTable {
				t.Errorf("Root.Kind = %q, want %q", topo.Root.Kind, RelKindPartitionedTable)
			}
			// The root is classified as parent, never as a leaf (FR-PLAN-2).
			for _, l := range topo.Leaves {
				if l.OID == topo.Root.OID {
					t.Fatalf("root %s appears in Leaves", l)
				}
				if l.ParentOID != rootOID {
					t.Errorf("leaf %s has ParentOID %d, want %d", l, l.ParentOID, rootOID)
				}
			}
		})
	}
}

// TestDiscoverLeafOrderIsDeterministic proves planning does not inherit the
// server's row order. Two planning passes must produce identical node
// sequences, or the digest is not reproducible.
func TestDiscoverLeafOrderIsDeterministic(t *testing.T) {
	f := tree(protocol.StrategyRange, "orders_c", "orders_a", "orders_b")
	topo := mustDiscover(t, f)

	want := []string{"orders_a", "orders_b", "orders_c"}
	got := topo.LeafNames()
	if len(got) != len(want) {
		t.Fatalf("LeafNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LeafNames = %v, want %v", got, want)
		}
	}

	// A second pass over the same catalog agrees.
	again := mustDiscover(t, f)
	for i := range again.Leaves {
		if again.Leaves[i].OID != topo.Leaves[i].OID {
			t.Fatalf("second Discover reordered leaves at %d", i)
		}
	}
}

// TestDiscoverRejections is the FR-PLAN-2 / FR-PLAN-3 / AC-11 table: every
// unsupported topology fails at plan time with its own distinct code.
func TestDiscoverRejections(t *testing.T) {
	tests := []struct {
		name  string
		build func() *FakeCatalog
		code  TopologyCode
	}{
		{
			name: "hash strategy",
			build: func() *FakeCatalog {
				return tree(protocol.StrategyHash, "orders_h0", "orders_h1")
			},
			code: CodeHashStrategy,
		},
		{
			name: "unknown strategy",
			build: func() *FakeCatalog {
				return tree(protocol.PartitionStrategy("SPIRAL"), "orders_s0")
			},
			code: CodeUnsupportedStrategy,
		},
		{
			name: "default partition",
			build: func() *FakeCatalog {
				f := tree(protocol.StrategyRange, "orders_2026_01")
				f.AddRelation(Relation{
					OID: leafBase + 50, Name: name("public", "orders_default"),
					Kind: RelKindTable, OwnerOID: ownerOID, ParentOID: rootOID,
					PartitionBound: "DEFAULT",
				})
				return f
			},
			code: CodeDefaultPartition,
		},
		{
			name: "default partition flagged without a bound",
			build: func() *FakeCatalog {
				f := tree(protocol.StrategyRange, "orders_2026_01")
				f.AddRelation(Relation{
					OID: leafBase + 50, Name: name("public", "orders_default"),
					Kind: RelKindTable, OwnerOID: ownerOID, ParentOID: rootOID,
					IsDefault: true,
				})
				return f
			},
			code: CodeDefaultPartition,
		},
		{
			name: "depth 2 sub-partitioned leaf",
			build: func() *FakeCatalog {
				f := tree(protocol.StrategyRange, "orders_2026_01")
				// orders_2026_02 is itself partitioned, with one grandchild.
				f.AddRelation(Relation{
					OID: leafBase + 50, Name: name("public", "orders_2026_02"),
					Kind: RelKindPartitionedTable, OwnerOID: ownerOID, ParentOID: rootOID,
					PartitionBound: "FOR VALUES FROM ('2026-02-01') TO ('2026-03-01')",
				})
				f.SetStrategy(leafBase+50, protocol.StrategyRange)
				f.AddRelation(Relation{
					OID: leafBase + 51, Name: name("public", "orders_2026_02_eu"),
					Kind: RelKindTable, OwnerOID: ownerOID, ParentOID: leafBase + 50,
					PartitionBound: "FOR VALUES IN ('eu')",
				})
				return f
			},
			code: CodeMultiLevel,
		},
		{
			name: "sub-partitioned leaf with no grandchildren",
			build: func() *FakeCatalog {
				f := tree(protocol.StrategyRange, "orders_2026_01")
				f.AddRelation(Relation{
					OID: leafBase + 50, Name: name("public", "orders_2026_02"),
					Kind: RelKindPartitionedTable, OwnerOID: ownerOID, ParentOID: rootOID,
					PartitionBound: "FOR VALUES FROM ('2026-02-01') TO ('2026-03-01')",
				})
				f.SetStrategy(leafBase+50, protocol.StrategyRange)
				return f
			},
			code: CodeMultiLevel,
		},
		{
			name: "not a partitioned table",
			build: func() *FakeCatalog {
				f := NewFakeCatalog()
				f.AddRole(ownerOID, "app_owner", true)
				f.AddRelation(Relation{
					OID: rootOID, Name: name("public", "orders"),
					Kind: RelKindTable, OwnerOID: ownerOID,
				})
				return f
			},
			code: CodeNotPartitioned,
		},
		{
			name: "partitioned table with no partitions",
			build: func() *FakeCatalog {
				return tree(protocol.StrategyRange)
			},
			code: CodeNoPartitions,
		},
		{
			name: "foreign table partition",
			build: func() *FakeCatalog {
				f := tree(protocol.StrategyRange, "orders_2026_01")
				f.AddRelation(Relation{
					OID: leafBase + 50, Name: name("public", "orders_remote"),
					Kind: RelKindForeignTable, OwnerOID: ownerOID, ParentOID: rootOID,
					PartitionBound: "FOR VALUES FROM ('2026-02-01') TO ('2026-03-01')",
				})
				return f
			},
			code: CodeUnsupportedPartitionKind,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Discover(ctx(), tc.build(), name("public", "orders"))
			if err == nil {
				t.Fatal("Discover succeeded, want a topology rejection")
			}
			code, ok := TopologyCodeOf(err)
			if !ok {
				t.Fatalf("error %v is not a *TopologyError", err)
			}
			if code != tc.code {
				t.Errorf("code = %q, want %q", code, tc.code)
			}
			// Every rejection is the same class and the same exit status
			// (AC-11, TRD §7.2.12).
			if !errors.Is(err, protocol.ErrUnsupportedTopology) {
				t.Errorf("errors.Is(err, ErrUnsupportedTopology) = false")
			}
			if got := protocol.ExitCodeFor(err); got != protocol.ExitUnsupportedTopology {
				t.Errorf("exit code = %d, want %d", got, protocol.ExitUnsupportedTopology)
			}
			// The message names the relation, so the operator can act on it.
			if !errors.Is(err, &TopologyError{Code: tc.code}) {
				t.Errorf("errors.Is against &TopologyError{Code: %q} = false", tc.code)
			}
		})
	}
}

// TestTopologyCodesAreDistinct is the literal reading of FR-PLAN-3: HASH and
// DEFAULT do not share a code.
func TestTopologyCodesAreDistinct(t *testing.T) {
	seen := map[TopologyCode]bool{}
	for _, c := range AllTopologyCodes() {
		if !c.Valid() {
			t.Errorf("%q is in AllTopologyCodes but not Valid", c)
		}
		if seen[c] {
			t.Errorf("duplicate code %q", c)
		}
		seen[c] = true
	}
	if CodeHashStrategy == CodeDefaultPartition {
		t.Fatal("FR-PLAN-3 requires distinct codes for HASH and DEFAULT")
	}
	hash := &TopologyError{Code: CodeHashStrategy}
	def := &TopologyError{Code: CodeDefaultPartition}
	if errors.Is(hash, def) {
		t.Error("a HASH rejection matched a DEFAULT rejection")
	}
	if !errors.Is(hash, &TopologyError{}) {
		t.Error("a codeless target should match any topology rejection")
	}
}

// TestDiscoverPropagatesCatalogErrors: a rejection must never be manufactured
// from an unreachable catalog.
func TestDiscoverPropagatesCatalogErrors(t *testing.T) {
	f := standardTree()
	f.Err = ErrCatalogUnavailable.Detailf("connection reset")
	_, err := Discover(ctx(), f, name("public", "orders"))
	if !errors.Is(err, ErrCatalogUnavailable) {
		t.Fatalf("err = %v, want ErrCatalogUnavailable", err)
	}
	if _, ok := TopologyCodeOf(err); ok {
		t.Error("a catalog failure was reported as a topology rejection")
	}
}

func TestDiscoverMissingRelation(t *testing.T) {
	f := standardTree()
	_, err := Discover(ctx(), f, name("public", "no_such_table"))
	if !errors.Is(err, ErrRelationNotFound) {
		t.Fatalf("err = %v, want ErrRelationNotFound", err)
	}
}

// TestDiscoverIsConstantQueryCount guards NFR-PERF-1: discovery must not issue
// one query per partition.
func TestDiscoverIsConstantQueryCount(t *testing.T) {
	small := standardTree()
	mustDiscover(t, small)

	names := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		names = append(names, "orders_p"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	big := tree(protocol.StrategyRange, names...)
	mustDiscover(t, big)

	for _, method := range []string{"LookupRelation", "PartitionTree", "PartitionStrategy"} {
		if small.Calls[method] != big.Calls[method] {
			t.Errorf("%s: %d calls at 3 partitions, %d at 200; discovery must be O(1) queries",
				method, small.Calls[method], big.Calls[method])
		}
	}
}

// TestTopologyFingerprint checks the projection into the protocol's input and
// the properties the fingerprint must have (FR-PLANFILE-4, AC-3, R6).
func TestTopologyFingerprint(t *testing.T) {
	topo := mustDiscover(t, standardTree())

	fp, err := topo.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fp == "" {
		t.Fatal("empty fingerprint")
	}

	t.Run("stable across calls", func(t *testing.T) {
		again, err := topo.Fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		if again != fp {
			t.Errorf("fingerprint changed between calls: %s vs %s", fp, again)
		}
	})

	t.Run("independent of leaf order", func(t *testing.T) {
		shuffled := topo
		shuffled.Leaves = []Relation{topo.Leaves[2], topo.Leaves[0], topo.Leaves[1]}
		got, err := shuffled.Fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		if got != fp {
			t.Errorf("fingerprint depends on leaf order")
		}
	})

	t.Run("relpages does not drift the fingerprint", func(t *testing.T) {
		// An autovacuum between plan and execute must not look like drift (R6).
		vacuumed := topo
		vacuumed.Leaves = append([]Relation(nil), topo.Leaves...)
		vacuumed.Leaves[0].RelPages *= 7
		got, err := vacuumed.Fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		if got != fp {
			t.Errorf("relpages changed the fingerprint; drift detection would fire constantly")
		}
	})

	t.Run("a new partition is drift", func(t *testing.T) {
		grown := topo
		grown.Leaves = append(append([]Relation(nil), topo.Leaves...), Relation{
			OID: 999, Name: name("public", "orders_2026_04"), Kind: RelKindTable,
			OwnerOID: ownerOID, ParentOID: rootOID,
		})
		got, err := grown.Fingerprint()
		if err != nil {
			t.Fatal(err)
		}
		if got == fp {
			t.Errorf("a new partition did not change the fingerprint")
		}
		changes := protocol.DiffTopology(topo.Input(), grown.Input())
		if len(changes) != 1 || changes[0].Change != protocol.TopologyPartitionAdded {
			t.Errorf("DiffTopology = %v, want one partition_added", changes)
		}
	})
}

func TestTopologyAccessors(t *testing.T) {
	topo := mustDiscover(t, standardTree())

	if got := topo.Relations(); len(got) != topo.LeafCount()+1 || got[0].OID != rootOID {
		t.Errorf("Relations() = %v, want root first then every leaf", got)
	}
	if got := topo.RelationOIDs(); len(got) != topo.LeafCount()+1 || got[0] != rootOID {
		t.Errorf("RelationOIDs() = %v", got)
	}
	if got, want := topo.TotalRelPages(), int64(1000+2000+3000); got != want {
		t.Errorf("TotalRelPages = %d, want %d", got, want)
	}
	if _, ok := topo.LeafByOID(leafOID(1)); !ok {
		t.Error("LeafByOID missed a known leaf")
	}
	if _, ok := topo.LeafByOID(4242); ok {
		t.Error("LeafByOID found an unknown leaf")
	}
	if got := topo.LeafOIDs(); len(got) != 3 {
		t.Errorf("LeafOIDs = %v", got)
	}
	in := topo.Input()
	if in.Root.OID != rootOID || len(in.Partitions) != 3 || in.Strategy != protocol.StrategyRange {
		t.Errorf("Input() = %+v", in)
	}
	if err := in.Validate(); err != nil {
		t.Errorf("Input().Validate: %v", err)
	}
}

func TestIsDefaultBound(t *testing.T) {
	tests := []struct {
		bound string
		want  bool
	}{
		{"DEFAULT", true},
		{"default", true},
		{" DEFAULT ", true},
		{"", false},
		{"FOR VALUES FROM ('a') TO ('b')", false},
		{"FOR VALUES IN ('DEFAULT')", false},
	}
	for _, tc := range tests {
		if got := IsDefaultBound(tc.bound); got != tc.want {
			t.Errorf("IsDefaultBound(%q) = %v, want %v", tc.bound, got, tc.want)
		}
	}
}

func TestRelKindPredicates(t *testing.T) {
	tests := []struct {
		kind                              RelKind
		partitioned, isIndex, canCarryIdx bool
	}{
		{RelKindTable, false, false, true},
		{RelKindPartitionedTable, true, false, false},
		{RelKindIndex, false, true, false},
		{RelKindPartitionedIndex, true, true, false},
		{RelKindForeignTable, false, false, false},
		{RelKindMatView, false, false, false},
		{RelKindView, false, false, false},
	}
	for _, tc := range tests {
		if got := tc.kind.IsPartitioned(); got != tc.partitioned {
			t.Errorf("%q.IsPartitioned() = %v, want %v", tc.kind, got, tc.partitioned)
		}
		if got := tc.kind.IsIndex(); got != tc.isIndex {
			t.Errorf("%q.IsIndex() = %v, want %v", tc.kind, got, tc.isIndex)
		}
		if got := tc.kind.CanCarryIndex(); got != tc.canCarryIdx {
			t.Errorf("%q.CanCarryIndex() = %v, want %v", tc.kind, got, tc.canCarryIdx)
		}
		if got := tc.kind.String(); got != string(tc.kind) {
			t.Errorf("String() = %q", got)
		}
	}
}

func TestStrategyFromCode(t *testing.T) {
	tests := []struct {
		code    string
		want    protocol.PartitionStrategy
		wantErr bool
	}{
		{"r", protocol.StrategyRange, false},
		{"l", protocol.StrategyList, false},
		{"h", protocol.StrategyHash, false},
		{"z", "", true},
		{"", "", true},
	}
	for _, tc := range tests {
		got, err := strategyFromCode(tc.code)
		if tc.wantErr {
			if !errors.Is(err, ErrCatalogUnavailable) {
				t.Errorf("strategyFromCode(%q) err = %v, want ErrCatalogUnavailable", tc.code, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("strategyFromCode(%q) = %q, %v", tc.code, got, err)
		}
	}
}

// treeOverride replaces one catalog method so the defensive branches in
// Discover can be reached. pg_partition_tree cannot really return these shapes;
// the point is that a server that surprised us would be reported rather than
// silently producing a Topology with no root.
type treeOverride struct {
	*FakeCatalog
	entries []TreeEntry
}

func (o treeOverride) PartitionTree(c context.Context, rootOID uint32) ([]TreeEntry, error) {
	return o.entries, nil
}

func TestDiscoverRejectsImpossibleTreeShapes(t *testing.T) {
	root := TreeEntry{
		Relation: Relation{OID: rootOID, Name: name("public", "orders"), Kind: RelKindPartitionedTable, OwnerOID: ownerOID},
		Level:    0,
	}
	leaf := TreeEntry{
		Relation: Relation{OID: leafOID(0), Name: name("public", "orders_p1"), Kind: RelKindTable, OwnerOID: ownerOID, ParentOID: rootOID},
		Level:    1,
		IsLeaf:   true,
	}

	tests := []struct {
		name    string
		entries []TreeEntry
		wantMsg string
	}{
		{"no rows at all", nil, "no rows"},
		{"two level-0 rows", []TreeEntry{root, root, leaf}, "two level-0"},
		{"no level-0 row", []TreeEntry{leaf}, "no level-0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := standardTree()
			_, err := Discover(ctx(), treeOverride{FakeCatalog: f, entries: tc.entries}, name("public", "orders"))
			if !errors.Is(err, ErrCatalogUnavailable) {
				t.Fatalf("err = %v, want ErrCatalogUnavailable", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("message %q does not contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestDiscoverKeepsTheTreeSnapshotOfTheRoot: the root state that reaches the
// fingerprint must be the one pg_partition_tree reported, so that both halves
// of the fingerprint come from one query and one snapshot.
func TestDiscoverKeepsTheTreeSnapshotOfTheRoot(t *testing.T) {
	f := standardTree()
	topo := mustDiscover(t, f)
	if topo.Root.Owner != "app_owner" {
		t.Errorf("Root.Owner = %q, want the resolved owner name", topo.Root.Owner)
	}
	if topo.Root.State().RelKind != string(RelKindPartitionedTable) {
		t.Errorf("Root.State().RelKind = %q", topo.Root.State().RelKind)
	}
}

func TestRelationStringers(t *testing.T) {
	r := Relation{OID: 1, Name: name("public", "orders"), Kind: RelKindPartitionedTable}
	if r.String() != "public.orders" {
		t.Errorf("Relation.String() = %q", r.String())
	}
	for _, c := range AllIndexConditions() {
		if c.String() != string(c) {
			t.Errorf("IndexCondition.String() = %q", c.String())
		}
	}
	for _, d := range []CleanupDecision{
		CleanupNone, CleanupDropWithProvenance, CleanupAdoptThenDrop, CleanupHalt,
	} {
		if d.String() != string(d) {
			t.Errorf("CleanupDecision.String() = %q", d.String())
		}
	}
}

// TestDiscoverAllowNoPartitions: the childless-tree rejection is a property of
// the operation, not of the topology. An index build cannot converge there; a
// drop can.
func TestDiscoverAllowNoPartitions(t *testing.T) {
	f := tree(protocol.StrategyRange)

	if _, err := Discover(ctx(), f, name("public", "orders")); err == nil {
		t.Fatal("Discover accepted a childless tree by default")
	}

	topo, err := Discover(ctx(), f, name("public", "orders"), AllowNoPartitions())
	if err != nil {
		t.Fatalf("Discover with AllowNoPartitions: %v", err)
	}
	if topo.LeafCount() != 0 {
		t.Errorf("LeafCount = %d, want 0", topo.LeafCount())
	}
	if topo.Root.OID != rootOID {
		t.Errorf("Root.OID = %d", topo.Root.OID)
	}
	// The fingerprint of a childless tree must still compute, or the plan
	// cannot be sealed.
	if _, err := topo.Fingerprint(); err != nil {
		t.Errorf("Fingerprint: %v", err)
	}

	// The relaxation is scoped: it does not smuggle a HASH or DEFAULT tree
	// through.
	hash := tree(protocol.StrategyHash)
	if _, err := Discover(ctx(), hash, name("public", "orders"), AllowNoPartitions()); err == nil {
		t.Error("AllowNoPartitions accepted a HASH tree")
	}
}
