package executor

import (
	"errors"
	"strings"
	"testing"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

func boolPtr(b bool) *bool { return &b }

func TestRenderStatements(t *testing.T) {
	tests := []struct {
		name   string
		node   func(*testing.T) protocol.Node
		want   string
		wantIn []string
	}{
		{
			name: "create parent invalid uses ON ONLY and a bare index name",
			node: func(t *testing.T) protocol.Node {
				return node("n", protocol.KindIndexCreateParentInvalid, &protocol.CreateParentInvalidParams{
					Parent:     obj(t, "public.orders"),
					Index:      obj(t, "public.orders_created_at_idx"),
					Definition: indexDef(),
				})
			},
			want: `CREATE INDEX "orders_created_at_idx" ON ONLY "public"."orders" USING "btree" ("created_at")`,
		},
		{
			name: "create concurrently on a leaf",
			node: func(t *testing.T) protocol.Node {
				return node("n", protocol.KindIndexCreateConcurrently, &protocol.CreateConcurrentlyParams{
					Partition:  obj(t, "public.orders_2026_03"),
					Index:      obj(t, "public.orders_created_at_idx_orders_2026_03"),
					Definition: indexDef(),
				})
			},
			want: `CREATE INDEX CONCURRENTLY "orders_created_at_idx_orders_2026_03" ON "public"."orders_2026_03" USING "btree" ("created_at")`,
		},
		{
			name: "unique, include, storage params, tablespace and a predicate",
			node: func(t *testing.T) protocol.Node {
				return node("n", protocol.KindIndexCreateConcurrently, &protocol.CreateConcurrentlyParams{
					Partition: obj(t, "public.orders_2026_03"),
					Index:     obj(t, "public.idx"),
					Definition: protocol.IndexDefinition{
						Method:        "btree",
						Unique:        true,
						Columns:       []protocol.IndexColumn{{Name: "tenant_id"}, {Name: "created_at", Descending: true, NullsFirst: boolPtr(true)}},
						Include:       []string{"status"},
						Where:         "status <> 'deleted'",
						Tablespace:    "fast",
						StorageParams: map[string]string{"fillfactor": "90", "deduplicate_items": "off"},
					},
				})
			},
			want: `CREATE UNIQUE INDEX CONCURRENTLY "idx" ON "public"."orders_2026_03" USING "btree" ` +
				`("tenant_id", "created_at" DESC NULLS FIRST) INCLUDE ("status") ` +
				`WITH ("deduplicate_items" = 'off', "fillfactor" = '90') TABLESPACE "fast" WHERE status <> 'deleted'`,
		},
		{
			name: "expression column, collation and opclass",
			node: func(t *testing.T) protocol.Node {
				return node("n", protocol.KindIndexCreateConcurrently, &protocol.CreateConcurrentlyParams{
					Partition: obj(t, "public.orders_2026_03"),
					Index:     obj(t, "public.idx"),
					Definition: protocol.IndexDefinition{
						Columns: []protocol.IndexColumn{
							{Expression: "lower(email)", Collation: "C", OpClass: "text_pattern_ops"},
							{Name: "created_at", NullsFirst: boolPtr(false)},
						},
					},
				})
			},
			want: `CREATE INDEX CONCURRENTLY "idx" ON "public"."orders_2026_03" ` +
				`((lower(email)) COLLATE "C" "text_pattern_ops", "created_at" NULLS LAST)`,
		},
		{
			name: "attach qualifies both index names",
			node: func(t *testing.T) protocol.Node {
				return node("n", protocol.KindIndexAttach, &protocol.AttachParams{
					ParentIndex: obj(t, "public.orders_created_at_idx"),
					ChildIndex:  obj(t, "public.orders_created_at_idx_orders_2026_03"),
				})
			},
			want: `ALTER INDEX "public"."orders_created_at_idx" ATTACH PARTITION "public"."orders_created_at_idx_orders_2026_03"`,
		},
		{
			name: "drop concurrently",
			node: func(t *testing.T) protocol.Node {
				n := node("n", protocol.KindIndexDropConcurrently, &protocol.DropConcurrentlyParams{
					Index:  obj(t, "public.orders_created_at_idx_orders_2026_03"),
					Reason: protocol.DropInvalidBuild,
				})
				n.Authorization = provenanceAuth(t, "public.orders_created_at_idx_orders_2026_03")
				return n
			},
			want: `DROP INDEX CONCURRENTLY "public"."orders_created_at_idx_orders_2026_03"`,
		},
		{
			name: "unqualified names render bare",
			node: func(t *testing.T) protocol.Node {
				return node("n", protocol.KindIndexCreateConcurrently, &protocol.CreateConcurrentlyParams{
					Partition:  obj(t, "orders_2026_03"),
					Index:      obj(t, "idx"),
					Definition: indexDef(),
				})
			},
			want: `CREATE INDEX CONCURRENTLY "idx" ON "orders_2026_03" USING "btree" ("created_at")`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := tc.node(t)
			if err := n.Validate(); err != nil {
				t.Fatalf("fixture node is invalid: %v", err)
			}
			got, err := Render(&n)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Render =\n  %s\nwant\n  %s", got, tc.want)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(got, want) {
					t.Fatalf("Render = %s, want it to contain %q", got, want)
				}
			}
		})
	}
}

