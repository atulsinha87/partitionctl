package io.github.atulsinha87.partitionctl.liquibase.statement;

/**
 * One SQL statement, a one-line human label, and optionally one progress line.
 *
 * <h2>label — for the preview</h2>
 * Emitted as a leading SQL comment so that {@code liquibase updateSQL} reads as a narrated
 * plan ("leaf 7 of 12 ...") at zero mechanism cost. It is a comment, not a second statement,
 * so nothing is ever clubbed into one query string — which matters, because multiple
 * statements in one string run in an implicit transaction and
 * {@code CREATE INDEX CONCURRENTLY} refuses to run in one.
 *
 * <h2>progress — for the operator watching a live run</h2>
 * Printed to the console by
 * {@link io.github.atulsinha87.partitionctl.liquibase.progress.ProgressSqlGenerator} at the
 * moment this statement is handed to JDBC, and <b>only</b> then — never during the preview,
 * never during Liquibase's up-front MDC pass.
 *
 * <p>It is deliberately <b>not</b> the same thing as the label. There are three or four
 * statements per partition ({@code SET}s, the build, the marker, the attach) and the
 * requirement is one line per partition, so exactly one statement in each partition's group
 * carries a progress line: the first one, which is emitted immediately before the long-running
 * work begins. Most statements carry {@code null} here and print nothing.
 */
public final class PlannedStatement {

    private final String sql;
    private final String label;
    private String progress;

    public PlannedStatement(String sql, String label) {
        this(sql, label, null);
    }

    public PlannedStatement(String sql, String label, String progress) {
        this.sql = sql;
        this.label = label;
        this.progress = progress;
    }

    public String getSql() {
        return sql;
    }

    /** One line, no newlines, or null. */
    public String getLabel() {
        return label;
    }

    /** One line printed to the console when this statement executes, or null for silence. */
    public String getProgress() {
        return progress;
    }

    /**
     * Set once, after a partition's whole statement group is known, on the group's first
     * statement. Kept mutable for exactly that reason: the summary of what will happen to a
     * leaf ("rebuild + attach" versus "build + attach") is only decided part-way through
     * emitting the group, and the line has to print before the group's first statement runs.
     */
    public PlannedStatement setProgress(String progress) {
        this.progress = progress;
        return this;
    }

    /** The text actually sent to PostgreSQL: the label as a leading comment, then the SQL. */
    public String toSql() {
        return label == null ? sql : "-- " + singleLine(label) + "\n" + sql;
    }

    /**
     * Flattens a label to one line before it is prefixed as a {@code --} comment.
     *
     * <p>Labels are assembled from catalog-derived identifiers -- schema, table, index and
     * partition names read back from {@code pg_class}. PostgreSQL permits a newline inside a
     * quoted identifier, so a relation named {@code "orders<newline>DROP TABLE t;--"} would end
     * the {@code --} comment and leave the remainder as a statement in its own right. Every
     * identifier <em>inside</em> the SQL is quoted through {@code Identifiers}; the comment was
     * the one place a raw name reached the wire unescaped.
     *
     * <p>Creating such a relation already requires DDL rights on the schema, so this is hardening
     * rather than a privilege boundary. It is cheap and it is at the single point every statement
     * passes through.
     */
    private static String singleLine(String text) {
        return text.replace("\r", " ").replace("\n", " ");
    }

    @Override
    public String toString() {
        return toSql();
    }
}
