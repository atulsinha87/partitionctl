package io.github.atulsinha87.partitionctl.liquibase.statement;

import io.github.atulsinha87.partitionctl.liquibase.catalog.IndexNaming;
import io.github.atulsinha87.partitionctl.liquibase.catalog.LeafPartition;
import io.github.atulsinha87.partitionctl.liquibase.catalog.TreeState;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.util.ArrayList;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * Regressions for four defects found by adversarial review against a live PostgreSQL 17.10,
 * each of which turned into a <b>permanently failing changeset</b>: with
 * {@code runInTransaction="false"} a failed changeset writes no DATABASECHANGELOG row, so
 * {@code liquibase update} retries it on every deploy and fails identically every time.
 *
 * <p>Each test here fails against the code as it was before the fix.
 */
class CreatePathRepairsTest {

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

    private static TreeState healthyTree(int leaves) {
        TreeState state = new TreeState();
        state.setRootRelkind("p");
        state.setParentIndexExists(false);
        state.setOriginalStatementTimeout("0");
        state.setOriginalLockTimeout("0");
        List<LeafPartition> list = new ArrayList<LeafPartition>();
        for (int i = 1; i <= leaves; i++) {
            LeafPartition leaf = new LeafPartition(SCHEMA, String.format("person_p%02d", i));
            state.addLeaf(leaf);
            list.add(leaf);
        }
        IndexNaming.assignChildIndexNames(INDEX, list);
        return state;
    }

    private static List<String> sqlOf(List<PlannedStatement> statements) {
        List<String> out = new ArrayList<String>();
        for (PlannedStatement statement : statements) {
            out.add(statement.getSql());
        }
        return out;
    }

    // ------------------------------------------------------------------ paceSeconds

    @Test
    @DisplayName("paceSeconds: the sleep runs with statement_timeout lifted, like every long wait")
    void theSleepIsBracketed() {
        // Measured on 17.10 with the adopter's statement_timeout at 1s and paceSeconds="3":
        // unbracketed, run 1 built and attached leaf 1 then died with "canceling statement due to
        // statement timeout", writing no DATABASECHANGELOG row; run 2 advanced exactly one more
        // leaf and died the same way. One leaf of progress per red build, and paceSeconds exists
        // for exactly the partition counts where that means hundreds of failed deploys.
        TreeState state = healthyTree(3);
        state.setOriginalStatementTimeout("1s");
        List<String> sql = sqlOf(StatementBuilder.build(plan().setPaceSeconds(3), state));

        int sleep = sql.indexOf("SELECT pg_sleep(3)");
        assertTrue(sleep > 0, "no pacing statement was emitted: " + sql);
        assertEquals("SET statement_timeout = 0", sql.get(sleep - 1),
                "the sleep is not bracketed, so a pace longer than the adopter's "
                        + "statement_timeout cancels the changeset: " + sql);
        assertEquals("SET statement_timeout = '1s'", sql.get(sleep + 1),
                "the adopter's own statement_timeout must be put straight back: " + sql);
    }

    @Test
    @DisplayName("paceSeconds: create and reindex bracket the sleep identically")
    void bothBuildersAgree() {
        // The create path shipped without this bracket while the reindex path had it. Asserting
        // them together is what stops the two drifting again.
        TreeState state = healthyTree(2);
        state.setOriginalStatementTimeout("1s");
        List<String> created = sqlOf(StatementBuilder.build(plan().setPaceSeconds(2), state));

        TreeState reindexable = healthyTree(2);
        reindexable.setOriginalStatementTimeout("1s");
        reindexable.setParentIndexExists(true);
        for (LeafPartition leaf : reindexable.getLeaves()) {
            leaf.addIndex(new io.github.atulsinha87.partitionctl.liquibase.catalog.LeafIndex(
                    leaf.getChildIndexName(), true, true, true));
        }
        List<String> reindexed = sqlOf(ReindexStatementBuilder.build(
                new ReindexIndexPlan().setSchemaName(SCHEMA).setTableName(TABLE)
                        .setIndexName(INDEX).setPaceSeconds(2), reindexable));

        for (List<String> sql : new List[] { created, reindexed }) {
            int sleep = sql.indexOf("SELECT pg_sleep(2)");
            assertTrue(sleep > 0, "no pacing statement: " + sql);
            assertEquals("SET statement_timeout = 0", sql.get(sleep - 1), sql.toString());
        }
    }

    // ------------------------------------------------------------------ multi-level

