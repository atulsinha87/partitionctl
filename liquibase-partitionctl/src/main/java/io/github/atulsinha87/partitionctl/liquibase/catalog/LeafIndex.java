package io.github.atulsinha87.partitionctl.liquibase.catalog;

/** One index that exists on one leaf partition, as PostgreSQL currently sees it. */
public final class LeafIndex {

    private final String indexName;
    private final boolean valid;
    private final boolean attachedToTargetParent;
    private final boolean attachedToAnyParent;
    private final String comment;

    public LeafIndex(String indexName, boolean valid,
                     boolean attachedToTargetParent, boolean attachedToAnyParent) {
        this(indexName, valid, attachedToTargetParent, attachedToAnyParent, null);
    }

    public LeafIndex(String indexName, boolean valid, boolean attachedToTargetParent,
                     boolean attachedToAnyParent, String comment) {
        this.indexName = indexName;
        this.valid = valid;
        this.attachedToTargetParent = attachedToTargetParent;
        this.attachedToAnyParent = attachedToAnyParent;
        this.comment = comment;
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

    /**
     * {@code obj_description} on the index, or null when it carries none.
     *
     * <p>This exists so the build can tell an index it left behind from one somebody else
     * created at the same name. Adopting a stranger's index into the tree is not itself
     * destructive, but it makes the tree look wholly ours to a later drop, which destroys
     * every attached child in one statement.
     */
    public String getComment() {
        return comment;
    }

    @Override
    public String toString() {
        return indexName + "[valid=" + valid + ",attachedToTarget=" + attachedToTargetParent
                + ",attachedToAny=" + attachedToAnyParent + "]";
    }
}
