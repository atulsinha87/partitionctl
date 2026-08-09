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
 * Reads everything a drop needs in <b>one</b> round trip: the partition tree, every index on
 * every leaf with its {@code indisvalid} and its {@code obj_description}, and the identity of
 * the relation the changeset named as {@code indexName}.
 *
 * <h2>Why one query rather than several</h2>
 * Beyond the obvious N+1 argument, the drop makes an authorisation decision from what it reads:
 * "this comment says the plugin built it, therefore destroy it". Reading the ownership evidence
 * in one statement and the object list in another opens a window where the two disagree. One
 * statement is one snapshot.
 *
 * <h2>Why it must be memoised</h2>
 * {@code Change.generateStatements(Database)} is called roughly seven times per
 * {@code liquibase update} — validation, checksum, MDC logging, preview, execution. The method
 * is free of side effects and the result is cached per instance, keyed on the request.
 *
 * <h2>Deliberately separate from {@link PartitionDiscovery}</h2>
 * The create path's discovery answers "which leaves are covered". This one answers "what would
 * I destroy and may I". It needs comments and the named relation's {@code relkind}, neither of
 * which the create path reads. Widening the shared query would change code that is verified
 * live against a database, for fields it would never use. The duplicated part is the recursive
 * CTE over {@code pg_inherits}; the semantics of "enumerate the leaves" are fixed by
 * PostgreSQL, so there is nothing there to drift.
 */
public final class DropTargetDiscovery {

    static final String DISCOVERY_SQL =
        "WITH RECURSIVE tree AS ("
      + "    SELECT c.oid, c.relkind"
      + "      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace"
      + "     WHERE n.nspname = ? AND c.relname = ?"
      + "    UNION ALL"
      + "    SELECT c2.oid, c2.relkind"
      + "      FROM tree t"
      + "      JOIN pg_inherits i ON i.inhparent = t.oid"
      + "      JOIN pg_class c2 ON c2.oid = i.inhrelid"
      + "),"
        // Deliberately NOT filtered to relkind = 'I'. "no partitioned index of that name" and
        // "an ordinary index of that name" are different situations and get different messages.
      + "named AS ("
      + "    SELECT c.oid, c.relkind"
      + "      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace"
      + "     WHERE n.nspname = ? AND c.relname = ?"
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
      + "       NULL::text AS index_comment,"
      + "       (SELECT c.relkind::text FROM pg_class c"
      + "          JOIN pg_namespace n ON n.oid = c.relnamespace"
      + "         WHERE n.nspname = ? AND c.relname = ?) AS root_relkind,"
      + "       (SELECT relkind::text FROM named) AS named_relkind,"
      + "       (SELECT n2.nspname || '.' || t2.relname FROM named"
      + "          JOIN pg_index ix ON ix.indexrelid = named.oid"
      + "          JOIN pg_class t2 ON t2.oid = ix.indrelid"
      + "          JOIN pg_namespace n2 ON n2.oid = t2.relnamespace) AS named_table,"
      + "       COALESCE((SELECT ix.indisvalid FROM named"
      + "          JOIN pg_index ix ON ix.indexrelid = named.oid), false) AS named_valid,"
      + "       (SELECT obj_description(named.oid, 'pg_class') FROM named) AS named_comment,"
      + "       current_setting('statement_timeout') AS orig_statement_timeout,"
      + "       current_setting('lock_timeout') AS orig_lock_timeout"
      + " UNION ALL "
      + "SELECT 'L'::text,"
      + "       l.leaf_schema,"
      + "       l.leaf_name,"
      + "       ic.relname,"
      + "       COALESCE(ix.indisvalid, false),"
      + "       COALESCE(ii.inhparent = (SELECT oid FROM named), false),"
      + "       (ii.inhparent IS NOT NULL),"
      + "       obj_description(ic.oid, 'pg_class'),"
      + "       NULL::text, NULL::text, NULL::text, false, NULL::text, NULL::text, NULL::text"
      + "  FROM leaf l"
      + "  LEFT JOIN pg_index ix ON ix.indrelid = l.oid"
      + "  LEFT JOIN pg_class ic ON ic.oid = ix.indexrelid"
      + "  LEFT JOIN pg_inherits ii ON ii.inhrelid = ic.oid"
      + " ORDER BY 1, 2, 3, 4";

    private final Database database;

    private String cacheKey;
    private DropTarget cached;

    public DropTargetDiscovery(Database database) {
        this.database = database;
    }

    /** Repeated calls with the same arguments return the memoised result. */
    public DropTarget inspect(String schemaName, String tableName, String indexName) {
        String key = schemaName + " " + tableName + " " + indexName;
        if (cached != null && key.equals(cacheKey)) {
            return cached;
        }

        DropTarget target = new DropTarget();
        Map<String, DropLeaf> byLeaf = new LinkedHashMap<String, DropLeaf>();

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
                            target.setRootRelkind(rs.getString("root_relkind"));
                            target.setIndexRelkind(rs.getString("named_relkind"));
                            target.setIndexOwningTable(rs.getString("named_table"));
                            target.setIndexValid(rs.getBoolean("named_valid"));
                            target.setIndexComment(rs.getString("named_comment"));
                            target.setOriginalStatementTimeout(rs.getString("orig_statement_timeout"));
                            target.setOriginalLockTimeout(rs.getString("orig_lock_timeout"));
                            continue;
                        }
                        String leafSchema = rs.getString("leaf_schema");
                        String leafName = rs.getString("leaf_name");
                        String leafKey = leafSchema + " " + leafName;
                        DropLeaf leaf = byLeaf.get(leafKey);
                        if (leaf == null) {
                            leaf = new DropLeaf(leafSchema, leafName);
                            byLeaf.put(leafKey, leaf);
                            target.addLeaf(leaf);
                        }
                        String indexOnLeaf = rs.getString("index_name");
                        if (indexOnLeaf != null) {
                            leaf.addIndex(new DropCandidateIndex(
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
                    "partitionctl: drop-target discovery failed for " + schemaName + "." + tableName
                            + " (index " + indexName + "): " + e.getMessage(), e);
        }

        // The same generated name the create change would have used, truncation included, so a
        // leftover from an interrupted build is recognised by the name it was actually given.
        for (DropLeaf leaf : target.getLeaves()) {
            leaf.setChildIndexName(IndexNaming.childIndexName(indexName, leaf.getTableName()));
        }

        cacheKey = key;
        cached = target;
        return target;
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
