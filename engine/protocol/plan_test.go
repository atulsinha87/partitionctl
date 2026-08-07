package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSamplePlanValidates(t *testing.T) {
	if err := samplePlan(t).Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestPlanValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(p *Plan)
		wantErr error
	}{
		{"unsupported format version", func(p *Plan) { p.FormatVersion = 99 }, ErrUnsupportedFormatVersion},
		{"zero format version", func(p *Plan) { p.FormatVersion = 0 }, ErrUnsupportedFormatVersion},
		{"empty plan id", func(p *Plan) { p.PlanID = "" }, ErrInvalidPlan},
		{"unknown operation", func(p *Plan) { p.Operation = "backfill" }, ErrInvalidPlan},
		{"missing target table", func(p *Plan) { p.Target.Table = ObjectName{} }, ErrInvalidPlan},
		{"no nodes", func(p *Plan) { p.Nodes = nil }, ErrInvalidPlan},
		{"empty confirmation flag", func(p *Plan) { p.Confirmations = []Confirmation{{}} }, ErrInvalidPlan},
		{"duplicate node id", func(p *Plan) { p.Nodes[1].ID = p.Nodes[0].ID }, ErrInvalidPlan},
		{"empty node id", func(p *Plan) { p.Nodes[0].ID = "" }, ErrInvalidPlan},
		{"unknown node kind", func(p *Plan) { p.Nodes[0].Kind = "index.rebuild" }, ErrUnknownNodeKind},
		{"nil params", func(p *Plan) { p.Nodes[0].Params = nil }, ErrInvalidPlan},
		{"params kind mismatch", func(p *Plan) { p.Nodes[0].Params = &WaitParams{} }, ErrInvalidPlan},
		{"negative estimate", func(p *Plan) { p.Nodes[0].EstimatedSeconds = -1 }, ErrInvalidPlan},
		{"self dependency", func(p *Plan) { p.Nodes[2].DependsOn = []NodeID{p.Nodes[2].ID} }, ErrInvalidPlan},
		{"duplicate dependency", func(p *Plan) { p.Nodes[2].DependsOn = []NodeID{"assert", "assert"} }, ErrInvalidPlan},
		{"unknown dependency", func(p *Plan) { p.Nodes[2].DependsOn = []NodeID{"ghost"} }, ErrInvalidPlan},
		{"cycle", func(p *Plan) {
			p.Nodes[0].DependsOn = []NodeID{"verify-all"}
		}, ErrInvalidPlan},
		{"destructive node without authorization", func(p *Plan) {
			p.Nodes[1].Authorization = nil
		}, ErrAuthorizationUnsatisfied},
		{"non-destructive node with authorization", func(p *Plan) {
			p.Nodes[0].Authorization = &Authorization{Mode: AuthProvenance, Object: NewObjectName("public", "x")}
		}, ErrInvalidPlan},
		{"unknown authorization mode", func(p *Plan) {
			p.Nodes[1].Authorization.Mode = "because_i_said_so"
		}, ErrInvalidPlan},
		{"explicit authorization without a confirmation flag", func(p *Plan) {
			p.Nodes[1].Authorization.Mode = AuthExplicit
			p.Nodes[1].Authorization.RequiredConfirmation = ""
		}, ErrInvalidPlan},
		{"leftover authorization without a relation", func(p *Plan) {
			p.Nodes[1].Authorization.Mode = AuthLeftover
			p.Nodes[1].Authorization.Relation = nil
		}, ErrInvalidPlan},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := samplePlan(t)
			tc.mutate(p)
			err := p.Validate()
			if err == nil {
				t.Fatal("Validate accepted an invalid plan")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error %v is not %v", err, tc.wantErr)
			}
		})
	}
}

