package io.github.atulsinha87.partitionctl.liquibase.statement;

import io.github.atulsinha87.partitionctl.liquibase.catalog.LeafIndex;
import io.github.atulsinha87.partitionctl.liquibase.catalog.LeafPartition;
import io.github.atulsinha87.partitionctl.liquibase.catalog.TreeState;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

/** The planner is pure, so every catalog state worth testing is reachable without a database. */
class ReindexStatementBuilderTest {

    private static final String SCHEMA = "public";
    private static final String TABLE = "person";
    private static final String INDEX = "idx_personaddress";

    // ------------------------------------------------------------------ fixtures

    private static ReindexIndexPlan plan() {
        return new ReindexIndexPlan()
                .setSchemaName(SCHEMA)
                .setTableName(TABLE)
                .setIndexName(INDEX);
    }

    private static TreeState state(LeafPartition... leaves) {
        return state("0", leaves);
    }

    private static TreeState state(String incomingStatementTimeout, LeafPartition... leaves) {
        TreeState state = new TreeState();
        state.setRootRelkind("p");
        state.setParentIndexExists(true);
        state.setParentIndexValid(true);
        state.setOriginalStatementTimeout(incomingStatementTimeout);
        state.setOriginalLockTimeout("0");
        for (LeafPartition leaf : leaves) {
            state.addLeaf(leaf);
        }
        return state;
    }

    /** A leaf whose covering child index is attached to our parent. */
    private static LeafPartition covered(String name, boolean valid) {
        LeafPartition leaf = new LeafPartition(SCHEMA, name);
        leaf.addIndex(new LeafIndex(INDEX + "_" + name, valid, true, true));
        return leaf;
    }

    private static LeafPartition uncovered(String name) {
        return new LeafPartition(SCHEMA, name);
    }

    private static LeafPartition withExtraIndex(LeafPartition leaf, String indexName,
                                                boolean valid, boolean attachedToAny) {
        leaf.addIndex(new LeafIndex(indexName, valid, false, attachedToAny));
        return leaf;
    }

    /** The emitted statements, gate excluded, for the same reason {@link #count} excludes it. */
    private static String sqlOf(List<PlannedStatement> statements) {
        StringBuilder sb = new StringBuilder();
        for (PlannedStatement statement : statements) {
            if (!isGate(statement)) {
                sb.append(statement.getSql()).append('\n');
            }
        }
        return sb.toString();
    }

    /**
     * Counts emitted STATEMENTS only. The gate is a DO block whose error messages quote the very
     * SQL it is checking for -- "Remove a dead one with DROP INDEX CONCURRENTLY" -- so a naive
     * substring count over every statement is off by one on exactly the assertions that matter.
     */
    private static int count(List<PlannedStatement> statements, String needle) {
        int n = 0;
        for (PlannedStatement statement : statements) {
            if (!isGate(statement) && statement.getSql().contains(needle)) {
                n++;
            }
        }
        return n;
    }

    private static int indexOf(List<PlannedStatement> statements, String needle) {
        for (int i = 0; i < statements.size(); i++) {
            if (!isGate(statements.get(i)) && statements.get(i).getSql().contains(needle)) {
                return i;
            }
        }
        return -1;
    }

    /** The one-line labels, which is where the narration lives. */
    private static String labelsOf(List<PlannedStatement> statements) {
        StringBuilder sb = new StringBuilder();
        for (PlannedStatement statement : statements) {
            if (statement.getLabel() != null) {
                sb.append(statement.getLabel()).append('\n');
            }
        }
        return sb.toString();
    }

    private static boolean isGate(PlannedStatement statement) {
        return statement.getSql().startsWith("DO $partitionctl$");
    }

    // ------------------------------------------------------------------ the happy path

    @Test
    @DisplayName("a healthy tree: one REINDEX per leaf, then the gate, and no ATTACH anywhere")
    void healthyTree() {
        List<PlannedStatement> out = ReindexStatementBuilder.build(plan(),
                state(covered("person_p01", true), covered("person_p02", true),
                        covered("person_p03", true)));

        assertEquals(3, count(out, "REINDEX INDEX CONCURRENTLY"));
        assertEquals(0, count(out, "DROP INDEX"));
        // REINDEX swaps in place and the pg_inherits row survives, so nothing is ever re-attached
        // and nothing ever asks for an AccessExclusiveLock.
        assertFalse(sqlOf(out).contains("ATTACH PARTITION"));
        assertTrue(sqlOf(out).contains(
                "REINDEX INDEX CONCURRENTLY \"public\".\"idx_personaddress_person_p02\""));
        assertTrue(out.get(out.size() - 1).getSql().startsWith("DO $partitionctl$"));
    }

