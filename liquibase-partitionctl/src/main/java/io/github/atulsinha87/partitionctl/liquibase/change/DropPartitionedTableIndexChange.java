package io.github.atulsinha87.partitionctl.liquibase.change;

import io.github.atulsinha87.partitionctl.liquibase.catalog.DropTarget;
import io.github.atulsinha87.partitionctl.liquibase.catalog.DropTargetDiscovery;
import io.github.atulsinha87.partitionctl.liquibase.statement.DropIndexPlan;
import io.github.atulsinha87.partitionctl.liquibase.statement.DropStatementBuilder;
import io.github.atulsinha87.partitionctl.liquibase.statement.Statements;

import liquibase.change.AbstractChange;
import liquibase.change.ChangeMetaData;
import liquibase.change.DatabaseChange;
import liquibase.database.Database;
import liquibase.database.core.PostgresDatabase;
import liquibase.exception.ValidationErrors;
import liquibase.statement.SqlStatement;


/**
 * {@code <ext:dropPartitionedTableIndex>} — removes a partitioned index built by
 * {@code <createPartitionedTableIndex>}, together with every child index attached to it and any
 * free-standing leftover an interrupted build left behind.
 *
 * <pre>{@code
 * <changeSet author="x" id="2" runInTransaction="false">
 *   <ext:dropPartitionedTableIndex indexName="idx_personaddress"
 *       schemaName="public" tableName="person" confirmExclusiveLock="true"/>
 * </changeSet>
 * }</pre>
 *
 * <h2>There is no online path, and that is why confirmExclusiveLock exists</h2>
 * Every alternative is rejected by PostgreSQL 17, re-measured on 17.10:
 * {@code DROP INDEX CONCURRENTLY} on a partitioned index is refused outright; an attached child
 * index cannot be dropped on its own ("because index ... requires it"); and no
 * {@code ALTER INDEX ... DETACH PARTITION} exists to peel one off first. What remains is
 * {@code DROP INDEX} on the parent, which takes an AccessExclusiveLock on the parent table
 * <em>and every leaf partition simultaneously</em>. One line of XML naming one index stalls the
 * whole table, and nothing in the changeset shows that, so the acknowledgement is explicit.
 *
 * <p>The check is made against live state, not the attribute alone: a run with only
 * free-standing leftovers to remove is fully online — {@code DROP INDEX CONCURRENTLY} handles
 * those — and a re-run with nothing left to do takes no locks at all. Neither is made to ask
 * for a confirmation it does not need.
 *
 * <h2>The ownership marker</h2>
 * The change refuses any index that does not carry the {@code COMMENT ON INDEX} marker
 * {@code createPartitionedTableIndex} writes, so a copied changelog or a typo'd
 * {@code indexName} cannot delete an index this plugin never built. It is <b>evidence, not
 * authorisation</b>: a human can write that comment by hand, and anyone who can run this
 * changeset can run {@code DROP INDEX} directly. It defends against the accident, which is how
 * indexes actually get deleted by mistake.
 *
 * <h2>runInTransaction="false" is mandatory; runAlways is not wanted</h2>
 * {@code DROP INDEX CONCURRENTLY} cannot run inside a transaction block, and partial progress
 * has to survive a failure so a re-run resumes rather than restarts. Unlike
 * {@code createPartitionedTableIndex}, this change should <b>not</b> normally carry
 * {@code runAlways="true"}: a drop is a one-shot intent, and re-running it forever would keep
 * re-asserting a deletion long after the changelog has moved on.
 *
 * <h2>No rollback</h2>
 * Rebuilding the tree would take hours and is not something a rollback can honestly offer (L11).
 */
@DatabaseChange(
        name = "dropPartitionedTableIndex",
        description = "Drops a partitioned index and every child index attached to it, plus any "
                + "free-standing leftover from an interrupted build. Requires the partitionctl "
                + "ownership marker and an explicit confirmExclusiveLock acknowledgement",
        priority = ChangeMetaData.PRIORITY_DEFAULT)
public class DropPartitionedTableIndexChange extends AbstractChange {

    public static final String NAMESPACE =
            "http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl";

    // Every field here MUST have an identically named attribute in partitionctl.xsd.
    // A mismatch binds to null silently, with no error. DropXsdBindingTest enforces it.
    private String schemaName;
    private String tableName;
    private String indexName;
    private Boolean confirmExclusiveLock;
    private String lockTimeout = "5s";
    private String exclusiveLockTimeout = "5s";
    private Integer exclusiveRetries = 5;
    private String exclusiveTotalTimeout = "5min";

    private transient DropTargetDiscovery discovery;

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
     * Acknowledges that dropping the attached tree locks the parent table and every leaf
     * exclusively. Required before an attached tree is dropped, and only then.
     */
    public Boolean getConfirmExclusiveLock() {
        return confirmExclusiveLock;
    }

    public void setConfirmExclusiveLock(Boolean confirmExclusiveLock) {
        this.confirmExclusiveLock = confirmExclusiveLock;
    }

