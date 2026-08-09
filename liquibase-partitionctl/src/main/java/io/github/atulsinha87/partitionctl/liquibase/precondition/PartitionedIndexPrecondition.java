package io.github.atulsinha87.partitionctl.liquibase.precondition;

import io.github.atulsinha87.partitionctl.liquibase.catalog.GateInspection;
import io.github.atulsinha87.partitionctl.liquibase.catalog.GateSnapshot;

import liquibase.changelog.ChangeSet;
import liquibase.changelog.DatabaseChangeLog;
import liquibase.changelog.visitor.ChangeExecListener;
import liquibase.database.Database;
import liquibase.database.DatabaseConnection;
import liquibase.database.core.PostgresDatabase;
import liquibase.database.jvm.JdbcConnection;
import liquibase.exception.PreconditionErrorException;
import liquibase.exception.PreconditionFailedException;
import liquibase.exception.ValidationErrors;
import liquibase.exception.Warnings;
import liquibase.precondition.AbstractPrecondition;
import liquibase.precondition.FailedPrecondition;

import java.sql.Connection;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;

/**
 * Shared plumbing for the three partitionctl gates.
 *
 * <p>Each gate is read-only: one SELECT against {@code pg_catalog} on the JDBC connection
 * Liquibase is already authenticated on. No second credential, no subprocess, no {@code SET},
 * no DDL.
 *
 * <h2>FAILED versus ERROR, and why the split is not arbitrary</h2>
 * Liquibase routes the two through different policies — {@code onFail} and {@code onError} — and
 * an adopter can set {@code onFail="MARK_RAN"} or {@code "CONTINUE"} to make a gate advisory. So
 * the rule here is:
 *
 * <ul>
 *   <li><b>FAILED</b> — the catalog does not say what the gate asserts. Missing table, missing
 *       index, an unusable leaf. These are the verdicts the adopter's {@code onFail} policy is
 *       for.</li>
 *   <li><b>ERROR</b> — the gate could not form an opinion: a required attribute never reached the
 *       Java, the database is not PostgreSQL, the query threw. Routing these through
 *       {@code onFail} would let {@code onFail="MARK_RAN"} silently swallow a misconfigured
 *       changelog, which is precisely the failure mode a gate exists to prevent.</li>
 * </ul>
 *
 * <h2>The null-attribute guard is not defensive padding</h2>
 * An XSD attribute name that does not match the Java property binds to <b>null</b> silently, with
 * no error of any kind — the sharpest edge in the whole extension mechanism, and measured
 * directly against liquibase-core 4.33.0. Without an explicit guard the gate would run a
 * query for an index literally named {@code null}, find nothing, and report a perfectly ordinary
 * "not found" — a misconfiguration wearing the costume of a real verdict. So a null identifier is
 * raised as an ERROR that names the attribute.
 */
public abstract class PartitionedIndexPrecondition extends AbstractPrecondition {

    public static final String NAMESPACE =
            "http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl";

    /** How many offending leaves a failure message names before it summarises the rest. */
    static final int MAX_NAMED_LEAVES = 5;

    // Every field here MUST have an identically named attribute in partitionctl.xsd.
    //
    // schemaName / tableName / indexName, matching BOTH the changes in this extension and
    // Liquibase's own preconditions -- liquibase.precondition.core.IndexExistsPrecondition
    // declares exactly getSchemaName/getTableName/getIndexName. An earlier draft used the bare
    // schema/table/index and justified it in the XSD as "inherited from the already-published
    // gate"; nothing has ever been published (the whole io.github.atulsinha87 groupId is a 404 on
    // Maven Central), so that justification was false and the asymmetry was pure adopter cost:
    // the obvious carry-over from createPartitionedTableIndex simply failed to parse.
    private String schemaName;
    private String tableName;
    private String indexName;

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

    @Override
    public String getSerializedObjectNamespace() {
        return NAMESPACE;
    }

    @Override
    public Warnings warn(Database database) {
        return new Warnings();
    }

    @Override
    public ValidationErrors validate(Database database) {
        ValidationErrors errors = new ValidationErrors();
        for (String missing : missingAttributes()) {
            errors.addError(getName() + ": '" + missing + "' attribute is required");
        }
        if (database != null && !(database instanceof PostgresDatabase)) {
            errors.addError(getName() + " is PostgreSQL-only; this changelog is running against "
                    + database.getShortName());
        }
        return errors;
    }

