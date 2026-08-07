package planner

import (
	"errors"
	"testing"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// TestFakeCatalogDerivesTheTree: the fake must behave like pg_partition_tree,
// not like a hand-written answer, or every test built on it proves nothing.
func TestFakeCatalogDerivesTheTree(t *testing.T) {
	f := NewFakeCatalog()
	f.AddRole(ownerOID, "app_owner", true)
	f.AddRelation(Relation{OID: 100, Name: name("public", "orders"), Kind: RelKindPartitionedTable, OwnerOID: ownerOID})
	f.SetStrategy(100, protocol.StrategyRange)
	f.AddRelation(Relation{OID: 200, Name: name("public", "orders_a"), Kind: RelKindTable, OwnerOID: ownerOID, ParentOID: 100})
	f.AddRelation(Relation{OID: 210, Name: name("public", "orders_b"), Kind: RelKindPartitionedTable, OwnerOID: ownerOID, ParentOID: 100})
	f.AddRelation(Relation{OID: 211, Name: name("public", "orders_b_eu"), Kind: RelKindTable, OwnerOID: ownerOID, ParentOID: 210})
	// A relation outside the tree must not appear.
	f.AddRelation(Relation{OID: 300, Name: name("public", "customers"), Kind: RelKindTable, OwnerOID: ownerOID})

	got, err := f.PartitionTree(ctx(), 100)
	if err != nil {
		t.Fatalf("PartitionTree: %v", err)
	}

	want := []struct {
		oid    uint32
		level  int
		isLeaf bool
	}{
		{100, 0, false},
		{200, 1, true},
		{210, 1, false},
		{211, 2, true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].OID != w.oid || got[i].Level != w.level || got[i].IsLeaf != w.isLeaf {
			t.Errorf("entry %d = {oid %d level %d leaf %v}, want {%d %d %v}",
				i, got[i].OID, got[i].Level, got[i].IsLeaf, w.oid, w.level, w.isLeaf)
		}
	}
}

func TestFakeCatalogTreeOrderIsDeterministic(t *testing.T) {
	f := tree(protocol.StrategyRange, "orders_z", "orders_a", "orders_m")
	first, err := f.PartitionTree(ctx(), rootOID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := f.PartitionTree(ctx(), rootOID)
		if err != nil {
			t.Fatal(err)
		}
		for j := range first {
			if again[j].OID != first[j].OID {
				t.Fatalf("map iteration leaked into the tree order at %d", j)
			}
		}
	}
}

func TestFakeCatalogLookups(t *testing.T) {
	f := standardTree()

	t.Run("qualified", func(t *testing.T) {
		got, err := f.LookupRelation(ctx(), name("public", "orders"))
		if err != nil || got.OID != rootOID {
			t.Fatalf("LookupRelation = %+v, %v", got, err)
		}
	})
	t.Run("unqualified", func(t *testing.T) {
		got, err := f.LookupRelation(ctx(), name("", "orders"))
		if err != nil || got.OID != rootOID {
			t.Fatalf("LookupRelation = %+v, %v", got, err)
		}
	})
	t.Run("wrong schema", func(t *testing.T) {
		_, err := f.LookupRelation(ctx(), name("other", "orders"))
		if !errors.Is(err, ErrRelationNotFound) {
			t.Fatalf("err = %v, want ErrRelationNotFound", err)
		}
	})
	t.Run("ambiguous", func(t *testing.T) {
		f := standardTree()
		f.AddRelation(Relation{OID: 500, Name: name("archive", "orders"), Kind: RelKindPartitionedTable, OwnerOID: ownerOID})
		_, err := f.LookupRelation(ctx(), name("", "orders"))
		if !errors.Is(err, ErrAmbiguousRelation) {
			t.Fatalf("err = %v, want ErrAmbiguousRelation", err)
		}
	})
	t.Run("index", func(t *testing.T) {
		f := standardTree()
		f.AddIndex(idx(indexBase, "public", parentIndexName, rootOID, "orders", true, true, true))
		got, err := f.LookupIndex(ctx(), name("public", parentIndexName))
		if err != nil || got.OID != indexBase {
			t.Fatalf("LookupIndex = %+v, %v", got, err)
		}
		if _, err := f.LookupIndex(ctx(), name("public", "nope")); !errors.Is(err, ErrIndexNotFound) {
			t.Errorf("err = %v, want ErrIndexNotFound", err)
		}
	})
	t.Run("ambiguous index", func(t *testing.T) {
		f := standardTree()
		f.AddIndex(idx(indexBase, "a", "i", rootOID, "orders", true, true, true))
		f.AddIndex(idx(indexBase+1, "b", "i", rootOID, "orders", true, true, true))
		if _, err := f.LookupIndex(ctx(), name("", "i")); !errors.Is(err, ErrAmbiguousRelation) {
			t.Errorf("err = %v, want ErrAmbiguousRelation", err)
		}
	})
}

func TestFakeCatalogErrorInjection(t *testing.T) {
	sentinel := errors.New("catalog is down")
	f := standardTree()
	f.Err = sentinel

	checks := []struct {
		name string
		call func() error
	}{
		{"AssertReadOnly", func() error { return f.AssertReadOnly(ctx()) }},
		{"CurrentRole", func() error { _, err := f.CurrentRole(ctx()); return err }},
		{"CurrentDatabase", func() error { _, err := f.CurrentDatabase(ctx()); return err }},
		{"ServerVersionNum", func() error { _, err := f.ServerVersionNum(ctx()); return err }},
		{"LookupRelation", func() error { _, err := f.LookupRelation(ctx(), name("public", "orders")); return err }},
		{"PartitionTree", func() error { _, err := f.PartitionTree(ctx(), rootOID); return err }},
		{"PartitionStrategy", func() error { _, err := f.PartitionStrategy(ctx(), rootOID); return err }},
		{"IndexesOnRelations", func() error { _, err := f.IndexesOnRelations(ctx(), []uint32{rootOID}); return err }},
		{"LookupIndex", func() error { _, err := f.LookupIndex(ctx(), name("public", "i")); return err }},
		{"RoleMemberships", func() error { _, err := f.RoleMemberships(ctx(), "migrator", []uint32{ownerOID}); return err }},
	}
	for _, c := range checks {
		if err := c.call(); !errors.Is(err, sentinel) {
			t.Errorf("%s returned %v, want the injected error", c.name, err)
		}
	}
}

func TestFakeCatalogReadOnlyFlag(t *testing.T) {
	f := standardTree()
	if err := f.AssertReadOnly(ctx()); err != nil {
		t.Errorf("a fresh fake should be read-only: %v", err)
	}
	f.ReadOnly = false
	if err := f.AssertReadOnly(ctx()); !errors.Is(err, ErrNotReadOnly) {
		t.Errorf("err = %v, want ErrNotReadOnly", err)
	}
}

func TestFakeCatalogUnpartitionedStrategy(t *testing.T) {
	f := NewFakeCatalog()
	f.AddRelation(Relation{OID: 100, Name: name("public", "plain"), Kind: RelKindTable})
	_, err := f.PartitionStrategy(ctx(), 100)
	code, ok := TopologyCodeOf(err)
	if !ok || code != CodeNotPartitioned {
		t.Fatalf("err = %v, want CodeNotPartitioned", err)
	}
}

func TestFakeCatalogUnknownRoot(t *testing.T) {
	f := standardTree()
	if _, err := f.PartitionTree(ctx(), 999); !errors.Is(err, ErrRelationNotFound) {
		t.Fatalf("err = %v, want ErrRelationNotFound", err)
	}
}

func TestFakeCatalogDefaultsIsDefaultFromBound(t *testing.T) {
	f := NewFakeCatalog()
	f.AddRelation(Relation{OID: 1, Name: name("public", "d"), Kind: RelKindTable, PartitionBound: "DEFAULT"})
	if !f.Relations[1].IsDefault {
		t.Error("AddRelation did not derive IsDefault from the bound")
	}
}

func TestFakeCatalogResolvesOwnerName(t *testing.T) {
	f := NewFakeCatalog()
	f.AddRole(ownerOID, "app_owner", true)
	f.AddRelation(Relation{OID: 1, Name: name("public", "t"), Kind: RelKindTable, OwnerOID: ownerOID})
	if got := f.Relations[1].Owner; got != "app_owner" {
		t.Errorf("Owner = %q, want app_owner", got)
	}
}

func TestFakeClaims(t *testing.T) {
	known := name("public", "orders_idx_p1")
	c := NewFakeClaims(known)

	if run, ok, err := c.ClaimsObject(ctx(), known); err != nil || !ok || run == "" {
		t.Errorf("ClaimsObject(known) = %q, %v, %v", run, ok, err)
	}
	if _, ok, err := c.ClaimsObject(ctx(), name("public", "someone_elses_idx")); err != nil || ok {
		t.Errorf("ClaimsObject(unknown) = %v, %v", ok, err)
	}

	sentinel := errors.New("state store unreachable")
	c.Err = sentinel
	if _, _, err := c.ClaimsObject(ctx(), known); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the injected error", err)
	}
}

// The marker helpers round-trip through the real parser, so a fake cannot
// produce a marker the production code would classify differently.
func TestFakeCatalogMarkers(t *testing.T) {
	idxName := name("public", "orders_idx_p1")
	f := NewFakeCatalog().Mark(idxName, "run-7")

	m, status, err := IndexMarker(ctx(), f, idxName)
	if err != nil {
		t.Fatalf("IndexMarker: %v", err)
	}
	if status != protocol.MarkerOurs || m.Run != "run-7" {
		t.Fatalf("status = %v, marker = %+v", status, m)
	}

	f.Comment(idxName, "the DBA wrote this")
	if _, status, err := IndexMarker(ctx(), f, idxName); err != nil || status != protocol.MarkerForeign {
		t.Fatalf("status = %v, err = %v; a human comment is foreign", status, err)
	}

	if _, status, err := IndexMarker(ctx(), f, name("public", "nothing_here")); err != nil ||
		status != protocol.MarkerAbsent {
		t.Fatalf("status = %v, err = %v; an unknown index is absent, not an error", status, err)
	}
}
