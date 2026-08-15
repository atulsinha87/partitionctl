package io.github.atulsinha87.partitionctl.liquibase.statement;

/** The request half of reindex planning: what the changelog asked for. */
public final class ReindexIndexPlan {

    private String schemaName;
    private String tableName;
    private String indexName;

    /**
     * {@code lock_timeout} for {@code REINDEX INDEX CONCURRENTLY} and for the
     * {@code DROP INDEX CONCURRENTLY} of a leftover. Both take {@code ShareUpdateExclusiveLock},
     * which is compatible with reads and writes and queues nothing harmful behind it, so waiting
     * is nearly free — and the work being protected can be hours.
     *
     * <p>There is deliberately no second, shorter timeout here. Reindex emits no
     * {@code ALTER INDEX … ATTACH PARTITION}: {@code REINDEX INDEX CONCURRENTLY} swaps the index
     * in place and the {@code pg_inherits} attachment survives, measured on 14.23 and 17.10. So
     * nothing this operation emits ever asks for an {@code AccessExclusiveLock}.
     */
    private String lockTimeout = "5s";

    /** {@code SELECT pg_sleep(n)} after each leaf that was actually rebuilt. Null or 0 disables it. */
    private Integer paceSeconds;

    public String getSchemaName() {
        return schemaName;
    }

    public ReindexIndexPlan setSchemaName(String schemaName) {
        this.schemaName = schemaName;
        return this;
    }

    public String getTableName() {
        return tableName;
    }

    public ReindexIndexPlan setTableName(String tableName) {
        this.tableName = tableName;
        return this;
    }

    public String getIndexName() {
        return indexName;
    }

    public ReindexIndexPlan setIndexName(String indexName) {
        this.indexName = indexName;
        return this;
    }

    public String getLockTimeout() {
        return lockTimeout;
    }

    public ReindexIndexPlan setLockTimeout(String lockTimeout) {
        this.lockTimeout = lockTimeout;
        return this;
    }

    public Integer getPaceSeconds() {
        return paceSeconds;
    }

    public ReindexIndexPlan setPaceSeconds(Integer paceSeconds) {
        this.paceSeconds = paceSeconds;
        return this;
    }
}
