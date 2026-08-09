package io.github.atulsinha87.partitionctl.liquibase.change;

import liquibase.changelog.ChangeSet;
import liquibase.changelog.DatabaseChangeLog;
import liquibase.database.core.H2Database;
import liquibase.database.core.PostgresDatabase;
import liquibase.exception.ValidationErrors;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * The destructive change, and the only one with no unit test until now -- create and reindex both
 * had one.
 *
 * <p>{@code validate(Database)} is the last gate that runs before anything is executed, and this
 * change takes {@code AccessExclusiveLock} on the parent table and every leaf simultaneously. A
 * validation hole here is not a wrong index; it is a table locked against all reads and writes on
 * a wrong name.
 */
class DropPartitionedTableIndexChangeTest {

    private static DropPartitionedTableIndexChange wellFormed() {
        DropPartitionedTableIndexChange change = new DropPartitionedTableIndexChange();
        change.setSchemaName("public");
        change.setTableName("orders");
        change.setIndexName("idx_orders_created");
        change.setConfirmExclusiveLock(Boolean.TRUE);
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

    /**
     * Attaches the change to a changeSet with the given transaction setting.
     *
     * <p>The 10-argument constructor, because runInTransaction is its 8th parameter. The shorter
     * form's 4th boolean is runOnChange and leaves runInTransaction at its default of true, which
     * is a quiet way to write a test that asserts the opposite of what it appears to.
     */
    private static DropPartitionedTableIndexChange inChangeSet(
            DropPartitionedTableIndexChange change, boolean runInTransaction) {
        ChangeSet cs = new ChangeSet("id", "author", false, false, "changelog.xml",
                null, null, runInTransaction, null, new DatabaseChangeLog("changelog.xml"));
        change.setChangeSet(cs);
        return change;
    }

    @Test
    @DisplayName("a well-formed change validates clean")
    void wellFormedPasses() {
        ValidationErrors errors = wellFormed().validate(new PostgresDatabase());
        assertFalse(errors.hasErrors(), errors.getErrorMessages().toString());
    }

    @Test
    @DisplayName("each required identifier is reported by name when missing")
    void requiredIdentifiersAreChecked() {
        DropPartitionedTableIndexChange noSchema = wellFormed();
        noSchema.setSchemaName(null);
        assertTrue(hasErrorContaining(noSchema.validate(new PostgresDatabase()), "schemaName is required"));

        DropPartitionedTableIndexChange noTable = wellFormed();
        noTable.setTableName("   ");
        assertTrue(hasErrorContaining(noTable.validate(new PostgresDatabase()), "tableName is required"),
                "whitespace must count as absent, not as a name");

        DropPartitionedTableIndexChange noIndex = wellFormed();
        noIndex.setIndexName("");
        assertTrue(hasErrorContaining(noIndex.validate(new PostgresDatabase()), "indexName is required"));
    }

    @Test
    @DisplayName("a blank timeout is rejected rather than silently sent to PostgreSQL")
    void blankTimeoutsAreRejected() {
        DropPartitionedTableIndexChange lock = wellFormed();
        lock.setLockTimeout("");
        assertTrue(hasErrorContaining(lock.validate(new PostgresDatabase()), "lockTimeout must not be empty"));

        DropPartitionedTableIndexChange exclusive = wellFormed();
        exclusive.setExclusiveLockTimeout("  ");
        assertTrue(hasErrorContaining(exclusive.validate(new PostgresDatabase()),
                "exclusiveLockTimeout must not be empty"));

        DropPartitionedTableIndexChange total = wellFormed();
        total.setExclusiveTotalTimeout(null);
        assertTrue(hasErrorContaining(total.validate(new PostgresDatabase()),
                "exclusiveTotalTimeout must not be empty"));
    }

    @Test
    @DisplayName("exclusiveRetries counts attempts, so anything below 1 is rejected")
    void retriesBelowOneIsRejected() {
        for (Integer bad : new Integer[]{null, 0, -1}) {
            DropPartitionedTableIndexChange change = wellFormed();
            change.setExclusiveRetries(bad);
            ValidationErrors errors = change.validate(new PostgresDatabase());
            assertTrue(hasErrorContaining(errors, "exclusiveRetries must be at least 1"),
                    "exclusiveRetries=" + bad + " should be rejected: " + errors.getErrorMessages());
        }

        DropPartitionedTableIndexChange one = wellFormed();
        one.setExclusiveRetries(1);
        assertFalse(one.validate(new PostgresDatabase()).hasErrors(),
                "1 is a single attempt and must be allowed");
    }

    @Test
    @DisplayName("a non-PostgreSQL target is rejected, naming the database it actually got")
    void nonPostgresIsRejected() {
        ValidationErrors errors = wellFormed().validate(new H2Database());

        assertTrue(errors.hasErrors());
        assertTrue(hasErrorContaining(errors, "supports PostgreSQL only"), errors.getErrorMessages().toString());
        assertTrue(hasErrorContaining(errors, "H2Database"),
                "the message must name what it got, or the reader cannot tell what is wired up: "
                        + errors.getErrorMessages());
    }

    @Test
    @DisplayName("runInTransaction=\"true\" is rejected: DROP INDEX CONCURRENTLY cannot run in a transaction")
    void runInTransactionTrueIsRejected() {
        ValidationErrors errors =
                inChangeSet(wellFormed(), true).validate(new PostgresDatabase());

        assertTrue(errors.hasErrors());
        assertTrue(hasErrorContaining(errors, "requires runInTransaction=\"false\""),
                errors.getErrorMessages().toString());
    }

    @Test
    @DisplayName("runInTransaction=\"false\" is accepted")
    void runInTransactionFalseIsAccepted() {
        ValidationErrors errors =
                inChangeSet(wellFormed(), false).validate(new PostgresDatabase());

        assertFalse(errors.hasErrors(), errors.getErrorMessages().toString());
    }

    @Test
    @DisplayName("no database still validates the change's own fields, so a null target hides nothing")
    void nullDatabaseStillChecksTheChange() {
        DropPartitionedTableIndexChange change = wellFormed();
        change.setIndexName(null);

        assertTrue(hasErrorContaining(change.validate(null), "indexName is required"));
    }

    @Test
    @DisplayName("the statement list is volatile: it is a function of live catalog state")
    void statementsAreNeverCached() {
        assertTrue(wellFormed().generateStatementsVolatile(new PostgresDatabase()),
                "caching the statements would reuse a partition list discovered against another database");
    }

    @Test
    @DisplayName("rollback is refused: rebuilding the index is hours of work, not an inverse")
    void rollbackIsNotSupported() {
        assertFalse(wellFormed().supportsRollback(new PostgresDatabase()));
    }

    @Test
    @DisplayName("supports() is PostgreSQL-only")
    void supportsPostgresOnly() {
        assertTrue(wellFormed().supports(new PostgresDatabase()));
        assertFalse(wellFormed().supports(new H2Database()));
    }

    @Test
    @DisplayName("the confirmation message names the index and the table it came off")
    void confirmationMessageIsSpecific() {
        String message = wellFormed().getConfirmationMessage();

        assertTrue(message.contains("idx_orders_created"), message);
        assertTrue(message.contains("public.orders"), message);
    }

    @Test
    @DisplayName("the serialized namespace is the extension's, so the ext: prefix resolves")
    void namespaceIsTheExtensionNamespace() {
        assertEquals(DropPartitionedTableIndexChange.NAMESPACE,
                wellFormed().getSerializedObjectNamespace());
    }

    @Test
    @DisplayName("defaults are the documented ones")
    void defaultsMatchTheDocumentation() {
        DropPartitionedTableIndexChange fresh = new DropPartitionedTableIndexChange();

        assertEquals("15min", fresh.getLockTimeout());
        assertEquals("5s", fresh.getExclusiveLockTimeout());
        assertEquals("5min", fresh.getExclusiveTotalTimeout());
        assertEquals(Integer.valueOf(5), fresh.getExclusiveRetries());
    }
}