func TestRenderQuotesHostileIdentifiers(t *testing.T) {
	// T2: an identifier that tries to break out of its quoting must not.
	n := node("n", protocol.KindIndexAttach, &protocol.AttachParams{
		ParentIndex: protocol.NewObjectName("public", `evil"; DROP TABLE orders; --`),
		ChildIndex:  obj(t, "public.child"),
	})
	got, err := Render(&n)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := `ALTER INDEX "public"."evil""; DROP TABLE orders; --" ATTACH PARTITION "public"."child"`
	if got != want {
		t.Fatalf("Render =\n  %s\nwant\n  %s", got, want)
	}
}

func TestRenderIsDeterministicAcrossStorageParamOrder(t *testing.T) {
	// Map iteration order is not stable, and the executor must render the same
	// bytes every time.
	build := func() protocol.Node {
		return node("n", protocol.KindIndexCreateConcurrently, &protocol.CreateConcurrentlyParams{
			Partition: obj(t, "public.p"),
			Index:     obj(t, "public.i"),
			Definition: protocol.IndexDefinition{
				Columns: []protocol.IndexColumn{{Name: "a"}},
				StorageParams: map[string]string{
					"fillfactor": "90", "deduplicate_items": "off", "vacuum_cleanup_index_scale_factor": "0.2",
				},
			},
		})
	}
	first := build()
	want, err := Render(&first)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for i := 0; i < 50; i++ {
		n := build()
		got, err := Render(&n)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if got != want {
			t.Fatalf("Render is not deterministic:\n  %s\n  %s", want, got)
		}
	}
}

func TestRenderEscapesStorageParamValues(t *testing.T) {
	n := node("n", protocol.KindIndexCreateConcurrently, &protocol.CreateConcurrentlyParams{
		Partition: obj(t, "public.p"),
		Index:     obj(t, "public.i"),
		Definition: protocol.IndexDefinition{
			Columns:       []protocol.IndexColumn{{Name: "a"}},
			StorageParams: map[string]string{"note": "it's fine"},
		},
	})
	got, err := Render(&n)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got, `"note" = 'it''s fine'`) {
		t.Fatalf("Render = %s, want a doubled quote in the literal", got)
	}
}

