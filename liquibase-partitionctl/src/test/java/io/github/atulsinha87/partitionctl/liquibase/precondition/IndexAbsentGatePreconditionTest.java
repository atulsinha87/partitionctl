package io.github.atulsinha87.partitionctl.liquibase.precondition;

import io.github.atulsinha87.partitionctl.liquibase.catalog.GateSnapshot;
import io.github.atulsinha87.partitionctl.liquibase.catalog.GateSnapshots;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.util.ArrayList;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

class IndexAbsentGatePreconditionTest {

    private static final String INDEX = "idx_personaddress";

    // ---------------------------------------------------------------- passes

    @Test
    @DisplayName("passes when the index is gone and the leaves carry nothing of ours")
    void passesOnACleanTree() {
        assertPasses(GateSnapshots.partitionedTable()
                .bareLeaf("public", "person_p01")
                .bareLeaf("public", "person_p02")
                .build());
    }

    @Test
    @DisplayName("an unrelated index on a leaf is not our leftover")
    void ignoresUnrelatedIndexes() {
        assertPasses(GateSnapshots.partitionedTable()
                .leafWithIndex("public", "person_p01", "person_p01_created_idx",
                        true, true, true, false)
                .build());
    }

    @Test
    @DisplayName("a name that merely starts the same is not our leftover")
    void ignoresANameThatOnlyLooksLikeOurs() {
        // idx_personaddress_person_p01_extra is not the convention name and is not a leftover
        // PostgreSQL would derive from it either.
        assertPasses(GateSnapshots.partitionedTable()
                .leafWithIndex("public", "person_p01", INDEX + "_person_p01_extra",
                        true, true, true, false)
                .build());
    }

    // ---------------------------------------------------------------- fails

    @Test
    @DisplayName("fails while the partitioned index is still there")
    void failsWhileTheIndexExists() {
        assertFailsWith(GateSnapshots.partitionedTable()
                        .partitionedIndex("public.person")
                        .coveredLeaf("public", "person_p01", INDEX + "_person_p01")
                        .build(),
                "still exists: a partitioned index");
    }

    @Test
    @DisplayName("fails on an index of that name that was never partitioned")
    void failsOnAnOrdinaryIndexOfTheSameName() {
        assertFailsWith(GateSnapshots.partitionedTable()
                        .ordinaryIndex("public.person")
                        .bareLeaf("public", "person_p01")
                        .build(),
                "still exists: an ordinary index");
    }

    @Test
    @DisplayName("fails on an index of that name on some other table: gone means gone everywhere")
    void failsOnTheSameNameElsewhere() {
        assertFailsWith(GateSnapshots.partitionedTable()
                        .partitionedIndex("public.other")
                        .bareLeaf("public", "person_p01")
                        .build(),
                "on public.other");
    }

    @Test
    @DisplayName("fails on a free-standing orphan bearing the convention name")
    void failsOnAnOrphanedChildIndex() {
        // DROP INDEX on the parent removes the ATTACHED children. An index an interrupted
        // CREATE INDEX CONCURRENTLY built but never attached survives it untouched.
        assertFailsWith(GateSnapshots.partitionedTable()
                        .leafWithIndex("public", "person_p01", INDEX + "_person_p01",
                                false, true, true, false)
                        .build(),
                "1 child index(es) of public." + INDEX + " are still on the leaf partitions");
    }

    @Test
    @DisplayName("fails on a _ccnew or _ccold derived from the convention name")
    void failsOnAReindexLeftover() {
        assertFailsWith(GateSnapshots.partitionedTable()
                        .leafWithIndex("public", "person_p01", INDEX + "_person_p01_ccold",
                                false, true, true, false)
                        .build(),
                "leftover of an interrupted REINDEX");
    }

    @Test
    @DisplayName("fails when the table does not exist rather than passing vacuously")
    void failsWhenTheTableIsAbsent() {
        // "the index is gone" is trivially true when the table is gone too, and a typo'd
        // tableName would otherwise sail through the one gate meant to catch leftovers.
        assertFailsWith(GateSnapshots.none().build(), "table public.person does not exist");
    }

    @Test
    @DisplayName("names both the surviving parent and the orphans in one verdict")
    void reportsEveryReasonAtOnce() {
        List<String> failures = evaluate(GateSnapshots.partitionedTable()
                .partitionedIndex("public.person")
                .leafWithIndex("public", "person_p01", INDEX + "_person_p01",
                        true, true, true, false)
                .build());
        assertEquals(2, failures.size(), failures.toString());
    }

    @Test
    @DisplayName("an ATTACHED child of the surviving parent is not reported as a leftover too")
    void doesNotCallAnAttachedChildAFreeStandingOrphan() {
        // Found by running it. The first live run reported the six properly attached children of
        // a still-present parent as "free-standing" leftovers that "dropping the partitioned
        // parent does not remove" -- doubly wrong, since DROP INDEX on the parent removes exactly
        // those. Nothing that is attached to anything can be a leftover.
        List<String> failures = evaluate(GateSnapshots.partitionedTable()
                .partitionedIndex("public.person")
                .coveredLeaf("public", "person_p01", INDEX + "_person_p01")
                .coveredLeaf("public", "person_p02", INDEX + "_person_p02")
                .build());
        assertEquals(1, failures.size(), failures.toString());
        assertTrue(failures.get(0).contains("still exists"), failures.get(0));
        assertTrue(!failures.get(0).contains("free-standing"), failures.get(0));
    }

    // ---------------------------------------------------------------- helpers

    private static IndexAbsentGatePrecondition gate() {
        IndexAbsentGatePrecondition gate = new IndexAbsentGatePrecondition();
        gate.setSchemaName("public");
        gate.setTableName("person");
        gate.setIndexName(INDEX);
        return gate;
    }

    private static List<String> evaluate(GateSnapshot snapshot) {
        List<String> failures = new ArrayList<String>();
        gate().evaluate(snapshot, failures);
        return failures;
    }

    private static void assertPasses(GateSnapshot snapshot) {
        assertEquals(new ArrayList<String>(), evaluate(snapshot));
    }

    private static void assertFailsWith(GateSnapshot snapshot, String expected) {
        List<String> failures = evaluate(snapshot);
        assertTrue(!failures.isEmpty(), "expected a failure mentioning: " + expected);
        assertTrue(PartitionedIndexPrecondition.join(failures, " ").contains(expected),
                "expected a failure mentioning \"" + expected + "\" but got: " + failures);
    }
}
