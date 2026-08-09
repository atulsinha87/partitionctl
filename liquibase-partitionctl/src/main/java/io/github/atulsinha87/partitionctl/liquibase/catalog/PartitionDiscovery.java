package io.github.atulsinha87.partitionctl.liquibase.catalog;

import liquibase.database.Database;
import liquibase.database.DatabaseConnection;
import liquibase.database.jvm.JdbcConnection;
import liquibase.exception.UnexpectedLiquibaseException;

import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.util.LinkedHashMap;
import java.util.Map;

/**
 * Reads the live catalog and returns the whole picture in <b>one</b> round trip.
 *
 * <h2>Why one query</h2>
 * A recursive CTE over {@code pg_inherits} enumerates every leaf of the target table, and
 * the same statement left-joins each leaf's indexes so that {@code pg_index.indisvalid} and
 * the {@code pg_inherits} link to the parent index come back with them. No per-leaf query,
 * no N+1, and — critically — one consistent snapshot rather than a sequence of reads that
 * could disagree with each other.
 *
 * <h2>Why it must be memoised</h2>
 * {@code Change.generateStatements(Database)} is called roughly seven times per
 * {@code liquibase update}: validation, checksum, MDC logging, preview, execution. Measured
 * on 4.33.0. Discovery is therefore cached per instance and keyed on the request, and the
 * method is free of side effects — it only SELECTs.
 *
 * <h2>The tree walk must agree with PostgreSQL's own partition descriptor</h2>
 * The recursive step skips {@code pg_inherits} rows with {@code inhdetachpending}, because
 * {@code RelationGetPartitionDesc} does. A partition left half-detached by an interrupted
 * {@code ALTER TABLE ... DETACH PARTITION ... CONCURRENTLY} keeps its {@code pg_inherits} row
 * with that flag set, and the state persists silently until somebody runs {@code ... FINALIZE}.
 * Measured on 17.10 against a rolling-window table with one such partition:
 * <pre>
 * this CTE without the filter       evt_1, evt_2, evt_3   &lt;- 3 "leaves"
 * this CTE with    the filter       evt_2, evt_3          &lt;- 2
 * plain CREATE INDEX on the parent  2 children, parent indisvalid = t
 * ALTER INDEX ... ATTACH of a child built on evt_1
 *   ERROR: cannot attach index "idx_dp_evt_1" as a partition of index "idx_dp"
 * </pre>
 * So without the filter the planner builds an index it can never attach, fails, records nothing,
 * and fails identically on every re-run. With it, discovery matches what the server itself does:
 * the detaching partition is simply not part of the table any more. Rolling retention on a
 * time-range table is this product's core use case and {@code DETACH CONCURRENTLY} is the online
 * way to expire a partition, so this state is reachable in normal operation.
 */
public final class PartitionDiscovery {

