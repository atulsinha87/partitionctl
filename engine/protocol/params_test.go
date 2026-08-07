package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Every kind must have params, and NewParams must return the type whose Kind()
// answers with that kind. This is what makes Node's decode dispatch total.
func TestNewParamsCoversTheVocabulary(t *testing.T) {
	for _, kind := range AllNodeKinds() {
		p, err := NewParams(kind)
		if err != nil {
			t.Fatalf("NewParams(%s): %v", kind, err)
		}
		if p.Kind() != kind {
			t.Errorf("NewParams(%s).Kind() = %s", kind, p.Kind())
		}
		if reflect.ValueOf(p).Kind() != reflect.Ptr {
			t.Errorf("NewParams(%s) returned a non-pointer, which json.Unmarshal cannot fill", kind)
		}
	}
	if _, err := NewParams("column.backfill"); err == nil {
		t.Error("NewParams accepted a kind outside the vocabulary")
	}
}

// FR-PLANFILE-6: params are the authoritative input, so they must survive the
// file round trip exactly.
func TestParamsRoundTripLosslessly(t *testing.T) {
	rel := NewObjectName("public", "orders_2026_01")
	idx := NewObjectName("public", "orders_idx")
	child := NewObjectName("public", "orders_idx_orders_2026_01")
	count := 12

	cases := []NodeParams{
		&CatalogAssertParams{Assertions: []Assertion{
			{Assertion: AssertPartitionDepth, Relation: &rel, Expected: []string{"1"}, FailureCode: ExitUnsupportedTopology, Message: "v0.1 supports one level"},
			{Assertion: AssertRoleMembership, Relation: &rel, Role: "migrator", FailureCode: ExitInsufficientPrivilege},
		}},
		&CreateParentInvalidParams{Parent: rel, Index: idx, Definition: IndexDefinition{
			Method:        "gin",
			Columns:       []IndexColumn{{Name: "payload", OpClass: "jsonb_path_ops"}},
			Unique:        true,
			Include:       []string{"tenant_id", "region"},
			Where:         "deleted_at IS NULL",
			Tablespace:    "ssd",
			StorageParams: map[string]string{"fastupdate": "off"},
		}},
		&CreateConcurrentlyParams{Partition: rel, Index: child, ParentIndex: &idx, Definition: IndexDefinition{
			Columns: []IndexColumn{
				{Name: "created_at", Descending: true, NullsFirst: boolPtr(true)},
				{Name: "id", NullsFirst: boolPtr(false)},
				{Expression: "coalesce(status, 'none')"},
			},
		}},
		&AttachParams{ParentIndex: idx, ChildIndex: child},
		&VerifyParams{Checks: []VerifyCheck{
			{Check: CheckIndexValid, Index: &child},
			{Check: CheckIndexAttached, Index: &child, ParentIndex: &idx},
			{Check: CheckLeafIndexCount, ParentIndex: &idx, ExpectedCount: &count},
			{Check: CheckNoLeftoverIndexes, Relation: &rel},
		}},
		&WaitParams{Seconds: 45, Reason: "pace the build"},
		&DropConcurrentlyParams{Index: child, Relation: &rel, Reason: DropCCNew},
		&ReindexConcurrentlyParams{Index: child, Relation: &rel, ParentIndex: &idx, EstimatedPeakBytes: 1 << 40},
		&DropPartitionedParams{Parent: rel, Index: idx, LeafCount: 400},
	}

	if len(cases) != len(AllNodeKinds()) {
		t.Fatalf("round-trip cases cover %d kinds, the vocabulary has %d", len(cases), len(AllNodeKinds()))
	}

	for _, want := range cases {
		t.Run(string(want.Kind()), func(t *testing.T) {
			if err := want.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			encoded, err := json.Marshal(want)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			got, err := NewParams(want.Kind())
			if err != nil {
				t.Fatalf("NewParams: %v", err)
			}
			if err := json.Unmarshal(encoded, got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip lost information:\n got %+v\nwant %+v", got, want)
			}
			again, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("Marshal (second): %v", err)
			}
			if string(again) != string(encoded) {
				t.Fatalf("re-encoding is not a fixed point:\n%s\n%s", encoded, again)
			}
		})
	}
}