    @Test
    @DisplayName("the covering index is reindexed under the name the catalog reports, not a generated one")
    void reindexesWhatIsActuallyThere() {
        // A partition added after the index was built gets a PostgreSQL-chosen child index name.
        LeafPartition leaf = new LeafPartition(SCHEMA, "person_p07");
        leaf.addIndex(new LeafIndex("person_p07_address_idx", true, true, true));

        List<PlannedStatement> out = ReindexStatementBuilder.build(plan(), state(leaf));

        assertTrue(sqlOf(out).contains(
                "REINDEX INDEX CONCURRENTLY \"public\".\"person_p07_address_idx\""));
    }

    @Test
    @DisplayName("an INVALID attached child is reindexed, not skipped -- that is the repair")
    void invalidChildIsRebuilt() {
        List<PlannedStatement> out = ReindexStatementBuilder.build(plan(),
                state(covered("person_p01", false)));

        assertEquals(1, count(out, "REINDEX INDEX CONCURRENTLY"));
        assertTrue(labelsOf(out).contains("rebuilding INVALID"),
                "updateSQL should say why a leaf that looks covered is being rebuilt");
    }

    // ------------------------------------------------------------------ leftovers

    @Test
    @DisplayName("_ccnew means the rebuild failed: drop it and reindex the leaf anyway")
    void ccnewIsDroppedAndTheLeafStillRebuilt() {
        LeafPartition leaf = withExtraIndex(covered("person_p01", true),
                INDEX + "_person_p01_ccnew", false, false);

        List<PlannedStatement> out = ReindexStatementBuilder.build(plan(), state(leaf));

        int drop = indexOf(out, "DROP INDEX CONCURRENTLY");
        int reindex = indexOf(out, "REINDEX INDEX CONCURRENTLY");
        assertTrue(drop >= 0 && reindex >= 0);
        assertTrue(drop < reindex,
                "the drop has to precede the rebuild or PostgreSQL just makes a _ccnew1 beside it");
        assertTrue(sqlOf(out).contains(
                "DROP INDEX CONCURRENTLY \"public\".\"idx_personaddress_person_p01_ccnew\""));
    }

    @Test
    @DisplayName("_ccold means the rebuild succeeded: drop it and SKIP the leaf")
    void ccoldSkipsTheLeaf() {
        LeafPartition done = withExtraIndex(covered("person_p01", true),
                INDEX + "_person_p01_ccold", false, false);
        LeafPartition todo = covered("person_p02", true);

        List<PlannedStatement> out = ReindexStatementBuilder.build(plan(), state(done, todo));

        assertEquals(1, count(out, "DROP INDEX CONCURRENTLY"));
        assertEquals(1, count(out, "REINDEX INDEX CONCURRENTLY"));
        assertTrue(sqlOf(out).contains("\"idx_personaddress_person_p02\""));
        assertFalse(sqlOf(out).contains("REINDEX INDEX CONCURRENTLY \"public\""
                + ".\"idx_personaddress_person_p01\""));
    }

    @Test
    @DisplayName("_ccold beside a _ccnew: both dropped, still skipped -- an earlier attempt did finish")
    void ccoldWinsOverCcnew() {
        LeafPartition leaf = covered("person_p01", true);
        withExtraIndex(leaf, INDEX + "_person_p01_ccold", false, false);
        withExtraIndex(leaf, INDEX + "_person_p01_ccnew", false, false);

        List<PlannedStatement> out = ReindexStatementBuilder.build(plan(), state(leaf));

        assertEquals(2, count(out, "DROP INDEX CONCURRENTLY"));
        assertEquals(0, count(out, "REINDEX INDEX CONCURRENTLY"));
    }

    @Test
    @DisplayName("a _ccold beside an INVALID base is not trusted -- the leaf is rebuilt")
    void ccoldDoesNotSkipAnInvalidBase() {
        LeafPartition leaf = withExtraIndex(covered("person_p01", false),
                INDEX + "_person_p01_ccold", false, false);

        List<PlannedStatement> out = ReindexStatementBuilder.build(plan(), state(leaf));

        assertEquals(1, count(out, "DROP INDEX CONCURRENTLY"));
        assertEquals(1, count(out, "REINDEX INDEX CONCURRENTLY"));
    }

