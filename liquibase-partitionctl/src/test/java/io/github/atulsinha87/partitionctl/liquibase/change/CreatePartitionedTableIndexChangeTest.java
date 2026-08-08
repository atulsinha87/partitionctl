package io.github.atulsinha87.partitionctl.liquibase.change;

import liquibase.change.ColumnConfig;
import liquibase.changelog.ChangeSet;
import liquibase.changelog.DatabaseChangeLog;
import liquibase.database.core.H2Database;
import liquibase.database.core.PostgresDatabase;
import liquibase.exception.ValidationErrors;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * {@code validate(Database)} runs before any DDL. These are the checks that stop a typo from
 * silently producing a wrong index.
 */
class CreatePartitionedTableIndexChangeTest {

    private static CreatePartitionedTableIndexChange wellFormed() {
        CreatePartitionedTableIndexChange change = new CreatePartitionedTableIndexChange();
        change.setSchemaName("public");
        change.setTableName("person");
        change.setIndexName("idx_personaddress");
        ColumnConfig column = new ColumnConfig();
        column.setName("address");
        column.setDescending(true);
        change.addColumn(column);
        return change;
    }

    private static boolean hasErrorContaining(ValidationErrors errors, String fragment) {
        for (String message : errors.getErrorMessages()) {
            if (message.contains(fragment)) {
                return true;
            }
        }
        return false;
    }

    @Test
    @DisplayName("a well-formed change validates clean")
    void wellFormedPasses() {
        ValidationErrors errors = wellFormed().validate(new PostgresDatabase());
        assertFalse(errors.hasErrors(), errors.getErrorMessages().toString());
    }

    @Test
    @DisplayName("a typo'd <colum> leaves getColumns() empty, and THAT is what the column check catches")
    void noColumnsIsRejected() {
        // This is exactly the state a misspelled child element produces: the element binds to
        // nothing, addColumn is never called, and without this check the change would go on to
        // emit CREATE INDEX ... () against a real table.
        CreatePartitionedTableIndexChange change = wellFormed();
        change.setColumns(new java.util.ArrayList<ColumnConfig>());

        ValidationErrors errors = change.validate(new PostgresDatabase());
        assertTrue(errors.hasErrors());
        assertTrue(hasErrorContaining(errors, "at least one <column> is required"),
                errors.getErrorMessages().toString());
        assertTrue(hasErrorContaining(errors, "spelled <column .../> exactly"),
                errors.getErrorMessages().toString());
    }

    @Test
    @DisplayName("a <column> with no name is rejected")
    void namelessColumnIsRejected() {
        CreatePartitionedTableIndexChange change = wellFormed();
        change.addColumn(new ColumnConfig());

        ValidationErrors errors = change.validate(new PostgresDatabase());
        assertTrue(hasErrorContaining(errors, "has no name attribute"),
                errors.getErrorMessages().toString());
    }

    @Test
    @DisplayName("required attributes are required")
    void requiredAttributes() {
        CreatePartitionedTableIndexChange change = new CreatePartitionedTableIndexChange();
        ColumnConfig column = new ColumnConfig();
        column.setName("address");
        change.addColumn(column);

        ValidationErrors errors = change.validate(new PostgresDatabase());
        assertTrue(hasErrorContaining(errors, "schemaName is required"), errors.toString());
        assertTrue(hasErrorContaining(errors, "tableName is required"), errors.toString());
        assertTrue(hasErrorContaining(errors, "indexName is required"), errors.toString());
    }

    @Test
    @DisplayName("PostgreSQL only")
    void nonPostgresIsRejected() {
        ValidationErrors errors = wellFormed().validate(new H2Database());
        assertTrue(hasErrorContaining(errors, "supports PostgreSQL only"),
                errors.getErrorMessages().toString());
        assertFalse(wellFormed().supports(new H2Database()));
        assertTrue(wellFormed().supports(new PostgresDatabase()));
    }

    @Test
    @DisplayName("runInTransaction=\"false\" is mandatory and its absence is explained")
    void runInTransactionMustBeFalse() {
        CreatePartitionedTableIndexChange change = wellFormed();
        ChangeSet inTransaction = new ChangeSet("id", "author", false, true,
                "changelog.xml", null, null, new DatabaseChangeLog("changelog.xml"));
        change.setChangeSet(inTransaction);

        ValidationErrors errors = change.validate(new PostgresDatabase());
        assertTrue(hasErrorContaining(errors, "requires runInTransaction=\"false\""),
                errors.getErrorMessages().toString());

        ChangeSet outOfTransaction = new ChangeSet("id", "author", false, false,
                "changelog.xml", null, null, false, null, new DatabaseChangeLog("changelog.xml"));
        change.setChangeSet(outOfTransaction);
        assertFalse(change.validate(new PostgresDatabase()).hasErrors());
    }

    @Test
    @DisplayName("no rollback, statements are volatile, namespace is overridden")
    void contractBits() {
        CreatePartitionedTableIndexChange change = wellFormed();
        assertFalse(change.supportsRollback(new PostgresDatabase()));
        assertTrue(change.generateStatementsVolatile(new PostgresDatabase()));
        assertTrue(CreatePartitionedTableIndexChange.NAMESPACE
                .equals(change.getSerializedObjectNamespace()));
        assertTrue(change.getConfirmationMessage().contains("idx_personaddress"));
    }
}
