package io.github.atulsinha87.partitionctl.liquibase.catalog;

/**
 * One index sitting on one leaf partition, as the read-only gates need to see it.
 *
 * <p>Distinct from {@link LeafIndex}, which the {@code Change} path uses, for two reasons that
 * are not stylistic. First, the gates read {@code indisready} and {@code indislive} as well as
 * {@code indisvalid}, and those three answer different questions (see {@link #isUsable()}).
 * Second, {@code LeafIndex} is exercised live by the create, drop and reindex changes; widening
 * its constructor to carry two more flags would edit verified code to serve a read-only gate.
 */
public final class GateIndex {

    private final String indexName;
    private final boolean valid;
    private final boolean ready;
    private final boolean live;
    private final boolean covering;
    private final boolean attachedToAnyParent;

    public GateIndex(String indexName, boolean valid, boolean ready, boolean live,
                     boolean covering, boolean attachedToAnyParent) {
        this.indexName = indexName;
        this.valid = valid;
        this.ready = ready;
        this.live = live;
        this.covering = covering;
        this.attachedToAnyParent = attachedToAnyParent;
    }

    public String getIndexName() {
        return indexName;
    }

    /** {@code pg_index.indisvalid}: false means the planner will not use it. */
    public boolean isValid() {
        return valid;
    }

    /**
     * {@code pg_index.indisready}: false means INSERTs are not maintaining it, so it is not
     * merely stale, it is diverging from the table for as long as it stays that way.
     */
    public boolean isReady() {
        return ready;
    }

    /**
     * {@code pg_index.indislive}: false means a {@code DROP INDEX CONCURRENTLY} is in progress.
     * The index is on its way out and nothing should be built on the assumption it exists.
     */
    public boolean isLive() {
        return live;
    }

    /**
     * True when this index is a descendant of the partitioned index the gate names — at any
     * depth. Read from {@code pg_inherits}, never from the index name.
     */
    public boolean isCovering() {
        return covering;
    }

    /** True when this index inherits from <em>any</em> partitioned index. */
    public boolean isAttachedToAnyParent() {
        return attachedToAnyParent;
    }

    /** Valid, ready and live: all three, which is the only state a gate should pass. */
    public boolean isUsable() {
        return valid && ready && live;
    }

    /** The failing flags, for a message that says which one is wrong rather than "unhealthy". */
    public String describeFlags() {
        StringBuilder text = new StringBuilder();
        appendFlag(text, "indisvalid", valid);
        appendFlag(text, "indisready", ready);
        appendFlag(text, "indislive", live);
        return text.toString();
    }

    private static void appendFlag(StringBuilder text, String name, boolean value) {
        if (text.length() > 0) {
            text.append(", ");
        }
        text.append(name).append('=').append(value);
    }

    @Override
    public String toString() {
        return indexName + "[" + describeFlags() + ", covering=" + covering + "]";
    }
}
