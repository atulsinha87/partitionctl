package io.github.atulsinha87.partitionctl.liquibase.catalog;

import java.util.ArrayList;
import java.util.Collections;
import java.util.List;

/** Everything one discovery round trip learned about the target tree. Immutable once built. */
public final class TreeState {

    private boolean parentIndexExists;
    private boolean parentIndexValid;
    private String originalStatementTimeout;
    private String originalLockTimeout;
    private String rootRelkind;
    private int intermediatePartitionedCount;
    private String parentIndexOwningTable;
    private final List<LeafPartition> leaves = new ArrayList<LeafPartition>();

    /**
     * How many relations below the root are themselves partitioned tables ({@code relkind = 'p'}),
     * i.e. how many levels of sub-partitioning exist. Zero for the ordinary single-level table.
     *
     * <p>Non-zero means {@link #getLeaves()} holds grandchildren, and an index built on a
     * grandchild can never be attached to an index on the root — measured on 17.10,
     * {@code ALTER INDEX ... ATTACH PARTITION} answers "is not an index on any partition of
     * table". The build path refuses rather than producing a permanently failing changeset.
     */
    public int getIntermediatePartitionedCount() {
        return intermediatePartitionedCount;
    }

    public void setIntermediatePartitionedCount(int intermediatePartitionedCount) {
        this.intermediatePartitionedCount = intermediatePartitionedCount;
    }

    /**
     * {@code schema.table} the existing parent index is actually built on, from
     * {@code pg_index.indrelid}, or null when no index of that name exists. Index names are
     * unique per schema but say nothing about which table they belong to, so this is the only
     * way to notice that a changeset has named an index belonging to a different table.
     */
    public String getParentIndexOwningTable() {
        return parentIndexOwningTable;
    }

    public void setParentIndexOwningTable(String parentIndexOwningTable) {
        this.parentIndexOwningTable = parentIndexOwningTable;
    }

    /**
     * {@code pg_class.relkind} of the target table: {@code 'p'} for a partitioned table,
     * {@code 'r'} for an ordinary one, null when it does not exist.
     */
    public String getRootRelkind() {
        return rootRelkind;
    }

    public void setRootRelkind(String rootRelkind) {
        this.rootRelkind = rootRelkind;
    }

    /** True when a partitioned index ({@code relkind = 'I'}) with the requested name exists. */
    public boolean isParentIndexExists() {
        return parentIndexExists;
    }

    public void setParentIndexExists(boolean parentIndexExists) {
        this.parentIndexExists = parentIndexExists;
    }

    /**
     * {@code pg_index.indisvalid} on the parent. A parent stays invalid from the moment it is
     * created {@code ON ONLY} until the final {@code ATTACH}, which validates it automatically.
     */
    public boolean isParentIndexValid() {
        return parentIndexValid;
    }

    public void setParentIndexValid(boolean parentIndexValid) {
        this.parentIndexValid = parentIndexValid;
    }

    /** The session's incoming {@code statement_timeout}, so it can be restored exactly. */
    public String getOriginalStatementTimeout() {
        return originalStatementTimeout;
    }

    public void setOriginalStatementTimeout(String originalStatementTimeout) {
        this.originalStatementTimeout = originalStatementTimeout;
    }

    /** The session's incoming {@code lock_timeout}, so it can be restored exactly. */
    public String getOriginalLockTimeout() {
        return originalLockTimeout;
    }

    public void setOriginalLockTimeout(String originalLockTimeout) {
        this.originalLockTimeout = originalLockTimeout;
    }

    public List<LeafPartition> getLeaves() {
        return Collections.unmodifiableList(leaves);
    }

    public void addLeaf(LeafPartition leaf) {
        leaves.add(leaf);
    }

    /** Mutable view, used only by {@link IndexNaming#assignChildIndexNames}. */
    List<LeafPartition> mutableLeaves() {
        return leaves;
    }
}
