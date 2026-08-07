package protocol

import (
	"errors"
	"math/rand"
	"testing"
)

const goldenFingerprint = "sha256:6a2d13530cf4fe26f1ef0a5d704fabcd5160706fa2c8c43a1080fd9ab60a1d7a"

func TestFingerprintGolden(t *testing.T) {
	got, err := sampleTopology().Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if got != goldenFingerprint {
		t.Fatalf("fingerprint drifted.\n got %s\nwant %s", got, goldenFingerprint)
	}
}

// FR-PLANFILE-4: the fingerprint must be order-independent over the partition
// set, because partition discovery order is not guaranteed by any catalog
// query.
func TestFingerprintIsOrderIndependent(t *testing.T) {
	base := TopologyInput{
		Root:     RelationState{OID: 16400, Schema: "public", Name: "orders", RelKind: "p", OwnerOID: 10},
		Strategy: StrategyRange,
	}
	const n = 200
	for i := 0; i < n; i++ {
		base.Partitions = append(base.Partitions, RelationState{
			OID: uint32(20000 + i), Schema: "public", Name: partitionName(i), RelKind: "r",
			OwnerOID: 10, ParentOID: 16400, PartitionBound: "FOR VALUES IN (" + partitionName(i) + ")",
		})
	}

	want, err := base.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 50; trial++ {
		shuffled := TopologyInput{Root: base.Root, Strategy: base.Strategy}
		shuffled.Partitions = append(shuffled.Partitions, base.Partitions...)
		rng.Shuffle(len(shuffled.Partitions), func(i, j int) {
			shuffled.Partitions[i], shuffled.Partitions[j] = shuffled.Partitions[j], shuffled.Partitions[i]
		})
		got, err := shuffled.Fingerprint()
		if err != nil {
			t.Fatalf("Fingerprint: %v", err)
		}
		if got != want {
			t.Fatalf("trial %d: shuffling changed the fingerprint: %s != %s", trial, got, want)
		}
	}
}

func TestFingerprintIsStable(t *testing.T) {
	top := sampleTopology()
	want, err := top.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	for i := 0; i < 200; i++ {
		got, err := top.Fingerprint()
		if err != nil {
			t.Fatalf("Fingerprint: %v", err)
		}
		if got != want {
			t.Fatalf("iteration %d: %s != %s", i, got, want)
		}
	}
}

// Every field that is in the input must move the fingerprint, and adding or
// removing a partition must move it (AC-3, R6: new partitions are the expected
// drift on a time-partitioned table).
func TestFingerprintCoversEveryField(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *TopologyInput)
	}{
		{"root oid", func(t *TopologyInput) { t.Root.OID = 99 }},
		{"root schema", func(t *TopologyInput) { t.Root.Schema = "other" }},
		{"root name", func(t *TopologyInput) { t.Root.Name = "other" }},
		{"root relkind", func(t *TopologyInput) { t.Root.RelKind = "r" }},
		{"root owner", func(t *TopologyInput) { t.Root.OwnerOID = 11 }},
		{"strategy", func(t *TopologyInput) { t.Strategy = StrategyList }},
		{"partition oid", func(t *TopologyInput) { t.Partitions[0].OID = 99999 }},
		{"partition name", func(t *TopologyInput) { t.Partitions[0].Name = "renamed" }},
		{"partition schema", func(t *TopologyInput) { t.Partitions[0].Schema = "other" }},
		{"partition relkind", func(t *TopologyInput) { t.Partitions[0].RelKind = "p" }},
		{"partition owner", func(t *TopologyInput) { t.Partitions[0].OwnerOID = 11 }},
		{"partition parent", func(t *TopologyInput) { t.Partitions[0].ParentOID = 1 }},
		{"partition bound", func(t *TopologyInput) { t.Partitions[0].PartitionBound = "FOR VALUES FROM (x) TO (y)" }},
		{"partition default flag", func(t *TopologyInput) { t.Partitions[0].IsDefault = true }},
		{"partition added", func(t *TopologyInput) {
			t.Partitions = append(t.Partitions, RelationState{OID: 16412, Schema: "public", Name: "orders_2026_03", RelKind: "r"})
		}},
		{"partition removed", func(t *TopologyInput) { t.Partitions = t.Partitions[:1] }},
	}

	want, err := sampleTopology().Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t2 *testing.T) {
			top := sampleTopology()
			tc.mutate(&top)
			got, err := top.Fingerprint()
			if err != nil {
				t2.Fatalf("Fingerprint: %v", err)
			}
			if got == want {
				t2.Fatalf("mutating %s did not change the fingerprint", tc.name)
			}
		})
	}
}

// Swapping the identities of two partitions must be visible. This is the case a
// naive XOR combiner would miss.
func TestFingerprintDetectsSwappedIdentities(t *testing.T) {
	a := sampleTopology()
	b := sampleTopology()
	b.Partitions[0].Name, b.Partitions[1].Name = b.Partitions[1].Name, b.Partitions[0].Name

	fa, err := a.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	fb, err := b.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fa == fb {
		t.Fatal("swapping two partitions' names did not change the fingerprint")
	}
}

func TestFingerprintRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		in   TopologyInput
	}{
		{"no root name", TopologyInput{Strategy: StrategyRange}},
		{"unnamed partition", TopologyInput{
			Root:       RelationState{OID: 1, Name: "orders"},
			Partitions: []RelationState{{OID: 2}},
		}},
		{"duplicate oids", TopologyInput{
			Root: RelationState{OID: 1, Name: "orders"},
			Partitions: []RelationState{
				{OID: 2, Name: "a"},
				{OID: 2, Name: "b"},
			},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.in.Fingerprint(); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("Fingerprint accepted invalid input: %v", err)
			}
		})
	}
}

func TestVerifyTopology(t *testing.T) {
	top := sampleTopology()
	fp, err := top.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	p := samplePlan(t)
	p.TopologyFingerprint = fp
	if err := p.VerifyTopology(top); err != nil {
		t.Fatalf("VerifyTopology on a matching topology: %v", err)
	}

	// AC-3: a partition added between plan and execute must abort.
	drifted := sampleTopology()
	drifted.Partitions = append(drifted.Partitions, RelationState{
		OID: 16412, Schema: "public", Name: "orders_2026_03", RelKind: "r", ParentOID: 16400,
	})
	err = p.VerifyTopology(drifted)
	if !errors.Is(err, ErrTopologyDrift) {
		t.Fatalf("error %v is not ErrTopologyDrift", err)
	}
	if code := ExitCodeFor(err); code != ExitTopologyDrift {
		t.Fatalf("exit code %d, want %d", code, ExitTopologyDrift)
	}

	p.TopologyFingerprint = ""
	if err := p.VerifyTopology(top); !errors.Is(err, ErrTopologyDrift) {
		t.Fatalf("a plan with no fingerprint was accepted: %v", err)
	}
}

// AC-3 requires the drift to be *named*, not merely detected.
func TestDiffTopology(t *testing.T) {
	planned := sampleTopology()

	t.Run("identical", func(t *testing.T) {
		if got := DiffTopology(planned, sampleTopology()); len(got) != 0 {
			t.Fatalf("got %v, want no changes", got)
		}
	})

	t.Run("added", func(t *testing.T) {
		live := sampleTopology()
		live.Partitions = append(live.Partitions, RelationState{
			OID: 16412, Schema: "public", Name: "orders_2026_03", RelKind: "r", ParentOID: 16400,
		})
		got := DiffTopology(planned, live)
		if len(got) != 1 || got[0].Change != TopologyPartitionAdded || got[0].Relation != "public.orders_2026_03" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("removed", func(t *testing.T) {
		live := sampleTopology()
		live.Partitions = live.Partitions[:1]
		got := DiffTopology(planned, live)
		if len(got) != 1 || got[0].Change != TopologyPartitionRemoved || got[0].Relation != "public.orders_2026_02" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("changed", func(t *testing.T) {
		live := sampleTopology()
		live.Partitions[0].PartitionBound = "FOR VALUES FROM ('2026-01-15') TO ('2026-02-01')"
		got := DiffTopology(planned, live)
		if len(got) != 1 || got[0].Change != TopologyPartitionChanged || got[0].OID != 16410 {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("root and strategy", func(t *testing.T) {
		live := sampleTopology()
		live.Root.OwnerOID = 11
		live.Strategy = StrategyList
		got := DiffTopology(planned, live)
		if len(got) != 2 || got[0].Change != TopologyRootChanged || got[1].Change != TopologyStrategyChanged {
			t.Fatalf("got %v", got)
		}
	})

	// Deterministic ordering: same inputs, same output, regardless of the
	// order the partitions arrive in.
	t.Run("deterministic order", func(t *testing.T) {
		live := sampleTopology()
		live.Partitions = live.Partitions[:1]
		live.Partitions = append(live.Partitions,
			RelationState{OID: 16420, Schema: "public", Name: "orders_2026_04", RelKind: "r", ParentOID: 16400},
			RelationState{OID: 16413, Schema: "public", Name: "orders_2026_03", RelKind: "r", ParentOID: 16400},
		)
		want := DiffTopology(planned, live)
		if len(want) != 3 {
			t.Fatalf("expected 3 changes, got %v", want)
		}
		for i := 1; i < len(want); i++ {
			if want[i-1].OID > want[i].OID {
				t.Fatalf("changes are not OID-ordered: %v", want)
			}
		}
		live.Partitions[1], live.Partitions[2] = live.Partitions[2], live.Partitions[1]
		got := DiffTopology(planned, live)
		if len(got) != len(want) {
			t.Fatalf("reordering changed the diff length")
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("reordering changed the diff at %d: %v vs %v", i, got[i], want[i])
			}
		}
	})
}

func TestPartitionStrategySupport(t *testing.T) {
	if !StrategyRange.SupportedInV01() || !StrategyList.SupportedInV01() {
		t.Error("RANGE and LIST must be supported in v0.1")
	}
	if StrategyHash.SupportedInV01() {
		t.Error("HASH must not be supported in v0.1 (FR-PLAN-3)")
	}
}

func partitionName(i int) string {
	const digits = "0123456789"
	return "orders_p" + string([]byte{digits[i/100%10], digits[i/10%10], digits[i%10]})
}