func TestRenderReturnsNothingForNonDDLKinds(t *testing.T) {
	for _, n := range []protocol.Node{
		node("a", protocol.KindCatalogAssert, &protocol.CatalogAssertParams{
			Assertions: []protocol.Assertion{{Assertion: protocol.AssertNoDefaultPartition}},
		}),
		node("v", protocol.KindIndexVerify, &protocol.VerifyParams{
			Checks: []protocol.VerifyCheck{{Check: protocol.CheckIndexValid, Index: objPtr(t, "public.i")}},
		}),
		node("w", protocol.KindWait, &protocol.WaitParams{Seconds: 1}),
	} {
		n := n
		sql, err := Render(&n)
		if err != nil {
			t.Fatalf("Render(%s): %v", n.Kind, err)
		}
		if sql != "" {
			t.Fatalf("Render(%s) = %q, want an empty statement", n.Kind, sql)
		}
		if n.Kind.IssuesDDL() {
			t.Fatalf("%s is marked as issuing DDL but renders nothing", n.Kind)
		}
	}
}

func TestRenderRefusesAnIndexInAnotherSchema(t *testing.T) {
	// PostgreSQL puts an index in its table's schema. A plan that asks for
	// something else is asking for the impossible, and saying so at render time
	// beats a confusing syntax error six hours into a run.
	n := node("n", protocol.KindIndexCreateConcurrently, &protocol.CreateConcurrentlyParams{
		Partition:  obj(t, "public.orders_2026_03"),
		Index:      obj(t, "other.idx"),
		Definition: indexDef(),
	})
	_, err := Render(&n)
	if !errors.Is(err, protocol.ErrInvalidPlan) {
		t.Fatalf("error = %v, want ErrInvalidPlan", err)
	}
}

func TestRenderTheTwoLateKinds(t *testing.T) {
	reindex := node("n", protocol.KindIndexReindexConcurrently, &protocol.ReindexConcurrentlyParams{
		Index: obj(t, "public.orders_idx_p1"),
	})
	got, err := Render(&reindex)
	if err != nil {
		t.Fatalf("Render(reindex): %v", err)
	}
	if got != `REINDEX INDEX CONCURRENTLY "public"."orders_idx_p1"` {
		t.Fatalf("Render(reindex) = %q", got)
	}

	drop := node("n", protocol.KindIndexDropPartitioned, &protocol.DropPartitionedParams{
		Parent: obj(t, "public.orders"), Index: obj(t, "public.orders_idx"), LeafCount: 12,
	})
	got, err = Render(&drop)
	if err != nil {
		t.Fatalf("Render(drop_partitioned): %v", err)
	}
	// No CASCADE: the statement cascades to attached children on its own. No
	// CONCURRENTLY: PostgreSQL rejects it on a partitioned index. No IF EXISTS:
	// an index that is already gone is a question for the planner.
	if got != `DROP INDEX "public"."orders_idx"` {
		t.Fatalf("Render(drop_partitioned) = %q", got)
	}
}

func TestRenderRefusesUnknownKinds(t *testing.T) {
	unknown := protocol.Node{ID: "n", Kind: protocol.NodeKind("index.teleport")}
	if _, err := Render(&unknown); !errors.Is(err, protocol.ErrUnknownNodeKind) {
		t.Fatalf("error = %v, want ErrUnknownNodeKind", err)
	}

	if _, err := Render(nil); !errors.Is(err, protocol.ErrInvalidPlan) {
		t.Fatalf("error = %v, want ErrInvalidPlan for a nil node", err)
	}
}

func TestRenderRefusesMismatchedParams(t *testing.T) {
	// Kind says attach; params are for a create. Node.Validate catches this in
	// a well-formed plan, so reaching Render means validation was bypassed.
	n := protocol.Node{
		ID:     "n",
		Kind:   protocol.KindIndexAttach,
		Params: &protocol.WaitParams{Seconds: 1},
	}
	if _, err := Render(&n); !errors.Is(err, protocol.ErrInvalidPlan) {
		t.Fatalf("error = %v, want ErrInvalidPlan", err)
	}
}