// A *bool must distinguish "unset" from "false" through the file, because
// NULLS FIRST/LAST defaults depend on the sort direction.
func TestNullsFirstTristate(t *testing.T) {
	for _, want := range []*bool{nil, boolPtr(true), boolPtr(false)} {
		col := IndexColumn{Name: "created_at", NullsFirst: want}
		encoded, err := json.Marshal(col)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var got IndexColumn
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		switch {
		case want == nil && got.NullsFirst != nil:
			t.Errorf("nil became %v", *got.NullsFirst)
		case want != nil && got.NullsFirst == nil:
			t.Errorf("%v became nil (encoded as %s)", *want, encoded)
		case want != nil && *got.NullsFirst != *want:
			t.Errorf("%v became %v", *want, *got.NullsFirst)
		}
	}
}

func TestParamsValidation(t *testing.T) {
	rel := NewObjectName("public", "orders")
	idx := NewObjectName("public", "orders_idx")
	bad := NewObjectName("public", "a\x00b")
	negative := -1

	tests := []struct {
		name   string
		params NodeParams
		want   string
	}{
		{"assert with no assertions", &CatalogAssertParams{}, "no assertions"},
		{"assert unknown predicate", &CatalogAssertParams{Assertions: []Assertion{{Assertion: "vibes"}}}, "unknown assertion"},
		{"assert role membership without a role", &CatalogAssertParams{Assertions: []Assertion{{Assertion: AssertRoleMembership, Relation: &rel}}}, "requires role"},
		{"parent invalid without a parent", &CreateParentInvalidParams{Index: idx, Definition: okDefinition()}, "parent is required"},
		{"parent invalid without columns", &CreateParentInvalidParams{Parent: rel, Index: idx}, "no columns"},
		{"parent invalid with a bad identifier", &CreateParentInvalidParams{Parent: rel, Index: bad, Definition: okDefinition()}, "index"},
		{"column with neither name nor expression", &CreateParentInvalidParams{Parent: rel, Index: idx, Definition: IndexDefinition{Columns: []IndexColumn{{}}}}, "neither name nor expression"},
		{"column with both", &CreateParentInvalidParams{Parent: rel, Index: idx, Definition: IndexDefinition{Columns: []IndexColumn{{Name: "a", Expression: "b"}}}}, "exactly one"},
		{"cic without an index", &CreateConcurrentlyParams{Partition: rel, Definition: okDefinition()}, "index is required"},
		{"attach without a child", &AttachParams{ParentIndex: idx}, "child_index is required"},
		{"verify with no checks", &VerifyParams{}, "no checks"},
		{"verify unknown check", &VerifyParams{Checks: []VerifyCheck{{Check: "looks_fine"}}}, "unknown check"},
		{"verify index_valid without an index", &VerifyParams{Checks: []VerifyCheck{{Check: CheckIndexValid}}}, "requires index"},
		{"verify attachment without a parent", &VerifyParams{Checks: []VerifyCheck{{Check: CheckIndexAttached, Index: &idx}}}, "parent_index"},
		{"verify leaf count without a count", &VerifyParams{Checks: []VerifyCheck{{Check: CheckLeafIndexCount, ParentIndex: &idx}}}, "expected_count"},
		{"verify negative leaf count", &VerifyParams{Checks: []VerifyCheck{{Check: CheckLeafIndexCount, ParentIndex: &idx, ExpectedCount: &negative}}}, "negative"},
		{"verify leftovers without a relation", &VerifyParams{Checks: []VerifyCheck{{Check: CheckNoLeftoverIndexes}}}, "requires relation"},
		{"negative wait", &WaitParams{Seconds: -1}, "negative"},
		{"drop without an index", &DropConcurrentlyParams{}, "index is required"},
		{"drop with an unknown reason", &DropConcurrentlyParams{Index: idx, Reason: "felt like it"}, "unknown drop reason"},
		{"reindex without an index", &ReindexConcurrentlyParams{}, "index is required"},
		{"reindex with negative storage", &ReindexConcurrentlyParams{Index: idx, EstimatedPeakBytes: -1}, "negative"},
		{"drop partitioned without a parent", &DropPartitionedParams{Index: idx}, "parent is required"},
		{"drop partitioned with a negative leaf count", &DropPartitionedParams{Parent: rel, Index: idx, LeafCount: -1}, "negative"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.params.Validate()
			if err == nil {
				t.Fatal("Validate accepted invalid params")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("message %q does not contain %q", err, tc.want)
			}
		})
	}
}

