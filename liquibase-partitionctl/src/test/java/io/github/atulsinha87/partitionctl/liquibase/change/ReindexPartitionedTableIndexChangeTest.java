package io.github.atulsinha87.partitionctl.liquibase.change;

import liquibase.changelog.ChangeSet;
import liquibase.changelog.DatabaseChangeLog;
import liquibase.database.core.MySQLDatabase;
import liquibase.database.core.PostgresDatabase;
import liquibase.exception.ValidationErrors;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

/** {@code validate()} runs over the whole changelog before any changeset executes. */
class ReindexPartitionedTableIndexChangeTest {

    private static ReindexPartitionedTableIndexChange change() {
        ReindexPartitionedTableIndexChange change = new ReindexPartitionedTableIndexChange();
        change.setSchemaName("public");
        change.setTableName("person");
        change.setIndexName("idx_personaddress");
        return change;
    }

    private static String messages(ValidationErrors errors) {
        return errors.getErrorMessages().toString();
    }

    @Test
    @DisplayName("a complete change validates")
    void valid() {
        assertFalse(change().validate(new PostgresDatabase()).hasErrors());
    }

    @Test
    @DisplayName("every required attribute is checked by name")
    void requiredAttributes() {
        ReindexPartitionedTableIndexChange change = new ReindexPartitionedTableIndexChange();
        String errors = messages(change.validate(new PostgresDatabase()));
        assertTrue(errors.contains("schemaName is required"), errors);
        assertTrue(errors.contains("tableName is required"), errors);
        assertTrue(errors.contains("indexName is required"), errors);
    }

    @Test
    @DisplayName("runInTransaction=\"true\" is refused, because CONCURRENTLY cannot run in one")
    void runInTransactionMustBeFalse() {
        ReindexPartitionedTableIndexChange change = change();
        change.setChangeSet(new ChangeSet("id", "author", false, true,
                "changelog.xml", null, null, new DatabaseChangeLog("changelog.xml")));

        String errors = messages(change.validate(new PostgresDatabase()));
        assertTrue(errors.contains("runInTransaction=\"false\""), errors);
    }

    @Test
    @DisplayName("runInTransaction=\"false\" passes")
    void runInTransactionFalseIsAccepted() {
        ReindexPartitionedTableIndexChange change = change();
        change.setChangeSet(new ChangeSet("id", "author", false, false,
                "changelog.xml", null, null, false, null, new DatabaseChangeLog("changelog.xml")));

        assertFalse(change.validate(new PostgresDatabase()).hasErrors());
    }

    @Test
    @DisplayName("a non-PostgreSQL target is refused by name")
    void postgresOnly() {
        String errors = messages(change().validate(new MySQLDatabase()));
        assertTrue(errors.contains("PostgreSQL only"), errors);
        assertFalse(change().supports(new MySQLDatabase()));
        assertTrue(change().supports(new PostgresDatabase()));
    }

    @Test
    @DisplayName("negative pacing and an empty lockTimeout are refused")
    void tuningKnobsAreChecked() {
        ReindexPartitionedTableIndexChange change = change();
        change.setPaceSeconds(-1);
        change.setLockTimeout("  ");

        String errors = messages(change.validate(new PostgresDatabase()));
        assertTrue(errors.contains("paceSeconds must not be negative"), errors);
        assertTrue(errors.contains("lockTimeout must not be empty"), errors);
    }

    @Test
    @DisplayName("the defaults are the measured ones")
    void defaults() {
        ReindexPartitionedTableIndexChange change = new ReindexPartitionedTableIndexChange();
        // ShareUpdateExclusiveLock queues nothing harmful behind it, so waiting is nearly free
        // and the work being protected can be hours.
        org.junit.jupiter.api.Assertions.assertEquals("15min", change.getLockTimeout());
        org.junit.jupiter.api.Assertions.assertNull(change.getPaceSeconds());
    }

    @Test
    @DisplayName("no rollback, always volatile, and the namespace is overridden")
    void contract() {
        ReindexPartitionedTableIndexChange change = change();
        assertFalse(change.supportsRollback(new PostgresDatabase()));
        assertTrue(change.generateStatementsVolatile(new PostgresDatabase()));
        assertNotNull(change.getSerializedObjectNamespace());
        assertTrue(change.getConfirmationMessage().contains("idx_personaddress"));
    }
}
