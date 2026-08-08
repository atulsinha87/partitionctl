package io.github.atulsinha87.partitionctl.liquibase.statement;

import java.util.ArrayList;
import java.util.List;

/** The request half of statement planning: what the changelog asked for. */
public final class CreateIndexPlan {

    private String schemaName;
    private String tableName;
    private String indexName;
    private List<IndexColumn> columns = new ArrayList<IndexColumn>();

    /**
     * {@code lock_timeout} for CREATE/DROP/REINDEX INDEX CONCURRENTLY. These take
     * ShareUpdateExclusiveLock, which is compatible with reads and writes and queues nothing
     * harmful behind it, so waiting is nearly free — and the work being protected can be hours.
     */
    private String lockTimeout = "15min";

    /**
     * {@code lock_timeout} for ALTER INDEX ... ATTACH PARTITION. AccessExclusiveLock on the
     * child index. A queued exclusive request blocks everything behind it — a plain SELECT
     * conflicting with nothing was measured waiting 8 seconds behind one — so this is short.
     */
    private String attachLockTimeout = "30s";

    /** {@code SELECT pg_sleep(n)} between leaves. Null or 0 disables it. */
    private Integer paceSeconds;

    public String getSchemaName() {
        return schemaName;
    }

    public CreateIndexPlan setSchemaName(String schemaName) {
        this.schemaName = schemaName;
        return this;
    }

    public String getTableName() {
        return tableName;
    }

    public CreateIndexPlan setTableName(String tableName) {
        this.tableName = tableName;
        return this;
    }

    public String getIndexName() {
        return indexName;
    }

    public CreateIndexPlan setIndexName(String indexName) {
        this.indexName = indexName;
        return this;
    }

    public List<IndexColumn> getColumns() {
        return columns;
    }

    public CreateIndexPlan setColumns(List<IndexColumn> columns) {
        this.columns = columns;
        return this;
    }

    public CreateIndexPlan addColumn(String name, boolean descending) {
        this.columns.add(new IndexColumn(name, descending));
        return this;
    }

    public String getLockTimeout() {
        return lockTimeout;
    }

    public CreateIndexPlan setLockTimeout(String lockTimeout) {
        this.lockTimeout = lockTimeout;
        return this;
    }

    public String getAttachLockTimeout() {
        return attachLockTimeout;
    }

    public CreateIndexPlan setAttachLockTimeout(String attachLockTimeout) {
        this.attachLockTimeout = attachLockTimeout;
        return this;
    }

    public Integer getPaceSeconds() {
        return paceSeconds;
    }

    public CreateIndexPlan setPaceSeconds(Integer paceSeconds) {
        this.paceSeconds = paceSeconds;
        return this;
    }
}