func TestRenderedSQLInThePlanIsIgnored(t *testing.T) {
	// FR-PLANFILE-7: the reviewer's preview is never executed. Here it says
	// something destructive and the executor must not care.
	h := newHarness()
	plan := createChainPlan(t)
	for i := range plan.Nodes {
		plan.Nodes[i].RenderedSQL = "DROP TABLE public.orders"
	}
	if err := plan.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if _, err := h.run(t, plan); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, s := range h.sql.stmts {
		if strings.Contains(s.SQL, "DROP TABLE") {
			t.Fatalf("node %q executed its rendered_sql: %q", s.NodeID, s.SQL)
		}
	}
	stmt, ok := h.sql.statementFor("n5")
	if !ok || !strings.HasPrefix(stmt.SQL, "ALTER INDEX ") {
		t.Fatalf("n5 statement = %q, want SQL re-rendered from params", stmt.SQL)
	}
}

// TestQualifiedOpClassAndCollationRender covers the managed-PostgreSQL layout
// where an extension lives outside the search path.
//
// pg_opclass and pg_collation are schema-qualified catalogs, so
// `extensions.gin_trgm_ops` is the ordinary way to name a trigram opclass when
// pg_trgm is installed into its own schema, which RDS, Aurora and Cloud SQL all
// encourage. Rendering the whole string through QuoteIdentifier produced
// ("body" "extensions.gin_trgm_ops"): a single quoted identifier containing a
// dot, which no opclass can ever be named. The plan validated, sealed, was
// reviewed and took the advisory lock, and the very first statement failed with
// 42704, classified terminal.
func TestQualifiedOpClassAndCollationRender(t *testing.T) {
	n := node("n1", protocol.KindIndexCreateConcurrently, &protocol.CreateConcurrentlyParams{
		Partition: obj(t, "public.orders_2026_03"),
		Index:     obj(t, "public.orders_body_idx"),
		Definition: protocol.IndexDefinition{
			Method: "gin",
			Columns: []protocol.IndexColumn{{
				Name:      "body",
				OpClass:   "extensions.gin_trgm_ops",
				Collation: "intl.en_US",
			}},
		},
	})

	got, err := Render(&n)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{`"extensions"."gin_trgm_ops"`, `"intl"."en_US"`} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered SQL does not contain %s:\n%s", want, got)
		}
	}
	for _, bad := range []string{`"extensions.gin_trgm_ops"`, `"intl.en_US"`} {
		if strings.Contains(got, bad) {
			t.Errorf("rendered SQL quotes a qualified name as one identifier (%s):\n%s", bad, got)
		}
	}
}

// TestUnqualifiedOpClassStillRendersBare keeps the common case unchanged.
func TestUnqualifiedOpClassStillRendersBare(t *testing.T) {
	n := node("n1", protocol.KindIndexCreateConcurrently, &protocol.CreateConcurrentlyParams{
		Partition: obj(t, "public.orders_2026_03"),
		Index:     obj(t, "public.orders_body_idx"),
		Definition: protocol.IndexDefinition{
			Method:  "btree",
			Columns: []protocol.IndexColumn{{Name: "email", OpClass: "text_pattern_ops", Collation: "C"}},
		},
	})
	got, err := Render(&n)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(got, `"text_pattern_ops"`) || !strings.Contains(got, `COLLATE "C"`) {
		t.Errorf("unqualified names did not render bare:\n%s", got)
	}
}

// TestAccessMethodRejectsAQualifiedName: pg_am has no namespace column, so an
// access method is never schema-qualified. A dotted value is a specification
// error and must be caught at plan time, not at 42704 mid-run.
func TestAccessMethodRejectsAQualifiedName(t *testing.T) {
	d := protocol.IndexDefinition{
		Method:  "extensions.gin",
		Columns: []protocol.IndexColumn{{Name: "body"}},
	}
	if err := d.Validate(); err == nil {
		t.Fatal("a schema-qualified access method validated; pg_am has no namespace")
	}
}
