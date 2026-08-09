package io.github.atulsinha87.partitionctl.liquibase.precondition;

import io.github.atulsinha87.partitionctl.liquibase.catalog.GateSnapshot;
import io.github.atulsinha87.partitionctl.liquibase.catalog.GateSnapshots;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.util.ArrayList;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

class ReindexGatePreconditionTest {

    private static final String INDEX = "idx_personaddress";
    private static final String CHILD = INDEX + "_person_p01";

    // ---------------------------------------------------------------- passes

    @Test
    @DisplayName("passes on a healthy tree with no leftovers")
    void passesOnACleanTree() {
        assertPasses(GateSnapshots.partitionedTable()
                .partitionedIndex("public.person")
                .coveredLeaf("public", "person_p01", CHILD)
                .coveredLeaf("public", "person_p02", INDEX + "_person_p02")
                .build());
    }

    @Test
    @DisplayName("passes with an invalid parent: strictness is composed, not duplicated")
    void passesOnAnInvalidParent() {
        // An adopter who wants indisvalid on the parent adds partitionctlIndexGate
        // requireValidParent="true" to the same <preConditions> block; they are ANDed.
        assertPasses(GateSnapshots.partitionedTable()
                .partitionedIndex("public.person", false, true, true)
                .coveredLeaf("public", "person_p01", CHILD)
                .build());
    }

    @Test
    @DisplayName("another index's _ccnew on the same leaf is not this index's problem")
    void ignoresAForeignLeftover() {
        // It may well be an unrelated maintenance job's rebuild, still legitimately running.
        // Halting here would let that job block this changeset with no repair path we own.
        assertPasses(GateSnapshots.partitionedTable()
                .partitionedIndex("public.person")
                .coveredLeaf("public", "person_p01", CHILD)
                .andLeftover("person_p01_created_idx_ccnew", false)
                .build());
    }

    // ---------------------------------------------------------------- fails

    @Test
    @DisplayName("fails on a _ccnew: the rebuild failed and the base index is still the old copy")
    void failsOnACcnew() {
        assertFailsWith(GateSnapshots.partitionedTable()
                        .partitionedIndex("public.person")
                        .coveredLeaf("public", "person_p01", CHILD)
                        .andLeftover(CHILD + "_ccnew", false)
                        .build(),
                "the rebuild of " + CHILD + " FAILED");
    }

    @Test
    @DisplayName("fails on a _ccold: the rebuild succeeded and the superseded copy is still there")
    void failsOnACcold() {
        assertFailsWith(GateSnapshots.partitionedTable()
                        .partitionedIndex("public.person")
                        .coveredLeaf("public", "person_p01", CHILD)
                        .andLeftover(CHILD + "_ccold", true)
                        .build(),
                "the rebuild of " + CHILD + " SUCCEEDED");
    }

    @Test
    @DisplayName("fails on a numbered leftover: _ccnew1 is what PostgreSQL uses when _ccnew is taken")
    void failsOnANumberedLeftover() {
        assertFailsWith(GateSnapshots.partitionedTable()
                        .partitionedIndex("public.person")
                        .coveredLeaf("public", "person_p01", CHILD)
                        .andLeftover(CHILD + "_ccnew1", false)
                        .build(),
                "_ccnew");
    }

    @Test
    @DisplayName("the leftover name is derived from the CATALOG's base name, not a generated one")
    void derivesTheLeftoverFromTheCoveringIndexName() {
        // A tree built by a plain CREATE INDEX has PostgreSQL-chosen child names, so a leftover
        // check that assumed our naming convention would see nothing at all.
        assertFailsWith(GateSnapshots.partitionedTable()
                        .partitionedIndex("public.person")
                        .coveredLeaf("public", "person_p01", "person_p01_address_idx")
                        .andLeftover("person_p01_address_idx_ccnew", false)
                        .build(),
                "person_p01_address_idx_ccnew");
    }

    @Test
    @DisplayName("a broken tree fails before the leftover check, and says so specifically")
    void failsOnAnUncoveredLeafFirst() {
        List<String> failures = evaluate(GateSnapshots.partitionedTable()
                .partitionedIndex("public.person")
                .bareLeaf("public", "person_p01")
                .build());
        assertEquals(1, failures.size(), failures.toString());
        assertTrue(failures.get(0).contains("no child index attached"), failures.get(0));
    }

    @Test
    @DisplayName("fails when the index does not exist")
    void failsWhenTheIndexIsAbsent() {
        assertFailsWith(GateSnapshots.partitionedTable().bareLeaf("public", "person_p01").build(),
                "no index named public." + INDEX + " exists");
    }

    @Test
    @DisplayName("refuses to pass vacuously on a partitioned table with no partitions")
    void failsOnAPartitionedTableWithNoLeaves() {
        assertFailsWith(GateSnapshots.partitionedTable().partitionedIndex("public.person").build(),
                "no leaf partitions");
    }

    // ---------------------------------------------------------------- helpers

    private static ReindexGatePrecondition gate() {
        ReindexGatePrecondition gate = new ReindexGatePrecondition();
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
