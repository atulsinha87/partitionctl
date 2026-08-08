package io.github.atulsinha87.partitionctl.liquibase.statement;

import io.github.atulsinha87.partitionctl.liquibase.catalog.IndexNaming;
import io.github.atulsinha87.partitionctl.liquibase.catalog.LeafIndex;
import io.github.atulsinha87.partitionctl.liquibase.catalog.LeafPartition;
import io.github.atulsinha87.partitionctl.liquibase.catalog.TreeState;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.util.ArrayList;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

/** The planner is pure, so every interesting catalog state is reachable without a database. */
class StatementBuilderTest {

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

    private static TreeState state(boolean parentExists, boolean parentValid, LeafPartition... leaves) {
        TreeState state = new TreeState();
        state.setRootRelkind("p");
        state.setParentIndexExists(parentExists);
        state.setParentIndexValid(parentValid);
        state.setOriginalStatementTimeout("0");
        state.setOriginalLockTimeout("0");
        List<LeafPartition> list = new ArrayList<LeafPartition>();
        for (LeafPartition leaf : leaves) {
            state.addLeaf(leaf);
            list.add(leaf);
        }
        IndexNaming.assignChildIndexNames(INDEX, list);
        return state;
    }

    private static String sqlOf(List<PlannedStatement> statements) {
        StringBuilder sb = new StringBuilder();
        for (PlannedStatement statement : statements) {
            sb.append(statement.getSql()).append('\n');
        }
        return sb.toString();
    }

    private static int count(List<PlannedStatement> statements, String needle) {
        int n = 0;
        for (PlannedStatement statement : statements) {
            if (statement.getSql().contains(needle)) {
                n++;
            }
        }
        return n;
    }

    private static int indexOf(List<PlannedStatement> statements, String needle) {
        for (int i = 0; i < statements.size(); i++) {
            if (statements.get(i).getSql().contains(needle)) {
                return i;
            }
        }
        return -1;
    }

    // ------------------------------------------------------------------ green field

    @Test
    @DisplayName("nothing built yet: parent ON ONLY, then build+attach per leaf, then the gate")
    void greenField() {
        List<PlannedStatement> statements = StatementBuilder.build(plan(),
                state(false, false,
                        new LeafPartition(SCHEMA, "person_p01"),
                        new LeafPartition(SCHEMA, "person_p02")));

        String sql = sqlOf(statements);
        assertTrue(sql.contains("CREATE INDEX \"idx_personaddress\" ON ONLY "
                + "\"public\".\"person\" (\"address\" DESC)"), sql);
        assertEquals(2, count(statements, "CREATE INDEX CONCURRENTLY"), sql);
        assertEquals(2, count(statements, "ATTACH PARTITION"), sql);
        assertEquals(0, count(statements, "DROP INDEX"), sql);
        assertTrue(statements.get(statements.size() - 1).getSql().startsWith("DO $partitionctl$"), sql);

        assertTrue(sql.contains("CREATE INDEX CONCURRENTLY \"idx_personaddress_person_p01\" "
                + "ON \"public\".\"person_p01\" (\"address\" DESC)"), sql);
        assertTrue(sql.contains("ALTER INDEX \"public\".\"idx_personaddress\" ATTACH PARTITION "
                + "\"public\".\"idx_personaddress_person_p01\""), sql);
    }

    // ------------------------------------------------------- THE DEFECT THAT MUST NOT SHIP

    @Test
    @DisplayName("an INVALID leftover child is treated as ABSENT: dropped and rebuilt, never attached bare")
    void invalidLeftoverIsRebuiltNotAttached() {
        LeafPartition interrupted = new LeafPartition(SCHEMA, "person_p01");
        // exactly the name our convention generates, left invalid by an interrupted CIC
        interrupted.addIndex(new LeafIndex("idx_personaddress_person_p01", false, false, false));

        List<PlannedStatement> statements = StatementBuilder.build(plan(),
                state(true, false, interrupted, new LeafPartition(SCHEMA, "person_p02")));

        String sql = sqlOf(statements);
        int drop = indexOf(statements, "DROP INDEX CONCURRENTLY \"public\".\"idx_personaddress_person_p01\"");
        int build = indexOf(statements, "CREATE INDEX CONCURRENTLY \"idx_personaddress_person_p01\"");
        int attach = indexOf(statements, "ATTACH PARTITION \"public\".\"idx_personaddress_person_p01\"");

        assertTrue(drop >= 0, "the invalid leftover must be dropped, not reused:\n" + sql);
        assertTrue(build > drop, "and rebuilt after the drop:\n" + sql);
        assertTrue(attach > build, "and only then attached:\n" + sql);

        // The parent already exists, so it must not be created a second time.
        assertEquals(0, count(statements, "ON ONLY"), sql);
    }

