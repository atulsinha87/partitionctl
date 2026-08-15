package io.github.atulsinha87.partitionctl.liquibase.statement;

/** The request half of drop planning: what the changelog asked for. */
public final class DropIndexPlan {

    private String schemaName;
    private String tableName;
    private String indexName;

    /**
     * The adopter's explicit acknowledgement that the whole table is about to be locked.
     * Required before an attached tree is dropped, and only then — see
     * {@link DropStatementBuilder} for why the check is state-dependent rather than a plain
     * required attribute.
     */
    private boolean confirmExclusiveLock;

    /**
     * {@code lock_timeout} for {@code DROP INDEX CONCURRENTLY} on a free-standing leftover.
     * ShareUpdateExclusiveLock: compatible with reads and writes, nothing queues harmfully
     * behind it, so waiting is nearly free. Default {@code 15min}.
     */
    private String lockTimeout = "5s";

    /**
     * {@code lock_timeout} for {@code DROP INDEX} on the partitioned parent. AccessExclusiveLock
     * on the parent table <em>and every leaf</em>, so every second spent waiting stalls the whole
     * table — and a queued exclusive request blocks everything behind it, measured at 8 seconds
     * for a plain SELECT that conflicted with nothing. Short on purpose, with retries.
     * Default {@code 5s}.
     *
     * <p><b>Per lock, not per statement.</b> The drop takes one AccessExclusiveLock per leaf plus
     * two, holding each while it waits for the next, so the waits add: one attempt can hold the
     * table for up to this value × (leaves + 2). Measured on 17.10 with 8 leaves, two contended,
     * and this at its default: {@code DROP INDEX / Time: 6876.232 ms}. The ceiling on the whole
     * operation is {@link #getExclusiveTotalTimeout()}.
     */
    private String exclusiveLockTimeout = "5s";

    /** Attempts at the exclusive drop before giving up. Default 5. */
    private int exclusiveRetries = 5;

    /**
     * Hard ceiling on the entire exclusive drop — every attempt and every backoff together —
     * applied as {@code statement_timeout} around the retry block. Default {@code 5min}.
     *
     * <p>This is the only bound that is not per-lock, and therefore the only one that answers
     * "how long can this stall my table". Reaching it cancels the statement; because the retry
     * block is one transaction, the cancel rolls it back completely. Measured: after such a cancel
     * the parent index and all its children were still present and the session held no locks. The
     * changeset then fails, nothing is written to DATABASECHANGELOG, and re-running retries — so
     * hitting this ceiling costs a failed deploy, never a half-dropped index.
     *
     * <p>Generous by default because the uncontended cost is nowhere near it: the same 8-partition
     * drop with nothing blocking took {@code Time: 21.561 ms}. Raise it for a table with hundreds
     * of partitions and heavy traffic; lower it if a deploy must never hold the table long.
     */
    private String exclusiveTotalTimeout = "5min";

    public String getSchemaName() {
        return schemaName;
    }

    public DropIndexPlan setSchemaName(String schemaName) {
        this.schemaName = schemaName;
        return this;
    }

    public String getTableName() {
        return tableName;
    }

    public DropIndexPlan setTableName(String tableName) {
        this.tableName = tableName;
        return this;
    }

    public String getIndexName() {
        return indexName;
    }

    public DropIndexPlan setIndexName(String indexName) {
        this.indexName = indexName;
        return this;
    }

    public boolean isConfirmExclusiveLock() {
        return confirmExclusiveLock;
    }

    public DropIndexPlan setConfirmExclusiveLock(boolean confirmExclusiveLock) {
        this.confirmExclusiveLock = confirmExclusiveLock;
        return this;
    }

    public String getLockTimeout() {
        return lockTimeout;
    }

    public DropIndexPlan setLockTimeout(String lockTimeout) {
        this.lockTimeout = lockTimeout;
        return this;
    }

    public String getExclusiveLockTimeout() {
        return exclusiveLockTimeout;
    }

    public DropIndexPlan setExclusiveLockTimeout(String exclusiveLockTimeout) {
        this.exclusiveLockTimeout = exclusiveLockTimeout;
        return this;
    }

    public String getExclusiveTotalTimeout() {
        return exclusiveTotalTimeout;
    }

    public DropIndexPlan setExclusiveTotalTimeout(String exclusiveTotalTimeout) {
        this.exclusiveTotalTimeout = exclusiveTotalTimeout;
        return this;
    }

    public int getExclusiveRetries() {
        return exclusiveRetries;
    }

    public DropIndexPlan setExclusiveRetries(int exclusiveRetries) {
        this.exclusiveRetries = exclusiveRetries;
        return this;
    }
}
