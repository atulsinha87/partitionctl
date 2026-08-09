package io.github.atulsinha87.partitionctl.liquibase.progress;

import liquibase.Scope;
import liquibase.database.Database;
import liquibase.exception.ValidationErrors;
import liquibase.sql.Sql;
import liquibase.sql.UnparsedSql;
import liquibase.sqlgenerator.SqlGeneratorChain;
import liquibase.sqlgenerator.core.AbstractSqlGenerator;

/**
 * Prints one console line per partition, <b>while the run is happening</b>.
 *
 * <h2>Why a SqlGenerator and not a log call from the Change</h2>
 * {@code generateStatements} returns the whole plan before a single statement executes, so
 * anything printed from there can only announce the plan — it can never report progress
 * through it. Liquibase asks a {@code SqlGenerator} for a statement's SQL <b>lazily, once per
 * statement, from inside {@code JdbcExecutor}, immediately before that statement goes to
 * JDBC</b>. That is the hook, and it is the only one that streams.
 *
 * <h2>The stack check is not optional</h2>
 * Without it every line prints <b>twice</b>: once as an up-front dump of the entire plan, once
 * correctly interleaved. {@code ChangeSet.addSqlMdc()} renders the whole statement array to
 * populate the MDC logging context, unconditionally, immediately before
 * {@code Database.executeStatements(...)}, and there is no flag to disable it. The same check
 * is what keeps {@code liquibase updateSQL} clean: under a preview a {@code LoggingExecutor} is
 * on the stack, not {@code JdbcExecutor}, nothing executes, and nothing should be announced.
 *
 * <p>Cost: one {@code new Throwable()} per statement. At 400 partitions that is roughly 1,600
 * throwaway stack captures — microseconds against hours of index builds.
 *
 * <h2>Why the UI channel and not the log channel</h2>
 * Measured A/B on identical runs. Under the Maven plugin both channels behave the same, because
 * it bridges Liquibase's UI service into Maven's logger. On the Liquibase CLI with
 * {@code --logLevel=severe} the log channel is silenced and the UI channel still prints, because
 * there the UI service is a direct console write. The UI channel is never weaker and is
 * sometimes stronger, so it is the one to ship.
 *
 * <p>Registered in {@code META-INF/services/liquibase.sqlgenerator.SqlGenerator}. It must extend
 * {@code AbstractSqlGenerator}: implementing the bare {@code SqlGenerator} interface does not
 * compile, it also carries {@code warn()}, {@code generateStatementsIsVolatile()} and
 * {@code generateRollbackStatementsIsVolatile()}.
 */
public class ProgressSqlGenerator extends AbstractSqlGenerator<ProgressSqlStatement> {

    /** Prefix on every line, so an operator can grep a long Maven log for just this. */
    public static final String PREFIX = "[partitionctl] ";

    private static final String EXECUTOR = "liquibase.executor.jvm.JdbcExecutor";

    @Override
    public int getPriority() {
        return PRIORITY_DEFAULT;
    }

    @Override
    public boolean supports(ProgressSqlStatement statement, Database database) {
        return true;
    }

    @Override
    public ValidationErrors validate(ProgressSqlStatement statement, Database database,
                                     SqlGeneratorChain<ProgressSqlStatement> chain) {
        return new ValidationErrors();
    }

    @Override
    public Sql[] generateSql(ProgressSqlStatement statement, Database database,
                             SqlGeneratorChain<ProgressSqlStatement> chain) {
        if (statement.getProgress() != null && aboutToExecute()) {
            Scope.getCurrentScope().getUI().sendMessage(PREFIX + statement.getProgress());
        }
        // ";" is what RawSqlStatement uses, so the updateSQL script is unchanged by this class.
        return new Sql[] { new UnparsedSql(statement.getSql(), ";") };
    }

    /**
     * True only when {@code JdbcExecutor} is on the call stack, i.e. this statement is about to
     * really run against the database. False during the MDC pass and during any preview.
     */
    static boolean aboutToExecute() {
        for (StackTraceElement frame : new Throwable().getStackTrace()) {
            if (EXECUTOR.equals(frame.getClassName())) {
                return true;
            }
        }
        return false;
    }
}
