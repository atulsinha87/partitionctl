package io.github.atulsinha87.partitionctl.liquibase.precondition;

import io.github.atulsinha87.partitionctl.liquibase.catalog.GateSnapshot;
import io.github.atulsinha87.partitionctl.liquibase.catalog.GateSnapshots;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.util.ArrayList;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * A gate that has only ever passed has never been tested, so every case here asserts a direction:
 * either an empty failure list, or a failure whose text names the specific thing that is wrong.
 */
class IndexGatePreconditionTest {

    private static final String INDEX = "idx_personaddress";

    // ---------------------------------------------------------------- passes

    @Test
    @DisplayName("passes on a tree where every leaf is covered and usable")
    void passesOnAHealthyTree() {
        assertPasses(gate(false), healthy().build());
    }

    @Test
    @DisplayName("passes with an invalid PARENT by default: that flag does not affect planning")
    void passesOnAnInvalidParentByDefault() {
        // L3: once an invalid child is attached, the parent's indisvalid=f is permanent and no
        // REINDEX variant clears it. Failing by default would break every future deploy.
        assertPasses(gate(false), GateSnapshots.partitionedTable()
                .partitionedIndex("public.person", false, true, true)
                .coveredLeaf("public", "person_p01", INDEX + "_person_p01")
                .build());
    }

    @Test
    @DisplayName("passes when the covering index has a name PostgreSQL chose, not ours")
    void passesOnAPostgresNamedChild() {
        // A partition attached to an already-indexed table gets a child index PostgreSQL names.
        // Coverage keys on pg_inherits, so the name is irrelevant.
        assertPasses(gate(false), GateSnapshots.partitionedTable()
                .partitionedIndex("public.person")
                .coveredLeaf("public", "person_p07", "person_p07_address_idx")
                .build());
    }

    // ---------------------------------------------------------------- fails

    @Test
    @DisplayName("fails when the index does not exist")
    void failsWhenTheIndexIsAbsent() {
        assertFailsWith(gate(false), GateSnapshots.partitionedTable()
                        .bareLeaf("public", "person_p01").build(),
                "no index named public." + INDEX + " exists");
    }

    @Test
    @DisplayName("fails when a leaf has no child index attached, even though the parent exists")
    void failsOnAnUncoveredLeaf() {
        // The exact state CREATE INDEX ON ONLY leaves at step 1 of 3, and the state the old
        // pg_indexes-only check passed on: parent present, not one leaf attached.
        assertFailsWith(gate(false), GateSnapshots.partitionedTable()
                        .partitionedIndex("public.person", false, true, true)
                        .leafWithIndex("public", "person_p01", "some_other_idx",
                                true, true, true, false)
                        .build(),
                "1 of 1 leaf partition(s) have no child index attached");
    }

    @Test
    @DisplayName("fails when a covering index is attached but invalid")
    void failsOnAnInvalidLeafIndex() {
        assertFailsWith(gate(false), GateSnapshots.partitionedTable()
                        .partitionedIndex("public.person")
                        .leafWithIndex("public", "person_p01", INDEX + "_person_p01",
                                false, true, true, true)
                        .build(),
                "indisvalid=false");
    }

    @Test
    @DisplayName("fails on indisready=false: the index is not merely stale, it is diverging")
    void failsOnANotReadyLeafIndex() {
        assertFailsWith(gate(false), GateSnapshots.partitionedTable()
                        .partitionedIndex("public.person")
                        .leafWithIndex("public", "person_p01", INDEX + "_person_p01",
                                true, false, true, true)
                        .build(),
                "indisready=false");
    }

    @Test
    @DisplayName("fails on indislive=false: a DROP INDEX CONCURRENTLY is in progress")
    void failsOnANotLiveLeafIndex() {
        assertFailsWith(gate(false), GateSnapshots.partitionedTable()
                        .partitionedIndex("public.person")
                        .leafWithIndex("public", "person_p01", INDEX + "_person_p01",
                                true, true, false, true)
                        .build(),
                "indislive=false");
    }

