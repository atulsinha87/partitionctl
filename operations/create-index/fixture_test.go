package createindex

import (
	"context"
	"hash/fnv"
	"testing"
	"time"

	"github.com/atulsinha87/partitionctl/engine/planner"
	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// The fixture builds a [planner.FakeCatalog] and runs this operation through a
// real [planner.Host].
//
// There is no second CatalogReader here and no hand-written stand-in for one.
// That is the point of B1: engine/planner owns the fake, so an operation's
// tests exercise the same discovery, the same topology rejection and the same
// privilege check the CLI runs, rather than a parallel implementation that
// could agree with the operation while disagreeing with production.

const (
	testSchema = "public"
	testTable  = "orders"
	testIndex  = "orders_created_at_idx"
	testRole   = "migrator"
	testDB     = "shop"
)

const (
	rootOID        uint32 = 100
	ownerOID       uint32 = 10
	parentIndexOID uint32 = 500
	foreignOID     uint32 = 999
	firstLeafOID   uint32 = 1000
	firstChildOID  uint32 = 2000
)

var (
	fixedNow    = func() time.Time { return time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC) }
	testPlanID  = protocol.PlanID("plan-fixed-for-tests")
	testPlanner = Planner{}
)

func obj(name string) protocol.ObjectName { return protocol.NewObjectName(testSchema, name) }

// child is the index name the tool generates for a leaf, from the one shared
// generator (FR-PLAN-11).
func child(leaf string) protocol.ObjectName {
	return protocol.NewObjectName(testSchema, protocol.ChildIndexName(testIndex, leaf))
}

// fakeCatalog is a declarative description of a catalog. It is not a
// CatalogReader: [fakeCatalog.build] turns it into a [planner.FakeCatalog],
// which is.
type fakeCatalog struct {
	// leaves are the partitions, in the order they were declared. Order here
	// is deliberately meaningless to the plan: the host sorts.
	leaves []leafDecl
	// pages is relpages per leaf.
	pages map[protocol.ObjectName]int64
	// indexes is what already exists, keyed by index name.
	indexes map[protocol.ObjectName]planner.Index
	// bounds overrides a leaf's partition bound, which is how a DEFAULT
	// partition is declared.
	bounds map[protocol.ObjectName]string

	strategy  protocol.PartitionStrategy
	claims    planner.ClaimLookup
	estimator planner.Estimator
	database  string
}

type leafDecl struct {
	Name protocol.ObjectName
	OID  uint32
}

func newCatalog(leaves ...string) *fakeCatalog {
	c := &fakeCatalog{
		pages:    map[protocol.ObjectName]int64{},
		indexes:  map[protocol.ObjectName]planner.Index{},
		bounds:   map[protocol.ObjectName]string{},
		strategy: protocol.StrategyRange,
		database: testDB,
	}
	for _, l := range leaves {
		c.addLeaf(obj(l))
	}
	return c
}

// addLeaf appends a partition, giving it an OID derived from its name alone.
//
// Deriving the OID from the name rather than from the declaration order is what
// makes two catalogs that declare the same partitions in different orders the
// *same* tree, which is what the determinism test needs: the topology
// fingerprint is over OIDs. A name declared twice gets two adjacent OIDs, which
// is how a tree with genuinely colliding child index names is built.
func (c *fakeCatalog) addLeaf(name protocol.ObjectName) *fakeCatalog {
	oid := stableOID(name)
	for c.oidTaken(oid) {
		oid++
	}
	c.leaves = append(c.leaves, leafDecl{Name: name, OID: oid})
	return c
}

// setLeaves replaces the partition set.
func (c *fakeCatalog) setLeaves(names ...protocol.ObjectName) *fakeCatalog {
	c.leaves = nil
	for _, n := range names {
		c.addLeaf(n)
	}
	return c
}

func (c *fakeCatalog) oidTaken(oid uint32) bool {
	for _, l := range c.leaves {
		if l.OID == oid {
			return true
		}
	}
	return false
}

func stableOID(name protocol.ObjectName) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name.String()))
	return firstLeafOID + h.Sum32()%1_000_000
}

func (c *fakeCatalog) build() *planner.FakeCatalog {
	f := planner.NewFakeCatalog()
	f.Role = testRole
	f.Database = c.database
	f.AddRole(ownerOID, "app_owner", true)
	f.AddRelation(planner.Relation{
		OID: rootOID, Name: obj(testTable), Kind: planner.RelKindPartitionedTable,
		OwnerOID: ownerOID,
	})
	f.SetStrategy(rootOID, c.strategy)

	for _, l := range c.leaves {
		bound, ok := c.bounds[l.Name]
		if !ok {
			bound = "FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')"
		}
		f.AddRelation(planner.Relation{
			OID: l.OID, Name: l.Name, Kind: planner.RelKindTable, OwnerOID: ownerOID,
			ParentOID: rootOID, PartitionBound: bound, RelPages: c.pages[l.Name],
		})
	}

	// pg_index joins on OIDs, so an index has to name its relation the way the
	// server does: by indrelid. Resolving it here from the relation name the
	// fixture declared is what keeps the declarations readable.
	tableOID := map[protocol.ObjectName]uint32{obj(testTable): rootOID}
	for _, l := range c.leaves {
		tableOID[l.Name] = l.OID
	}
	for name, idx := range c.indexes {
		idx.Name = name
		if idx.OID == 0 {
			idx.OID = stableOID(name) + firstChildOID
		}
		if idx.TableOID == 0 {
			idx.TableOID = tableOID[idx.Table]
		}
		f.AddIndex(idx)
	}
	return f
}

