package planner

import (
	"context"
	"testing"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// ctx is the context every test uses. Go 1.22 has no testing.T.Context.
func ctx() context.Context { return context.Background() }

const (
	ownerOID   uint32 = 10
	strangeOID uint32 = 11
	rootOID    uint32 = 100
	leafBase   uint32 = 200
	indexBase  uint32 = 900
)

func name(schema, n string) protocol.ObjectName { return protocol.NewObjectName(schema, n) }

// tree builds a single-level partitioned table with the given strategy and
// leaves, all owned by a role the connected role is a member of.
func tree(strategy protocol.PartitionStrategy, leaves ...string) *FakeCatalog {
	f := NewFakeCatalog()
	f.AddRole(ownerOID, "app_owner", true)
	f.AddRole(strangeOID, "someone_else", false)
	f.AddRelation(Relation{
		OID:      rootOID,
		Name:     name("public", "orders"),
		Kind:     RelKindPartitionedTable,
		OwnerOID: ownerOID,
		RelPages: 0,
	})
	f.SetStrategy(rootOID, strategy)
	for i, leaf := range leaves {
		f.AddRelation(Relation{
			OID:            leafBase + uint32(i),
			Name:           name("public", leaf),
			Kind:           RelKindTable,
			OwnerOID:       ownerOID,
			ParentOID:      rootOID,
			RelPages:       int64(1000 * (i + 1)),
			PartitionBound: "FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')",
		})
	}
	return f
}

// standardTree is the fixture most tests use: three RANGE partitions.
func standardTree() *FakeCatalog {
	return tree(protocol.StrategyRange, "orders_2026_01", "orders_2026_02", "orders_2026_03")
}

func leafOID(i int) uint32 { return leafBase + uint32(i) }

// childIndex is the name the planner will generate for leaf i under the parent
// index "orders_created_at_idx".
func childIndex(t *testing.T, parent string, leaf string) protocol.ObjectName {
	t.Helper()
	return name("public", protocol.ChildIndexName(parent, leaf))
}

// idx builds an index record with all three pg_index flags set the way the
// caller asks.
func idx(oid uint32, schema, iname string, tableOID uint32, table string, valid, ready, live bool) Index {
	return Index{
		OID:      oid,
		Name:     name(schema, iname),
		Kind:     RelKindIndex,
		OwnerOID: ownerOID,
		RelPages: 100,
		TableOID: tableOID,
		Table:    name(schema, table),
		IsValid:  valid,
		IsReady:  ready,
		IsLive:   live,
	}
}

func mustDiscover(t *testing.T, f *FakeCatalog) Topology {
	t.Helper()
	topo, err := Discover(ctx(), f, name("public", "orders"))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return topo
}