// INV-8: at most one index.drop_partitioned per plan, and no other destructive
// node alongside it. FR-DROP-3 / AC-13: the acknowledgement must be recorded.
func TestPlanValidateEnforcesDropInvariants(t *testing.T) {
	parent := NewObjectName("public", "orders")
	idx := NewObjectName("public", "orders_legacy_idx")
	leaf := NewObjectName("public", "orders_2026_01")
	orphan := NewObjectName("public", "orders_legacy_idx_orders_2026_01")

	dropNode := func(id NodeID) Node {
		return Node{
			ID:     id,
			Kind:   KindIndexDropPartitioned,
			Params: &DropPartitionedParams{Parent: parent, Index: idx, LeafCount: 400},
			Authorization: &Authorization{
				Mode: AuthExplicit, Object: idx, RequiredConfirmation: ConfirmExclusiveLock,
			},
		}
	}

	base := func() *Plan {
		return &Plan{
			FormatVersion: PlanFormatVersion,
			PlanID:        "drop-1",
			Operation:     OpDropIndex,
			Target:        Target{Table: parent, Index: &idx},
			CreatedAt:     Now(),
			Confirmations: []Confirmation{{Flag: ConfirmExclusiveLock, Actor: "atul", At: Now()}},
			Nodes:         []Node{dropNode("drop")},
			// FR-PLANFILE-4: a plan is bound to the tree it was computed over,
			// so Validate requires one.
			TopologyFingerprint: testFingerprint,
		}
	}

	t.Run("valid", func(t *testing.T) {
		if err := base().Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("two drop_partitioned nodes", func(t *testing.T) {
		p := base()
		p.Nodes = append(p.Nodes, dropNode("drop2"))
		err := p.Validate()
		if !errors.Is(err, ErrInvalidPlan) || !strings.Contains(err.Error(), "INV-8") {
			t.Fatalf("got %v", err)
		}
	})

	// INV-8 as amended. An unattached orphan leaf index survives the parent's
	// cascade, so the only correct drop sequence removes the orphans first and
	// then drops the parent (TRD §7.2.13). The original invariant forbade any
	// other destructive node, which made every correct drop plan invalid.
	t.Run("orphan cleanup alongside the partitioned drop is permitted", func(t *testing.T) {
		p := base()
		p.Nodes = append(p.Nodes, Node{
			ID:     "orphan-cleanup",
			Kind:   KindIndexDropConcurrently,
			Params: &DropConcurrentlyParams{Index: orphan, Relation: &leaf, Reason: DropUnattachedOrphan},
			Authorization: &Authorization{
				Mode: AuthProvenance, Object: orphan, Relation: &leaf,
			},
		})
		if err := p.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("acknowledgement not recorded", func(t *testing.T) {
		p := base()
		p.Confirmations = nil
		err := p.Validate()
		if !errors.Is(err, ErrInvalidPlan) || !strings.Contains(err.Error(), ConfirmExclusiveLock) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("a different acknowledgement does not count", func(t *testing.T) {
		p := base()
		p.Confirmations = []Confirmation{{Flag: "--yes-really", At: Now()}}
		if err := p.Validate(); !errors.Is(err, ErrInvalidPlan) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestTopologicalOrder(t *testing.T) {
	p := samplePlan(t)
	order, err := p.TopologicalOrder()
	if err != nil {
		t.Fatalf("TopologicalOrder: %v", err)
	}
	if len(order) != len(p.Nodes) {
		t.Fatalf("got %d ids for %d nodes", len(order), len(p.Nodes))
	}

	position := make(map[NodeID]int, len(order))
	for i, id := range order {
		position[id] = i
	}
	for _, n := range p.Nodes {
		for _, d := range n.DependsOn {
			if position[d] >= position[n.ID] {
				t.Errorf("dependency %s is not before %s", d, n.ID)
			}
		}
	}
}

// Determinism matters because `render`, `--dry-run` and the executor must all
// agree on the sequence without coordinating.
func TestTopologicalOrderIsDeterministic(t *testing.T) {
	// A wide graph: one root, many independent chains, one terminal. This is
	// the shape of every v0.1 plan.
	p := &Plan{
		FormatVersion:       PlanFormatVersion,
		PlanID:              "wide",
		Operation:           OpCreateIndex,
		Target:              Target{Table: NewObjectName("public", "orders")},
		CreatedAt:           Now(),
		TopologyFingerprint: testFingerprint,
	}
	parent := NewObjectName("public", "orders_idx")
	p.Nodes = append(p.Nodes, Node{
		ID:     "root",
		Kind:   KindCatalogAssert,
		Params: &CatalogAssertParams{Assertions: []Assertion{{Assertion: AssertRelationIsPartitioned}}},
	})
	var leaves []NodeID
	for i := 0; i < 50; i++ {
		id := NodeID("leaf-" + string(rune('a'+i%26)) + string(rune('0'+i/26)))
		idx := NewObjectName("public", "orders_idx_p"+string(rune('a'+i%26))+string(rune('0'+i/26)))
		p.Nodes = append(p.Nodes, Node{
			ID:        id,
			Kind:      KindIndexAttach,
			DependsOn: []NodeID{"root"},
			Params:    &AttachParams{ParentIndex: parent, ChildIndex: idx},
		})
		leaves = append(leaves, id)
	}
	p.Nodes = append(p.Nodes, Node{
		ID:        "terminal",
		Kind:      KindIndexVerify,
		DependsOn: leaves,
		Params:    &VerifyParams{Checks: []VerifyCheck{{Check: CheckParentIndexValid, ParentIndex: &parent}}},
	})
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	first, err := p.TopologicalOrder()
	if err != nil {
		t.Fatalf("TopologicalOrder: %v", err)
	}
	for i := 0; i < 100; i++ {
		got, err := p.TopologicalOrder()
		if err != nil {
			t.Fatalf("TopologicalOrder: %v", err)
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("iteration %d position %d: %s != %s", i, j, got[j], first[j])
			}
		}
	}
	// Ties break by position in Plan.Nodes, so the order is the plan's order
	// for a graph with no cross-chain edges.
	for i, n := range p.Nodes {
		if first[i] != n.ID {
			t.Fatalf("position %d: got %s, want %s", i, first[i], n.ID)
		}
	}
}

func TestTopologicalOrderDetectsCycles(t *testing.T) {
	p := samplePlan(t)
	p.Nodes[0].DependsOn = []NodeID{"attach"}
	_, err := p.TopologicalOrder()
	if !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("error %v is not ErrInvalidPlan", err)
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("message %q does not name the cycle", err)
	}
}

func TestPlanHelpers(t *testing.T) {
	p := samplePlan(t)

	if p.Confirmed(ConfirmExclusiveLock) {
		t.Error("Confirmed reported an acknowledgement that was never given")
	}
	p.Confirmations = []Confirmation{{Flag: ConfirmExclusiveLock, At: Now()}}
	if !p.Confirmed(ConfirmExclusiveLock) {
		t.Error("Confirmed did not find a recorded acknowledgement")
	}

	n, ok := p.NodeByID("attach")
	if !ok || n.Kind != KindIndexAttach {
		t.Errorf("NodeByID(attach) = %v, %v", n, ok)
	}
	if _, ok := p.NodeByID("ghost"); ok {
		t.Error("NodeByID found a node that does not exist")
	}

	if got := p.TotalEstimatedSeconds(); got != 3605 {
		t.Errorf("TotalEstimatedSeconds() = %d, want 3605", got)
	}
}

func TestDecodePlanChecksFormatVersion(t *testing.T) {
	p := samplePlan(t)
	encoded, err := EncodePlan(p)
	if err != nil {
		t.Fatalf("EncodePlan: %v", err)
	}
	// A version beyond anything this binary supports, i.e. a plan written by a
	// newer build.
	current := fmt.Sprintf(`"format_version": %d`, PlanFormatVersion)
	bumped := strings.Replace(string(encoded), current, `"format_version": 99`, 1)
	if bumped == string(encoded) {
		t.Fatal("fixture does not contain the format version")
	}

	_, err = DecodePlan([]byte(bumped))
	if !errors.Is(err, ErrUnsupportedFormatVersion) {
		t.Fatalf("error %v is not ErrUnsupportedFormatVersion", err)
	}
	if !strings.Contains(err.Error(), "99") {
		t.Fatalf("message %q does not name the version", err)
	}
}

func TestDecodePlanRejectsMalformedJSON(t *testing.T) {
	if _, err := DecodePlan([]byte(`{`)); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("error %v is not ErrInvalidPlan", err)
	}
}

func TestDecodePlanRejectsAnUnknownNodeKind(t *testing.T) {
	// NFR-COMPAT-3's version gate is only meaningful if an unreadable node is
	// loud. A plan from a newer binary must not decode with its params dropped.
	raw := `{
	  "format_version": 1,
	  "plan_id": "p",
	  "operation": "create-index",
	  "target": {"table": {"schema": "public", "name": "orders"}},
	  "created_at": "2026-08-07T12:00:00Z",
	  "nodes": [{"id": "n", "kind": "column.backfill", "params": {"batch": 1000}}]
	}`
	if _, err := DecodePlan([]byte(raw)); !errors.Is(err, ErrUnknownNodeKind) {
		t.Fatalf("error %v is not ErrUnknownNodeKind", err)
	}
}

func TestEncodePlanIsReviewable(t *testing.T) {
	p := samplePlan(t)
	encoded, err := EncodePlan(p)
	if err != nil {
		t.Fatalf("EncodePlan: %v", err)
	}
	if !strings.HasSuffix(string(encoded), "\n") {
		t.Error("plan file does not end with a newline")
	}
	if !strings.Contains(string(encoded), "\n  \"plan_id\"") {
		t.Error("plan file is not indented for review")
	}
	// The reviewer must be able to read the SQL in the diff.
	if !strings.Contains(string(encoded), "rendered_sql") {
		t.Error("plan file carries no rendered SQL for the reviewer")
	}
	if !json.Valid(encoded) {
		t.Error("plan file is not valid JSON")
	}
}

func TestOperationValidity(t *testing.T) {
	for _, op := range AllOperations() {
		if !op.Valid() {
			t.Errorf("%s is not valid", op)
		}
	}
	if Operation("backfill-column").Valid() {
		t.Error("an unshipped operation reported valid")
	}
	if len(AllOperations()) != 3 {
		t.Errorf("v0.1 ships three operations, got %d", len(AllOperations()))
	}
}