    @Test
    @DisplayName("another index's leftover on the same partition is left strictly alone")
    void foreignLeftoversAreNotTouched() {
        LeafPartition leaf = withExtraIndex(covered("person_p01", true),
                "someone_elses_index_ccnew", false, false);

        List<PlannedStatement> out = ReindexStatementBuilder.build(plan(), state(leaf));

        assertEquals(0, count(out, "DROP INDEX"),
                "that _ccnew may be a DBA's rebuild still in flight");
        assertEquals(1, count(out, "REINDEX INDEX CONCURRENTLY"));
    }

    @Test
    @DisplayName("a VALID index whose name merely ends in _ccnew is never dropped")
    void aLiveIndexNamedLikeALeftoverSurvives() {
        LeafPartition leaf = withExtraIndex(covered("person_p01", true),
                INDEX + "_person_p01_ccnew", true, false);

        List<PlannedStatement> out = ReindexStatementBuilder.build(plan(), state(leaf));

        assertEquals(0, count(out, "DROP INDEX"));
    }

    @Test
    @DisplayName("an ATTACHED index named like a leftover is never dropped either")
    void anAttachedIndexNamedLikeALeftoverSurvives() {
        LeafPartition leaf = withExtraIndex(covered("person_p01", true),
                INDEX + "_person_p01_ccnew", false, true);

        List<PlannedStatement> out = ReindexStatementBuilder.build(plan(), state(leaf));

        assertEquals(0, count(out, "DROP INDEX"));
    }

    @Test
    @DisplayName("the leftover of a 63-byte child index is recognised despite PostgreSQL's truncation")
    void truncatedLeftoverIsRecognised() {
        String base = "idx_" + repeat('a', 59);           // 63 bytes
        String leftover = "idx_" + repeat('a', 53) + "_ccnew"; // also 63 bytes
        LeafPartition leaf = new LeafPartition(SCHEMA, "person_p01");
        leaf.addIndex(new LeafIndex(base, true, true, true));
        leaf.addIndex(new LeafIndex(leftover, false, false, false));

        List<PlannedStatement> out = ReindexStatementBuilder.build(plan(), state(leaf));

        assertTrue(sqlOf(out).contains("DROP INDEX CONCURRENTLY \"public\".\"" + leftover + "\""));
    }

    // ------------------------------------------------------------------ not ready

    @Test
    @DisplayName("no parent index: emit the gate alone and let it diagnose at execution time")
    void missingParentEmitsOnlyTheGate() {
        TreeState state = state(covered("person_p01", true));
        state.setParentIndexExists(false);

        List<PlannedStatement> out = ReindexStatementBuilder.build(plan(), state);

        assertEquals(1, out.size());
        assertTrue(out.get(0).getSql().startsWith("DO $partitionctl$"));
        assertTrue(out.get(0).getSql().contains("is not a partitioned index"));
    }

    @Test
    @DisplayName("an uncovered leaf stops the whole run BEFORE any rebuild, not after")
    void uncoveredLeafEmitsOnlyTheGate() {
        List<PlannedStatement> out = ReindexStatementBuilder.build(plan(),
                state(covered("person_p01", true), uncovered("person_p02")));

        assertEquals(1, out.size());
        assertEquals(0, count(out, "REINDEX"));
        assertTrue(out.get(0).getSql().contains("covers only % of % leaf partitions"));
    }

    @Test
    @DisplayName("an ordinary table, or one that does not exist yet, emits only the gate")
    void nonPartitionedTargetEmitsOnlyTheGate() {
        TreeState ordinary = state(covered("person_p01", true));
        ordinary.setRootRelkind("r");
        assertEquals(1, ReindexStatementBuilder.build(plan(), ordinary).size());

        TreeState missing = state();
        missing.setRootRelkind(null);
        missing.setParentIndexExists(false);
        assertEquals(1, ReindexStatementBuilder.build(plan(), missing).size());

        assertFalse(ReindexStatementBuilder.readyToReindex(missing));
    }

    // ------------------------------------------------------------------ timeouts and pacing

    @Test
    @DisplayName("PostgreSQL's default statement_timeout of 0 means not one SET is emitted")
    void nothingIsEmittedWhenTheDefaultAlreadyHolds() {
        List<PlannedStatement> out = ReindexStatementBuilder.build(plan(),
                state("0", covered("person_p01", true)));

        assertEquals(0, count(out, "SET statement_timeout"));
    }

