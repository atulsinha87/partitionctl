package io.github.atulsinha87.partitionctl.liquibase.catalog;

import java.util.ArrayList;
import java.util.Collections;
import java.util.List;

/** Everything one discovery round trip learned about what a drop would destroy. */
public final class DropTarget {

    private String rootRelkind;
    private String indexRelkind;
    private String indexOwningTable;
    private boolean indexValid;
    private String indexComment;
    private String originalStatementTimeout;
    private String originalLockTimeout;
    private final List<DropLeaf> leaves = new ArrayList<DropLeaf>();

    /**
     * {@code pg_class.relkind} of the target table: {@code 'p'} partitioned, {@code 'r'}
     * ordinary, null when no relation of that name exists in that schema.
     */
    public String getRootRelkind() {
        return rootRelkind;
    }

    public void setRootRelkind(String rootRelkind) {
        this.rootRelkind = rootRelkind;
    }

    /**
     * {@code pg_class.relkind} of whatever relation the changeset's {@code indexName} names:
     * {@code 'I'} a partitioned index (the only thing this change can drop), {@code 'i'} an
     * ordinary index, {@code 'r'}/{@code 'p'} a table that happens to carry that name, null
     * when nothing of that name exists.
     *
     * <p>Deliberately not reduced to a boolean. "There is no partitioned index called that"
     * and "there is an ordinary index called that" need different answers: the first is a
     * no-op, the second is a mistake worth refusing.
     */
    public String getIndexRelkind() {
        return indexRelkind;
    }

    public void setIndexRelkind(String indexRelkind) {
        this.indexRelkind = indexRelkind;
    }

    /**
     * {@code schema.table} the named index is actually built on, from {@code pg_index.indrelid}.
     * Null when the name is not an index at all. Checked against the changeset's own
     * {@code tableName}, because index names are unique per schema but say nothing about which
     * table they belong to — without this, a changeset naming the right index and the wrong
     * table would drop the right index anyway, with no diagnostic.
     */
    public String getIndexOwningTable() {
        return indexOwningTable;
    }

    public void setIndexOwningTable(String indexOwningTable) {
        this.indexOwningTable = indexOwningTable;
    }

    /** {@code pg_index.indisvalid} of the named index. */
    public boolean isIndexValid() {
        return indexValid;
    }

    public void setIndexValid(boolean indexValid) {
        this.indexValid = indexValid;
    }

    /** {@code obj_description} of the named index, or null. The ownership evidence. */
    public String getIndexComment() {
        return indexComment;
    }

    public void setIndexComment(String indexComment) {
        this.indexComment = indexComment;
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

    public List<DropLeaf> getLeaves() {
        return Collections.unmodifiableList(leaves);
    }

    public void addLeaf(DropLeaf leaf) {
        leaves.add(leaf);
    }

    /** True when the changeset's {@code indexName} is a partitioned index — an attached tree. */
    public boolean isPartitionedIndexPresent() {
        return "I".equals(indexRelkind);
    }

    /** Every leaf index that inherits from the named parent. These die with the parent. */
    public List<DropCandidateIndex> attachedChildren() {
        List<DropCandidateIndex> children = new ArrayList<DropCandidateIndex>();
        for (DropLeaf leaf : leaves) {
            DropCandidateIndex child = leaf.getAttachedChild();
            if (child != null) {
                children.add(child);
            }
        }
        return children;
    }

    /** Every leaf carrying a free-standing leftover at this extension's generated name. */
    public List<DropLeaf> leavesWithOrphans() {
        List<DropLeaf> withOrphans = new ArrayList<DropLeaf>();
        for (DropLeaf leaf : leaves) {
            if (leaf.getOrphan() != null) {
                withOrphans.add(leaf);
            }
        }
        return withOrphans;
    }
}
