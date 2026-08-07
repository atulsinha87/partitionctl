package createindex

import (
	"strings"
	"testing"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

func boolPtr(b bool) *bool { return &b }

func TestRenderDefinition(t *testing.T) {
	tests := []struct {
		name string
		def  protocol.IndexDefinition
		want string
	}{
		{
			name: "single column",
			def: protocol.IndexDefinition{
				Method:  "btree",
				Columns: []protocol.IndexColumn{{Name: "created_at"}},
			},
			want: ` USING "btree" ("created_at")`,
		},
		{
			name: "no method uses the server default",
			def:  protocol.IndexDefinition{Columns: []protocol.IndexColumn{{Name: "a"}}},
			want: ` ("a")`,
		},
		{
			name: "ordering, collation and opclass",
			def: protocol.IndexDefinition{
				Method: "btree",
				Columns: []protocol.IndexColumn{{
					Name: "name", Collation: "C", OpClass: "text_pattern_ops",
					Descending: true, NullsFirst: boolPtr(true),
				}},
			},
			want: ` USING "btree" ("name" COLLATE "C" "text_pattern_ops" DESC NULLS FIRST)`,
		},
		{
			name: "nulls last",
			def: protocol.IndexDefinition{
				Columns: []protocol.IndexColumn{{Name: "a", NullsFirst: boolPtr(false)}},
			},
			want: ` ("a" NULLS LAST)`,
		},
		{
			name: "expression column is parenthesised, not quoted",
			def: protocol.IndexDefinition{
				Columns: []protocol.IndexColumn{{Expression: "lower(email)"}},
			},
			want: ` ((lower(email)))`,
		},
		{
			name: "include, storage params, tablespace and where",
			def: protocol.IndexDefinition{
				Method:        "btree",
				Columns:       []protocol.IndexColumn{{Name: "a"}, {Name: "b"}},
				Include:       []string{"c", "d"},
				StorageParams: map[string]string{"fillfactor": "90", "deduplicate_items": "on"},
				Tablespace:    "fast",
				Where:         "deleted_at IS NULL",
			},
			want: ` USING "btree" ("a", "b") INCLUDE ("c", "d")` +
				` WITH ("deduplicate_items" = 'on', "fillfactor" = '90')` +
				` TABLESPACE "fast" WHERE deleted_at IS NULL`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderDefinition(tc.def); got != tc.want {
				t.Errorf("renderDefinition()\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestRenderDefinitionSortsStorageParams(t *testing.T) {
	def := protocol.IndexDefinition{
		Columns: []protocol.IndexColumn{{Name: "a"}},
		StorageParams: map[string]string{
			"z": "1", "m": "2", "a": "3", "q": "4", "b": "5", "y": "6",
		},
	}
	first := renderDefinition(def)
	// Go randomizes map iteration, so a stable render is what keeps the digest
	// stable across processes.
	for i := 0; i < 50; i++ {
		if got := renderDefinition(def); got != first {
			t.Fatalf("renderDefinition() is not stable across map iterations:\n %q\n %q", first, got)
		}
	}
	if !strings.Contains(first, `"a" = '3', "b" = '5', "m" = '2'`) {
		t.Errorf("storage params are not sorted: %q", first)
	}
}

func TestQuoteLiteralEscapes(t *testing.T) {
	if got, want := quoteLiteral("it's"), `'it''s'`; got != want {
		t.Errorf("quoteLiteral() = %q, want %q", got, want)
	}
}

func TestRenderedSQLPreviews(t *testing.T) {
	parent := protocol.NewObjectName("public", "orders")
	parentIdx := protocol.NewObjectName("public", "orders_idx")
	leaf := protocol.NewObjectName("archive", "orders_2026_01")
	childIdx := protocol.NewObjectName("archive", "orders_idx_orders_2026_01")
	def := protocol.IndexDefinition{
		Method:  "btree",
		Columns: []protocol.IndexColumn{{Name: "created_at"}},
	}

	tests := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "create parent invalid",
			got: renderCreateParentInvalid(&protocol.CreateParentInvalidParams{
				Parent: parent, Index: parentIdx, Definition: def,
			}),
			want: `CREATE INDEX "orders_idx" ON ONLY "public"."orders" USING "btree" ("created_at");`,
		},
		{
			name: "create concurrently",
			got: renderCreateConcurrently(&protocol.CreateConcurrentlyParams{
				Partition: leaf, Index: childIdx, Definition: def,
			}),
			want: `CREATE INDEX CONCURRENTLY "orders_idx_orders_2026_01" ON "archive"."orders_2026_01" USING "btree" ("created_at");`,
		},
		{
			name: "unique",
			got: renderCreateConcurrently(&protocol.CreateConcurrentlyParams{
				Partition: leaf, Index: childIdx,
				Definition: protocol.IndexDefinition{
					Method: "btree", Unique: true,
					Columns: []protocol.IndexColumn{{Name: "id"}},
				},
			}),
			want: `CREATE UNIQUE INDEX CONCURRENTLY "orders_idx_orders_2026_01" ON "archive"."orders_2026_01" USING "btree" ("id");`,
		},
		{
			name: "attach",
			got: renderAttach(&protocol.AttachParams{
				ParentIndex: parentIdx, ChildIndex: childIdx,
			}),
			want: `ALTER INDEX "public"."orders_idx" ATTACH PARTITION "archive"."orders_idx_orders_2026_01";`,
		},
		{
			name: "drop concurrently",
			got:  renderDropConcurrently(&protocol.DropConcurrentlyParams{Index: childIdx}),
			want: `DROP INDEX CONCURRENTLY "archive"."orders_idx_orders_2026_01";`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("\n got %s\nwant %s", tc.got, tc.want)
			}
		})
	}
}

// PostgreSQL forbids a schema on the index name in CREATE INDEX: the index goes
// into its table's schema whatever the statement says. A runbook that says
// otherwise does not run.
func TestRenderedCreateDoesNotQualifyTheIndexName(t *testing.T) {
	sql := renderCreateConcurrently(&protocol.CreateConcurrentlyParams{
		Partition: protocol.NewObjectName("archive", "p1"),
		Index:     protocol.NewObjectName("archive", "p1_idx"),
		Definition: protocol.IndexDefinition{
			Columns: []protocol.IndexColumn{{Name: "a"}},
		},
	})
	if strings.Contains(sql, `"archive"."p1_idx"`) {
		t.Errorf("CREATE INDEX qualified the index name: %s", sql)
	}
	if !strings.Contains(sql, `CONCURRENTLY "p1_idx" ON "archive"."p1"`) {
		t.Errorf("unexpected rendering: %s", sql)
	}
}

// NFR-SEC-4: identifiers reach SQL only through the protocol's quoting.
func TestRenderQuotesHostileIdentifiers(t *testing.T) {
	hostile := `evil"; DROP TABLE users; --`
	sql := renderCreateConcurrently(&protocol.CreateConcurrentlyParams{
		Partition: protocol.NewObjectName("public", hostile),
		Index:     protocol.NewObjectName("public", "i"),
		Definition: protocol.IndexDefinition{
			Columns: []protocol.IndexColumn{{Name: hostile}},
		},
	})
	if !strings.Contains(sql, protocol.QuoteIdentifier(hostile)) {
		t.Fatalf("hostile identifier was not quoted through the protocol: %s", sql)
	}
	if strings.Contains(sql, `"evil"; DROP`) {
		t.Fatalf("quoting did not contain the identifier: %s", sql)
	}
}

func TestPlanNodesCarryRenderedSQL(t *testing.T) {
	cat := newCatalog("p1")
	cat.indexes[child("p1")] = invalidIndex("p1")
	claims := &fakeClaims{claimed: map[protocol.ObjectName]bool{child("p1"): true}}
	p := mustPlan(t, testPlannerWith(claims), newSpec(), cat)

	for i := range p.Nodes {
		n := &p.Nodes[i]
		if n.RenderedSQL == "" {
			t.Errorf("node %q of kind %q carries no rendered_sql for the reviewer", n.ID, n.Kind)
			continue
		}
		if n.Kind.IssuesDDL() {
			if !strings.HasSuffix(n.RenderedSQL, ";") {
				t.Errorf("node %q renders %q, which is not a statement", n.ID, n.RenderedSQL)
			}
		} else if !strings.HasPrefix(n.RenderedSQL, "--") {
			t.Errorf("node %q issues no DDL but renders %q rather than a comment", n.ID, n.RenderedSQL)
		}
	}
}

func TestRenderCreateParentInvalidUnique(t *testing.T) {
	got := renderCreateParentInvalid(&protocol.CreateParentInvalidParams{
		Parent: protocol.NewObjectName("public", "orders"),
		Index:  protocol.NewObjectName("public", "orders_pk_idx"),
		Definition: protocol.IndexDefinition{
			Method: "btree", Unique: true,
			Columns: []protocol.IndexColumn{{Name: "id"}},
		},
	})
	want := `CREATE UNIQUE INDEX "orders_pk_idx" ON ONLY "public"."orders" USING "btree" ("id");`
	if got != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}