    /**
     * One statement. Row kind {@code 'T'} carries tree-level facts (does the parent index
     * exist, is it valid, what timeouts is this session running with). Row kind {@code 'L'}
     * carries one (leaf, index-on-that-leaf) pair, with a NULL index name for a leaf that
     * has no indexes at all.
     */
    static final String DISCOVERY_SQL =
        "WITH RECURSIVE tree AS ("
      + "    SELECT c.oid, c.relkind, 0 AS depth"
      + "      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace"
      + "     WHERE n.nspname = ? AND c.relname = ?"
      + "    UNION ALL"
      + "    SELECT c2.oid, c2.relkind, t.depth + 1"
      + "      FROM tree t"
      + "      JOIN pg_inherits i ON i.inhparent = t.oid AND NOT i.inhdetachpending"
      + "      JOIN pg_class c2 ON c2.oid = i.inhrelid"
      + "),"
      + "parent_idx AS ("
      + "    SELECT c.oid, COALESCE(i.indisvalid, false) AS valid,"
      + "           tn.nspname || '.' || t.relname AS owning_table"
      + "      FROM pg_class c"
      + "      JOIN pg_namespace n ON n.oid = c.relnamespace"
      + "      LEFT JOIN pg_index i ON i.indexrelid = c.oid"
      + "      LEFT JOIN pg_class t ON t.oid = i.indrelid"
      + "      LEFT JOIN pg_namespace tn ON tn.oid = t.relnamespace"
      + "     WHERE n.nspname = ? AND c.relname = ? AND c.relkind = 'I'"
      + "),"
      + "leaf AS ("
      + "    SELECT c.oid, n.nspname AS leaf_schema, c.relname AS leaf_name"
      + "      FROM tree t"
      + "      JOIN pg_class c ON c.oid = t.oid"
      + "      JOIN pg_namespace n ON n.oid = c.relnamespace"
      + "     WHERE t.relkind = 'r'"
      + ")"
      + "SELECT 'T'::text AS row_kind,"
      + "       NULL::text AS leaf_schema,"
      + "       NULL::text AS leaf_name,"
      + "       NULL::text AS index_name,"
      + "       false AS index_valid,"
      + "       false AS attached_to_target,"
      + "       false AS attached_to_any,"
      + "       EXISTS (SELECT 1 FROM parent_idx) AS parent_exists,"
      + "       COALESCE((SELECT valid FROM parent_idx), false) AS parent_valid,"
      + "       current_setting('statement_timeout') AS orig_statement_timeout,"
      + "       current_setting('lock_timeout') AS orig_lock_timeout,"
      + "       (SELECT c.relkind::text FROM pg_class c"
      + "          JOIN pg_namespace n ON n.oid = c.relnamespace"
      + "         WHERE n.nspname = ? AND c.relname = ?) AS root_relkind,"
      + "       (SELECT count(*) FROM tree WHERE relkind = 'p' AND depth > 0)::int"
      + "         AS intermediate_partitioned,"
      + "       (SELECT owning_table FROM parent_idx) AS parent_owning_table,"
      + "       NULL::text AS index_comment"
      + " UNION ALL "
      + "SELECT 'L'::text,"
      + "       l.leaf_schema,"
      + "       l.leaf_name,"
      + "       ic.relname,"
      + "       COALESCE(ix.indisvalid, false),"
      + "       COALESCE(ii.inhparent = (SELECT oid FROM parent_idx), false),"
      + "       (ii.inhparent IS NOT NULL),"
      + "       false,"
      + "       false,"
      + "       NULL::text,"
      + "       NULL::text,"
      + "       NULL::text,"
      + "       0,"
      + "       NULL::text,"
      // The child index's COMMENT. An index at the conventional name may have been built by a
      // DBA rather than by us, and adopting it silently is what let a later drop destroy it.
      + "       obj_description(ic.oid, 'pg_class')"
      + "  FROM leaf l"
      + "  LEFT JOIN pg_index ix ON ix.indrelid = l.oid"
      + "  LEFT JOIN pg_class ic ON ic.oid = ix.indexrelid"
      + "  LEFT JOIN pg_inherits ii ON ii.inhrelid = ic.oid"
      + " ORDER BY 1, 2, 3, 4";

    private final Database database;

    private String cacheKey;
    private TreeState cached;

    public PartitionDiscovery(Database database) {
        this.database = database;
    }

    /**
     * Discovers the tree. Repeated calls with the same arguments return the memoised result;
     * different arguments re-query, so a Change instance reused by a host cannot serve a
     * stale answer for a different table.
     */
    public TreeState inspect(String schemaName, String tableName, String indexName) {
        return inspect(schemaName, tableName, indexName, true);
    }

