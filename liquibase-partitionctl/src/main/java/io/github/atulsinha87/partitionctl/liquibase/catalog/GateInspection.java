package io.github.atulsinha87.partitionctl.liquibase.catalog;

import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.util.LinkedHashMap;
import java.util.Map;

/**
 * The one read-only query behind all three preconditions.
 *
 * <h2>Why the gates do not reuse {@link PartitionDiscovery}</h2>
 * Two differences, both load-bearing rather than cosmetic:
 *
 * <ol>
 *   <li><b>It walks the index tree recursively.</b> {@code PartitionDiscovery} answers "is this
 *       leaf covered" with {@code inhparent = <the parent index>} — direct children only. On a
 *       <em>multi-level</em> partitioned table that is wrong: the parent index's direct children
 *       are intermediate partitioned indexes ({@code relkind='I'}), and the real leaf indexes are
 *       grandchildren. Measured on 17.10 with a two-level table {@code ml} (2 intermediate
 *       partitions, 4 leaves): {@code SELECT count(*) FROM pg_inherits WHERE inhparent =
 *       'idx_ml'::regclass} returns <b>2</b>, and all four leaves read as uncovered. A gate that
 *       fails on every healthy multi-level tree is worse than no gate. This query recurses
 *       through the index tree, so all four leaves read as covered.</li>
 *   <li><b>It reads {@code indisready} and {@code indislive}, not only {@code indisvalid}.</b>
 *       The gates assert health; the changes only need to know what to rebuild.</li>
 * </ol>
 *
 * <p>{@code PartitionDiscovery} is live-verified by three changes and is left untouched.
 *
 * <h2>Not memoised, deliberately</h2>
 * {@code Change.generateStatements} is called several times per update, which is why discovery is
 * cached there. A precondition's {@code check()} is called once when the changeset is evaluated,
 * and its whole job is to report what the catalog says <em>at that moment</em>. Caching a gate
 * verdict would be a bug, not an optimisation.
 */
public final class GateInspection {

    /**
     * One statement, one snapshot. Row kind {@code 'T'} carries the facts about the named table
     * and the named index; row kind {@code 'L'} carries one (leaf, index-on-that-leaf) pair, with
     * a NULL index name for a leaf carrying no indexes at all.
     */
    static final String GATE_SQL =
        "WITH RECURSIVE tree AS ("
      + "    SELECT c.oid, c.relkind"
      + "      FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace"
      + "     WHERE n.nspname = ? AND c.relname = ?"
      + "    UNION ALL"
      // NOT inhdetachpending, matching RelationGetPartitionDesc. A partition left half-detached
      // by an interrupted DETACH PARTITION ... CONCURRENTLY is not part of the table any more and
      // PostgreSQL will not index it, so counting it as a leaf would report a complete, healthy
      // tree as "covers only N of N+1 leaf partitions" and fail the gate forever.
      + "    SELECT c2.oid, c2.relkind"
      + "      FROM tree t"
      + "      JOIN pg_inherits i ON i.inhparent = t.oid AND NOT i.inhdetachpending"
      + "      JOIN pg_class c2 ON c2.oid = i.inhrelid"
      + "),"
      // The named index is looked up by schema + name ALONE, never joined to the target table:
      // an index of the right name on the wrong table has to be reportable as exactly that.
      + "named AS ("
      + "    SELECT c.oid, c.relkind::text AS relkind, ix.indrelid,"
      + "           COALESCE(ix.indisvalid, false) AS valid,"
      + "           COALESCE(ix.indisready, false) AS ready,"
      + "           COALESCE(ix.indislive, false)  AS live"
      + "      FROM pg_class c"
      + "      JOIN pg_namespace n ON n.oid = c.relnamespace"
      + "      LEFT JOIN pg_index ix ON ix.indexrelid = c.oid"
      + "     WHERE n.nspname = ? AND c.relname = ? AND c.relkind IN ('i', 'I')"
      + "),"
      // Recursive, not direct-children: see the class comment. On a single-level table this
      // reduces to the direct children and costs nothing.
      + "idx_tree AS ("
      + "    SELECT oid FROM named"
      + "    UNION ALL"
      + "    SELECT c2.oid"
      + "      FROM idx_tree it"
      + "      JOIN pg_inherits i ON i.inhparent = it.oid"
      + "      JOIN pg_class c2 ON c2.oid = i.inhrelid"
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
      + "       false AS idx_valid,"
      + "       false AS idx_ready,"
      + "       false AS idx_live,"
      + "       false AS covering,"
      + "       false AS attached_to_any,"
      + "       EXISTS (SELECT 1 FROM named) AS named_exists,"
      + "       (SELECT relkind FROM named) AS named_relkind,"
      + "       (SELECT valid FROM named) AS named_valid,"
      + "       (SELECT ready FROM named) AS named_ready,"
      + "       (SELECT live  FROM named) AS named_live,"
      + "       (SELECT n.nspname || '.' || c.relname FROM named"
      + "          JOIN pg_class c ON c.oid = named.indrelid"
      + "          JOIN pg_namespace n ON n.oid = c.relnamespace) AS named_on_table,"
      + "       (SELECT c.relkind::text FROM pg_class c"
      + "          JOIN pg_namespace n ON n.oid = c.relnamespace"
      + "         WHERE n.nspname = ? AND c.relname = ?) AS root_relkind"
      + " UNION ALL "
      + "SELECT 'L'::text,"
      + "       l.leaf_schema,"
      + "       l.leaf_name,"
      + "       ic.relname,"
      + "       COALESCE(ix.indisvalid, false),"
      + "       COALESCE(ix.indisready, false),"
      + "       COALESCE(ix.indislive, false),"
      + "       (ic.oid IN (SELECT oid FROM idx_tree)),"
      + "       (ii.inhparent IS NOT NULL),"
      + "       false,"
      + "       NULL::text,"
      + "       NULL::boolean,"
      + "       NULL::boolean,"
      + "       NULL::boolean,"
      + "       NULL::text,"
      + "       NULL::text"
      + "  FROM leaf l"
      + "  LEFT JOIN pg_index ix ON ix.indrelid = l.oid"
      + "  LEFT JOIN pg_class ic ON ic.oid = ix.indexrelid"
      + "  LEFT JOIN pg_inherits ii ON ii.inhrelid = ic.oid"
      + " ORDER BY 1, 2, 3, 4";