    /**
     * Runs the gate. Subclasses do not override this — they answer
     * {@link #evaluate(GateSnapshot, List)} with the reasons the catalog fails their assertion.
     */
    @Override
    public final void check(Database database, DatabaseChangeLog changeLog, ChangeSet changeSet,
                            ChangeExecListener execListener)
            throws PreconditionFailedException, PreconditionErrorException {

        List<String> missing = missingAttributes();
        if (!missing.isEmpty()) {
            throw error(changeLog, getName() + ": the attribute(s) " + missing
                    + " are missing or did not bind. If they are present in the changelog, the "
                    + "XSD attribute name and the Java property name have drifted apart, which "
                    + "binds silently to null.");
        }
        if (!(database instanceof PostgresDatabase)) {
            throw error(changeLog, getName() + " is PostgreSQL-only; this changelog is running "
                    + "against " + (database == null ? "no database" : database.getShortName()));
        }

        GateSnapshot snapshot;
        try {
            DatabaseConnection dbConnection = database.getConnection();
            if (!(dbConnection instanceof JdbcConnection)) {
                throw error(changeLog, getName() + " requires a JdbcConnection, got "
                        + (dbConnection == null ? "none" : dbConnection.getClass().getName()));
            }
            Connection connection = ((JdbcConnection) dbConnection).getUnderlyingConnection();
            snapshot = GateInspection.inspect(connection, schemaName, tableName, indexName);
        } catch (PreconditionErrorException e) {
            throw e;
        } catch (Exception e) {
            throw new PreconditionErrorException(e, changeLog, this);
        }

        List<String> failures = new ArrayList<String>();
        evaluate(snapshot, failures);
        if (!failures.isEmpty()) {
            List<FailedPrecondition> reported = new ArrayList<FailedPrecondition>();
            reported.add(new FailedPrecondition(
                    "partitionctl " + getName() + " on " + qualifiedIndex() + ": "
                            + join(failures, " "), changeLog, this));
            throw new PreconditionFailedException(reported);
        }
    }

    /**
     * Adds one sentence per reason the catalog does not satisfy this gate. An empty list means
     * the gate passes. Called with a snapshot that is already known to have bound attributes and
     * a PostgreSQL connection behind it; everything else is the subclass's to judge.
     */
    protected abstract void evaluate(GateSnapshot snapshot, List<String> failures);

    /**
     * The checks every gate shares: the table has to exist, be partitioned, and have at least one
     * leaf. Returns true when the snapshot is worth inspecting further.
     *
     * <p>The zero-leaf case is a failure rather than a vacuous pass on purpose. "Every leaf is
     * healthy" and "no leftover on any leaf" are both trivially true of a partitioned table with
     * no partitions, and a gate that passes because there was nothing to check protects nothing.
     */
    protected final boolean requireLeafPartitions(GateSnapshot snapshot, List<String> failures) {
        if (!snapshot.tableExists()) {
            failures.add("table " + schemaName + "." + tableName + " does not exist.");
            return false;
        }
        if (!snapshot.tableIsPartitioned()) {
            failures.add(schemaName + "." + tableName + " is not a partitioned table (pg_class.relkind = '"
                    + snapshot.getRootRelkind() + "', expected 'p'). This gate is about partition "
                    + "trees; for an ordinary table use Liquibase's own <indexExists> "
                    + "precondition.");
            return false;
        }
        if (snapshot.getLeaves().isEmpty()) {
            failures.add(schemaName + "." + tableName + " is partitioned but has no leaf partitions, so "
                    + "there is nothing to assert. Refusing to pass vacuously.");
            return false;
        }
        return true;
    }

    /** {@code schemaName.indexName}, for messages. */
    protected final String qualifiedIndex() {
        return schemaName + "." + indexName;
    }

    /** {@code schemaName.tableName}, for messages. */
    protected final String qualifiedTable() {
        return schemaName + "." + tableName;
    }

    private List<String> missingAttributes() {
        List<String> missing = new ArrayList<String>();
        if (isBlank(schemaName)) {
            missing.add("schemaName");
        }
        if (isBlank(tableName)) {
            missing.add("tableName");
        }
        if (isBlank(indexName)) {
            missing.add("indexName");
        }
        return missing;
    }

    private PreconditionErrorException error(DatabaseChangeLog changeLog, String message) {
        return new PreconditionErrorException(new IllegalStateException(message), changeLog, this);
    }

    /**
     * Names the first few offenders and counts the rest. A 400-partition table with every leaf
     * broken should produce a readable sentence, not four hundred index names.
     */
    static String summarise(List<String> items) {
        if (items.size() <= MAX_NAMED_LEAVES) {
            return join(items, ", ");
        }
        List<String> head = new ArrayList<String>(items.subList(0, MAX_NAMED_LEAVES));
        return join(head, ", ") + " and " + (items.size() - MAX_NAMED_LEAVES) + " more";
    }

    static String join(List<String> items, String separator) {
        StringBuilder text = new StringBuilder();
        for (String item : items) {
            if (text.length() > 0) {
                text.append(separator);
            }
            text.append(item);
        }
        return text.toString();
    }

    static <T extends Comparable<T>> List<T> sorted(List<T> items) {
        List<T> copy = new ArrayList<T>(items);
        Collections.sort(copy);
        return copy;
    }

    static boolean isBlank(String value) {
        return value == null || value.trim().isEmpty();
    }
}
