package io.github.atulsinha87.partitionctl.liquibase.statement;

/**
 * One SQL statement plus a one-line human label.
 *
 * <p>The label is emitted as a leading SQL comment so that {@code liquibase updateSQL}
 * reads as a narrated plan ("leaf 7 of 12 ...") at zero mechanism cost. It is a comment,
 * not a second statement, so nothing is ever clubbed into one query string — which matters,
 * because multiple statements in one string run in an implicit transaction and
 * {@code CREATE INDEX CONCURRENTLY} refuses to run in one.
 */
public final class PlannedStatement {

    private final String sql;
    private final String label;

    public PlannedStatement(String sql, String label) {
        this.sql = sql;
        this.label = label;
    }

    public String getSql() {
        return sql;
    }

    /** One line, no newlines, or null. */
    public String getLabel() {
        return label;
    }

    /** The text actually sent to PostgreSQL: the label as a leading comment, then the SQL. */
    public String toSql() {
        return label == null ? sql : "-- " + label + "\n" + sql;
    }

    @Override
    public String toString() {
        return toSql();
    }
}
