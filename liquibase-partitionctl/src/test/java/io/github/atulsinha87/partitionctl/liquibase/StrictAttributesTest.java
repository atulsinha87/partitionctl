package io.github.atulsinha87.partitionctl.liquibase;

import io.github.atulsinha87.partitionctl.liquibase.change.CreatePartitionedTableIndexChange;

import liquibase.changelog.ChangeLogParameters;
import liquibase.changelog.DatabaseChangeLog;
import liquibase.parser.core.xml.XMLChangeLogSAXParser;
import liquibase.resource.ClassLoaderResourceAccessor;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * The silent-null-binding defect, and the guard that closes it.
 *
 * <h2>What was measured</h2>
 * A changelog that declares {@code xmlns:ext} but does <b>not</b> list partitionctl.xsd in its
 * {@code xsi:schemaLocation} — a one-line omission Liquibase never complains about — binds the
 * element normally and silently discards every attribute it does not recognise. Run against
 * PostgreSQL 17.10 through liquibase-maven-plugin 4.33.0 with
 * {@code uniq="true" usin="brin" wher="status &lt;&gt; 'archived'" lockTimout="1s"}:
 * <pre>
 * BUILD SUCCESS
 * relname   | indisunique | amname | predicate
 * idx_noxsd | f           | btree  |
 * </pre>
 * A non-unique full btree index where a unique partial brin index was asked for, recorded as
 * EXECUTED. And not correctable afterwards: coverage keys on {@code pg_inherits}, never on the
 * index definition, so a corrected changeset aimed at the same index emits zero statements.
 *
 * <p>{@code validate()} cannot catch this — it sees a field that was simply never set, which is
 * indistinguishable from the attribute being omitted on purpose. The XSD does catch it, but only
 * when the adopter references it. The guard in {@code load()} always runs.
 *
 * <p>{@code no-ext-schemalocation.xml} carries the omission deliberately; if it is ever "fixed"
 * these tests still pass for the wrong reason, so {@link #theChangelogReallyOmitsTheSchema}
 * pins it.
 */
class StrictAttributesTest {

    private static DatabaseChangeLog parse(String file) throws Exception {
        return new XMLChangeLogSAXParser().parse(file, new ChangeLogParameters(),
                new ClassLoaderResourceAccessor());
    }

    private static String messageOf(Throwable thrown) {
        StringBuilder all = new StringBuilder();
        for (Throwable t = thrown; t != null; t = t.getCause()) {
            all.append(t.getMessage()).append(' ');
        }
        return all.toString();
    }

    @Test
    @DisplayName("a misspelled attribute is refused even when the ext XSD is not referenced")
    void misspelledAttributeIsRefusedWithoutTheXsd() {
        Exception thrown = assertThrows(Exception.class,
                () -> parse("changelogs/no-ext-schemalocation.xml"));
        String message = messageOf(thrown);
        assertTrue(message.contains("uniq"),
                "the offending attribute must be named: " + message);
        assertTrue(message.contains("did you mean \"unique\""),
                "and the correct spelling suggested: " + message);
    }

    @Test
    @DisplayName("CONTROL: that changelog really does omit the ext XSD, or this proves nothing")
    void theChangelogReallyOmitsTheSchema() throws Exception {
        String xml = new java.util.Scanner(
                StrictAttributesTest.class.getClassLoader()
                        .getResourceAsStream("changelogs/no-ext-schemalocation.xml"),
                "UTF-8").useDelimiter("\\A").next();
        assertTrue(xml.contains("xmlns:ext="),
                "the element must still bind, or the test is about something else");
        int schemaLocation = xml.indexOf("schemaLocation");
        assertTrue(schemaLocation > 0);
        assertTrue(!xml.substring(schemaLocation).contains("partitionctl.xsd"),
                "this changelog is supposed to OMIT partitionctl.xsd from xsi:schemaLocation; "
                        + "with it present the XSD would do the refusing and the guard would be "
                        + "untested");
    }

    @Test
    @DisplayName("a correctly spelled changelog is unaffected")
    void correctSpellingStillParses() throws Exception {
        DatabaseChangeLog log = parse("changelogs/binding-shape.xml");
        CreatePartitionedTableIndexChange change = (CreatePartitionedTableIndexChange)
                log.getChangeSets().get(0).getChanges().get(0);
        assertEquals(Boolean.TRUE, change.getUnique());
        assertEquals("brin", change.getUsing());
        assertEquals("status <> 'archived'", change.getWhere());
        assertEquals("9min", change.getLockTimeout());
    }

    @Test
    @DisplayName("both <column> forms are still accepted -- they are children, not attributes")
    void columnChildrenAreNotMistakenForAttributes() throws Exception {
        // Measured: Liquibase normalises <column/> and <ext:column/> to the same ParsedNode name,
        // so the guard sees "column" in both cases and must allow it.
        DatabaseChangeLog log = parse("changelogs/binding-prefixed-column.xml");
        CreatePartitionedTableIndexChange change = (CreatePartitionedTableIndexChange)
                log.getChangeSets().get(0).getChanges().get(0);
        assertEquals(1, change.getColumns().size());
        assertEquals("created_at", change.getColumns().get(0).getName());
    }
}
