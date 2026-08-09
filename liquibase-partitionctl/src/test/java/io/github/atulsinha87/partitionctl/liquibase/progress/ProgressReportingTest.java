package io.github.atulsinha87.partitionctl.liquibase.progress;

import io.github.atulsinha87.partitionctl.liquibase.catalog.IndexNaming;
import io.github.atulsinha87.partitionctl.liquibase.catalog.LeafIndex;
import io.github.atulsinha87.partitionctl.liquibase.catalog.LeafPartition;
import io.github.atulsinha87.partitionctl.liquibase.catalog.TreeState;
import io.github.atulsinha87.partitionctl.liquibase.statement.CreateIndexPlan;
import io.github.atulsinha87.partitionctl.liquibase.statement.PlannedStatement;
import io.github.atulsinha87.partitionctl.liquibase.statement.StatementBuilder;
import io.github.atulsinha87.partitionctl.liquibase.statement.Statements;

import liquibase.sql.Sql;
import liquibase.statement.SqlStatement;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.util.ArrayList;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * The operator-visible half: exactly one line per partition, naming the partition and its
 * position, and nothing at all during a preview.
 */
class ProgressReportingTest {

    private static final String SCHEMA = "public";
    private static final String TABLE = "orders";
    private static final String INDEX = "idx_orders_addr";

    private static CreateIndexPlan plan() {
        return new CreateIndexPlan()
                .setSchemaName(SCHEMA)
                .setTableName(TABLE)
                .setIndexName(INDEX)
                .addColumn("address", false);
    }

    private static TreeState twelveLeaves() {
        TreeState state = new TreeState();
        state.setRootRelkind("p");
        state.setParentIndexExists(false);
        state.setParentIndexValid(false);
        state.setOriginalStatementTimeout("0");
        state.setOriginalLockTimeout("0");
        List<LeafPartition> leaves = new ArrayList<LeafPartition>();
        for (int i = 1; i <= 12; i++) {
            leaves.add(new LeafPartition(SCHEMA, String.format("orders_2024_%02d", i)));
        }
        for (LeafPartition leaf : leaves) {
            state.addLeaf(leaf);
        }
        IndexNaming.assignChildIndexNames(INDEX, leaves);
        return state;
    }

    private static List<String> progressLines(List<PlannedStatement> statements) {
        List<String> lines = new ArrayList<String>();
        for (PlannedStatement statement : statements) {
            if (statement.getProgress() != null) {
                lines.add(statement.getProgress());
            }
        }
        return lines;
    }

    @Test
    @DisplayName("one line per partition, not one per statement")
    void oneLinePerPartition() {
        List<PlannedStatement> statements = StatementBuilder.build(plan(), twelveLeaves());
        List<String> lines = progressLines(statements);

        // 12 partitions + the parent ON ONLY + the closing verification.
        assertEquals(14, lines.size(), lines.toString());
        assertTrue(statements.size() > 40,
                "with 3-4 statements per leaf there must be far more statements than lines, "
                        + "otherwise this test is not proving anything: " + statements.size());
    }

    @Test
    @DisplayName("each line names the partition and its position, right-aligned")
    void positionAndPartition() {
        List<String> lines = progressLines(StatementBuilder.build(plan(), twelveLeaves()));

        assertTrue(lines.get(1).startsWith("[ 1/12] public.orders_2024_01"), lines.get(1));
        assertTrue(lines.get(7).startsWith("[ 7/12] public.orders_2024_07"), lines.get(7));
        assertTrue(lines.get(12).startsWith("[12/12] public.orders_2024_12"), lines.get(12));
    }

    @Test
    @DisplayName("the line prints BEFORE the partition's work, not after")
    void lineComesFirstInTheGroup() {
        List<PlannedStatement> statements = StatementBuilder.build(plan(), twelveLeaves());

        for (int i = 0; i < statements.size(); i++) {
            String progress = statements.get(i).getProgress();
            if (progress == null || !progress.contains("public.orders_2024_07")) {
                continue;
            }
            // Everything this leaf does must come at or after the line that announces it.
            int build = -1;
            for (int j = 0; j < statements.size(); j++) {
                if (statements.get(j).getSql().contains("\"orders_2024_07\"")) {
                    build = j;
                    break;
                }
            }
            assertTrue(build > i, "announced at " + i + " but the work starts at " + build);
            return;
        }
        throw new AssertionError("no progress line for orders_2024_07");
    }

    @Test
    @DisplayName("a leaf that needs nothing done produces no line")
    void silentWhenNothingHappens() {
        TreeState state = twelveLeaves();
        state.setParentIndexExists(true);
        // A VALID parent, because an invalid one deliberately does speak -- see
        // parentInvalidStillSpeaksWhenThereIsNoOtherWork below.
        state.setParentIndexValid(true);
        for (LeafPartition leaf : state.getLeaves()) {
            leaf.addIndex(new LeafIndex("pg_chose_this_name", true, true, true));
        }

        List<String> lines = progressLines(StatementBuilder.build(plan(), state));
        assertEquals(1, lines.size(), "only the verification should speak: " + lines);
        assertTrue(lines.get(0).startsWith("verifying "), lines.get(0));
    }

