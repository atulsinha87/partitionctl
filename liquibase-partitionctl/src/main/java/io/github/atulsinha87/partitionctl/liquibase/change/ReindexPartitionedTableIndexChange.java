package io.github.atulsinha87.partitionctl.liquibase.change;

import io.github.atulsinha87.partitionctl.liquibase.catalog.PartitionDiscovery;
import io.github.atulsinha87.partitionctl.liquibase.catalog.TreeState;
import io.github.atulsinha87.partitionctl.liquibase.statement.ReindexIndexPlan;
import io.github.atulsinha87.partitionctl.liquibase.statement.ReindexStatementBuilder;
import io.github.atulsinha87.partitionctl.liquibase.statement.Statements;

import liquibase.change.AbstractChange;
import liquibase.change.ChangeMetaData;
import liquibase.change.DatabaseChange;
import liquibase.database.Database;
import liquibase.database.core.PostgresDatabase;
import liquibase.exception.ValidationErrors;
import liquibase.statement.SqlStatement;


/**
 * {@code <ext:reindexPartitionedTableIndex>} — rebuilds every leaf index of an existing
 * partitioned index without blocking reads or writes.
 *
 * <pre>{@code
 * <changeSet author="x" id="2" runInTransaction="false" runAlways="true">
 *   <ext:reindexPartitionedTableIndex indexName="idx_personaddress"
 *       schemaName="public" tableName="person"/>
 * </changeSet>
 * }</pre>
 *
 * <h2>Why this is not just one statement</h2>
 * It could be: {@code REINDEX INDEX CONCURRENTLY} does work on a partitioned index, on 14.23 and
 * 17.10, and PostgreSQL loops the partitions itself. The reasons for driving the leaves instead
 * are set out in full — including the ones that turned out not to hold — in
 * {@link ReindexStatementBuilder}. The short version: it buys pacing, per-leaf leftover cleanup
 * that PostgreSQL never does for you, a readable plan, and the single case where the catalog can
 * actually prove a leaf is already fresh. It does <b>not</b> buy general resume, and this class
 * does not pretend otherwise.
 *
 * <h2>runInTransaction="false" is mandatory</h2>
 * {@code REINDEX INDEX CONCURRENTLY} and {@code DROP INDEX CONCURRENTLY} both refuse to run
 * inside a transaction block, and partial progress has to survive a failure. {@link #validate}
 * refuses without it.
 *
 * <h2>runAlways="true" is the recommended default</h2>
 * Reindexing is maintenance, not a one-off migration: a changeset that succeeded is never re-run,
 * so without it this would rebuild the index once, ever.
 */
@DatabaseChange(
        name = "reindexPartitionedTableIndex",
        description = "Rebuilds every leaf index of a partitioned PostgreSQL index without "
                + "blocking reads or writes, using REINDEX INDEX CONCURRENTLY per leaf and "
                + "cleaning up the _ccnew / _ccold leftovers an interrupted run leaves behind",
        priority = ChangeMetaData.PRIORITY_DEFAULT)
public class ReindexPartitionedTableIndexChange extends AbstractChange {

    public static final String NAMESPACE =
            "http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl";

    // Every field here MUST have an identically named attribute in partitionctl.xsd.
    // A mismatch binds to null silently, with no error. ReindexXsdBindingTest enforces it.
    private String schemaName;
    private String tableName;
    private String indexName;
    private String lockTimeout = "15min";
    private Integer paceSeconds;

    private transient PartitionDiscovery discovery;

    public String getSchemaName() {
        return schemaName;
    }

    public void setSchemaName(String schemaName) {
        this.schemaName = schemaName;
    }

    public String getTableName() {
        return tableName;
    }

    public void setTableName(String tableName) {
        this.tableName = tableName;
    }

    public String getIndexName() {
        return indexName;
    }

    public void setIndexName(String indexName) {
        this.indexName = indexName;
    }

    /**
     * {@code lock_timeout} for the concurrent rebuilds and for dropping leftovers. Default
     * {@code 15min}. There is no second, shorter timeout: reindex emits no
     * {@code ALTER INDEX … ATTACH PARTITION}, so nothing it runs takes an
     * {@code AccessExclusiveLock}.
     */
    public String getLockTimeout() {
        return lockTimeout;
    }

    public void setLockTimeout(String lockTimeout) {
        this.lockTimeout = lockTimeout;
    }

    /** Seconds to sleep after each leaf that was actually rebuilt. Default none. */
    public Integer getPaceSeconds() {
        return paceSeconds;
    }