    private GateInspection() {
    }

    /** Runs the gate query on an already-open connection. Read-only: one SELECT, no SET. */
    public static GateSnapshot inspect(Connection connection, String schemaName,
                                       String tableName, String indexName) throws SQLException {
        GateSnapshot snapshot = new GateSnapshot();
        Map<String, GateLeaf> byLeaf = new LinkedHashMap<String, GateLeaf>();

        try (PreparedStatement ps = connection.prepareStatement(GATE_SQL)) {
            ps.setString(1, schemaName);
            ps.setString(2, tableName);
            ps.setString(3, schemaName);
            ps.setString(4, indexName);
            ps.setString(5, schemaName);
            ps.setString(6, tableName);
            try (ResultSet rs = ps.executeQuery()) {
                while (rs.next()) {
                    if ("T".equals(rs.getString("row_kind"))) {
                        snapshot.setNamedIndexExists(rs.getBoolean("named_exists"));
                        snapshot.setNamedIndexRelkind(rs.getString("named_relkind"));
                        snapshot.setNamedIndexValid(rs.getBoolean("named_valid"));
                        snapshot.setNamedIndexReady(rs.getBoolean("named_ready"));
                        snapshot.setNamedIndexLive(rs.getBoolean("named_live"));
                        snapshot.setNamedIndexOnTable(rs.getString("named_on_table"));
                        snapshot.setRootRelkind(rs.getString("root_relkind"));
                        continue;
                    }
                    String leafSchema = rs.getString("leaf_schema");
                    String leafName = rs.getString("leaf_name");
                    String key = leafSchema + " " + leafName;
                    GateLeaf leaf = byLeaf.get(key);
                    if (leaf == null) {
                        leaf = new GateLeaf(leafSchema, leafName);
                        byLeaf.put(key, leaf);
                        snapshot.addLeaf(leaf);
                    }
                    String indexOnLeaf = rs.getString("index_name");
                    if (indexOnLeaf != null) {
                        leaf.addIndex(new GateIndex(
                                indexOnLeaf,
                                rs.getBoolean("idx_valid"),
                                rs.getBoolean("idx_ready"),
                                rs.getBoolean("idx_live"),
                                rs.getBoolean("covering"),
                                rs.getBoolean("attached_to_any")));
                    }
                }
            }
        }
        return snapshot;
    }
}
