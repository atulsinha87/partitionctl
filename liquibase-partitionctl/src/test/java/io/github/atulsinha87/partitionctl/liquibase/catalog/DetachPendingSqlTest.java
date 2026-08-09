package io.github.atulsinha87.partitionctl.liquibase.catalog;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * Every walk of a partition tree must agree with PostgreSQL's own partition descriptor.
 *
 * <p>An interrupted {@code ALTER TABLE ... DETACH PARTITION ... CONCURRENTLY} leaves
 * {@code pg_inherits.inhdetachpending = t}, and that state persists silently until somebody runs
 * {@code ... FINALIZE}. {@code RelationGetPartitionDesc} excludes such a partition, so PostgreSQL
 * itself will not index it and {@code ALTER INDEX ... ATTACH PARTITION} rejects an index built on
 * one. Measured on 17.10 against a rolling-window table with one detaching partition:
 * <pre>
 * this CTE without the filter       evt_1, evt_2, evt_3   &lt;- 3 "leaves"
 * this CTE with    the filter       evt_2, evt_3          &lt;- 2
 * plain CREATE INDEX on the parent  2 children, parent indisvalid = t
 * ALTER INDEX ... ATTACH of a child built on evt_1
 *   ERROR: cannot attach index "idx_dp_evt_1" as a partition of index "idx_dp"
 * </pre>
 * Unfiltered, the build path creates an index it can never attach and fails on every deploy
 * forever; the gates report a complete, healthy tree as "covers only N of N+1 leaf partitions".
 * Rolling retention on a time-range table is this product's core use case and
 * {@code DETACH CONCURRENTLY} is the online way to expire a partition, so the state is reachable
 * in normal operation.
 */
class DetachPendingSqlTest {

    @Test
    @DisplayName("partition discovery skips a partition that is being detached")
    void discoverySkipsDetaching() {
        assertTrue(PartitionDiscovery.DISCOVERY_SQL.contains("NOT i.inhdetachpending"),
                PartitionDiscovery.DISCOVERY_SQL);
    }

    @Test
    @DisplayName("the precondition gates skip it too, or a healthy tree reads as incomplete")
    void gatesSkipDetaching() {
        assertTrue(GateInspection.GATE_SQL.contains("NOT i.inhdetachpending"),
                GateInspection.GATE_SQL);
    }
}
