package io.github.atulsinha87.partitionctl.liquibase.catalog;

/** One index that exists on one leaf partition, as PostgreSQL currently sees it. */
public final class LeafIndex {

    private final String indexName;
    private final boolean valid;
    private final boolean attachedToTargetParent;
    private final boolean attachedToAnyParent;

    public LeafIndex(String indexName, boolean valid,
                     boolean attachedToTargetParent, boolean attachedToAnyParent) {
        this.indexName = indexName;
        this.valid = valid;
        this.attachedToTargetParent = attachedToTargetParent;
        this.attachedToAnyParent = attachedToAnyParent;
    }

    public String getIndexName() {
        return indexName;
    }

    /** {@code pg_index.indisvalid}. False means an interrupted build left it unusable. */
    public boolean isValid() {
        return valid;
    }

    /** True when this index inherits from the parent index this changeset is building. */
    public boolean isAttachedToTargetParent() {
        return attachedToTargetParent;
    }

    /** True when this index inherits from <em>any</em> partitioned index. */
    public boolean isAttachedToAnyParent() {
        return attachedToAnyParent;
    }

    @Override
    public String toString() {
        return indexName + "[valid=" + valid + ",attachedToTarget=" + attachedToTargetParent
                + ",attachedToAny=" + attachedToAnyParent + "]";
    }
}
