package io.github.atulsinha87.partitionctl.liquibase.statement;

import io.github.atulsinha87.partitionctl.liquibase.catalog.IndexNaming;
import io.github.atulsinha87.partitionctl.liquibase.catalog.LeafIndex;
import io.github.atulsinha87.partitionctl.liquibase.catalog.LeafPartition;
import io.github.atulsinha87.partitionctl.liquibase.catalog.OwnershipMarker;
import io.github.atulsinha87.partitionctl.liquibase.catalog.TreeState;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.util.ArrayList;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * The two gaps against M4-PLAN.md that this module had drifted away from:
 * the {@code COMMENT ON INDEX} ownership marker (§5.2) and the index-shape attributes (§5.1).
 *
 * <p>The shape assertions all take the same form — <b>the parent statement and every child
 * statement must carry it</b> — because PostgreSQL 17.10 refuses to attach a child whose
 * definition differs from the parent's, measured for uniqueness, predicate and access method
 * alike: {@code cannot attach index "..." as a partition of index "..." DETAIL: The index
 * definitions do not match.}
 */
class IndexShapeAndMarkerTest {

    private static final String SCHEMA = "public";
    private static final String TABLE = "person";
    private static final String INDEX = "idx_personaddress";

    private static CreateIndexPlan plan() {
        return new CreateIndexPlan()
                .setSchemaName(SCHEMA)
                .setTableName(TABLE)
                .setIndexName(INDEX)
                .addColumn("address", true);
    }

    private static TreeState twoLeaves() {
        TreeState state = new TreeState();
        state.setRootRelkind("p");
        state.setParentIndexExists(false);
        state.setParentIndexValid(false);
        state.setOriginalStatementTimeout("0");
        state.setOriginalLockTimeout("0");
        List<LeafPartition> leaves = new ArrayList<LeafPartition>();
        leaves.add(new LeafPartition(SCHEMA, "person_p01"));
        leaves.add(new LeafPartition(SCHEMA, "person_p02"));
        for (LeafPartition leaf : leaves) {
            state.addLeaf(leaf);
        }
        IndexNaming.assignChildIndexNames(INDEX, leaves);
        return state;
    }

    private static List<String> sqlOf(List<PlannedStatement> statements) {
        List<String> out = new ArrayList<String>();
        for (PlannedStatement statement : statements) {
            out.add(statement.getSql());
        }
        return out;
    }

    private static List<String> creates(List<PlannedStatement> statements) {
        List<String> out = new ArrayList<String>();
        for (String sql : sqlOf(statements)) {
            if (sql.startsWith("CREATE ") && sql.contains(" INDEX ")) {
                out.add(sql);
            }
        }
        return out;
    }

    // ================================================================= gap 1: the marker

    @Test
    @DisplayName("every CREATE INDEX CONCURRENTLY is followed by the ownership marker for that child")
    void everyBuildIsMarked() {
        List<PlannedStatement> statements = StatementBuilder.build(plan(), twoLeaves());
        List<String> sql = sqlOf(statements);

        int marked = 0;
        for (int i = 0; i < sql.size(); i++) {
            if (!sql.get(i).startsWith("CREATE INDEX CONCURRENTLY")) {
                continue;
            }
            assertTrue(i + 1 < sql.size(), "a build with nothing after it: " + sql);
            String next = sql.get(i + 1);
            assertTrue(next.startsWith("COMMENT ON INDEX "),
                    "M4-PLAN 5.2 puts the marker on its own line immediately after the build, "
                            + "but the next statement was: " + next);
            // ...and it must name the index that was just built, not some other one.
            String child = sql.get(i).split("\"")[1];
            assertTrue(next.contains("\"" + child + "\""),
                    "marker names the wrong index: built " + child + ", marked " + next);
            marked++;
        }
        assertEquals(2, marked, "one marker per leaf built");
    }

    @Test
    @DisplayName("the marker is the string the drop change recognises, and carries the changeset id")
    void theMarkerIsTheOneTheDropReads() {
        List<PlannedStatement> statements = StatementBuilder.build(
                plan().setChangeSetId("db/changelog.xml::7::alice"), twoLeaves());

        String comment = null;
        for (String sql : sqlOf(statements)) {
            if (sql.startsWith("COMMENT ON INDEX ")) {
                comment = sql;
                break;
            }
        }
        assertNotNull(comment, "no COMMENT ON INDEX emitted at all");

        // The literal between the outer single quotes is what obj_description() returns.
        String marker = comment.substring(comment.indexOf('\'') + 1, comment.lastIndexOf('\''));
        assertTrue(OwnershipMarker.isOurs(marker),
                "dropPartitionedTableIndex would not recognise this: " + marker);
        assertFalse(OwnershipMarker.isForeign(marker), marker);
        assertTrue(marker.contains("parent=" + INDEX), marker);
        assertTrue(marker.contains("table=public.person"), marker);
        assertTrue(marker.contains("changeset=db/changelog.xml::7::alice"), marker);
    }

    @Test
    @DisplayName("the marker lands BEFORE the attach, so an interrupted run still leaves it ours")
    void markerPrecedesTheAttach() {
        List<String> sql = sqlOf(StatementBuilder.build(plan(), twoLeaves()));
        int firstComment = -1;
        int firstAttach = -1;
        for (int i = 0; i < sql.size(); i++) {
            if (firstComment < 0 && sql.get(i).startsWith("COMMENT ON INDEX ")) {
                firstComment = i;
            }
            if (firstAttach < 0 && sql.get(i).contains("ATTACH PARTITION")) {
                firstAttach = i;
            }
        }
        assertTrue(firstComment >= 0 && firstAttach >= 0, sql.toString());
        assertTrue(firstComment < firstAttach,
                "a run that dies between build and attach must still leave a marked index; "
                        + "marker at " + firstComment + ", attach at " + firstAttach);
    }