    @Test
    @DisplayName("an invalid parent is announced on the progress channel, not from generateStatements")
    void parentInvalidStillSpeaksWhenThereIsNoOtherWork() {
        // Regression for the preview leak: this notice used to be printed straight to the UI from
        // generateStatements, which also runs under `liquibase updateSQL`, where nothing executes
        // and stdout may BE the migration script. Riding on a statement's progress line means
        // ProgressSqlGenerator's JdbcExecutor check keeps it out of a preview for free.
        TreeState state = twelveLeaves();
        state.setParentIndexExists(true);
        state.setParentIndexValid(false);
        for (LeafPartition leaf : state.getLeaves()) {
            leaf.addIndex(new LeafIndex("pg_chose_this_name", true, true, true));
        }

        List<String> lines = progressLines(StatementBuilder.build(plan(), state));
        assertEquals(1, lines.size(), "expected exactly one statement to carry a line: " + lines);
        assertTrue(lines.get(0).contains("indisvalid = false"), lines.get(0));
        assertTrue(lines.get(0).contains("verifying "),
                "the notice must ride ON the existing line, not replace it: " + lines.get(0));
    }

    @Test
    @DisplayName("the line says what will happen, and the repair paths say so distinctly")
    void theLineNamesTheAction() {
        TreeState state = twelveLeaves();
        state.setParentIndexExists(true);
        // leaf 1: an interrupted build left an invalid index under the conventional name
        state.getLeaves().get(0).addIndex(
                new LeafIndex("idx_orders_addr_orders_2024_01", false, false, false));
        // leaf 2: an invalid index is already ATTACHED -- the dangerous one, repaired in place
        state.getLeaves().get(1).addIndex(new LeafIndex("whatever", false, true, true));

        List<String> lines = progressLines(StatementBuilder.build(plan(), state));
        assertTrue(lines.get(0).contains("drop INVALID leftover, rebuild + attach"), lines.get(0));
        assertTrue(lines.get(1).contains("repair attached INVALID child"), lines.get(1));
        assertTrue(lines.get(2).contains("build + attach"), lines.get(2));
    }

    // ----------------------------------------------------------------- the mechanism

    @Test
    @DisplayName("the progress line rides on the statement and never reaches the SQL")
    void progressIsNotInTheSql() {
        SqlStatement[] statements =
                Statements.toSqlStatements(StatementBuilder.build(plan(), twelveLeaves()));

        int withProgress = 0;
        for (SqlStatement statement : statements) {
            ProgressSqlStatement one = (ProgressSqlStatement) statement;
            if (one.getProgress() != null) {
                withProgress++;
                assertFalse(one.getSql().contains(one.getProgress()),
                        "the progress line must not be pasted into the SQL: " + one.getSql());
            }
        }
        assertEquals(14, withProgress);
    }

    @Test
    @DisplayName("the generator emits the SQL unchanged, with RawSqlStatement's own delimiter")
    void generatorPassesTheSqlThrough() {
        ProgressSqlGenerator generator = new ProgressSqlGenerator();
        ProgressSqlStatement statement =
                new ProgressSqlStatement("SELECT 1", "[ 1/1] public.p  build + attach");

        Sql[] sql = generator.generateSql(statement, null, null);
        assertEquals(1, sql.length);
        assertEquals("SELECT 1", sql[0].toSql());
        assertEquals(";", sql[0].getEndDelimiter(),
                "RawSqlStatement uses \";\", so updateSQL output must be byte-identical");
    }

    @Test
    @DisplayName("nothing is announced unless JdbcExecutor is on the stack")
    void silentOffTheExecutionPath() {
        // This test itself is not JdbcExecutor, so the guard must be false here. That is the
        // whole mechanism: it is what keeps `liquibase updateSQL` clean and what stops every
        // line printing twice during Liquibase's up-front MDC pass over the statement array.
        assertFalse(ProgressSqlGenerator.aboutToExecute());

        ProgressSqlGenerator generator = new ProgressSqlGenerator();
        Sql[] sql = generator.generateSql(
                new ProgressSqlStatement("SELECT 1", "would have printed"), null, null);
        assertNotNull(sql);
        assertEquals("SELECT 1", sql[0].toSql());
    }

    @Test
    @DisplayName("a statement with no progress line is silent even when it does execute")
    void nullProgressIsSilent() {
        assertNull(new ProgressSqlStatement("SET lock_timeout = '15min'", null).getProgress());
    }

    @Test
    @DisplayName("skipOnUnsupported is false, so DDL can never be silently dropped")
    void neverSkipped() {
        assertFalse(new ProgressSqlStatement("SELECT 1", null).skipOnUnsupported());
    }
}