    /** {@code lock_timeout} for the concurrent drops of free-standing leftovers. Default 15min. */
    public String getLockTimeout() {
        return lockTimeout;
    }

    public void setLockTimeout(String lockTimeout) {
        this.lockTimeout = lockTimeout;
    }

    /** {@code lock_timeout} for the exclusive {@code DROP INDEX} on the parent. Default 5s. */
    public String getExclusiveLockTimeout() {
        return exclusiveLockTimeout;
    }

    public void setExclusiveLockTimeout(String exclusiveLockTimeout) {
        this.exclusiveLockTimeout = exclusiveLockTimeout;
    }

    /** Attempts at the exclusive drop before giving up, with doubling backoff. Default 5. */
    public Integer getExclusiveRetries() {
        return exclusiveRetries;
    }

    /**
     * Hard ceiling on the whole exclusive drop, every attempt and backoff together, applied as
     * {@code statement_timeout}. Default {@code 5min}. It is the only bound that is not per-lock:
     * {@code exclusiveLockTimeout} caps one lock acquisition, and the drop takes one per leaf plus
     * two, holding each while it waits for the next. Hitting this cancels the statement, which
     * rolls the retry block back completely -- nothing is dropped and re-running retries.
     */
    public String getExclusiveTotalTimeout() {
        return exclusiveTotalTimeout;
    }

    public void setExclusiveTotalTimeout(String exclusiveTotalTimeout) {
        this.exclusiveTotalTimeout = exclusiveTotalTimeout;
    }

    public void setExclusiveRetries(Integer exclusiveRetries) {
        this.exclusiveRetries = exclusiveRetries;
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
            errors.addError("dropPartitionedTableIndex: schemaName is required");
        }
        if (isBlank(tableName)) {
            errors.addError("dropPartitionedTableIndex: tableName is required");
        }
        if (isBlank(indexName)) {
            errors.addError("dropPartitionedTableIndex: indexName is required");
        }
        if (isBlank(lockTimeout)) {
            errors.addError("dropPartitionedTableIndex: lockTimeout must not be empty");
        }
        if (isBlank(exclusiveTotalTimeout)) {
            errors.addError("dropPartitionedTableIndex: exclusiveTotalTimeout must not be empty");
        }
        if (isBlank(exclusiveLockTimeout)) {
            errors.addError("dropPartitionedTableIndex: exclusiveLockTimeout must not be empty");
        }
        if (exclusiveRetries == null || exclusiveRetries < 1) {
            errors.addError("dropPartitionedTableIndex: exclusiveRetries must be at least 1 "
                    + "(it counts attempts, not retries after the first)");
        }
        if (database != null && !(database instanceof PostgresDatabase)) {
            errors.addError("dropPartitionedTableIndex supports PostgreSQL only, but the target "
                    + "database is " + database.getShortName()
                    + " (" + database.getClass().getSimpleName() + ")");
        }
        if (getChangeSet() != null && getChangeSet().isRunInTransaction()) {
            errors.addError("dropPartitionedTableIndex requires runInTransaction=\"false\" on the "
                    + "changeSet (DROP INDEX CONCURRENTLY, used for free-standing leftovers, "
                    + "cannot run inside a transaction block, and partial progress must survive a "
                    + "failure for a re-run to resume rather than restart)");
        }
        return errors;
    }

    /** See {@link StrictAttributes} -- a misspelled attribute otherwise binds to null in silence. */
    @Override
    public void load(liquibase.parser.core.ParsedNode parsedNode,
                     liquibase.resource.ResourceAccessor resourceAccessor)
            throws liquibase.parser.core.ParsedNodeException {
        StrictAttributes.rejectUnknown(parsedNode, "dropPartitionedTableIndex",
                getSerializableFields());
        super.load(parsedNode, resourceAccessor);
    }

    @Override
    public SqlStatement[] generateStatements(Database database) {
        if (discovery == null) {
            discovery = new DropTargetDiscovery(database);
        }
        DropTarget target = discovery.inspect(schemaName, tableName, indexName);

        DropIndexPlan plan = new DropIndexPlan()
                .setSchemaName(schemaName)
                .setTableName(tableName)
                .setIndexName(indexName)
                .setConfirmExclusiveLock(Boolean.TRUE.equals(confirmExclusiveLock))
                .setLockTimeout(lockTimeout)
                .setExclusiveLockTimeout(exclusiveLockTimeout)
                .setExclusiveRetries(exclusiveRetries == null ? 5 : exclusiveRetries)
                .setExclusiveTotalTimeout(exclusiveTotalTimeout);

        return Statements.toSqlStatements(DropStatementBuilder.build(plan, target));
    }

    @Override
    public String getConfirmationMessage() {
        return "dropPartitionedTableIndex: " + indexName + " and every child index attached to it "
                + "removed from " + schemaName + "." + tableName;
    }

    private static boolean isBlank(String value) {
        return value == null || value.trim().isEmpty();
    }
}
