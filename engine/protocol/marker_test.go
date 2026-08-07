package protocol

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func sampleMarker() Marker {
	return Marker{
		Run:    "run-8f2a1c",
		Plan:   "sha256:9d3f",
		Op:     string(OpCreateIndex),
		Role:   MarkerRoleLeaf,
		Parent: "public.orders_created_at_idx",
		At:     "2026-08-07T21:33:04Z",
	}
}

func TestFormatMarkerRoundTrips(t *testing.T) {
	want := sampleMarker()
	text, err := FormatMarker(want)
	if err != nil {
		t.Fatalf("FormatMarker: %v", err)
	}
	if !strings.HasPrefix(text, MarkerSentinel) {
		t.Fatalf("marker %q does not start with the sentinel", text)
	}
	if strings.Contains(text, "\n") {
		t.Fatalf("marker is not one line: %q", text)
	}
	got, status := ParseMarker(text)
	if status != MarkerOurs {
		t.Fatalf("status = %v, want MarkerOurs", status)
	}
	if got != want {
		t.Fatalf("round trip lost data:\n got %+v\nwant %+v", got, want)
	}
}

func TestFormatMarkerIsStable(t *testing.T) {
	// The marker text is embedded in rendered_sql, which is inside the plan
	// digest. Two renders of the same facts must produce the same bytes.
	a, err := FormatMarker(sampleMarker())
	if err != nil {
		t.Fatalf("FormatMarker: %v", err)
	}
	b, err := FormatMarker(sampleMarker())
	if err != nil {
		t.Fatalf("FormatMarker: %v", err)
	}
	if a != b {
		t.Fatalf("marker text is not stable:\n%q\n%q", a, b)
	}
}

func TestFormatMarkerRefusesIncompleteMarkers(t *testing.T) {
	for name, m := range map[string]Marker{
		"no run":  {Op: "create-index", Role: MarkerRoleLeaf, At: "2026-08-07T00:00:00Z"},
		"no op":   {Run: "r", Role: MarkerRoleLeaf, At: "2026-08-07T00:00:00Z"},
		"no role": {Run: "r", Op: "create-index", At: "2026-08-07T00:00:00Z"},
		"bad role": {Run: "r", Op: "create-index", Role: "sideways",
			At: "2026-08-07T00:00:00Z"},
		"no timestamp": {Run: "r", Op: "create-index", Role: MarkerRoleLeaf},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := FormatMarker(m); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("FormatMarker(%+v) = %v, want ErrInvalidPlan", m, err)
			}
		})
	}
}

func TestParseMarkerIsTotal(t *testing.T) {
	ours, err := FormatMarker(sampleMarker())
	if err != nil {
		t.Fatalf("FormatMarker: %v", err)
	}
	cases := []struct {
		name    string
		comment string
		want    MarkerStatus
	}{
		{"empty", "", MarkerAbsent},
		{"whitespace only", "   \n\t ", MarkerAbsent},
		{"ours", ours, MarkerOurs},
		{"ours with surrounding space", "  " + ours + "\n", MarkerOurs},
		{"a human comment", "created by dba for the quarterly report", MarkerForeign},
		{"a comment that mentions us", "see the partitionctl runbook", MarkerForeign},
		{"a future version", `partitionctl:v2:{"run":"r"}`, MarkerUnreadable},
		{"sentinel with broken json", MarkerSentinel + `{"run":`, MarkerUnreadable},
		{"sentinel with no payload", MarkerSentinel, MarkerUnreadable},
		{"sentinel with an empty object", MarkerSentinel + `{}`, MarkerUnreadable},
		{"sentinel naming no run", MarkerSentinel + `{"role":"leaf"}`, MarkerUnreadable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, got := ParseMarker(c.comment)
			if got != c.want {
				t.Fatalf("ParseMarker(%q) = %v, want %v", c.comment, got, c.want)
			}
		})
	}
}

func TestRenderCommentQuotesEverything(t *testing.T) {
	idx := NewObjectName("pub lic", `we"ird`)
	got := RenderComment(idx, "it's a marker")
	want := `COMMENT ON INDEX "pub lic"."we""ird" IS 'it''s a marker'`
	if got != want {
		t.Fatalf("RenderComment =\n %q\nwant %q", got, want)
	}
}

