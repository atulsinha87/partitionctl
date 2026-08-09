package io.github.atulsinha87.partitionctl.liquibase.catalog;

/**
 * One index sitting on one leaf partition, as the drop needs to see it.
 *
 * <p>Separate from {@link LeafIndex}, which the create path uses, for one reason: the drop is
 * the only operation that has to read {@code obj_description} to decide whether it is allowed
 * to destroy the object. Widening {@code LeafIndex} would change a type the live-verified
 * create path depends on, to carry a field that path never reads.
 */
public final class DropCandidateIndex {

    private final String indexName;
    private final boolean valid;
    private final boolean attachedToTargetParent;
    private final boolean attachedToAnyParent;
    private final String comment;

    public DropCandidateIndex(String indexName, boolean valid, boolean attachedToTargetParent,
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

    /** {@code pg_index.indisvalid}. An invalid orphan is an interrupted build's leftover. */
    public boolean isValid() {
        return valid;
    }

    /** True when this index inherits from the partitioned index this changeset names. */
    public boolean isAttachedToTargetParent() {
        return attachedToTargetParent;
    }

    /** True when this index inherits from <em>any</em> partitioned index. */
    public boolean isAttachedToAnyParent() {
        return attachedToAnyParent;
    }

    /** {@code obj_description(indexrelid, 'pg_class')}, or null when the index has no comment. */
    public String getComment() {
        return comment;
    }

    /** True when this index is free-standing: it belongs to no partitioned index at all. */
    public boolean isOrphan() {
        return !attachedToAnyParent;
    }

    @Override
    public String toString() {
        return indexName + "[valid=" + valid + ",attachedToTarget=" + attachedToTargetParent
                + ",attachedToAny=" + attachedToAnyParent + ",comment=" + comment + "]";
    }
}
