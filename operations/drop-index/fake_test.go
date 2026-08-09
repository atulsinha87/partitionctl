package dropindex

import (
	"testing"
	"time"

	"github.com/atulsinha87/partitionctl/engine/planner"
	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// The fixture is an in-memory catalog. Nothing in this package's tests reaches
// a database: the whole point of the planner/executor split is that the half
// that decides what to do is testable without one, and a suite that needed a
// server would make that claim unfalsifiable.

const (
	ownerOID    = 10
	rootOID     = 1000
	leafOIDBase = 1000
	targetOID   = 2000
	childOIDs   = 2000
)

var planTime = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

// fixture is a partitioned table, its partitioned index, and one attached leaf
// index per partition: the state a completed CreatePartitionedIndex leaves.
type fixture struct {
	cat      *planner.FakeCatalog
	root     protocol.ObjectName
	index    protocol.ObjectName
	leaves   []protocol.ObjectName
	children []protocol.ObjectName
}

func newFixture(t *testing.T, leafCount int) *fixture {
	t.Helper()

	f := &fixture{
		cat:   planner.NewFakeCatalog(),
		root:  protocol.NewObjectName("public", "events"),
		index: protocol.NewObjectName("public", "idx_events_ts"),
	}
	f.cat.AddRole(ownerOID, "app_owner", true)
	f.cat.AddRelation(planner.Relation{
		OID:      rootOID,
		Name:     f.root,
		Kind:     planner.RelKindPartitionedTable,
		OwnerOID: ownerOID,
	})
	f.cat.SetStrategy(rootOID, protocol.StrategyRange)

	for i := 1; i <= leafCount; i++ {
		leaf := protocol.NewObjectName("public", leafName(i))
		f.leaves = append(f.leaves, leaf)
		f.cat.AddRelation(planner.Relation{
			OID:            uint32(leafOIDBase + i),
			Name:           leaf,
			Kind:           planner.RelKindTable,
			OwnerOID:       ownerOID,
			ParentOID:      rootOID,
			RelPages:       1000,
			PartitionBound: "FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')",
		})
	}

	children, err := protocol.ChildIndexNamesQualified(f.index.Name, f.leaves)
	if err != nil {
		t.Fatalf("generate child index names: %v", err)
	}
	f.children = children

	f.cat.AddIndex(planner.Index{
		OID:      targetOID,
		Name:     f.index,
		Kind:     planner.RelKindPartitionedIndex,
		OwnerOID: ownerOID,
		TableOID: rootOID,
		Table:    f.root,
		IsValid:  true,
		IsReady:  true,
		IsLive:   true,
	})
	for i, child := range f.children {
		f.cat.AddIndex(planner.Index{
			OID:            uint32(childOIDs + i + 1),
			Name:           child,
			Kind:           planner.RelKindIndex,
			OwnerOID:       ownerOID,
			TableOID:       uint32(leafOIDBase + i + 1),
			Table:          f.leaves[i],
			IsValid:        true,
			IsReady:        true,
			IsLive:         true,
			ParentIndexOID: targetOID,
		})
	}
	return f
}

// leafName produces events_p01 … events_p12, which sort in the order an
// operator reads them.
func leafName(i int) string {
	digits := "0123456789"
	return "events_p" + string([]byte{digits[i/10], digits[i%10]})
}

// detachChild replaces the attached leaf index at position i with an
// unattached one: the orphan an abandoned CreatePartitionedIndex leaves.
func (f *fixture) detachChild(i int, comment string) {
	for j := range f.cat.Indexes {
		if f.cat.Indexes[j].Name == f.children[i] {
			f.cat.Indexes[j].ParentIndexOID = 0
			f.cat.Indexes[j].IsValid = false
			f.cat.Indexes[j].Comment = comment
			return
		}
	}
	panic("dropindex: fixture has no child index at position " + f.children[i].String())
}

// setTarget mutates the target index, for the refusal cases.
func (f *fixture) setTarget(mutate func(*planner.Index)) {
	for j := range f.cat.Indexes {
		if f.cat.Indexes[j].Name == f.index {
			mutate(&f.cat.Indexes[j])
			return
		}
	}
	panic("dropindex: fixture has no target index")
}

// ourMarker is a well-formed PartitionCTL ownership marker naming run.
func ourMarker(t *testing.T, run string) string {
	t.Helper()
	text, err := protocol.FormatMarker(protocol.Marker{
		Run:  run,
		Plan: "sha256:0123456789abcdef",
		Op:   string(protocol.OpCreateIndex),
		Role: protocol.MarkerRoleLeaf,
		At:   protocol.MarkerTime(planTime),
	})
	if err != nil {
		t.Fatalf("format marker: %v", err)
	}
	return text
}

// host wires the fixture into a planner host configured the way the CLI must
// configure it for this operation.
func (f *fixture) host(claims planner.ClaimLookup) *planner.Host {
	return &planner.Host{
		Catalog:         f.cat,
		Claims:          claims,
		Now:             func() time.Time { return planTime },
		NewPlanID:       func() protocol.PlanID { return "drop-index-0123456789abcdef" },
		DiscoverOptions: DiscoverOptions(),
	}
}

// spec is the operator's request, with or without the acknowledgement.
func (f *fixture) spec(confirmed bool) planner.Specification {
	s := planner.Specification{
		Operation: protocol.OpDropIndex,
		Table:     f.root,
		Index:     f.index,
		Actor:     "operator",
	}
	if confirmed {
		s.Confirmations = []protocol.Confirmation{{
			Flag:  protocol.ConfirmExclusiveLock,
			Actor: "operator",
			At:    protocol.NewTimestamp(planTime),
			Note:  "reviewed the maintenance window",
		}}
	}
	return s
}
