package io.github.atulsinha87.partitionctl.liquibase.statement;

import io.github.atulsinha87.partitionctl.liquibase.progress.ProgressSqlStatement;

import liquibase.statement.SqlStatement;

import java.util.List;

/**
 * The one place a planned statement becomes a Liquibase statement.
 *
 * <p>All three changes go through here so that progress reporting cannot be wired into one and
 * forgotten in another — which is exactly what happened to the ownership marker, built by the
 * drop and never emitted by the create.
 */
public final class Statements {

    private Statements() {
    }

    public static SqlStatement[] toSqlStatements(List<PlannedStatement> planned) {
        SqlStatement[] statements = new SqlStatement[planned.size()];
        for (int i = 0; i < planned.size(); i++) {
            PlannedStatement one = planned.get(i);
            statements[i] = new ProgressSqlStatement(one.toSql(), one.getProgress());
        }
        return statements;
    }
}
