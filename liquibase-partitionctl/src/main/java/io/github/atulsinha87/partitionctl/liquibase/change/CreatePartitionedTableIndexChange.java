package io.github.atulsinha87.partitionctl.liquibase.change;

import io.github.atulsinha87.partitionctl.liquibase.catalog.PartitionDiscovery;
import io.github.atulsinha87.partitionctl.liquibase.catalog.TreeState;
import io.github.atulsinha87.partitionctl.liquibase.statement.CreateIndexPlan;
import io.github.atulsinha87.partitionctl.liquibase.statement.PlannedStatement;
import io.github.atulsinha87.partitionctl.liquibase.statement.StatementBuilder;

import liquibase.Scope;
import liquibase.change.AbstractChange;
import liquibase.change.ChangeMetaData;
import liquibase.change.ChangeWithColumns;
import liquibase.change.ColumnConfig;
import liquibase.change.DatabaseChange;
import liquibase.database.Database;
import liquibase.database.core.PostgresDatabase;
import liquibase.exception.ValidationErrors;
import liquibase.statement.SqlStatement;
import liquibase.statement.core.RawSqlStatement;

import java.util.ArrayList;
import java.util.List;

/**
 * {@code <ext:createPartitionedTableIndex>} — builds an index across every leaf partition of
 * a partitioned PostgreSQL table without blocking writes.
 *
 * <pre>{@code
 * <changeSet author="x" id="1" runInTransaction="false">
 *   <ext:createPartitionedTableIndex indexName="idx_personaddress"
 *       schemaName="public" tableName="person">
 *     <column descending="true" name="address"/>
 *   </ext:createPartitionedTableIndex>
 * </changeSet>
 * }</pre>
 *
 * <h2>Why this cannot be Liquibase's own createIndex</h2>
 * {@code CREATE INDEX CONCURRENTLY} is rejected outright on a partitioned parent. The only
 * non-blocking route is: create the parent {@code ON ONLY} (deliberately invalid), build one
 * index concurrently per leaf, attach each one — and on the final attach PostgreSQL validates
 * the parent by itself. There is no explicit validation statement, and the statement list is
 * a function of the live catalog, so it cannot be authored by hand.
 *
 * <h2>runInTransaction="false" is mandatory</h2>
 * One flag, two properties: {@code CREATE INDEX CONCURRENTLY} cannot run inside a transaction
 * block, and partial progress has to survive a failure for resume to work. {@link #validate}
 * refuses without it.
 *
 * <h2>runAlways="true" is the recommended default</h2>
 * A changeset that succeeded is never re-run, so partitions created afterwards would silently
 * go unindexed. With {@code runAlways="true"} discovery runs on every deploy and emits nothing
 * when there is nothing to do.
 */
@DatabaseChange(
        name = "createPartitionedTableIndex",
        description = "Creates an index across every partition of a partitioned PostgreSQL table "
                + "without blocking writes, using CREATE INDEX CONCURRENTLY per leaf plus "
                + "ALTER INDEX ... ATTACH PARTITION",
        priority = ChangeMetaData.PRIORITY_DEFAULT)