    @Test
    @DisplayName("a multi-level partitioned table is refused before anything is created")
    void multiLevelIsRefused() {
        // Measured on 17.10. Discovery walks the whole tree, so the GRANDCHILDREN come back as
        // leaves; ALTER INDEX ... ATTACH PARTITION resolves the child against the parent table's
        // DIRECT partitions only and answers:
        //   ERROR:  cannot attach index "idx_ml_ml_a1" as a partition of index "idx_ml"
        //   DETAIL:  Index "idx_ml_ml_a1" is not an index on any partition of table "ml".
        // Without the guard that is unrecoverable: an invalid parent and an orphan child index are
        // left behind and every re-run repeats the same failure.
        TreeState state = healthyTree(4);
        state.setIntermediatePartitionedCount(2);

        PlanException thrown = assertThrows(PlanException.class, () ->
                StatementBuilder.build(plan(), state));
        assertTrue(thrown.getMessage().contains("MULTI-LEVEL"), thrown.getMessage());
        assertTrue(thrown.getMessage().contains("Nothing was executed"), thrown.getMessage());
    }

    @Test
    @DisplayName("an ordinary single-level table is unaffected by the multi-level guard")
    void singleLevelStillBuilds() {
        TreeState state = healthyTree(4);
        assertEquals(0, state.getIntermediatePartitionedCount());
        assertTrue(sqlOf(StatementBuilder.build(plan(), state)).size() > 4);
    }

    // ------------------------------------------------------------------ wrong table

    @Test
    @DisplayName("an index of the right name on the WRONG table is refused, not adopted as parent")
    void wrongTableIsRefused() {
        // Index names are unique per schema and say nothing about which table they index. Measured:
        // indexName="idx_wrongtable" tableName="wt_right" against an idx_wrongtable already on
        // wt_other emitted no "ON ONLY" line at all -- the planner accepted the other table's index
        // as "already exists" -- then failed at the first ATTACH with a PostgreSQL message that
        // never mentions the wrong table, leaving an orphan child index behind on every re-run.
        TreeState state = healthyTree(3);
        state.setParentIndexExists(true);
        state.setParentIndexValid(true);
        state.setParentIndexOwningTable("public.somewhere_else");

        PlanException thrown = assertThrows(PlanException.class, () ->
                StatementBuilder.build(plan(), state));
        assertTrue(thrown.getMessage().contains("public.somewhere_else"), thrown.getMessage());
        assertTrue(thrown.getMessage().contains("public.person"), thrown.getMessage());
        assertTrue(thrown.getMessage().contains("Nothing was executed"), thrown.getMessage());
    }

    @Test
    @DisplayName("the right index on the right table is not disturbed by the wrong-table guard")
    void rightTablePasses() {
        TreeState state = healthyTree(3);
        state.setParentIndexExists(true);
        state.setParentIndexValid(true);
        state.setParentIndexOwningTable("public.person");
        assertTrue(sqlOf(StatementBuilder.build(plan(), state)).size() > 3);
    }

    // ------------------------------------------------------------------ detach pending

    @Test
    @DisplayName("every recursive partition walk skips inhdetachpending, as PostgreSQL's own does")
    void detachPendingIsExcludedEverywhere() {
        // An interrupted ALTER TABLE ... DETACH PARTITION ... CONCURRENTLY leaves
        // pg_inherits.inhdetachpending = t, and the state persists silently until someone runs
        // ... FINALIZE. RelationGetPartitionDesc excludes such a partition, so PostgreSQL will not
        // index it and ATTACH rejects an index built on it. Measured on 17.10, rolling-window
        // table with one detaching partition:
        //   discovery CTE without the filter   evt_1, evt_2, evt_3   <- 3 "leaves"
        //   discovery CTE with    the filter   evt_2, evt_3          <- 2, matching
        //   plain CREATE INDEX on the parent   2 children, parent indisvalid = t
        // Rolling retention on a time-range table is this product's core use case, so this state
        // is reachable in normal operation -- not a corner.
        // Discovery's own query is asserted in DetachPendingSqlTest, which lives in the catalog
        // package where the constant is visible.
        TreeState state = healthyTree(2);
        String verify = null;
        for (PlannedStatement statement : StatementBuilder.build(plan(), state)) {
            if (statement.getSql().startsWith("DO $partitionctl$")) {
                verify = statement.getSql();
            }
        }
        assertTrue(verify != null && verify.contains("NOT i.inhdetachpending"),
                "the verification block counts leaves its own way; unfiltered it reports a "
                        + "complete tree as \"covers only N of N+1\" and fails the changeset: "
                        + verify);
    }
}
