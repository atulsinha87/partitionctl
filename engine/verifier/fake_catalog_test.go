package verifier

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// fakeCatalog is an in-memory [Catalog]. The whole engine is unit-testable with
// no live PostgreSQL (HANDOFF §3), and this is the verifier's half of that.
type fakeCatalog struct {
	indexes  map[string]IndexState
	parents  map[string]protocol.ObjectName
	leaves   map[string][]protocol.ObjectName
	comments map[string]string

	// Injected read failures, one per method, so a catalog error can be
	// distinguished from a false assertion in every code path.
	failIndex       error
	failIndexParent error
	failAttached    error
	failLeaves      error
	failTree        error
	failComment     error

	calls []string
}

var _ Catalog = (*fakeCatalog)(nil)

func newFakeCatalog() *fakeCatalog {
	return &fakeCatalog{
		indexes:  map[string]IndexState{},
		parents:  map[string]protocol.ObjectName{},
		leaves:   map[string][]protocol.ObjectName{},
		comments: map[string]string{},
	}
}

// IndexComment implements [Catalog].
func (f *fakeCatalog) IndexComment(_ context.Context, index protocol.ObjectName) (string, bool, error) {
	f.calls = append(f.calls, "IndexComment:"+index.String())
	if f.failComment != nil {
		return "", false, f.failComment
	}
	c, ok := f.comments[index.String()]
	if !ok || c == "" {
		return "", false, nil
	}
	return c, true, nil
}

// comment sets an arbitrary comment on an index, which is how a test builds the
// "somebody else wrote this" case.
func (f *fakeCatalog) comment(index protocol.ObjectName, text string) *fakeCatalog {
	f.comments[index.String()] = text
	return f
}

// mark writes a well-formed PartitionCTL ownership marker onto an index.
func (f *fakeCatalog) mark(index protocol.ObjectName, run string) *fakeCatalog {
	text, err := protocol.FormatMarker(protocol.Marker{
		Run: run, Plan: "sha256:fake", Op: string(protocol.OpCreateIndex),
		Role: protocol.MarkerRoleLeaf, At: "2026-08-07T12:00:00Z",
	})
	if err != nil {
		panic("verifier: fakeCatalog.mark: " + err.Error())
	}
	return f.comment(index, text)
}

// putIndex records an index and returns the catalog for chaining.
func (f *fakeCatalog) putIndex(st IndexState) *fakeCatalog {
	f.indexes[st.Name.String()] = st
	return f
}

// attach records a pg_inherits edge from a child index to a partitioned index.
func (f *fakeCatalog) attach(child, parent protocol.ObjectName) *fakeCatalog {
	f.parents[child.String()] = parent
	return f
}

// detach removes the pg_inherits edge, leaving the child index in place.
func (f *fakeCatalog) detach(child protocol.ObjectName) *fakeCatalog {
	delete(f.parents, child.String())
	return f
}

// dropIndex removes an index and any edge it had.
func (f *fakeCatalog) dropIndex(name protocol.ObjectName) *fakeCatalog {
	delete(f.indexes, name.String())
	delete(f.parents, name.String())
	return f
}

// setLeaves records the leaf partitions of a table.
func (f *fakeCatalog) setLeaves(table protocol.ObjectName, leaves ...protocol.ObjectName) *fakeCatalog {
	f.leaves[table.String()] = leaves
	return f
}

// mutate applies fn to a stored index, so a test can flip exactly one catalog
// bit and assert exactly one assertion fails.
func (f *fakeCatalog) mutate(name protocol.ObjectName, fn func(*IndexState)) *fakeCatalog {
	st, ok := f.indexes[name.String()]
	if !ok {
		panic("fakeCatalog.mutate: no index named " + name.String())
	}
	fn(&st)
	f.indexes[name.String()] = st
	return f
}

// resolve finds a stored index by name. An unqualified argument matches on the
// bare name, mirroring how PostgreSQL resolves through search_path.
func (f *fakeCatalog) resolve(name protocol.ObjectName) (IndexState, bool) {
	if st, ok := f.indexes[name.String()]; ok {
		return st, true
	}
	if name.Schema != "" {
		return IndexState{}, false
	}
	keys := make([]string, 0, len(f.indexes))
	for k := range f.indexes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if f.indexes[k].Name.Name == name.Name {
			return f.indexes[k], true
		}
	}
	return IndexState{}, false
}

func (f *fakeCatalog) Index(ctx context.Context, name protocol.ObjectName) (IndexState, bool, error) {
	f.calls = append(f.calls, "Index("+name.String()+")")
	if f.failIndex != nil {
		return IndexState{}, false, f.failIndex
	}
	st, ok := f.resolve(name)
	return st, ok, nil
}

