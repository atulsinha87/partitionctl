package io.github.atulsinha87.partitionctl.liquibase.precondition;

import liquibase.changelog.ChangeSet;
import liquibase.changelog.DatabaseChangeLog;
import liquibase.changelog.visitor.ChangeExecListener;
import liquibase.database.Database;
import liquibase.database.DatabaseConnection;
import liquibase.database.jvm.JdbcConnection;
import liquibase.exception.PreconditionErrorException;
import liquibase.exception.PreconditionFailedException;
import liquibase.exception.ValidationErrors;
import liquibase.exception.Warnings;
import liquibase.precondition.AbstractPrecondition;
import liquibase.precondition.FailedPrecondition;

import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.util.ArrayList;
import java.util.List;

/**
 * {@code <ext:partitionctlIndexGate schema="..." table="..." index="..."/>}
 *
 * <p>Passes iff {@code pg_indexes} shows an index with the given name on the given
 * schema-qualified table. Runs a live SELECT on the JDBC connection Liquibase is already
 * authenticated on — no second credential, no subprocess.
 *
 * <p>Useful independently of the changes in this jar: it lets a different changeset assert an
 * index is present before depending on it, including when the index was built out of band.
 */
public class IndexGatePrecondition extends AbstractPrecondition {

    public static final String NAMESPACE =
            "http://www.liquibase.org/xml/ns/dbchangelog-ext/partitionctl";

    private String schema;
    private String table;
    private String index;

    public String getSchema() {
        return schema;
    }

    public void setSchema(String schema) {
        this.schema = schema;
    }

    public String getTable() {
        return table;
    }

    public void setTable(String table) {
        this.table = table;
    }

    public String getIndex() {
        return index;
    }

    public void setIndex(String index) {
        this.index = index;
    }

    @Override
    public String getName() {
        return "partitionctlIndexGate";
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
        if (isBlank(schema)) {
            errors.addError("partitionctlIndexGate: 'schema' attribute is required");
        }
        if (isBlank(table)) {
            errors.addError("partitionctlIndexGate: 'table' attribute is required");
        }
        if (isBlank(index)) {
            errors.addError("partitionctlIndexGate: 'index' attribute is required");
        }
        return errors;
    }

    @Override
    public void check(Database database, DatabaseChangeLog changeLog, ChangeSet changeSet,
                      ChangeExecListener execListener)
            throws PreconditionFailedException, PreconditionErrorException {

        boolean found;
        try {
            DatabaseConnection dbConnection = database.getConnection();
            if (!(dbConnection instanceof JdbcConnection)) {
                throw new PreconditionErrorException(
                        new IllegalStateException("partitionctlIndexGate requires a JdbcConnection, got "
                                + (dbConnection == null ? "none" : dbConnection.getClass().getName())),
                        changeLog, this);
            }
            Connection connection = ((JdbcConnection) dbConnection).getUnderlyingConnection();

            String sql = "SELECT 1 FROM pg_indexes WHERE schemaname = ? AND tablename = ? AND indexname = ?";
            try (PreparedStatement ps = connection.prepareStatement(sql)) {
                ps.setString(1, schema);
                ps.setString(2, table);
                ps.setString(3, index);
                try (ResultSet rs = ps.executeQuery()) {
                    found = rs.next();
                }
            }
        } catch (PreconditionErrorException e) {
            throw e;
        } catch (Exception e) {
            throw new PreconditionErrorException(e, changeLog, this);
        }

        if (!found) {
            List<FailedPrecondition> failures = new ArrayList<FailedPrecondition>();
            failures.add(new FailedPrecondition(
                    "Index '" + index + "' not found on " + schema + "." + table
                            + " (checked live via pg_indexes on the changelog's own JDBC connection)",
                    changeLog, this));
            throw new PreconditionFailedException(failures);
        }
    }

    private static boolean isBlank(String value) {
        return value == null || value.trim().isEmpty();
    }
}
