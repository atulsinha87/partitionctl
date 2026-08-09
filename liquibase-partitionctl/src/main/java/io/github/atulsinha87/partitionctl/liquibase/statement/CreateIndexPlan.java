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

    /**
     * {@code CREATE UNIQUE INDEX}. Null or false builds an ordinary index.
     *
     * <p>PostgreSQL requires a unique index on a partitioned table to contain every partitioning
     * column — measured on 17.10: {@code CREATE UNIQUE INDEX ... ON ONLY p (addr)} where the
     * table is partitioned by {@code id} fails with "unique constraint on partitioned table must
     * include all partitioning columns". That message names the missing column, so it is not
     * re-checked here.
     */
    private Boolean unique;

    /** Index access method: {@code btree} (PostgreSQL's default), {@code gin}, {@code brin}, … */
    private String using;

    /** Partial index predicate. Raw SQL — see {@link #getWhere()}. */
    private String where;

    /**
     * Liquibase's {@code path::id::author} for the running changeset, recorded in the ownership
     * marker so a human reading {@code \d+} can find the changelog that built the index.
     */
    private String changeSetId;

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

    public Boolean getUnique() {
        return unique;
    }

    public boolean isUnique() {
        return Boolean.TRUE.equals(unique);
    }

    public CreateIndexPlan setUnique(Boolean unique) {
        this.unique = unique;
        return this;
    }

    /**
     * The access method, or null for PostgreSQL's default.
     *
     * <p>Emitted as a quoted identifier, which makes it case-sensitive against {@code pg_am}.
     * Measured on 17.10: {@code USING "btree"} works, {@code USING "BTREE"} fails with
     * "access method \"BTREE\" does not exist". Every access method PostgreSQL ships is
     * lowercase, so write it lowercase.
     */
    public String getUsing() {
        return using;
    }

    public CreateIndexPlan setUsing(String using) {
        this.using = using;
        return this;
    }

    /**
     * The partial-index predicate, or null.
     *
     * <h2>This is raw SQL and is not escaped</h2>
     * A predicate is an arbitrary SQL expression — {@code status <> 'done'},
     * {@code created_at >= now() - interval '30 days'} — so there is no parameter to bind it to
     * and no quoting that would leave it meaning what the author wrote. It is concatenated into
     * the {@code CREATE INDEX} text verbatim, exactly as an author's SQL in a Liquibase
     * {@code <sql>} tag is. Whoever can edit the changelog can already run arbitrary SQL through
     * Liquibase, so this widens no boundary — but it does mean a changelog built by string
     * concatenation from untrusted input would carry that injection straight through, and
     * nothing here would catch it.
     *
     * <p>It is only ever placed at the end of a {@code CREATE INDEX} statement, never inside a
     * quoted literal and never inside a {@code --} comment label, so it cannot break out of a
     * context into one it was not written for.
     */
    public String getWhere() {
        return where;
    }

    public CreateIndexPlan setWhere(String where) {
        this.where = where;
        return this;
    }

    public String getChangeSetId() {
        return changeSetId;
    }

    public CreateIndexPlan setChangeSetId(String changeSetId) {
        this.changeSetId = changeSetId;
        return this;
    }
}
