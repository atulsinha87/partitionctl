package io.github.atulsinha87.partitionctl.liquibase.catalog;

import liquibase.database.core.H2Database;
import liquibase.exception.UnexpectedLiquibaseException;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * The {@code ResultSet}-to-{@link TreeState} mapping in {@link PartitionDiscovery}, which the live
 * end-to-end run exercises only along its happy path.
 *
 * <p>Every fixture below is shaped like a real row set from the discovery query: one {@code T} row
 * carrying the tree-wide scalars, then one row per leaf-and-index pair, with the index columns null
 * where a leaf carries no index (the query's LEFT JOIN).
 */
class PartitionDiscoveryMappingTest {

    /** The tree-wide row. Column names are the query's, so a rename breaks this test loudly. */
    private static Object[] treeRow(boolean parentExists, boolean parentValid, String rootRelkind,
                                    int intermediatePartitioned, String owningTable) {
        return new Object[]{
                "row_kind", "T",
                "parent_exists", parentExists,
                "parent_valid", parentValid,
                "orig_statement_timeout", "30s",
                "orig_lock_timeout", "10s",
                "root_relkind", rootRelkind,
                "intermediate_partitioned", intermediatePartitioned,
                "parent_owning_table", owningTable,
        };
    }

    private static Object[] leafRow(String schema, String name, String indexName,
                                    boolean valid, boolean attachedToTarget, boolean attachedToAny,
                                    String comment) {
        return new Object[]{
                "row_kind", "L",
                "leaf_schema", schema,
                "leaf_name", name,
                "index_name", indexName,
                "index_valid", valid,
                "attached_to_target", attachedToTarget,
                "attached_to_any", attachedToAny,
                "index_comment", comment,
        };
    }

    @Test
    @DisplayName("the T row populates every tree-wide scalar")
    void treeRowIsMapped() {
        FakeJdbc jdbc = FakeJdbc.returning()
                .row(treeRow(true, false, "p", 2, "public.orders"));

        TreeState state = new PartitionDiscovery(jdbc.database())
                .inspect("public", "orders", "idx_orders");

        assertTrue(state.isParentIndexExists());
        assertFalse(state.isParentIndexValid(), "an invalid parent must not be reported valid");
        assertEquals("30s", state.getOriginalStatementTimeout());
        assertEquals("10s", state.getOriginalLockTimeout());
        assertEquals("p", state.getRootRelkind());
        assertEquals(2, state.getIntermediatePartitionedCount());
        assertEquals("public.orders", state.getParentIndexOwningTable());
    }

    @Test
    @DisplayName("one row per leaf-and-index pair collapses into one leaf carrying both indexes")
    void repeatedLeafRowsGroupIntoOneLeaf() {
        FakeJdbc jdbc = FakeJdbc.returning()
                .row(treeRow(true, true, "p", 0, "public.orders"))
                .row(leafRow("public", "orders_2024_01", "idx_a", true, true, true, "partitionctl owner=liquibase"))
                .row(leafRow("public", "orders_2024_01", "idx_b", false, false, false, null));

        TreeState state = new PartitionDiscovery(jdbc.database())
                .inspect("public", "orders", "idx_orders");

        assertEquals(1, state.getLeaves().size(), "the same leaf on two rows must not become two leaves");
        LeafPartition leaf = state.getLeaves().get(0);
        assertEquals("public", leaf.getSchemaName());
        assertEquals("orders_2024_01", leaf.getTableName());

        List<LeafIndex> indexes = leaf.getIndexes();
        assertEquals(2, indexes.size());
        assertEquals("idx_a", indexes.get(0).getIndexName());
        assertTrue(indexes.get(0).isValid());
        assertTrue(indexes.get(0).isAttachedToTargetParent());
        assertEquals("partitionctl owner=liquibase", indexes.get(0).getComment());
        assertEquals("idx_b", indexes.get(1).getIndexName());
        assertFalse(indexes.get(1).isValid());
        assertNull(indexes.get(1).getComment(), "a null comment must stay null, not become \"null\"");
    }

    @Test
    @DisplayName("a leaf with no index at all yields a leaf with an empty index list")
    void leafWithNullIndexNameIsStillALeaf() {
        // The LEFT JOIN case: the partition exists, nothing is indexed on it yet. This is the
        // ordinary state before the first run, so getting it wrong would break every fresh install.
        FakeJdbc jdbc = FakeJdbc.returning()
                .row(treeRow(false, false, "p", 0, null))
                .row(leafRow("public", "orders_2024_02", null, false, false, false, null));

        TreeState state = new PartitionDiscovery(jdbc.database())
                .inspect("public", "orders", "idx_orders");

        assertEquals(1, state.getLeaves().size());
        assertTrue(state.getLeaves().get(0).getIndexes().isEmpty(),
                "a null index_name must add no LeafIndex");
    }

    @Test
    @DisplayName("leaf order follows the query's order, which is what the progress output counts through")
    void leafOrderIsPreserved() {
        FakeJdbc jdbc = FakeJdbc.returning()
                .row(treeRow(true, true, "p", 0, "public.orders"))
                .row(leafRow("public", "orders_2024_01", null, false, false, false, null))
                .row(leafRow("public", "orders_2024_02", null, false, false, false, null))
                .row(leafRow("public", "orders_2024_03", null, false, false, false, null));

        TreeState state = new PartitionDiscovery(jdbc.database())
                .inspect("public", "orders", "idx_orders");

        assertEquals(3, state.getLeaves().size());
        assertEquals("orders_2024_01", state.getLeaves().get(0).getTableName());
        assertEquals("orders_2024_02", state.getLeaves().get(1).getTableName());
        assertEquals("orders_2024_03", state.getLeaves().get(2).getTableName());
    }

    @Test
    @DisplayName("two leaves with the same name in different schemas stay two leaves")
    void sameNameInDifferentSchemasAreDistinctLeaves() {
        // PostgreSQL lets a partition live in a different schema from its parent, so leaf names
        // are only unique per schema. Keying the grouping on the name alone silently merges them,
        // and one of the two partitions then goes unindexed with no error anywhere.
        FakeJdbc jdbc = FakeJdbc.returning()
                .row(treeRow(true, true, "p", 0, "public.orders"))
                .row(leafRow("public", "orders_2024_01", "idx_a", true, true, true, null))
                .row(leafRow("archive", "orders_2024_01", "idx_b", true, true, true, null));

        TreeState state = new PartitionDiscovery(jdbc.database())
                .inspect("public", "orders", "idx_orders");

        assertEquals(2, state.getLeaves().size(),
                "same table name in two schemas must not collapse into one leaf");
        assertEquals("public", state.getLeaves().get(0).getSchemaName());
        assertEquals("archive", state.getLeaves().get(1).getSchemaName());
        assertEquals(1, state.getLeaves().get(0).getIndexes().size());
        assertEquals(1, state.getLeaves().get(1).getIndexes().size());
    }

    @Test
    @DisplayName("the query is parameterised, and the identifiers are bound not interpolated")
    void identifiersAreBoundAsParameters() {
        FakeJdbc jdbc = FakeJdbc.returning().row(treeRow(false, false, "p", 0, null));

        new PartitionDiscovery(jdbc.database()).inspect("app", "orders", "idx_orders");

        assertFalse(jdbc.preparedSql.contains("'app'"),
                "a schema name interpolated into the SQL text would be an injection surface");
        assertEquals("app", jdbc.boundParameters.get(1));
        assertEquals("orders", jdbc.boundParameters.get(2));
        assertEquals("app", jdbc.boundParameters.get(3));
        assertEquals("idx_orders", jdbc.boundParameters.get(4));
        assertEquals("app", jdbc.boundParameters.get(5));
        assertEquals("orders", jdbc.boundParameters.get(6));
    }

    @Test
    @DisplayName("a driver failure at query time is wrapped with the table and index named")
    void queryFailureIsWrappedWithContext() {
        FakeJdbc jdbc = FakeJdbc.returning().failingOnQuery("connection reset");

        UnexpectedLiquibaseException e = assertThrows(UnexpectedLiquibaseException.class,
                () -> new PartitionDiscovery(jdbc.database()).inspect("public", "orders", "idx_orders"));

        String message = e.getMessage();
        assertTrue(message.contains("public.orders"), message);
        assertTrue(message.contains("idx_orders"), message);
        assertTrue(message.contains("connection reset"),
                "the driver's own message must survive, or the cause is unrecoverable: " + message);
    }

    @Test
    @DisplayName("a driver failure part-way through iteration does not yield a half-built tree")
    void failureMidIterationDoesNotReturnPartialState() {
        // The dangerous shape: some leaves mapped, then the connection drops. Returning what was
        // read so far would look like a smaller partition set and silently under-index the table.
        FakeJdbc jdbc = FakeJdbc.returning()
                .row(treeRow(true, true, "p", 0, "public.orders"))
                .row(leafRow("public", "orders_2024_01", null, false, false, false, null))
                .row(leafRow("public", "orders_2024_02", null, false, false, false, null))
                .failingAfter(2, "server closed the connection unexpectedly");

        assertThrows(UnexpectedLiquibaseException.class,
                () -> new PartitionDiscovery(jdbc.database()).inspect("public", "orders", "idx_orders"));
    }

    @Test
    @DisplayName("a non-JDBC connection is refused with a message naming what it got")
    void nonJdbcConnectionIsRefused() {
        UnexpectedLiquibaseException e = assertThrows(UnexpectedLiquibaseException.class,
                () -> new PartitionDiscovery(new H2Database()).inspect("public", "orders", "idx_orders"));

        assertTrue(e.getMessage().contains("JDBC"), e.getMessage());
    }

    @Test
    @DisplayName("discovery is memoised: generateStatements is called about seven times per update")
    void repeatedInspectionsReuseTheResult() {
        FakeJdbc jdbc = FakeJdbc.returning()
                .row(treeRow(true, true, "p", 0, "public.orders"))
                .row(leafRow("public", "orders_2024_01", null, false, false, false, null));
        PartitionDiscovery discovery = new PartitionDiscovery(jdbc.database());

        TreeState first = discovery.inspect("public", "orders", "idx_orders");
        TreeState second = discovery.inspect("public", "orders", "idx_orders");

        assertSame(first, second, "the same arguments must not re-query; memoisation is load-bearing "
                + "because Liquibase calls generateStatements repeatedly per update");
    }

    @Test
    @DisplayName("a different target re-queries rather than returning the previous tree")
    void differentArgumentsInvalidateTheMemo() {
        FakeJdbc jdbc = FakeJdbc.returning()
                .row(treeRow(true, true, "p", 0, "public.orders"))
                .row(leafRow("public", "orders_2024_01", null, false, false, false, null));
        PartitionDiscovery discovery = new PartitionDiscovery(jdbc.database());

        TreeState first = discovery.inspect("public", "orders", "idx_orders");
        TreeState other = discovery.inspect("public", "invoices", "idx_invoices");

        assertTrue(first != other,
                "memoising across different tables would index one table using another's partitions");
    }
}
