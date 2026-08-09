package io.github.atulsinha87.partitionctl.liquibase.statement;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * The label is prefixed to every statement as a {@code --} comment, and it is assembled from
 * catalog-derived identifiers. PostgreSQL allows a newline inside a quoted identifier, so an
 * unflattened label ends the comment and turns whatever follows into a statement of its own.
 *
 * <p>Identifiers reaching the SQL body are quoted through {@code Identifiers}; the comment was the
 * one path where a raw catalog name was concatenated unescaped.
 */
class PlannedStatementCommentTest {

    @Test
    @DisplayName("a newline in the label cannot end the comment and start a statement")
    void newlineInLabelCannotEscapeTheComment() {
        PlannedStatement st =
                new PlannedStatement("SELECT 1", "leaf public.orders\nDROP TABLE victim; --");

        String sql = st.toSql();
        String comment = sql.substring(0, sql.indexOf('\n'));

        assertTrue(comment.startsWith("-- "), "the first line must still be the comment: " + comment);
        assertTrue(
                comment.contains("DROP TABLE victim;"),
                "the injected text must stay inside the comment, not escape it: " + comment);

        // Exactly one newline: the separator the method itself adds between comment and SQL.
        assertEquals(
                1,
                sql.length() - sql.replace("\n", "").length(),
                "toSql() must emit a single newline, the comment/SQL separator: " + sql);
        assertEquals("SELECT 1", sql.substring(sql.indexOf('\n') + 1));
    }

    @Test
    @DisplayName("a carriage return is flattened too")
    void carriageReturnIsFlattened() {
        PlannedStatement st = new PlannedStatement("SELECT 1", "leaf\rinjected");

        String sql = st.toSql();

        assertFalse(sql.contains("\r"), "a stray CR survived into the wire text: " + sql);
        assertEquals("-- leaf injected\nSELECT 1", sql);
    }

    @Test
    @DisplayName("an ordinary label is untouched")
    void ordinaryLabelIsUnchanged() {
        PlannedStatement st = new PlannedStatement("SELECT 1", "build + attach public.orders_2024_01");

        assertEquals("-- build + attach public.orders_2024_01\nSELECT 1", st.toSql());
    }

    @Test
    @DisplayName("a null label emits the bare SQL, as before")
    void nullLabelEmitsBareSql() {
        assertEquals("SELECT 1", new PlannedStatement("SELECT 1", null).toSql());
    }
}
