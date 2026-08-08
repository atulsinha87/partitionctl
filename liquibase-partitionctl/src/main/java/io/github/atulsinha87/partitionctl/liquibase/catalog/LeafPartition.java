package io.github.atulsinha87.partitionctl.liquibase.catalog;

import java.util.ArrayList;
import java.util.Collections;
import java.util.List;

/**
 * One leaf partition of the target table, plus every index that currently sits on it.
 *
 * <p>Two different questions are answered per leaf, and conflating them is the defect
 * this whole class exists to prevent:
 *
 * <ol>
 *   <li><b>Is this leaf already covered?</b> Answered from {@code pg_inherits} against the
 *       parent index OID — never by index name. PostgreSQL auto-creates child indexes
 *       with its own names on partitions added after the fact, so a name-based check
 *       reports those leaves as uncovered and tries to build a duplicate.</li>
 *   <li><b>What is the state of the index our naming convention would create?</b> Including
 *       {@code indisvalid}, so an interrupted {@code CREATE INDEX CONCURRENTLY} leftover
 *       is recognised as unusable rather than as "already exists".</li>
 * </ol>
 */
public final class LeafPartition {

    private final String schemaName;
    private final String tableName;
    private final List<LeafIndex> indexes = new ArrayList<LeafIndex>();
    private String childIndexName;

    public LeafPartition(String schemaName, String tableName) {
        this.schemaName = schemaName;
        this.tableName = tableName;
    }

    public String getSchemaName() {
        return schemaName;
    }

    public String getTableName() {
        return tableName;
    }

    public List<LeafIndex> getIndexes() {
        return Collections.unmodifiableList(indexes);
    }

    public void addIndex(LeafIndex index) {
        indexes.add(index);
    }

    /** The deterministic name this extension would give the child index. */
    public String getChildIndexName() {
        return childIndexName;
    }

    public void setChildIndexName(String childIndexName) {
        this.childIndexName = childIndexName;
    }

    /**
     * The index on this leaf that inherits from the parent index being built, or null.
     * Authoritative answer to "is this leaf covered", whatever the index is called.
     */
    public LeafIndex getCoveringIndex() {
        for (LeafIndex index : indexes) {
            if (index.isAttachedToTargetParent()) {
                return index;
            }
        }
        return null;
    }

    /** The index on this leaf literally named {@link #getChildIndexName()}, or null. */
    public LeafIndex getConventionallyNamedIndex() {
        if (childIndexName == null) {
            return null;
        }
        for (LeafIndex index : indexes) {
            if (childIndexName.equals(index.getIndexName())) {
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