func (f *fakeCatalog) IndexParent(ctx context.Context, child protocol.ObjectName) (protocol.ObjectName, bool, error) {
	f.calls = append(f.calls, "IndexParent("+child.String()+")")
	if f.failIndexParent != nil {
		return protocol.ObjectName{}, false, f.failIndexParent
	}
	st, ok := f.resolve(child)
	if !ok {
		return protocol.ObjectName{}, false, nil
	}
	parent, ok := f.parents[st.Name.String()]
	if !ok {
		return protocol.ObjectName{}, false, nil
	}
	return parent, true, nil
}

func (f *fakeCatalog) AttachedIndexes(ctx context.Context, parentIndex protocol.ObjectName) ([]IndexState, error) {
	f.calls = append(f.calls, "AttachedIndexes("+parentIndex.String()+")")
	if f.failAttached != nil {
		return nil, f.failAttached
	}
	var out []IndexState
	for child, parent := range f.parents {
		if !sameObject(parentIndex, parent) {
			continue
		}
		if st, ok := f.indexes[child]; ok {
			out = append(out, st)
		}
	}
	sortStates(out)
	return out, nil
}

func (f *fakeCatalog) LeafPartitions(ctx context.Context, table protocol.ObjectName) ([]protocol.ObjectName, error) {
	f.calls = append(f.calls, "LeafPartitions("+table.String()+")")
	if f.failLeaves != nil {
		return nil, f.failLeaves
	}
	out := append([]protocol.ObjectName(nil), f.leaves[table.String()]...)
	sortNames(out)
	return out, nil
}

func (f *fakeCatalog) TreeIndexes(ctx context.Context, table protocol.ObjectName) ([]IndexState, error) {
	f.calls = append(f.calls, "TreeIndexes("+table.String()+")")
	if f.failTree != nil {
		return nil, f.failTree
	}
	in := map[string]bool{table.String(): true}
	for _, leaf := range f.leaves[table.String()] {
		in[leaf.String()] = true
	}
	var out []IndexState
	for _, st := range f.indexes {
		if in[st.Relation.String()] {
			out = append(out, st)
		}
	}
	sortStates(out)
	return out, nil
}

func sortStates(s []IndexState) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Name.Schema != s[j].Name.Schema {
			return s[i].Name.Schema < s[j].Name.Schema
		}
		return s[i].Name.Name < s[j].Name.Name
	})
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const (
	testSchema      = "public"
	testTableName   = "orders"
	testParentIndex = "orders_created_at_idx"
)

func table() protocol.ObjectName { return protocol.NewObjectName(testSchema, testTableName) }

func parentIndex() protocol.ObjectName {
	return protocol.NewObjectName(testSchema, testParentIndex)
}

func leaf(i int) protocol.ObjectName {
	return protocol.NewObjectName(testSchema, fmt.Sprintf("orders_2026_%02d", i))
}

// childIndex is the leaf index name the planner would have generated
// (FR-PLAN-11). Tests derive it the same way the planner does rather than
// hard-coding it, so a change to the naming function surfaces here.
func childIndex(l protocol.ObjectName) protocol.ObjectName {
	return protocol.NewObjectName(l.Schema, protocol.ChildIndexName(testParentIndex, l.Name))
}

func usable(name, relation protocol.ObjectName) IndexState {
	return IndexState{Name: name, Relation: relation, Valid: true, Ready: true, Live: true}
}

// healthyCatalog is the catalog a completed CreatePartitionedIndex leaves
// behind over n leaf partitions: parent index valid, every leaf index valid,
// ready, live and attached.
func healthyCatalog(n int) *fakeCatalog {
	f := newFakeCatalog()
	leaves := make([]protocol.ObjectName, 0, n)
	parent := usable(parentIndex(), table())
	parent.Partitioned = true
	f.putIndex(parent)
	for i := 1; i <= n; i++ {
		l := leaf(i)
		leaves = append(leaves, l)
		c := childIndex(l)
		f.putIndex(usable(c, l))
		f.attach(c, parentIndex())
	}
	f.setLeaves(table(), leaves...)
	return f
}

// leftoverName builds the name PostgreSQL would leave behind for a failed or
// half-finished REINDEX CONCURRENTLY.
func leftoverName(base protocol.ObjectName, suffix string) protocol.ObjectName {
	return protocol.NewObjectName(base.Schema, base.Name+suffix)
}

func mustTime(s string) time.Time {
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return ts
}

func mustPass(t *testing.T, r Result) {
	t.Helper()
	if r.Status != StatusPass {
		t.Fatalf("check %s: got status %s (%s), want pass", r.Check, r.Status, r.Reason)
	}
}

func mustFail(t *testing.T, r Result) {
	t.Helper()
	if r.Status != StatusFail {
		t.Fatalf("check %s: got status %s (%s), want fail", r.Check, r.Status, r.Reason)
	}
	if r.Reason == "" {
		t.Fatalf("check %s failed with no reason", r.Check)
	}
}

func mustError(t *testing.T, r Result) {
	t.Helper()
	if r.Status != StatusError {
		t.Fatalf("check %s: got status %s (%s), want error", r.Check, r.Status, r.Reason)
	}
	if r.Reason == "" {
		t.Fatalf("check %s errored with no reason", r.Check)
	}
}