    @Test
    @DisplayName("no marker without a build: a leaf already covered emits nothing at all")
    void coveredLeafIsNotMarked() {
        TreeState state = twoLeaves();
        state.setParentIndexExists(true);
        state.getLeaves().get(0).addIndex(new LeafIndex("whatever_pg_called_it", true, true, true));

        List<String> sql = sqlOf(StatementBuilder.build(plan(), state));
        assertEquals(1, countMatching(sql, "COMMENT ON INDEX "),
                "only the uncovered leaf should be built and marked: " + sql);
    }

    // ================================================================= gap 2: index shape

    @Test
    @DisplayName("unique reaches the parent and every child alike")
    void uniqueOnBoth() {
        List<String> creates = creates(StatementBuilder.build(plan().setUnique(true), twoLeaves()));
        assertEquals(3, creates.size(), creates.toString());
        for (String sql : creates) {
            assertTrue(sql.startsWith("CREATE UNIQUE INDEX "), sql);
        }
        assertTrue(creates.get(0).startsWith("CREATE UNIQUE INDEX \"idx_personaddress\" ON ONLY "),
                creates.get(0));
        assertTrue(creates.get(1).startsWith("CREATE UNIQUE INDEX CONCURRENTLY "), creates.get(1));
    }

    @Test
    @DisplayName("unique defaults off, and unique=false is not the same as unique=true")
    void uniqueDefaultsOff() {
        for (CreateIndexPlan p : new CreateIndexPlan[] { plan(), plan().setUnique(false) }) {
            for (String sql : creates(StatementBuilder.build(p, twoLeaves()))) {
                assertFalse(sql.contains("UNIQUE"), sql);
            }
        }
    }

    @Test
    @DisplayName("using reaches the parent and every child, as a quoted identifier")
    void usingOnBoth() {
        List<String> creates = creates(StatementBuilder.build(plan().setUsing("gin"), twoLeaves()));
        assertEquals(3, creates.size(), creates.toString());
        for (String sql : creates) {
            assertTrue(sql.contains(" USING \"gin\" (\"address\" DESC)"), sql);
        }
    }

    @Test
    @DisplayName("where reaches the parent and every child, verbatim and last")
    void whereOnBoth() {
        List<String> creates = creates(StatementBuilder.build(
                plan().setWhere("status <> 'archived'"), twoLeaves()));
        assertEquals(3, creates.size(), creates.toString());
        for (String sql : creates) {
            assertTrue(sql.endsWith(" WHERE status <> 'archived'"),
                    "the predicate is raw SQL appended at the end, unescaped: " + sql);
        }
    }

    @Test
    @DisplayName("the predicate never reaches a SQL comment, where a newline would end the comment")
    void predicateStaysOutOfLabels() {
        // -- <label>\n<sql> is how a statement is rendered. A predicate containing a newline
        // inside a label would terminate the comment and turn the rest into executable SQL.
        List<PlannedStatement> statements = StatementBuilder.build(
                plan().setWhere("k > 1\n  AND j < 2"), twoLeaves());
        for (PlannedStatement statement : statements) {
            if (statement.getLabel() != null) {
                assertFalse(statement.getLabel().contains("\n"), statement.getLabel());
                assertFalse(statement.getLabel().contains("AND j < 2"),
                        "the predicate leaked into a comment: " + statement.getLabel());
            }
        }
    }

    @Test
    @DisplayName("all three together, in PostgreSQL's clause order, on parent and child alike")
    void allThreeTogether() {
        List<String> creates = creates(StatementBuilder.build(
                plan().setUnique(true).setUsing("btree").setWhere("k > 3"), twoLeaves()));

        assertEquals("CREATE UNIQUE INDEX \"idx_personaddress\" ON ONLY \"public\".\"person\" "
                        + "USING \"btree\" (\"address\" DESC) WHERE k > 3",
                creates.get(0));
        assertEquals("CREATE UNIQUE INDEX CONCURRENTLY \"idx_personaddress_person_p01\" "
                        + "ON \"public\".\"person_p01\" USING \"btree\" (\"address\" DESC) WHERE k > 3",
                creates.get(1));
    }

    @Test
    @DisplayName("a rebuild after an invalid leftover carries the shape too")
    void rebuildCarriesTheShape() {
        TreeState state = twoLeaves();
        state.setParentIndexExists(true);
        // an interrupted CIC left an invalid index under exactly the conventional name
        state.getLeaves().get(0).addIndex(
                new LeafIndex("idx_personaddress_person_p01", false, false, false));

        List<String> creates = creates(StatementBuilder.build(
                plan().setUnique(true).setUsing("brin"), state));
        assertEquals(2, creates.size(), creates.toString());
        for (String sql : creates) {
            assertTrue(sql.startsWith("CREATE UNIQUE INDEX CONCURRENTLY "), sql);
            assertTrue(sql.contains(" USING \"brin\" "), sql);
        }
    }

    private static int countMatching(List<String> sql, String prefix) {
        int n = 0;
        for (String one : sql) {
            if (one.startsWith(prefix)) {
                n++;
            }
        }
        return n;
    }
}
