package createindex

import (
	"context"
	"testing"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// fakeCatalog is a complete in-memory [CatalogReader]. The planner never needs
// a live PostgreSQL to be tested, which is the point of the interface.
type fakeCatalog struct {
	topology protocol.TopologyInput
	indexes  map[protocol.ObjectName]IndexState
	pages    map[protocol.ObjectName]int64
	members  map[protocol.ObjectName]bool

	// notMember lists relations the connected role does NOT own, overriding
	// the default of "member of everything".
	notMember map[protocol.ObjectName]bool

	topoErr, indexErr, pagesErr, memberErr error

	// asked records every name each method was asked about, so a test can
	// assert the planner batches its reads.
	askedIndexes  []protocol.ObjectName
	askedPages    []protocol.ObjectName
	askedMembers  []protocol.ObjectName
	topologyCalls int
}

func (f *fakeCatalog) Topology(_ context.Context, _ protocol.ObjectName) (protocol.TopologyInput, error) {
	f.topologyCalls++
	if f.topoErr != nil {
		return protocol.TopologyInput{}, f.topoErr
	}
	return f.topology, nil
}

func (f *fakeCatalog) Indexes(_ context.Context, names []protocol.ObjectName) (map[protocol.ObjectName]IndexState, error) {
	f.askedIndexes = append(f.askedIndexes, names...)
	if f.indexErr != nil {
		return nil, f.indexErr
	}
	out := make(map[protocol.ObjectName]IndexState)
	for _, n := range names {
		if st, ok := f.indexes[n]; ok {
			out[n] = st
		}
	}
	return out, nil
}

func (f *fakeCatalog) RelationPages(_ context.Context, names []protocol.ObjectName) (map[protocol.ObjectName]int64, error) {
	f.askedPages = append(f.askedPages, names...)
	if f.pagesErr != nil {
		return nil, f.pagesErr
	}
	out := make(map[protocol.ObjectName]int64)
	for _, n := range names {
		if p, ok := f.pages[n]; ok {
			out[n] = p
		}
	}
	return out, nil
}

func (f *fakeCatalog) OwnedByMemberRole(_ context.Context, _ string, names []protocol.ObjectName) (map[protocol.ObjectName]bool, error) {
	f.askedMembers = append(f.askedMembers, names...)
	if f.memberErr != nil {
		return nil, f.memberErr
	}
	out := make(map[protocol.ObjectName]bool)
	for _, n := range names {
		if f.members != nil {
			out[n] = f.members[n]
			continue
		}
		out[n] = !f.notMember[n]
	}
	return out, nil
}

// fakeClaims answers from a set of objects a notional crashed run still holds a
// live claim on. It is the second, narrower half of ownership: the first is the
// marker on the object, which arrives through IndexState.Comment.
type fakeClaims struct {
	claimed map[protocol.ObjectName]bool
	err     error
	asked   []protocol.ObjectName
}

func (f *fakeClaims) ClaimsObject(_ context.Context, o protocol.ObjectName) (string, bool, error) {
	f.asked = append(f.asked, o)
	if f.err != nil {
		return "", false, f.err
	}
	if !f.claimed[o] {
		return "", false, nil
	}
	return "run-crashed", true, nil
}

// ---------------------------------------------------------------------------
// Fixture builders
// ---------------------------------------------------------------------------

const (
	testSchema = "public"
	testTable  = "orders"
	testIndex  = "orders_created_at_idx"
	testRole   = "app_owner"
	rootOID    = uint32(16400)
)

func obj(name string) protocol.ObjectName { return protocol.NewObjectName(testSchema, name) }

// newTopology builds a one-level RANGE tree with the named leaves.
func newTopology(leaves ...string) protocol.TopologyInput {
	t := protocol.TopologyInput{
		Root: protocol.RelationState{
			OID: rootOID, Schema: testSchema, Name: testTable,
			RelKind: RelKindPartitionedTable, OwnerOID: 10,
		},
		Strategy: protocol.StrategyRange,
	}
	for i, l := range leaves {
		t.Partitions = append(t.Partitions, protocol.RelationState{
			OID: uint32(20000 + i), Schema: testSchema, Name: l,
			RelKind: RelKindTable, OwnerOID: 10, ParentOID: rootOID,
			PartitionBound: "FOR VALUES FROM ('x') TO ('y')",
		})
	}
	return t
}

func newSpec() Specification {
	return Specification{
		Database: "shop",
		Table:    obj(testTable),
		Index:    obj(testIndex),
		Definition: protocol.IndexDefinition{
			Method:  "btree",
			Columns: []protocol.IndexColumn{{Name: "created_at"}},
		},
		Role:        testRole,
		PaceSeconds: 30,
	}
}

func newCatalog(leaves ...string) *fakeCatalog {
	return &fakeCatalog{
		topology: newTopology(leaves...),
		indexes:  map[protocol.ObjectName]IndexState{},
		pages:    map[protocol.ObjectName]int64{},
	}
}

// child returns the generated leaf index name for a partition, exactly as the
// planner derives it (FR-PLAN-11).
func child(leaf string) protocol.ObjectName {
	return protocol.NewObjectName(testSchema, protocol.ChildIndexName(testIndex, leaf))
}

// builtIndex is a healthy, unattached leaf index.
func builtIndex(leaf string) IndexState {
	return IndexState{
		Index: child(leaf), Relation: obj(leaf),
		Valid: true, Ready: true, Live: true,
	}
}

// attachedIndex is a healthy leaf index attached to the parent index.
func attachedIndex(leaf string) IndexState {
	st := builtIndex(leaf)
	parent := obj(testIndex)
	st.AttachedTo = &parent
	return st
}

// invalidIndex is the wreckage of an interrupted CREATE INDEX CONCURRENTLY:
// live and ready, but not valid, and never attached.
func invalidIndex(leaf string) IndexState {
	return IndexState{
		Index: child(leaf), Relation: obj(leaf),
		Valid: false, Ready: true, Live: true,
	}
}

// parentIndexState is the partitioned parent index.
func parentIndexState(valid bool) IndexState {
	return IndexState{
		Index: obj(testIndex), Relation: obj(testTable),
		IsPartitioned: true, Valid: valid, Ready: true, Live: true,
	}
}

// marked stamps a well-formed PartitionCTL ownership marker onto an index
// state, which is what proves the object is this tool's to clean up (AC-6).
// The marker text is produced by the real formatter and read back by the real
// parser, so a fake cannot claim ownership in a shape production would reject.
func marked(st IndexState) IndexState {
	text, err := protocol.FormatMarker(protocol.Marker{
		Run: "run-0", Plan: "sha256:fake", Op: string(protocol.OpCreateIndex),
		Role: protocol.MarkerRoleLeaf, At: "2026-08-07T12:00:00Z",
	})
	if err != nil {
		panic("create-index: marked: " + err.Error())
	}
	st.Comment = text
	return st
}

// commented puts somebody else's comment on an index state.
func commented(st IndexState, text string) IndexState {
	st.Comment = text
	return st
}

// ---------------------------------------------------------------------------
// Assertions shared by the tests
// ---------------------------------------------------------------------------

// mustPlan plans and fails the test on error.
func mustPlan(t *testing.T, pl Planner, spec Specification, cat CatalogReader) *protocol.Plan {
	t.Helper()
	p, err := pl.Plan(context.Background(), spec, cat)
	if err != nil {
		t.Fatalf("Plan() error = %v, want nil", err)
	}
	if p == nil {
		t.Fatal("Plan() returned a nil plan and a nil error")
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("returned plan does not validate: %v", err)
	}
	if err := p.VerifyDigest(); err != nil {
		t.Fatalf("returned plan is not sealed: %v", err)
	}
	return p
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
	n, ok := p.NodeByID(id)
	if !ok {
		t.Fatalf("node %q not in plan; plan has %v", id, nodeIDs(p))
	}
	return n
}

func hasNode(p *protocol.Plan, id protocol.NodeID) bool {
	_, ok := p.NodeByID(id)
	return ok
}

func depsEqual(t *testing.T, p *protocol.Plan, id protocol.NodeID, want ...protocol.NodeID) {
	t.Helper()
	got := node(t, p, id).DependsOn
	if len(got) != len(want) {
		t.Fatalf("node %q depends on %v, want %v", id, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("node %q depends on %v, want %v", id, got, want)
		}
	}
}
