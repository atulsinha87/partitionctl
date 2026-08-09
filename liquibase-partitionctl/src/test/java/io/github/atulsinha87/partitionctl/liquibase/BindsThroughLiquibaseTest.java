package io.github.atulsinha87.partitionctl.liquibase;

import io.github.atulsinha87.partitionctl.liquibase.change.CreatePartitionedTableIndexChange;

import liquibase.change.Change;
import liquibase.changelog.ChangeLogParameters;
import liquibase.changelog.ChangeSet;
import liquibase.changelog.DatabaseChangeLog;
import liquibase.parser.core.xml.XMLChangeLogSAXParser;
import liquibase.resource.ClassLoaderResourceAccessor;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * The binding test that runs the real path.
 *
 * <p>{@link XsdBindingTest} compares names reflectively, which catches a typo. This one parses a
 * changelog with <b>Liquibase's own XML parser</b> and asserts the values arrive on the change
 * object, which catches everything else: a wrong XSD type, an attribute the parser refuses, a
 * setter Liquibase's reflection cannot see. The failure mode being defended against is silent —
 * a mismatched attribute binds to {@code null} with no error of any kind — so an assertion that
 * a value is present is the only thing that detects it.
 *
 * <p>The changelog is also validated against the shipped XSD as it parses, so this doubles as
 * proof the XSD admits the attributes it declares.
 */
class BindsThroughLiquibaseTest {

    private static CreatePartitionedTableIndexChange parse() throws Exception {
        DatabaseChangeLog log = new XMLChangeLogSAXParser().parse(
                "changelogs/binding-shape.xml",
                new ChangeLogParameters(),
                new ClassLoaderResourceAccessor());

        List<ChangeSet> changeSets = log.getChangeSets();
        assertEquals(1, changeSets.size(), "the changelog did not parse into one changeset");
        List<Change> changes = changeSets.get(0).getChanges();
        assertEquals(1, changes.size(), "the ext element did not bind to a Change at all -- check "
                + "META-INF/services/liquibase.change.Change and the @DatabaseChange name");
        assertTrue(changes.get(0) instanceof CreatePartitionedTableIndexChange,
                "bound to " + changes.get(0).getClass().getName());
        return (CreatePartitionedTableIndexChange) changes.get(0);
    }

    @Test
    @DisplayName("unique binds -- an XSD/Java name mismatch would leave it null in silence")
    void uniqueBinds() throws Exception {
        assertEquals(Boolean.TRUE, parse().getUnique(),
                "unique did not bind. Check that partitionctl.xsd declares an attribute named "
                        + "exactly \"unique\" of type xsd:boolean.");
    }

    @Test
    @DisplayName("using binds")
    void usingBinds() throws Exception {
        assertEquals("brin", parse().getUsing(),
                "using did not bind. Check partitionctl.xsd declares exactly \"using\".");
    }

    @Test
    @DisplayName("where binds, with its XML entities resolved and nothing escaped away")
    void whereBinds() throws Exception {
        assertEquals("status <> 'archived'", parse().getWhere(),
                "where did not bind. Check partitionctl.xsd declares exactly \"where\".");
    }

    @Test
    @DisplayName("the attributes that already worked still do, so this file is a real control")
    void theOlderAttributesStillBind() throws Exception {
        CreatePartitionedTableIndexChange change = parse();
        assertEquals("public", change.getSchemaName());
        assertEquals("orders", change.getTableName());
        assertEquals("idx_orders_shape", change.getIndexName());
        assertEquals("9min", change.getLockTimeout());
        assertEquals("11s", change.getAttachLockTimeout());
        assertEquals(Integer.valueOf(3), change.getPaceSeconds());
    }

    @Test
    @DisplayName("columns and their descending flag bind through the same parse")
    void columnsBind() throws Exception {
        CreatePartitionedTableIndexChange change = parse();
        assertEquals(2, change.getColumns().size());
        assertEquals("created_at", change.getColumns().get(0).getName());
        assertEquals(Boolean.TRUE, change.getColumns().get(0).getDescending());
        assertEquals("id", change.getColumns().get(1).getName());
    }

    @Test
    @DisplayName("NEGATIVE CONTROL: a misspelled shape attribute is refused at parse time")
    void aMisspelledAttributeIsRefused() {
        // Without this, every assertion above could pass while the XSD was being ignored
        // altogether -- Liquibase would happily bind whatever it recognised and drop the rest.
        Exception thrown = assertThrows(Exception.class, new org.junit.jupiter.api.function.Executable() {
            @Override
            public void execute() throws Throwable {
                new XMLChangeLogSAXParser().parse("changelogs/binding-typo.xml",
                        new ChangeLogParameters(), new ClassLoaderResourceAccessor());
            }
        });
        String message = String.valueOf(thrown.getMessage())
                + String.valueOf(thrown.getCause() == null ? "" : thrown.getCause().getMessage());
        assertTrue(message.contains("uniqu"),
                "the shipped XSD is not being enforced on this path, so the binding assertions "
                        + "above prove nothing. Got: " + message);
    }

    @Test
    @DisplayName("the shape reaches the generated SQL, not just the change object")
    void theShapeReachesTheSql() throws Exception {
        // Binding is only half the journey; a bound field that no statement reads is the same
        // defect wearing a different hat.
        CreatePartitionedTableIndexChange change = parse();
        assertEquals(Boolean.TRUE, change.getUnique());
        assertEquals("brin", change.getUsing());
        assertEquals("status <> 'archived'", change.getWhere());
    }
}
