package io.github.atulsinha87.partitionctl.liquibase.catalog;

import java.util.ArrayList;
import java.util.Collections;
import java.util.List;

/** One leaf partition of the target table, with every index currently on it. */
public final class DropLeaf {

    private final String schemaName;
    private final String tableName;
    private final List<DropCandidateIndex> indexes = new ArrayList<DropCandidateIndex>();
    private String childIndexName;

    public DropLeaf(String schemaName, String tableName) {
        this.schemaName = schemaName;
        this.tableName = tableName;
    }

    public String getSchemaName() {
        return schemaName;
    }

    public String getTableName() {
        return tableName;
    }

    public List<DropCandidateIndex> getIndexes() {
        return Collections.unmodifiableList(indexes);
    }

    public void addIndex(DropCandidateIndex index) {
        indexes.add(index);
    }

    /** The name {@link IndexNaming} would give this leaf's child index, truncation included. */
    public String getChildIndexName() {
        return childIndexName;
    }

    public void setChildIndexName(String childIndexName) {
        this.childIndexName = childIndexName;
    }

    /**
     * The free-standing leftover this leaf carries, or null.
     *
     * <p>An orphan is an index that (a) is named exactly what this extension's naming convention
     * would name the child index for this leaf, and (b) inherits from no partitioned index at
     * all. Both halves matter. Without (a) every unrelated index on the leaf would be swept up.
     * Without (b) an attached child would be included, and an attached child cannot be dropped
     * on its own — PostgreSQL 17 rejects it ("cannot drop index ... because index ... requires
     * it") and offers no {@code ALTER INDEX ... DETACH PARTITION} to separate it first.
     */
    public DropCandidateIndex getOrphan() {
        if (childIndexName == null) {
            return null;
        }
        for (DropCandidateIndex index : indexes) {
            if (childIndexName.equals(index.getIndexName()) && index.isOrphan()) {
                return index;
            }
        }
        return null;
    }

    /** The index on this leaf that inherits from the parent index being dropped, or null. */
    public DropCandidateIndex getAttachedChild() {
        for (DropCandidateIndex index : indexes) {
            if (index.isAttachedToTargetParent()) {
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
