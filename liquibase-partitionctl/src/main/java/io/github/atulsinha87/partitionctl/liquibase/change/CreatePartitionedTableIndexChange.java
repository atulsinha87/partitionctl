package io.github.atulsinha87.partitionctl.liquibase.change;

import io.github.atulsinha87.partitionctl.liquibase.catalog.PartitionDiscovery;
import io.github.atulsinha87.partitionctl.liquibase.catalog.TreeState;
import io.github.atulsinha87.partitionctl.liquibase.statement.CreateIndexPlan;
import io.github.atulsinha87.partitionctl.liquibase.statement.StatementBuilder;
import io.github.atulsinha87.partitionctl.liquibase.statement.Statements;

import liquibase.change.AbstractChange;
import liquibase.change.ChangeMetaData;
import liquibase.change.ChangeWithColumns;
import liquibase.change.ColumnConfig;
import liquibase.change.DatabaseChange;
import liquibase.changelog.ChangeSet;
import liquibase.database.Database;
import liquibase.database.core.PostgresDatabase;
import liquibase.exception.ValidationErrors;
import liquibase.statement.SqlStatement;

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
 *
 * <h2>Limitation: the shape is honoured at build time only</h2>
 * {@code unique}, {@code using}, {@code where} and the column list describe how to <em>build</em>
 * an index that is not there. Coverage is decided from {@code pg_inherits} — is this leaf
 * attached to this parent index — and never from the index's definition, because that is what
 * makes a partition PostgreSQL indexed itself count as covered. The consequence, measured: point
 * a second changeset at an index that already exists with a different {@code where}, and it emits
 * <b>zero</b> statements and leaves the original predicate in place. Liquibase's own checksum
 * catches an <em>edited</em> changeset; it cannot catch a new one aimed at the same index.
 * Changing the shape of an index that exists means dropping it and building it again.
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
    private String lockTimeout = "5s";
    private String attachLockTimeout = "5s";
    private Integer paceSeconds;
    private Boolean unique;
    private String using;
    private String where;
    private List<ColumnConfig> columns = new ArrayList<ColumnConfig>();

    /** {@code using} must be a bare identifier; it becomes a quoted identifier in the SQL. */
    private static final java.util.regex.Pattern IDENTIFIER =
            java.util.regex.Pattern.compile("[A-Za-z_][A-Za-z0-9_$]*");

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

    /**
     * {@code CREATE UNIQUE INDEX}. Default false.
     *
     * <p>PostgreSQL requires a unique index on a partitioned table to include every partitioning
     * column, and says so precisely when it does not — "unique constraint on partitioned table
     * must include all partitioning columns", naming the missing column. That check is left to
     * the server rather than duplicated here, where it would need the partition key.
     */
    public Boolean getUnique() {
        return unique;
    }

    public void setUnique(Boolean unique) {
        this.unique = unique;
    }

    /**
     * Index access method — {@code btree} (PostgreSQL's default), {@code gin}, {@code gist},
     * {@code brin}, {@code hash}, {@code spgist}, or one an extension provides.
     *
     * <p>Emitted as a quoted identifier, so it is matched case-sensitively against {@code pg_am}.
     * Measured on 17.10: {@code USING "btree"} works and {@code USING "BTREE"} fails with
     * "access method \"BTREE\" does not exist". Every access method PostgreSQL ships is
     * lowercase; write it lowercase.
     */
    public String getUsing() {
        return using;
    }

    public void setUsing(String using) {
        this.using = using;
    }

    /**
     * Partial-index predicate, without the {@code WHERE} keyword — for example
     * {@code status &lt;&gt; 'archived'}.
     *
     * <p><b>Raw SQL, not escaped, and it cannot be.</b> A predicate is an arbitrary expression,
     * so there is no parameter to bind it to. It is concatenated verbatim into the
     * {@code CREATE INDEX} text, exactly as the body of a Liquibase {@code <sql>} tag is. Anyone
     * who can edit the changelog can already run arbitrary SQL through Liquibase, so this opens
     * no new door — but a changelog generated by string concatenation from untrusted input would
     * carry that injection straight through, and nothing in this extension would catch it.
     */
    public String getWhere() {
        return where;
    }

    public void setWhere(String where) {
        this.where = where;
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
     * Refuses an attribute this element does not define, before Liquibase binds anything.
     *
     * <p>See {@link StrictAttributes}. Without it a misspelled attribute is dropped in total
     * silence whenever the adopter has not listed partitionctl.xsd in their
     * {@code xsi:schemaLocation} — measured, and {@code unique="true"} misspelled as
     * {@code uniq="true"} then ships a NON-UNIQUE index reported as success.
     */
    @Override
    public void load(liquibase.parser.core.ParsedNode parsedNode,
                     liquibase.resource.ResourceAccessor resourceAccessor)
            throws liquibase.parser.core.ParsedNodeException {
        StrictAttributes.rejectUnknown(parsedNode, "createPartitionedTableIndex",
                getSerializableFields(), "column");
        super.load(parsedNode, resourceAccessor);
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
        if (using != null && !IDENTIFIER.matcher(using.trim()).matches()) {
            // Not a substitute for escaping -- the value is quoted, so it cannot break out
            // whatever it contains. This refuses the nonsense case up front, with a message
            // that names the attribute, instead of letting PostgreSQL report a syntax error
            // halfway through a partition tree.
            errors.addError("createPartitionedTableIndex: using=\"" + using + "\" is not an index "
                    + "access method name. Expected a bare identifier such as btree, gin, gist, "
                    + "brin, hash or spgist. It is matched case-sensitively against pg_am, so "
                    + "write it lowercase.");
        }
        if (where != null && where.trim().isEmpty()) {
            errors.addError("createPartitionedTableIndex: where must not be empty. Omit the "
                    + "attribute entirely for a full index.");
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

        CreateIndexPlan plan = new CreateIndexPlan()
                .setSchemaName(schemaName)
                .setTableName(tableName)
                .setIndexName(indexName)
                .setLockTimeout(lockTimeout)
                .setAttachLockTimeout(attachLockTimeout)
                .setPaceSeconds(paceSeconds)
                .setUnique(unique)
                .setUsing(using)
                .setWhere(where)
                .setChangeSetId(changeSetId());
        for (ColumnConfig column : columns) {
            plan.addColumn(column.getName(), Boolean.TRUE.equals(column.getDescending()));
        }

        return Statements.toSqlStatements(StatementBuilder.build(plan, state));
    }

    /**
     * {@code path::id::author}, recorded in the ownership marker so a human reading {@code \d+}
     * can trace an index back to the changelog that built it. Null when the change is used
     * outside a changeset, which only happens in tests.
     */
    private String changeSetId() {
        ChangeSet changeSet = getChangeSet();
        if (changeSet == null) {
            return null;
        }
        return changeSet.getFilePath() + "::" + changeSet.getId() + "::" + changeSet.getAuthor();
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