func TestMarkerTimeIsUTCSeconds(t *testing.T) {
	loc := time.FixedZone("plus-two", 2*60*60)
	got := MarkerTime(time.Date(2026, 8, 7, 23, 33, 4, 500_000_000, loc))
	if got != "2026-08-07T21:33:04Z" {
		t.Fatalf("MarkerTime = %q", got)
	}
}

func TestMarkerTargetForSwitchesOnKindAlone(t *testing.T) {
	parent := NewObjectName("public", "orders")
	pidx := NewObjectName("public", "orders_idx")
	leaf := NewObjectName("public", "orders_p1")
	cidx := NewObjectName("public", "orders_idx_orders_p1")

	cases := []struct {
		name        string
		node        Node
		wantOK      bool
		wantIndex   ObjectName
		wantRole    string
		wantParent  string
		wantRewrite bool
	}{
		{
			name: "create_parent_invalid marks the parent",
			node: Node{ID: "a", Kind: KindIndexCreateParentInvalid, Params: &CreateParentInvalidParams{
				Parent: parent, Index: pidx, Definition: oneColumn(),
			}},
			wantOK: true, wantIndex: pidx, wantRole: MarkerRoleParent,
		},
		{
			name: "create_concurrently marks the leaf and names its parent",
			node: Node{ID: "b", Kind: KindIndexCreateConcurrently, Params: &CreateConcurrentlyParams{
				Partition: leaf, Index: cidx, Definition: oneColumn(), ParentIndex: &pidx,
			}},
			wantOK: true, wantIndex: cidx, wantRole: MarkerRoleLeaf, wantParent: pidx.String(),
		},
		{
			name: "attach re-marks the leaf as a backstop",
			node: Node{ID: "c", Kind: KindIndexAttach, Params: &AttachParams{
				ParentIndex: pidx, ChildIndex: cidx,
			}},
			wantOK: true, wantIndex: cidx, wantRole: MarkerRoleLeaf, wantParent: pidx.String(),
		},
		{
			name: "reindex rewrites the leaf marker",
			node: Node{ID: "d", Kind: KindIndexReindexConcurrently, Params: &ReindexConcurrentlyParams{
				Index: cidx, Relation: &leaf, ParentIndex: &pidx,
			}},
			wantOK: true, wantIndex: cidx, wantRole: MarkerRoleLeaf, wantParent: pidx.String(),
			wantRewrite: true,
		},
		{
			name: "drop_concurrently marks nothing",
			node: Node{ID: "e", Kind: KindIndexDropConcurrently, Params: &DropConcurrentlyParams{
				Index: cidx, Relation: &leaf,
			}},
		},
		{
			name: "drop_partitioned marks nothing",
			node: Node{ID: "f", Kind: KindIndexDropPartitioned, Params: &DropPartitionedParams{
				Parent: parent, Index: pidx,
			}},
		},
		{
			name: "wait marks nothing",
			node: Node{ID: "g", Kind: KindWait, Params: &WaitParams{Seconds: 1}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok, err := MarkerTargetFor(&c.node)
			if err != nil {
				t.Fatalf("MarkerTargetFor: %v", err)
			}
			if ok != c.wantOK {
				t.Fatalf("ok = %t, want %t", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if got.Index != c.wantIndex || got.Role != c.wantRole ||
				got.Parent != c.wantParent || got.Rewrite != c.wantRewrite {
				t.Fatalf("target = %+v", got)
			}
		})
	}
}

func TestRenderMarkerStatementPreservesCreationFactsOnReindex(t *testing.T) {
	cidx := NewObjectName("public", "orders_idx_p1")
	n := Node{ID: "r", Kind: KindIndexReindexConcurrently, Params: &ReindexConcurrentlyParams{Index: cidx}}
	prior := Marker{
		Run: "run-original", Plan: "sha256:aaa", Op: string(OpCreateIndex),
		Role: MarkerRoleLeaf, Parent: "public.orders_idx", At: "2026-01-01T00:00:00Z",
	}
	base := Marker{Run: "run-reindex", Plan: "sha256:bbb", Op: string(OpReindexIndex), At: "2026-08-07T00:00:00Z"}

	stmt, ok, err := RenderMarkerStatement(&n, base, prior, MarkerOurs)
	if err != nil || !ok {
		t.Fatalf("RenderMarkerStatement: ok=%t err=%v", ok, err)
	}
	text := stmt[strings.Index(stmt, "'")+1 : len(stmt)-1]
	got, status := ParseMarker(strings.ReplaceAll(text, "''", "'"))
	if status != MarkerOurs {
		t.Fatalf("status = %v", status)
	}
	if got.Run != "run-original" || got.At != "2026-01-01T00:00:00Z" || got.Op != string(OpCreateIndex) {
		t.Fatalf("reindex erased who built the index: %+v", got)
	}
	if got.Reindexed != "2026-08-07T00:00:00Z" || got.ReindexRun != "run-reindex" {
		t.Fatalf("reindex facts not recorded: %+v", got)
	}
}

func TestRenderMarkerStatementNeverOverwritesAForeignComment(t *testing.T) {
	cidx := NewObjectName("public", "orders_idx_p1")
	n := Node{ID: "r", Kind: KindIndexReindexConcurrently, Params: &ReindexConcurrentlyParams{Index: cidx}}
	base := Marker{Run: "run-1", Plan: "sha256:b", Op: string(OpReindexIndex), At: "2026-08-07T00:00:00Z"}

	for _, status := range []MarkerStatus{MarkerForeign, MarkerUnreadable} {
		if _, ok, err := RenderMarkerStatement(&n, base, Marker{}, status); ok || err != nil {
			t.Fatalf("status %v: ok=%t err=%v; a reindex must not overwrite it", status, ok, err)
		}
	}
	// An unmarked index we just rebuilt does get a marker: there is nothing to
	// overwrite, and recording the rebuild is what makes the leaf skippable.
	if _, ok, err := RenderMarkerStatement(&n, base, Marker{}, MarkerAbsent); !ok || err != nil {
		t.Fatalf("MarkerAbsent: ok=%t err=%v", ok, err)
	}
}

func TestNodeObjectNamesWhatEachKindTouches(t *testing.T) {
	pidx := NewObjectName("public", "orders_idx")
	cidx := NewObjectName("public", "orders_idx_p1")
	cases := []struct {
		node Node
		want ObjectName
		ok   bool
	}{
		{Node{Kind: KindIndexCreateParentInvalid, Params: &CreateParentInvalidParams{Index: pidx}}, pidx, true},
		{Node{Kind: KindIndexCreateConcurrently, Params: &CreateConcurrentlyParams{Index: cidx}}, cidx, true},
		{Node{Kind: KindIndexAttach, Params: &AttachParams{ChildIndex: cidx}}, cidx, true},
		{Node{Kind: KindIndexDropConcurrently, Params: &DropConcurrentlyParams{Index: cidx}}, cidx, true},
		{Node{Kind: KindIndexReindexConcurrently, Params: &ReindexConcurrentlyParams{Index: cidx}}, cidx, true},
		{Node{Kind: KindIndexDropPartitioned, Params: &DropPartitionedParams{Index: pidx}}, pidx, true},
		{Node{Kind: KindWait, Params: &WaitParams{}}, ObjectName{}, false},
		{Node{Kind: KindCatalogAssert, Params: &CatalogAssertParams{}}, ObjectName{}, false},
		{Node{Kind: KindIndexVerify, Params: &VerifyParams{}}, ObjectName{}, false},
	}
	for _, c := range cases {
		got, ok := c.node.Object()
		if ok != c.ok || got != c.want {
			t.Fatalf("%s: Object() = %v, %t; want %v, %t", c.node.Kind, got, ok, c.want, c.ok)
		}
	}
}

func oneColumn() IndexDefinition {
	return IndexDefinition{Columns: []IndexColumn{{Name: "created_at"}}}
}