    @Test
    @DisplayName("a VALID unattached child of the right name is attached without rebuilding")
    void validUnattachedChildIsJustAttached() {
        LeafPartition built = new LeafPartition(SCHEMA, "person_p01");
        built.addIndex(new LeafIndex("idx_personaddress_person_p01", true, false, false));

        List<PlannedStatement> statements = StatementBuilder.build(plan(), state(true, false, built));

        String sql = sqlOf(statements);
        assertEquals(0, count(statements, "CREATE INDEX CONCURRENTLY"), sql);
        assertEquals(0, count(statements, "DROP INDEX"), sql);
        assertEquals(1, count(statements, "ATTACH PARTITION"), sql);
    }

    // ------------------------------------------------------------------ resume / coverage

    @Test
    @DisplayName("a leaf already covered by a VALID child is skipped, whatever the index is called")
    void coveredLeafIsSkippedEvenUnderPostgresOwnName() {
        LeafPartition done = new LeafPartition(SCHEMA, "person_p03");
        // PostgreSQL names the child indexes it creates for partitions added later
        done.addIndex(new LeafIndex("person_p03_address_idx", true, true, true));
        LeafPartition todo = new LeafPartition(SCHEMA, "person_p04");

        List<PlannedStatement> statements = StatementBuilder.build(plan(), state(true, false, done, todo));

        String sql = sqlOf(statements);
        assertFalse(sql.contains("person_p03"), "name-based coverage would rebuild it:\n" + sql);
        assertEquals(1, count(statements, "CREATE INDEX CONCURRENTLY"), sql);
        assertEquals(1, count(statements, "ATTACH PARTITION"), sql);
    }

    @Test
    @DisplayName("everything already done emits no work at all, only the verification gate")
    void fullyBuiltTreeEmitsOnlyTheGate() {
        LeafPartition done = new LeafPartition(SCHEMA, "person_p01");
        done.addIndex(new LeafIndex("idx_personaddress_person_p01", true, true, true));

        List<PlannedStatement> statements = StatementBuilder.build(plan(), state(true, true, done));

        assertEquals(1, statements.size(), sqlOf(statements));
        assertTrue(statements.get(0).getSql().startsWith("DO $partitionctl$"));
    }

    // ------------------------------------------------- corrupted tree: invalid ATTACHED child

    @Test
    @DisplayName("an INVALID but ATTACHED child is repaired with REINDEX CONCURRENTLY, not dropped")
    void invalidAttachedChildIsReindexedInPlace() {
        LeafPartition corrupt = new LeafPartition(SCHEMA, "person_p01");
        corrupt.addIndex(new LeafIndex("idx_personaddress_person_p01", false, true, true));

        List<PlannedStatement> statements = StatementBuilder.build(plan(), state(true, false, corrupt));

        String sql = sqlOf(statements);
        assertTrue(sql.contains("REINDEX INDEX CONCURRENTLY "
                + "\"public\".\"idx_personaddress_person_p01\""), sql);
        // PostgreSQL 17 refuses to drop an attached child and has no DETACH for indexes.
        assertEquals(0, count(statements, "DROP INDEX"), sql);
        assertEquals(0, count(statements, "ATTACH PARTITION"), sql);
    }

    @Test
    @DisplayName("a name already attached to a DIFFERENT parent index fails at plan time")
    void nameTakenByAnotherTreeFailsLoudly() {
        LeafPartition leaf = new LeafPartition(SCHEMA, "person_p01");
        leaf.addIndex(new LeafIndex("idx_personaddress_person_p01", true, false, true));

        PlanException thrown = assertThrows(PlanException.class,
                () -> StatementBuilder.build(plan(), state(true, false, leaf)));
        assertTrue(thrown.getMessage().contains("DIFFERENT partitioned index"), thrown.getMessage());
        assertTrue(thrown.getMessage().contains("Nothing was executed"), thrown.getMessage());
    }

    // ------------------------------------------------------------------ timeouts

    @Test
    @DisplayName("statement_timeout is left alone when the session is already at PostgreSQL's default 0")
    void statementTimeoutUntouchedWhenAlreadyZero() {
        List<PlannedStatement> statements = StatementBuilder.build(plan(),
                state(false, false, new LeafPartition(SCHEMA, "person_p01")));
        assertEquals(0, count(statements, "statement_timeout"), sqlOf(statements));
    }