    @Test
    @DisplayName("statement_timeout is lifted around the rebuild AND around the leftover drop")
    void statementTimeoutIsLiftedAroundBothConcurrentStatements() {
        LeafPartition leaf = withExtraIndex(covered("person_p01", true),
                INDEX + "_person_p01_ccnew", false, false);

        List<PlannedStatement> out = ReindexStatementBuilder.build(plan(), state("5s", leaf));

        // DROP INDEX CONCURRENTLY waits for concurrent transactions exactly as the rebuild does;
        // measured, it is cancelled at 197ms under a 200ms adopter timeout and needs 9.5s.
        assertTrue(indexOf(out, "SET statement_timeout = 0") < indexOf(out, "DROP INDEX CONCURRENTLY"));
        assertEquals(2, count(out, "SET statement_timeout = 0"));
        assertEquals(2, count(out, "SET statement_timeout = '5s'"));
    }

    @Test
    @DisplayName("lock_timeout is set before the concurrent work and restored at the end")
    void lockTimeoutIsSetAndRestored() {
        TreeState state = state(covered("person_p01", true));
        state.setOriginalLockTimeout("250ms");

        List<PlannedStatement> out = ReindexStatementBuilder.build(
                plan().setLockTimeout("9min"), state);

        assertTrue(sqlOf(out).contains("SET lock_timeout = '9min'"));
        assertTrue(indexOf(out, "SET lock_timeout = '250ms'") > indexOf(out, "REINDEX"));
    }

    @Test
    @DisplayName("pacing follows a rebuild, is skipped for a skipped leaf, and is not itself timed out")
    void pacing() {
        LeafPartition skipped = withExtraIndex(covered("person_p01", true),
                INDEX + "_person_p01_ccold", false, false);
        List<PlannedStatement> out = ReindexStatementBuilder.build(
                plan().setPaceSeconds(4), state("5s", skipped, covered("person_p02", true)));

        assertEquals(1, count(out, "pg_sleep(4)"), "no pause is owed for a leaf that did no work");
        // measured: SET statement_timeout='200ms'; SELECT pg_sleep(1) is cancelled, so a pace
        // longer than the adopter's own timeout would fail the changeset for nothing.
        assertTrue(indexOf(out, "SET statement_timeout = 0") < indexOf(out, "pg_sleep(4)"));

        assertEquals(0, count(ReindexStatementBuilder.build(plan(), state(covered("p", true))),
                "pg_sleep"));
    }

    // ------------------------------------------------------------------ the gate

    @Test
    @DisplayName("the gate fails on damage that is repairable and only warns on damage that is not")
    void gateSeverity() {
        List<PlannedStatement> out = ReindexStatementBuilder.build(plan(),
                state(covered("person_p01", true)));
        String gate = out.get(out.size() - 1).getSql();

        assertTrue(gate.contains("RAISE EXCEPTION"));
        // an uncovered leaf and an invalid child are both repairable by re-running
        assertTrue(gate.contains("no complete index to reindex"));
        assertTrue(gate.contains("are INVALID"));
        // a parent stuck at indisvalid = false is not, and hard-failing would wedge runAlways
        assertTrue(gate.contains("RAISE WARNING"));
        assertTrue(gate.contains("indisvalid = false"));
    }

    @Test
    @DisplayName("the gate names multi-level partitioning instead of miscounting coverage")
    void gateNamesMultiLevel() {
        // Measured on a two-level table: the parent index's pg_inherits children are the two
        // intermediate partitioned indexes, not the four leaf indexes, so a direct-children count
        // reports "2 of 4" and means nothing. Say what the shape is instead.
        List<PlannedStatement> out = ReindexStatementBuilder.build(plan(),
                state(covered("person_p01", true)));
        String gate = out.get(out.size() - 1).getSql();

        assertTrue(gate.contains("MULTI-LEVEL partitioned index"), gate);
        assertTrue(gate.contains("REINDEX INDEX CONCURRENTLY %.%"), gate);
    }

    @Test
    @DisplayName("every emitted statement is one statement -- nothing is clubbed into a shared string")
    void oneStatementPerLine() {
        List<PlannedStatement> out = ReindexStatementBuilder.build(
                plan().setPaceSeconds(1),
                state("5s", withExtraIndex(covered("person_p01", true),
                        INDEX + "_person_p01_ccnew", false, false)));

        for (PlannedStatement statement : out) {
            String sql = statement.getSql();
            if (sql.startsWith("DO $partitionctl$")) {
                continue; // one statement, many lines
            }
            assertFalse(sql.contains(";"), "clubbed statements run in an implicit transaction, "
                    + "and REINDEX CONCURRENTLY refuses to run in one: " + sql);
            assertFalse(sql.contains("\n"), sql);
        }
    }

    private static String repeat(char c, int times) {
        StringBuilder sb = new StringBuilder();
        for (int i = 0; i < times; i++) {
            sb.append(c);
        }
        return sb.toString();
    }
}
