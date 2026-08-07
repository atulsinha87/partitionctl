package protocol

import (
	"testing"
	"time"
)

// samplePlan builds a small but representative CreatePartitionedIndex plan:
// assert -> parent -> (cic -> verify -> attach -> wait) -> final verify, plus a
// provenance-authorized cleanup drop. It exercises every field the digest
// covers, including a map, a pointer bool, and a destructive node.
func samplePlan(t *testing.T) *Plan {
	t.Helper()

	def := IndexDefinition{
		Method: "btree",
		Columns: []IndexColumn{
			{Name: "created_at", Descending: true, NullsFirst: boolPtr(false)},
			{Expression: "lower(email)", Collation: "C", OpClass: "text_pattern_ops"},
		},
		Include:       []string{"tenant_id"},
		Where:         "status <> 'deleted'",
		Tablespace:    "fastdisk",
		StorageParams: map[string]string{"fillfactor": "90", "deduplicate_items": "on"},
	}

	parent := NewObjectName("public", "orders")
	parentIdx := NewObjectName("public", "orders_created_at_idx")
	leaf := NewObjectName("public", "orders_2026_03")
	child := NewObjectName("public", ChildIndexName(parentIdx.Name, leaf.Name))
	expected := 1

	p := &Plan{
		FormatVersion: PlanFormatVersion,
		PlanID:        "plan-01HZ",
		Operation:     OpCreateIndex,
		Target:        Target{Database: "app", Table: parent, Index: &parentIdx},
		CreatedAt:     NewTimestamp(time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)),
		Nodes: []Node{
			{
				ID:   "assert",
				Kind: KindCatalogAssert,
				Params: &CatalogAssertParams{Assertions: []Assertion{
					{Assertion: AssertRelationIsPartitioned, Relation: &parent, FailureCode: ExitUnsupportedTopology},
					{Assertion: AssertPartitionStrategy, Relation: &parent, Expected: []string{"RANGE", "LIST"}, FailureCode: ExitUnsupportedTopology},
					{Assertion: AssertRoleMembership, Relation: &parent, Role: "app_migrator", FailureCode: ExitInsufficientPrivilege},
				}},
				RenderedSQL: "-- preconditions",
			},
			{
				ID:               "cleanup",
				Kind:             KindIndexDropConcurrently,
				DependsOn:        []NodeID{"assert"},
				Params:           &DropConcurrentlyParams{Index: child, Relation: &leaf, Reason: DropInvalidBuild},
				RenderedSQL:      `DROP INDEX CONCURRENTLY "public"."orders_created_at_idx_orders_2026_03";`,
				EstimatedSeconds: 5,
				Authorization: &Authorization{
					Mode:     AuthProvenance,
					Object:   child,
					Relation: &leaf,
					Note:     "INVALID index created by run 01HY",
				},
			},
			{
				ID:          "parent",
				Kind:        KindIndexCreateParentInvalid,
				DependsOn:   []NodeID{"cleanup"},
				Params:      &CreateParentInvalidParams{Parent: parent, Index: parentIdx, Definition: def},
				RenderedSQL: `CREATE INDEX "orders_created_at_idx" ON ONLY "public"."orders" ...;`,
			},
			{
				ID:               "cic",
				Kind:             KindIndexCreateConcurrently,
				DependsOn:        []NodeID{"parent"},
				Params:           &CreateConcurrentlyParams{Partition: leaf, Index: child, Definition: def, ParentIndex: &parentIdx},
				EstimatedSeconds: 3600,
			},
			{
				ID:        "verify-child",
				Kind:      KindIndexVerify,
				DependsOn: []NodeID{"cic"},
				Params: &VerifyParams{Checks: []VerifyCheck{
					{Check: CheckIndexValid, Index: &child},
				}},
			},
			{
				ID:        "attach",
				Kind:      KindIndexAttach,
				DependsOn: []NodeID{"verify-child"},
				Params:    &AttachParams{ParentIndex: parentIdx, ChildIndex: child},
			},
			{
				ID:        "pace",
				Kind:      KindWait,
				DependsOn: []NodeID{"attach"},
				Params:    &WaitParams{Seconds: 30, Reason: "let replicas catch up"},
			},
			{
				ID:        "verify-all",
				Kind:      KindIndexVerify,
				DependsOn: []NodeID{"pace"},
				Params: &VerifyParams{Checks: []VerifyCheck{
					{Check: CheckParentIndexValid, ParentIndex: &parentIdx},
					{Check: CheckLeafIndexCount, ParentIndex: &parentIdx, ExpectedCount: &expected},
				}},
			},
		},
		TopologyFingerprint: "sha256:" + "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
	}
	if err := p.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return p
}

// sampleTopology is a two-leaf RANGE-partitioned table.
func sampleTopology() TopologyInput {
	return TopologyInput{
		Root:     RelationState{OID: 16400, Schema: "public", Name: "orders", RelKind: "p", OwnerOID: 10},
		Strategy: StrategyRange,
		Partitions: []RelationState{
			{OID: 16410, Schema: "public", Name: "orders_2026_01", RelKind: "r", OwnerOID: 10, ParentOID: 16400,
				PartitionBound: "FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')"},
			{OID: 16411, Schema: "public", Name: "orders_2026_02", RelKind: "r", OwnerOID: 10, ParentOID: 16400,
				PartitionBound: "FOR VALUES FROM ('2026-02-01') TO ('2026-03-01')"},
		},
	}
}

func boolPtr(b bool) *bool { return &b }
func intPtr(i int) *int    { return &i }

// testFingerprint is a well-formed topology fingerprint for fixtures that are
// not about fingerprinting. Plan.Validate requires one (FR-PLANFILE-4).
const testFingerprint = FingerprintPrefix +
	"0000000000000000000000000000000000000000000000000000000000000000"