    public void setPaceSeconds(Integer paceSeconds) {
        this.paceSeconds = paceSeconds;
    }

    /** Required, or the change will not compile against AbstractChange's serialization contract. */
    @Override
    public String getSerializedObjectNamespace() {
        return NAMESPACE;
    }

    @Override
    public boolean supports(Database database) {
        return database instanceof PostgresDatabase;
    }

    @Override
    public boolean supportsRollback(Database database) {
        return false;
    }

    /** The statement list is a function of live catalog state, so it is never cacheable. */
    @Override
    public boolean generateStatementsVolatile(Database database) {
        return true;
    }

    @Override
    public ValidationErrors validate(Database database) {
        ValidationErrors errors = new ValidationErrors();

        if (isBlank(schemaName)) {
            errors.addError("reindexPartitionedTableIndex: schemaName is required");
        }
        if (isBlank(tableName)) {
            errors.addError("reindexPartitionedTableIndex: tableName is required");
        }
        if (isBlank(indexName)) {
            errors.addError("reindexPartitionedTableIndex: indexName is required");
        }
        if (isBlank(lockTimeout)) {
            errors.addError("reindexPartitionedTableIndex: lockTimeout must not be empty");
        }
        if (paceSeconds != null && paceSeconds < 0) {
            errors.addError("reindexPartitionedTableIndex: paceSeconds must not be negative");
        }
        if (database != null && !(database instanceof PostgresDatabase)) {
            errors.addError("reindexPartitionedTableIndex supports PostgreSQL only, but the target "
                    + "database is " + database.getShortName()
                    + " (" + database.getClass().getSimpleName() + ")");
        }
        if (getChangeSet() != null && getChangeSet().isRunInTransaction()) {
            errors.addError("reindexPartitionedTableIndex requires runInTransaction=\"false\" on the "
                    + "changeSet (REINDEX INDEX CONCURRENTLY and DROP INDEX CONCURRENTLY cannot run "
                    + "inside a transaction block, and partial progress must survive a failure)");
        }
        return errors;
    }

    /** See {@link StrictAttributes} -- a misspelled attribute otherwise binds to null in silence. */
    @Override
    public void load(liquibase.parser.core.ParsedNode parsedNode,
                     liquibase.resource.ResourceAccessor resourceAccessor)
            throws liquibase.parser.core.ParsedNodeException {
        StrictAttributes.rejectUnknown(parsedNode, "reindexPartitionedTableIndex",
                getSerializableFields());
        super.load(parsedNode, resourceAccessor);
    }

    @Override
    public SqlStatement[] generateStatements(Database database) {
        if (discovery == null) {
            discovery = new PartitionDiscovery(database);
        }
        TreeState state = discovery.inspectExisting(schemaName, tableName, indexName);

        if (!ReindexStatementBuilder.readyToReindex(state)) {
            // Never let an unusable answer become the memoised one. "The index does not exist"
            // or "three leaves are uncovered" may only mean that an earlier
            // <createPartitionedTableIndex> in the same changelog has not run yet, and a change
            // that memoised that answer would emit no work, hit a gate that by then passes, and
            // report success having reindexed nothing. Dropping the PartitionDiscovery drops its
            // cache, so a later call re-queries.
            //
            // Measured, and worth being straight about: this is defence, not a fix for something
            // observed. generateStatements was measured being called
            // seven times per update, four of them before the changelog lock. On 4.33.0 through
            // the maven plugin this change is called exactly TWICE, both inside its own
            // "Running Changeset" block and therefore after every earlier changeset has executed
            // -- create-then-reindex in one changelog was verified to emit all six rebuilds with
            // this guard removed. The difference is almost certainly generateStatementsVolatile()
            // returning true, which lets Liquibase skip the checksum and preview generations. That
            // is an implementation detail, not a contract, and the failure it would cause is a
            // silent no-op recorded as success, so one line and one extra run of one query is a
            // price worth paying.
            discovery = null;
        }

        ReindexIndexPlan plan = new ReindexIndexPlan()
                .setSchemaName(schemaName)
                .setTableName(tableName)
                .setIndexName(indexName)
                .setLockTimeout(lockTimeout)
                .setPaceSeconds(paceSeconds);

        return Statements.toSqlStatements(ReindexStatementBuilder.build(plan, state));
    }

    @Override
    public String getConfirmationMessage() {
        return "reindexPartitionedTableIndex: " + indexName + " over every partition of "
                + schemaName + "." + tableName;
    }

    private static boolean isBlank(String value) {
        return value == null || value.trim().isEmpty();
    }
}