// The SQL fragments that cannot be identifier-quoted must still be checked for
// well-formedness (see the package doc's trust boundary note).
func TestSQLFragmentsAreChecked(t *testing.T) {
	rel := NewObjectName("public", "orders")
	idx := NewObjectName("public", "orders_idx")

	for _, bad := range []string{"a\x00b", "a\xffb"} {
		p := &CreateParentInvalidParams{Parent: rel, Index: idx, Definition: IndexDefinition{
			Columns: []IndexColumn{{Expression: bad}},
		}}
		if err := p.Validate(); err == nil {
			t.Errorf("expression %q was accepted", bad)
		}
		p = &CreateParentInvalidParams{Parent: rel, Index: idx, Definition: IndexDefinition{
			Columns: []IndexColumn{{Name: "created_at"}},
			Where:   bad,
		}}
		if err := p.Validate(); err == nil {
			t.Errorf("predicate %q was accepted", bad)
		}
	}

	// A legitimate expression with a quoted literal must pass.
	ok := &CreateParentInvalidParams{Parent: rel, Index: idx, Definition: IndexDefinition{
		Columns: []IndexColumn{{Expression: "lower(email)"}},
		Where:   "status <> 'deleted; drop'",
	}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("a legitimate expression was rejected: %v", err)
	}
}

func TestNodeMarshalRejectsKindParamsMismatch(t *testing.T) {
	n := Node{ID: "n", Kind: KindWait, Params: &AttachParams{
		ParentIndex: NewObjectName("public", "a"), ChildIndex: NewObjectName("public", "b"),
	}}
	if _, err := json.Marshal(n); err == nil {
		t.Fatal("Marshal wrote a node whose kind and params disagree")
	}
}

func TestNodeUnmarshalDispatchesOnKind(t *testing.T) {
	for _, kind := range AllNodeKinds() {
		params, err := NewParams(kind)
		if err != nil {
			t.Fatalf("NewParams(%s): %v", kind, err)
		}
		raw := []byte(`{"id":"n","kind":"` + string(kind) + `","params":{}}`)
		var n Node
		if err := json.Unmarshal(raw, &n); err != nil {
			t.Fatalf("Unmarshal(%s): %v", kind, err)
		}
		if n.Kind != kind {
			t.Errorf("kind = %s, want %s", n.Kind, kind)
		}
		if reflect.TypeOf(n.Params) != reflect.TypeOf(params) {
			t.Errorf("kind %s decoded params of type %T, want %T", kind, n.Params, params)
		}
	}
}

func TestNodeUnmarshalHandlesNullParams(t *testing.T) {
	var n Node
	if err := json.Unmarshal([]byte(`{"id":"n","kind":"wait","params":null}`), &n); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if n.Params != nil {
		t.Fatalf("params = %v, want nil", n.Params)
	}
	// And a nil-params node must fail validation rather than execute blank.
	if err := n.Validate(); err == nil {
		t.Fatal("Validate accepted a node with no params")
	}
}

func TestNodeRoundTrip(t *testing.T) {
	p := samplePlan(t)
	for _, want := range p.Nodes {
		encoded, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var got Node
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("node %s did not round-trip:\n got %+v\nwant %+v", want.ID, got, want)
		}
	}
}

func okDefinition() IndexDefinition {
	return IndexDefinition{Columns: []IndexColumn{{Name: "created_at"}}}
}
