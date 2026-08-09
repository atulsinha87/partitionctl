package io.github.atulsinha87.partitionctl.liquibase.catalog;

import java.util.ArrayList;
import java.util.Collections;
import java.util.List;

/** One leaf partition, plus every index currently on it, as the read-only gates see it. */
public final class GateLeaf {

    private final String schemaName;
    private final String tableName;
    private final List<GateIndex> indexes = new ArrayList<GateIndex>();

    public GateLeaf(String schemaName, String tableName) {
        this.schemaName = schemaName;
        this.tableName = tableName;
    }

    public String getSchemaName() {
        return schemaName;
    }

    public String getTableName() {
        return tableName;
    }

    public List<GateIndex> getIndexes() {
        return Collections.unmodifiableList(indexes);
    }

    void addIndex(GateIndex index) {
        indexes.add(index);
    }

    /**
     * The index on this leaf that descends from the gated partitioned index, or null when this
     * leaf is not covered at all.
     *
     * <p>Answered from {@code pg_inherits}, never from the index name: PostgreSQL names the child
     * index itself when a partition is attached to an already-indexed table, so a name-based
     * answer reports those leaves as uncovered.
     */
    public GateIndex getCoveringIndex() {
        for (GateIndex index : indexes) {
            if (index.isCovering()) {
                return index;
            }
        }
        return null;
    }

    /** The index on this leaf with exactly this name, or null. */
    public GateIndex getIndexNamed(String name) {
        if (name == null) {
            return null;
        }
        for (GateIndex index : indexes) {
            if (name.equals(index.getIndexName())) {
                return index;
            }
        }
        return null;
    }

    @Override
    public String toString() {
        return schemaName + "." + tableName;
    }
}