func (c *fakeCatalog) host(t *testing.T) *planner.Host {
	t.Helper()
	return &planner.Host{
		Catalog:   c.build(),
		Claims:    c.claims,
		Estimator: c.estimator,
		Now:       fixedNow,
		NewPlanID: func() protocol.PlanID { return testPlanID },
	}
}

func newSpec() planner.Specification {
	return planner.Specification{
		Operation: protocol.OpCreateIndex,
		Table:     obj(testTable),
		Index:     obj(testIndex),
		Actor:     "tester",
		Definition: protocol.IndexDefinition{
			Method:  "btree",
			Columns: []protocol.IndexColumn{{Name: "created_at"}},
		},
	}
}

// tryPlan runs the whole planning sequence and returns whatever it produced.
func tryPlan(t *testing.T, cat *fakeCatalog, spec planner.Specification) (*protocol.Plan, []string, error) {
	t.Helper()
	out, err := cat.host(t).Run(context.Background(), testPlanner, spec)
	if err != nil {
		return nil, nil, err
	}
	return out.Plan, out.Notes, nil
}

func mustPlan(t *testing.T, cat *fakeCatalog, spec planner.Specification) *protocol.Plan {
	t.Helper()
	p, _, err := tryPlan(t, cat, spec)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("plan does not validate: %v", err)
	}
	if err := p.VerifyDigest(); err != nil {
		t.Fatalf("plan digest: %v", err)
	}
	return p
}

// ---------------------------------------------------------------------------
// Index states
// ---------------------------------------------------------------------------

func leafIndex(leaf string) planner.Index {
	return planner.Index{
		Name:     child(leaf),
		Kind:     planner.RelKindIndex,
		OwnerOID: ownerOID,
		Table:    obj(leaf),
		IsValid:  true, IsReady: true, IsLive: true,
	}
}

// builtIndex is a finished leaf index that has not been attached yet.
func builtIndex(leaf string) planner.Index { return leafIndex(leaf) }

// attachedIndex is a finished leaf index attached to the target parent index.
func attachedIndex(leaf string) planner.Index {
	i := leafIndex(leaf)
	i.ParentIndexOID = parentIndexOID
	return i
}

// invalidIndex is the wreckage of an interrupted CREATE INDEX CONCURRENTLY.
func invalidIndex(leaf string) planner.Index {
	i := leafIndex(leaf)
	i.IsValid = false
	return i
}

func parentIndexState(valid bool) planner.Index {
	return planner.Index{
		OID: parentIndexOID, Name: obj(testIndex), Kind: planner.RelKindPartitionedIndex,
		OwnerOID: ownerOID, Table: obj(testTable),
		IsValid: valid, IsReady: true, IsLive: true,
	}
}

// marked stamps PartitionCTL's ownership marker onto an index, which is what
// makes it provably ours (AC-6).
func marked(i planner.Index) planner.Index {
	text, err := protocol.FormatMarker(protocol.Marker{
		Run: "run-earlier", Plan: "sha256:earlier", Op: string(protocol.OpCreateIndex),
		Role: protocol.MarkerRoleLeaf, At: "2026-08-07T11:00:00Z",
	})
	if err != nil {
		panic("createindex: fixture marker: " + err.Error())
	}
	i.Comment = text
	return i
}

func commented(i planner.Index, text string) planner.Index {
	i.Comment = text
	return i
}

// ---------------------------------------------------------------------------
// Graph assertions
// ---------------------------------------------------------------------------

// hasWork reports whether a plan contains any node that issues DDL. A plan for
// a fully converged catalog carries only its precondition assert and its final
// verify, so it is a checked no-op rather than an empty file (AC-7).
func hasWork(p *protocol.Plan) bool {
	for i := range p.Nodes {
		if p.Nodes[i].Kind.IssuesDDL() {
			return true
		}
	}
	return false
}

func nodeIDs(p *protocol.Plan) []protocol.NodeID {
	out := make([]protocol.NodeID, len(p.Nodes))
	for i := range p.Nodes {
		out[i] = p.Nodes[i].ID
	}
	return out
}

func kindsOf(p *protocol.Plan) []protocol.NodeKind {
	out := make([]protocol.NodeKind, len(p.Nodes))
	for i := range p.Nodes {
		out[i] = p.Nodes[i].Kind
	}
	return out
}

func countKind(p *protocol.Plan, k protocol.NodeKind) int {
	n := 0
	for i := range p.Nodes {
		if p.Nodes[i].Kind == k {
			n++
		}
	}
	return n
}

func node(t *testing.T, p *protocol.Plan, id protocol.NodeID) *protocol.Node {
	t.Helper()
	for i := range p.Nodes {
		if p.Nodes[i].ID == id {
			return &p.Nodes[i]
		}
	}
	t.Fatalf("plan has no node %q; it has %v", id, nodeIDs(p))
	return nil
}

func hasNode(p *protocol.Plan, id protocol.NodeID) bool {
	for i := range p.Nodes {
		if p.Nodes[i].ID == id {
			return true
		}
	}
	return false
}

func depsEqual(t *testing.T, p *protocol.Plan, id protocol.NodeID, want ...protocol.NodeID) {
	t.Helper()
	got := node(t, p, id).DependsOn
	if len(got) != len(want) {
		t.Fatalf("node %q depends on %v, want %v", id, got, want)
	}
	seen := map[protocol.NodeID]bool{}
	for _, d := range got {
		seen[d] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Fatalf("node %q depends on %v, want %v", id, got, want)
		}
	}
}

func indexOf(ids []protocol.NodeID, want protocol.NodeID) int {
	for i, id := range ids {
		if id == want {
			return i
		}
	}
	return -1
}
