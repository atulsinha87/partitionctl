package io.github.atulsinha87.partitionctl.liquibase.progress;

import liquibase.statement.AbstractSqlStatement;

/**
 * A raw SQL statement that also carries the one console line to print when it executes.
 *
 * <p>Behaves exactly like Liquibase's own {@code RawSqlStatement} — same SQL, same {@code ";"}
 * end delimiter, so {@code liquibase updateSQL} produces a byte-identical script. The only
 * addition is {@link #getProgress()}, which {@link ProgressSqlGenerator} prints at the instant
 * the statement is handed to JDBC.
 *
 * <p>A null progress line means silence. Most statements — every {@code SET}, every pacing
 * sleep — are null, because the requirement is one line per partition, not per statement.
 */
public class ProgressSqlStatement extends AbstractSqlStatement {

    private final String sql;
    private final String progress;

    public ProgressSqlStatement(String sql, String progress) {
        this.sql = sql;
        this.progress = progress;
    }

    public String getSql() {
        return sql;
    }

    /** One line for the operator, or null to print nothing. */
    public String getProgress() {
        return progress;
    }

    /**
     * Never skip. The default would let an unsupported-statement path swallow DDL silently,
     * and every statement this extension emits is PostgreSQL-specific by construction.
     */
    @Override
    public boolean skipOnUnsupported() {
        return false;
    }

    @Override
    public String toString() {
        return sql;
    }
}