    /**
     * Discovery for an operation that only ever names indexes the catalog already contains, and
     * so generates no child index name of its own — reindex.
     *
     * <p>The one difference is the collision check. {@link IndexNaming#assignChildIndexNames}
     * refuses when two leaves would <em>generate</em> the same 63-byte name, which is exactly
     * right when we are about to create those indexes and would otherwise build one index for
     * two partitions. But that is a property of the request, not of the tree, so applying it to
     * an operation that generates nothing is a false failure. A 20-byte indexName over
     * partitions named {@code events_by_region_and_customer_status_p2024_01_01} and
     * {@code …_p2024_02_01} collides, yet a tree built by a plain {@code CREATE INDEX} on the
     * parent is healthy and perfectly reindexable — refusing there would leave that adopter no
     * way to reindex at all.
     */
    public TreeState inspectExisting(String schemaName, String tableName, String indexName) {
        return inspect(schemaName, tableName, indexName, false);
    }

    private TreeState inspect(String schemaName, String tableName, String indexName,
                              boolean assignChildIndexNames) {
        String key = schemaName + "\0" + tableName + "\0" + indexName
                + "\0" + assignChildIndexNames;
        if (cached != null && key.equals(cacheKey)) {
            return cached;
        }

        TreeState state = new TreeState();
        Map<String, LeafPartition> byLeaf = new LinkedHashMap<String, LeafPartition>();

        try {
            Connection connection = jdbc();
            try (PreparedStatement ps = connection.prepareStatement(DISCOVERY_SQL)) {
                ps.setString(1, schemaName);
                ps.setString(2, tableName);
                ps.setString(3, schemaName);
                ps.setString(4, indexName);
                ps.setString(5, schemaName);
                ps.setString(6, tableName);
                try (ResultSet rs = ps.executeQuery()) {
                    while (rs.next()) {
                        if ("T".equals(rs.getString("row_kind"))) {
                            state.setParentIndexExists(rs.getBoolean("parent_exists"));
                            state.setParentIndexValid(rs.getBoolean("parent_valid"));
                            state.setOriginalStatementTimeout(rs.getString("orig_statement_timeout"));
                            state.setOriginalLockTimeout(rs.getString("orig_lock_timeout"));
                            state.setRootRelkind(rs.getString("root_relkind"));
                            state.setIntermediatePartitionedCount(rs.getInt("intermediate_partitioned"));
                            state.setParentIndexOwningTable(rs.getString("parent_owning_table"));
                            continue;
                        }
                        String leafSchema = rs.getString("leaf_schema");
                        String leafName = rs.getString("leaf_name");
                        String leafKey = leafSchema + "\0" + leafName;
                        LeafPartition leaf = byLeaf.get(leafKey);
                        if (leaf == null) {
                            leaf = new LeafPartition(leafSchema, leafName);
                            byLeaf.put(leafKey, leaf);
                            state.addLeaf(leaf);
                        }
                        String indexOnLeaf = rs.getString("index_name");
                        if (indexOnLeaf != null) {
                            leaf.addIndex(new LeafIndex(
                                    indexOnLeaf,
                                    rs.getBoolean("index_valid"),
                                    rs.getBoolean("attached_to_target"),
                                    rs.getBoolean("attached_to_any"),
                                    rs.getString("index_comment")));
                        }
                    }
                }
            }
        } catch (Exception e) {
            throw new UnexpectedLiquibaseException(
                    "partitionctl: partition discovery failed for " + schemaName + "." + tableName
                            + " (index " + indexName + "): " + e.getMessage(), e);
        }

        if (assignChildIndexNames) {
            // Fails loudly here, before any statement is emitted, if two leaves would share a name.
            IndexNaming.assignChildIndexNames(indexName, state.mutableLeaves());
        }

        cacheKey = key;
        cached = state;
        return state;
    }

    private Connection jdbc() {
        DatabaseConnection dbc = database.getConnection();
        if (!(dbc instanceof JdbcConnection)) {
            throw new UnexpectedLiquibaseException(
                    "partitionctl requires a JDBC connection to discover partitions, got "
                            + (dbc == null ? "none" : dbc.getClass().getName()));
        }
        return ((JdbcConnection) dbc).getUnderlyingConnection();
    }
}