    @Test
    @DisplayName("requireValidParent=\"true\" turns the invalid parent into a failure")
    void requireValidParentIsHonoured() {
        assertFailsWith(gate(true), GateSnapshots.partitionedTable()
                        .partitionedIndex("public.person", false, true, true)
                        .coveredLeaf("public", "person_p01", INDEX + "_person_p01")
                        .build(),
                "requireValidParent=\"true\" was set");
    }

    @Test
    @DisplayName("fails when the index of that name sits on a different table")
    void failsOnTheRightIndexAndTheWrongTable() {
        // Index names are unique per schema but say nothing about which table they belong to.
        assertFailsWith(gate(false), GateSnapshots.partitionedTable()
                        .partitionedIndex("public.other")
                        .coveredLeaf("public", "person_p01", INDEX + "_person_p01")
                        .build(),
                "is a partitioned index on public.other");
    }

    @Test
    @DisplayName("fails when the name belongs to an ordinary index")
    void failsOnAnOrdinaryIndex() {
        assertFailsWith(gate(false), GateSnapshots.partitionedTable()
                        .ordinaryIndex("public.person")
                        .coveredLeaf("public", "person_p01", INDEX + "_person_p01")
                        .build(),
                "is an ordinary index");
    }

    @Test
    @DisplayName("fails when the table does not exist")
    void failsWhenTheTableIsAbsent() {
        assertFailsWith(gate(false), GateSnapshots.none().build(),
                "table public.person does not exist");
    }

    @Test
    @DisplayName("fails when the target is an ordinary table")
    void failsOnAnOrdinaryTable() {
        assertFailsWith(gate(false), GateSnapshots.ordinaryTable().build(),
                "is not a partitioned table");
    }

    @Test
    @DisplayName("refuses to pass vacuously on a partitioned table with no partitions")
    void failsOnAPartitionedTableWithNoLeaves() {
        // "every leaf is healthy" is trivially true of zero leaves. Passing there would report
        // success for a table that carries no index at all.
        assertFailsWith(gate(false), GateSnapshots.partitionedTable()
                        .partitionedIndex("public.person").build(),
                "no leaf partitions");
    }

    @Test
    @DisplayName("a long list of broken leaves is summarised, not printed in full")
    void summarisesLargeFailures() {
        GateSnapshots builder = GateSnapshots.partitionedTable().partitionedIndex("public.person");
        for (int i = 1; i <= 40; i++) {
            builder.bareLeaf("public", String.format("person_p%02d", i));
        }
        List<String> failures = evaluate(gate(false), builder.build());
        assertEquals(1, failures.size());
        assertTrue(failures.get(0).contains("40 of 40 leaf partition(s)"), failures.get(0));
        assertTrue(failures.get(0).contains("and 35 more"), failures.get(0));
    }

    // ---------------------------------------------------------------- helpers

    private static GateSnapshots healthy() {
        return GateSnapshots.partitionedTable()
                .partitionedIndex("public.person")
                .coveredLeaf("public", "person_p01", INDEX + "_person_p01")
                .coveredLeaf("public", "person_p02", INDEX + "_person_p02");
    }

    private static IndexGatePrecondition gate(boolean requireValidParent) {
        IndexGatePrecondition gate = new IndexGatePrecondition();
        gate.setSchemaName("public");
        gate.setTableName("person");
        gate.setIndexName(INDEX);
        if (requireValidParent) {
            gate.setRequireValidParent(Boolean.TRUE);
        }
        return gate;
    }

    private static List<String> evaluate(IndexGatePrecondition gate, GateSnapshot snapshot) {
        List<String> failures = new ArrayList<String>();
        gate.evaluate(snapshot, failures);
        return failures;
    }

    private static void assertPasses(IndexGatePrecondition gate, GateSnapshot snapshot) {
        assertEquals(new ArrayList<String>(), evaluate(gate, snapshot));
    }

    private static void assertFailsWith(IndexGatePrecondition gate, GateSnapshot snapshot,
                                        String expected) {
        List<String> failures = evaluate(gate, snapshot);
        assertTrue(!failures.isEmpty(), "expected a failure mentioning: " + expected);
        assertTrue(PartitionedIndexPrecondition.join(failures, " ").contains(expected),
                "expected a failure mentioning \"" + expected + "\" but got: " + failures);
    }
}