public class CreatePartitionedTableIndexChange extends AbstractChange
        implements ChangeWithColumns<ColumnConfig> {

    public static final String NAMESPACE =
            "http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl";

    // Every field here MUST have an identically named attribute in partitionctl.xsd.
    // A mismatch binds to null silently, with no error. XsdBindingTest enforces it.
    private String schemaName;
    private String tableName;
    private String indexName;
    private String lockTimeout = "15min";
    private String attachLockTimeout = "30s";
    private Integer paceSeconds;
    private List<ColumnConfig> columns = new ArrayList<ColumnConfig>();

    private transient PartitionDiscovery discovery;
    private transient boolean parentInvalidWarningEmitted;

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

    /** {@code lock_timeout} for the concurrent index builds. Default {@code 15min}. */
    public String getLockTimeout() {
        return lockTimeout;
    }

    public void setLockTimeout(String lockTimeout) {
        this.lockTimeout = lockTimeout;
    }

    /** {@code lock_timeout} for {@code ALTER INDEX ... ATTACH PARTITION}. Default {@code 30s}. */
    public String getAttachLockTimeout() {
        return attachLockTimeout;
    }

    public void setAttachLockTimeout(String attachLockTimeout) {
        this.attachLockTimeout = attachLockTimeout;
    }

    /** Seconds to sleep between leaves. Default none. */
    public Integer getPaceSeconds() {
        return paceSeconds;
    }

    public void setPaceSeconds(Integer paceSeconds) {
        this.paceSeconds = paceSeconds;
    }

    @Override
    public List<ColumnConfig> getColumns() {
        return columns;
    }

    @Override
    public void setColumns(List<ColumnConfig> columns) {
        this.columns = columns;
    }

    @Override
    public void addColumn(ColumnConfig column) {
        this.columns.add(column);
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

    /**
     * Runs before any DDL. The column check is the one that actually earns its keep: a typo'd
     * {@code <colum name="x"/>} leaves {@code getColumns()} empty, and without this the change
     * would emit {@code CREATE INDEX ... ()} instead of refusing.
     */
    @Override
    public ValidationErrors validate(Database database) {
        ValidationErrors errors = new ValidationErrors();

        if (isBlank(schemaName)) {
            errors.addError("createPartitionedTableIndex: schemaName is required");
        }
        if (isBlank(tableName)) {
            errors.addError("createPartitionedTableIndex: tableName is required");
        }
        if (isBlank(indexName)) {
            errors.addError("createPartitionedTableIndex: indexName is required");
        }
        if (columns == null || columns.isEmpty()) {
            errors.addError("createPartitionedTableIndex: at least one <column> is required. "
                    + "If you did supply one, check the element is spelled <column .../> exactly "
                    + "-- a misspelled element binds to nothing and leaves the column list empty.");
        } else {
            for (int i = 0; i < columns.size(); i++) {
                if (isBlank(columns.get(i).getName())) {
                    errors.addError("createPartitionedTableIndex: <column> " + (i + 1)
                            + " has no name attribute");
                }
            }
        }
        if (isBlank(lockTimeout)) {
            errors.addError("createPartitionedTableIndex: lockTimeout must not be empty");
        }
        if (isBlank(attachLockTimeout)) {
            errors.addError("createPartitionedTableIndex: attachLockTimeout must not be empty");
        }
        if (paceSeconds != null && paceSeconds < 0) {
            errors.addError("createPartitionedTableIndex: paceSeconds must not be negative");
        }
        if (database != null && !(database instanceof PostgresDatabase)) {
            errors.addError("createPartitionedTableIndex supports PostgreSQL only, but the target "
                    + "database is " + database.getShortName()
                    + " (" + database.getClass().getSimpleName() + ")");
        }
        if (getChangeSet() != null && getChangeSet().isRunInTransaction()) {
            errors.addError("createPartitionedTableIndex requires runInTransaction=\"false\" on the "
                    + "changeSet (CREATE INDEX CONCURRENTLY cannot run inside a transaction block, "
                    + "and partial progress must survive a failure for resume to work)");
        }
        return errors;
    }

    @Override
    public SqlStatement[] generateStatements(Database database) {
        if (discovery == null) {
            discovery = new PartitionDiscovery(database);
        }
        TreeState state = discovery.inspect(schemaName, tableName, indexName);

        warnOnceIfParentAlreadyInvalid(state);

        CreateIndexPlan plan = new CreateIndexPlan()
                .setSchemaName(schemaName)
                .setTableName(tableName)
                .setIndexName(indexName)
                .setLockTimeout(lockTimeout)
                .setAttachLockTimeout(attachLockTimeout)
                .setPaceSeconds(paceSeconds);
        for (ColumnConfig column : columns) {
            plan.addColumn(column.getName(), Boolean.TRUE.equals(column.getDescending()));
        }

        List<PlannedStatement> planned = StatementBuilder.build(plan, state);
        SqlStatement[] statements = new SqlStatement[planned.size()];
        for (int i = 0; i < planned.size(); i++) {
            statements[i] = new RawSqlStatement(planned.get(i).toSql());
        }
        return statements;
    }

    /**
     * A parent found already invalid means a previous run was interrupted or a previous buggy
     * run attached an invalid child. The gate at the end of the run reports the final state via
     * RAISE WARNING, which not every host surfaces; this says it up front, once, on the channel
     * that survives a low log level. Discovery is memoised, so it prints exactly once per update.
     */
    private void warnOnceIfParentAlreadyInvalid(TreeState state) {
        if (parentInvalidWarningEmitted || !state.isParentIndexExists() || state.isParentIndexValid()) {
            return;
        }
        parentInvalidWarningEmitted = true;
        Scope.getCurrentScope().getUI().sendMessage("[partitionctl] "
                + schemaName + "." + indexName + " exists but is currently indisvalid = false. "
                + "That is expected mid-build; this run will finish the outstanding leaves. "
                + "If it is still false afterwards, an invalid child index was attached by an "
                + "earlier run and the flag is permanent -- the index remains usable.");
    }

    @Override
    public String getConfirmationMessage() {
        return "createPartitionedTableIndex: " + indexName + " over every partition of "
                + schemaName + "." + tableName;
    }

    private static boolean isBlank(String value) {
        return value == null || value.trim().isEmpty();
    }
}