    @Test
    @DisplayName("a finite statement_timeout is lifted for the CIC only and restored right after")
    void statementTimeoutLiftedAroundTheConcurrentBuildOnly() {
        TreeState state = state(false, false, new LeafPartition(SCHEMA, "person_p01"));
        state.setOriginalStatementTimeout("30s");

        List<PlannedStatement> statements = StatementBuilder.build(plan(), state);
        String sql = sqlOf(statements);

        int zero = indexOf(statements, "SET statement_timeout = 0");
        int build = indexOf(statements, "CREATE INDEX CONCURRENTLY");
        int restore = indexOf(statements, "SET statement_timeout = '30s'");
        assertTrue(zero >= 0 && zero < build, sql);
        assertTrue(restore > build, sql);

        int attach = indexOf(statements, "ATTACH PARTITION");
        assertTrue(restore < attach, "the ATTACH must run under the adopter's own timeout:\n" + sql);
    }

    @Test
    @DisplayName("lock_timeout: 15min for the concurrent build, 30s for the attach, restored at the end")
    void lockTimeoutsDifferPerStatement() {
        List<PlannedStatement> statements = StatementBuilder.build(plan(),
                state(true, false, new LeafPartition(SCHEMA, "person_p01")));

        int build = indexOf(statements, "CREATE INDEX CONCURRENTLY");
        int attach = indexOf(statements, "ATTACH PARTITION");
        assertEquals("SET lock_timeout = '15min'", statements.get(build - 1).getSql());
        assertEquals("SET lock_timeout = '30s'", statements.get(attach - 1).getSql());

        // restored just before the gate, so a failing gate still leaves a clean session
        assertEquals("SET lock_timeout = '0'", statements.get(statements.size() - 2).getSql(),
                sqlOf(statements));
    }

    @Test
    @DisplayName("every statement stands alone -- nothing is clubbed into a shared query string")
    void oneStatementPerLine() {
        List<PlannedStatement> statements = StatementBuilder.build(plan(),
                state(false, false, new LeafPartition(SCHEMA, "person_p01")));
        for (PlannedStatement statement : statements) {
            String sql = statement.getSql();
            if (sql.startsWith("DO $partitionctl$")) {
                continue;   // a single statement whose body legitimately contains semicolons
            }
            assertFalse(sql.contains(";"),
                    "multiple statements in one string run in an implicit transaction, "
                            + "which CREATE INDEX CONCURRENTLY refuses: " + sql);
        }
    }

    @Test
    @DisplayName("the label rides along as a leading SQL comment, never as a second statement")
    void labelsAreComments() {
        List<PlannedStatement> statements = StatementBuilder.build(plan(),
                state(false, false, new LeafPartition(SCHEMA, "person_p01")));
        for (PlannedStatement statement : statements) {
            if (statement.getLabel() == null) {
                assertEquals(statement.getSql(), statement.toSql());
            } else {
                assertFalse(statement.getLabel().contains("\n"), "labels must be one line");
                assertTrue(statement.toSql().startsWith("-- " + statement.getLabel() + "\n"));
            }
        }
    }

    // ------------------------------------------------------------------ guards

    @Test
    @DisplayName("an ordinary table is refused with a message that says what to use instead")
    void refusesNonPartitionedTable() {
        TreeState state = state(false, false, new LeafPartition(SCHEMA, "person_p01"));
        state.setRootRelkind("r");

        PlanException thrown = assertThrows(PlanException.class,
                () -> StatementBuilder.build(plan(), state));
        assertTrue(thrown.getMessage().contains("not a partitioned table"), thrown.getMessage());
        assertTrue(thrown.getMessage().contains("createIndex"), thrown.getMessage());
    }

    @Test
    @DisplayName("a missing table is refused before anything runs")
    void refusesMissingTable() {
        TreeState state = state(false, false);
        state.setRootRelkind(null);

        PlanException thrown = assertThrows(PlanException.class,
                () -> StatementBuilder.build(plan(), state));
        assertTrue(thrown.getMessage().contains("does not exist"), thrown.getMessage());
    }

    @Test
    @DisplayName("paceSeconds emits pg_sleep between leaves and nothing when unset")
    void pacing() {
        TreeState state = state(false, false,
                new LeafPartition(SCHEMA, "person_p01"), new LeafPartition(SCHEMA, "person_p02"));

        assertEquals(0, count(StatementBuilder.build(plan(), state), "pg_sleep"));
        assertEquals(2, count(StatementBuilder.build(plan().setPaceSeconds(2), state),
                "SELECT pg_sleep(2)"));
    }
}
