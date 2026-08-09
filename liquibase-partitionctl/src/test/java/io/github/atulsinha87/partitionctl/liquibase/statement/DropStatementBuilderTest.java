package io.github.atulsinha87.partitionctl.liquibase.statement;

import io.github.atulsinha87.partitionctl.liquibase.catalog.DropCandidateIndex;
import io.github.atulsinha87.partitionctl.liquibase.catalog.DropLeaf;
import io.github.atulsinha87.partitionctl.liquibase.catalog.DropTarget;
import io.github.atulsinha87.partitionctl.liquibase.catalog.IndexNaming;
import io.github.atulsinha87.partitionctl.liquibase.catalog.OwnershipMarker;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * The drop planner, exercised without a database.
 *
 * <p>Every refusal here asserts two things: that the message says what is wrong, and that
 * <b>nothing at all was emitted</b>. A refusal that emitted half a plan would be worse than no
 * refusal, because Liquibase would run the half.
 */
class DropStatementBuilderTest {

    private static final String MARKER =
            OwnershipMarker.forChildIndex("idx_a", "public", "person", "changelog.xml::1::x");

    // ------------------------------------------------------------------ the confirmation

    @Test
    @DisplayName("refuses to drop an attached tree without confirmExclusiveLock, and emits nothing")
    void refusesTheTreeWithoutConfirmation() {
        DropTarget target = tree(3, true);
        PlanException e = assertThrows(PlanException.class,
                () -> DropStatementBuilder.build(plan().setConfirmExclusiveLock(false), target));

        assertTrue(e.getMessage().contains("confirmExclusiveLock=\"true\""), e.getMessage());
        assertTrue(e.getMessage().contains("all 3 leaf partition(s)"),
                "the message must name the real blast radius, not a generic warning: "
                        + e.getMessage());
        assertTrue(e.getMessage().contains("Nothing was executed"), e.getMessage());
    }

    @Test
    @DisplayName("a leftovers-only run needs no confirmation: nothing exclusive is taken")
    void leftoversOnlyNeedsNoConfirmation() {
        DropTarget target = leftoversOnly(2);
        List<PlannedStatement> statements =
                DropStatementBuilder.build(plan().setConfirmExclusiveLock(false), target);

        assertEquals(2, countMatching(statements, "DROP INDEX CONCURRENTLY"));
        assertEquals(0, countMatching(statements, "DO $partitionctl$\nDECLARE\n  attempt"),
                "no exclusive drop, so no confirmation is owed");
    }

    @Test
    @DisplayName("a run with nothing left to drop emits only the gate")
    void nothingToDoEmitsOnlyTheGate() {
        DropTarget target = base(3);
        target.setIndexRelkind(null);
        List<PlannedStatement> statements = DropStatementBuilder.build(plan(), target);

        assertEquals(1, statements.size(), sql(statements));
        assertTrue(statements.get(0).getSql().startsWith("DO $partitionctl$"));
        assertTrue(statements.get(0).getLabel().contains("nothing to drop"));
        assertEquals(0, countMatching(statements, "DROP INDEX"));
    }

    // ------------------------------------------------------------------ the ownership marker

    @Test
    @DisplayName("refuses a tree carrying no partitionctl marker anywhere")
    void refusesAnUnmarkedTree() {
        DropTarget target = tree(3, false);
        PlanException e = assertThrows(PlanException.class,
                () -> DropStatementBuilder.build(plan(), target));

        assertTrue(e.getMessage().contains("Nothing in the catalog says this plugin built it"),
                e.getMessage());
        assertTrue(e.getMessage().contains(OwnershipMarker.PREFIX), e.getMessage());
    }

    @Test
    @DisplayName("refuses a tree whose parent carries somebody else's comment, quoting it")
    void refusesAForeignCommentOnTheParent() {
        DropTarget target = tree(3, true);
        target.setIndexComment("CHG-4471 built by dba-team\nDO NOT DROP");

        PlanException e = assertThrows(PlanException.class,
                () -> DropStatementBuilder.build(plan(), target));

        assertTrue(e.getMessage().contains("CHG-4471 built by dba-team DO NOT DROP"),
                "the operator needs to see the comment that stopped the drop, on one line: "
                        + e.getMessage());
        assertFalse(e.getMessage().contains("\n"), "a refusal must stay one line: " + e.getMessage());
    }

    @Test
    @DisplayName("the Go CLI's marker is foreign: the two products do not interoperate")
    void refusesTheGoCliMarker() {
        DropTarget target = tree(3, false);
        target.setIndexComment("partitionctl:v1:{\"run\":\"run-7\",\"role\":\"parent\"}");

        PlanException e = assertThrows(PlanException.class,
                () -> DropStatementBuilder.build(plan(), target));
        assertTrue(e.getMessage().contains("partitionctl:v1:"), e.getMessage());
    }

    @Test
    @DisplayName("a marker on any attached child is enough, because PostgreSQL names some children itself")
    void aMarkedChildIsSufficientEvidence() {
        DropTarget target = base(3);
        target.setIndexRelkind("I");
        target.setIndexComment(null);
        // Leaf 1 was built by the plugin and stamped. Leaves 2 and 3 were partitions added
        // later, whose child indexes PostgreSQL created and named itself -- no comment, and a
        // name the convention would never generate.
        attach(target.getLeaves().get(0), "idx_a_person_p01", MARKER);
        attach(target.getLeaves().get(1), "person_p02_address_idx", null);
        attach(target.getLeaves().get(2), "person_p03_address_idx", null);

        List<PlannedStatement> statements = DropStatementBuilder.build(plan(), target);
        assertEquals(1, countMatching(statements, "DROP INDEX \"public\".\"idx_a\";"));
    }

    @Test
    @DisplayName("refuses unmarked leftovers and emits nothing, even when the tree itself is ours")
    void refusesUnmarkedLeftovers() {
        DropTarget target = tree(3, true);
        // A fourth leaf carrying a hand-built index at exactly the generated name.
        DropLeaf leaf = leaf(target, "person_p04");
        leaf.addIndex(new DropCandidateIndex(
                IndexNaming.childIndexName("idx_a", "person_p04"), true, false, false, null));

        PlanException e = assertThrows(PlanException.class,
                () -> DropStatementBuilder.build(plan(), target));

        assertTrue(e.getMessage().contains("carry no partitionctl marker"), e.getMessage());
        assertTrue(e.getMessage().contains("idx_a_person_p04"), e.getMessage());
        assertTrue(e.getMessage().contains("(no comment)"), e.getMessage());
        assertTrue(e.getMessage().contains("one changeset is one intent"),
                "refusing one object must be explained as refusing the whole run: " + e.getMessage());
    }

    // ------------------------------------------------------------------ the two paths

    @Test
    @DisplayName("mixed tree: leftovers go concurrently FIRST, then the exclusive drop")
    void mixedTreeDropsLeftoversFirst() {
        DropTarget target = tree(3, true);
        DropLeaf spare = leaf(target, "person_p04");
        spare.addIndex(new DropCandidateIndex(
                IndexNaming.childIndexName("idx_a", "person_p04"), false, false, false, MARKER));

        List<PlannedStatement> statements = DropStatementBuilder.build(plan(), target);
        String joined = sql(statements);

        int concurrent = joined.indexOf("DROP INDEX CONCURRENTLY \"public\".\"idx_a_person_p04\"");
        int exclusive = joined.indexOf("DROP INDEX \"public\".\"idx_a\";");
        assertTrue(concurrent >= 0, joined);
        assertTrue(exclusive >= 0, joined);
        assertTrue(concurrent < exclusive,
                "the online, individually-resumable work must come first: " + joined);
    }

    @Test
    @DisplayName("an attached child is never named by a DROP of its own")
    void neverDropsAnAttachedChildIndividually() {
        DropTarget target = tree(3, true);
        String joined = sql(DropStatementBuilder.build(plan(), target));

        for (DropLeaf leaf : target.getLeaves()) {
            String child = leaf.getAttachedChild().getIndexName();
            assertFalse(joined.contains("DROP INDEX \"public\".\"" + child + "\""),
                    "PostgreSQL rejects dropping an attached child on its own, and there is no "
                            + "DETACH to separate it first: " + joined);
        }
    }

    @Test
    @DisplayName("a leftover is dropped even when invalid -- an interrupted build's wreckage")
    void dropsAnInvalidLeftover() {
        DropTarget target = leftoversOnly(1);
        List<PlannedStatement> statements = DropStatementBuilder.build(plan(), target);
        assertTrue(sql(statements).contains("DROP INDEX CONCURRENTLY \"public\".\"idx_a_person_p01\""),
                sql(statements));
    }

    @Test
    @DisplayName("an index on a leaf that is not at the generated name is left alone")
    void ignoresUnrelatedIndexesOnLeaves() {
        DropTarget target = base(1);
        target.setIndexRelkind(null);
        target.getLeaves().get(0).addIndex(
                new DropCandidateIndex("somebody_elses_idx", true, false, false, MARKER));

        List<PlannedStatement> statements = DropStatementBuilder.build(plan(), target);
        assertEquals(1, statements.size(), "only the gate: " + sql(statements));
        assertFalse(sql(statements).contains("somebody_elses_idx"), sql(statements));
    }

    // ------------------------------------------------------------------ wrong target

    @Test
    @DisplayName("refuses an ordinary index and points at Liquibase's own dropIndex")
    void refusesAnOrdinaryIndex() {
        DropTarget target = base(2);
        target.setIndexRelkind("i");
        PlanException e = assertThrows(PlanException.class,
                () -> DropStatementBuilder.build(plan(), target));
        assertTrue(e.getMessage().contains("<dropIndex>"), e.getMessage());
    }

    @Test
    @DisplayName("refuses a partitioned index that belongs to a different table")
    void refusesAnIndexOnAnotherTable() {
        DropTarget target = tree(2, true);
        target.setIndexOwningTable("public.other_table");

        PlanException e = assertThrows(PlanException.class,
                () -> DropStatementBuilder.build(plan(), target));
        assertTrue(e.getMessage().contains("is a partitioned index on public.other_table"),
                e.getMessage());
    }

    @Test
    @DisplayName("refuses a table that is not partitioned, and one that does not exist")
    void refusesABadTable() {
        DropTarget ordinary = base(0);
        ordinary.setRootRelkind("r");
        assertTrue(assertThrows(PlanException.class,
                () -> DropStatementBuilder.build(plan(), ordinary))
                .getMessage().contains("is not a partitioned table"));

        DropTarget missing = base(0);
        missing.setRootRelkind(null);
        assertTrue(assertThrows(PlanException.class,
                () -> DropStatementBuilder.build(plan(), missing))
                .getMessage().contains("does not exist"));
    }

    // ------------------------------------------------------------------ timeouts

    @Test
    @DisplayName("lock_timeout: 15min for the concurrent drops, 5s for the exclusive one, restored after")
    void lockTimeoutsDifferByStatement() {
        DropTarget target = tree(2, true);
        target.setOriginalLockTimeout("250ms");
        DropLeaf spare = leaf(target, "person_p09");
        spare.addIndex(new DropCandidateIndex(
                IndexNaming.childIndexName("idx_a", "person_p09"), true, false, false, MARKER));

        List<String> sql = statementsOf(DropStatementBuilder.build(plan(), target));

        assertEquals("SET lock_timeout = '15min'", sql.get(0));
        assertTrue(sql.get(1).startsWith("DROP INDEX CONCURRENTLY"), sql.toString());
        assertEquals("SET lock_timeout = '5s'", sql.get(2));
        // The exclusive block always bounds itself, whatever the adopter arrived with.
        assertEquals("SET statement_timeout = '5min'", sql.get(3));
        assertTrue(sql.get(4).startsWith("DO $partitionctl$"), sql.toString());
        assertEquals("SET statement_timeout = '0'", sql.get(5));
        assertEquals("SET lock_timeout = '250ms'", sql.get(6));
    }

    @Test
    @DisplayName("statement_timeout is lifted around the concurrent drop and BOUNDED around the retry loop")
    void statementTimeoutLiftedWhereItMatters() {
        DropTarget target = tree(2, true);
        target.setOriginalStatementTimeout("30s");
        DropLeaf spare = leaf(target, "person_p09");
        spare.addIndex(new DropCandidateIndex(
                IndexNaming.childIndexName("idx_a", "person_p09"), true, false, false, MARKER));

        List<String> sql = statementsOf(DropStatementBuilder.build(plan(), target));

        // DROP INDEX CONCURRENTLY waits for concurrent transactions exactly as CIC does;
        // measured cancelled at 197ms under a 200ms adopter statement_timeout.
        assertEquals("SET statement_timeout = 0", sql.get(0));
        assertTrue(sql.get(2).startsWith("DROP INDEX CONCURRENTLY"), sql.toString());
        assertEquals("SET statement_timeout = '30s'", sql.get(3));

        // The retry loop gets a FINITE budget, not 0. lock_timeout bounds one lock acquisition,
        // and DROP INDEX on a partitioned parent takes one per leaf plus two, holding each while
        // it waits for the next -- measured on 17.10, 8 leaves with two contended under the 5s
        // default, the drop held the table for 6876 ms. statement_timeout = 0 removed the only
        // bound that was not per-lock. Cancelling is safe: the block is one transaction, so a
        // cancel rolls it back completely and nothing is dropped.
        int loop = indexOfFirst(sql, "DO $partitionctl$\nDECLARE\n  attempt");
        assertEquals("SET statement_timeout = '5min'", sql.get(loop - 1));
        assertEquals("SET statement_timeout = '30s'", sql.get(loop + 1));
    }

    @Test
    @DisplayName("the exclusive retry loop is NEVER emitted with statement_timeout = 0")
    void theRetryLoopIsAlwaysBounded() {
        // The regression that matters: an unbounded retry loop can hold AccessExclusiveLock on the
        // table and every leaf for exclusiveLockTimeout x (leaves + 2) per attempt, times the
        // retries, with nothing capping the total.
        for (String adopterTimeout : new String[] { "0", "30s", "200ms" }) {
            DropTarget target = tree(3, true);
            target.setOriginalStatementTimeout(adopterTimeout);
            List<String> sql = statementsOf(DropStatementBuilder.build(plan(), target));
            int loop = indexOfFirst(sql, "DO $partitionctl$\nDECLARE\n  attempt");
            assertEquals("SET statement_timeout = '5min'", sql.get(loop - 1),
                    "adopter statement_timeout=" + adopterTimeout + ": " + sql);
            assertEquals("SET statement_timeout = " + literalOf(adopterTimeout), sql.get(loop + 1),
                    "the session must be put back exactly as found: " + sql);
        }
    }

    private static String literalOf(String value) {
        return "'" + value + "'";
    }

    @Test
    @DisplayName("on the default statement_timeout only the exclusive block sets one, and restores 0")
    void noStatementTimeoutTrafficOnTheDefault() {
        // The concurrent paths stay silent when the adopter is already at PostgreSQL's default of
        // 0, because there is nothing to lift. The exclusive block is the exception on purpose: it
        // sets a finite ceiling regardless, since 0 there means "hold the table indefinitely".
        DropTarget target = tree(2, true);
        target.setOriginalStatementTimeout("0");
        List<String> sql = statementsOf(DropStatementBuilder.build(plan(), target));
        assertEquals(2, countOf(sql, "SET statement_timeout"), sql.toString());
        assertEquals("SET statement_timeout = '5min'", sql.get(indexOfFirst(sql,
                "DO $partitionctl$\nDECLARE\n  attempt") - 1));
        assertEquals("SET statement_timeout = '0'", sql.get(indexOfFirst(sql,
                "DO $partitionctl$\nDECLARE\n  attempt") + 1));
    }

    private static int countOf(List<String> sql, String prefix) {
        int n = 0;
        for (String one : sql) {
            if (one.startsWith(prefix)) {
                n++;
            }
        }
        return n;
    }

    // ------------------------------------------------------------------ shape invariants

    @Test
    @DisplayName("the concurrent drop is never inside a DO block: PostgreSQL rejects that outright")
    void concurrentDropIsNeverInsideAFunction() {
        DropTarget target = tree(2, true);
        DropLeaf spare = leaf(target, "person_p09");
        spare.addIndex(new DropCandidateIndex(
                IndexNaming.childIndexName("idx_a", "person_p09"), true, false, false, MARKER));

        for (PlannedStatement statement : DropStatementBuilder.build(plan(), target)) {
            if (statement.getSql().contains("CONCURRENTLY")) {
                assertFalse(statement.getSql().contains("$partitionctl$"),
                        "ERROR: DROP INDEX CONCURRENTLY cannot be executed from a function -- "
                                + statement.getSql());
            }
        }
    }

    @Test
    @DisplayName("the retry loop honours exclusiveRetries and exclusiveLockTimeout")
    void retryLoopUsesTheAttributes() {
        DropTarget target = tree(2, true);
        String sql = sql(DropStatementBuilder.build(
                plan().setExclusiveRetries(2).setExclusiveLockTimeout("900ms"), target));

        assertTrue(sql.contains("IF attempt >= 2 THEN"), sql);
        assertTrue(sql.contains("'900ms'"), sql);
        assertTrue(sql.contains("SET lock_timeout = '900ms'"), sql);
        assertTrue(sql.contains("EXCEPTION WHEN lock_not_available THEN"), sql);
        assertTrue(sql.contains("PERFORM pg_sleep(backoff)"), sql);
        assertTrue(sql.contains("backoff := backoff * 2"), sql);
    }

    @Test
    @DisplayName("the gate checks the parent AND every leftover it planned to remove")
    void gateChecksEverythingItPlanned() {
        DropTarget target = tree(2, true);
        DropLeaf spare = leaf(target, "person_p09");
        spare.addIndex(new DropCandidateIndex(
                IndexNaming.childIndexName("idx_a", "person_p09"), true, false, false, MARKER));

        List<PlannedStatement> statements = DropStatementBuilder.build(plan(), target);
        String gate = statements.get(statements.size() - 1).getSql();

        assertTrue(gate.contains("c.relkind = 'I'"), gate);
        assertTrue(gate.contains("still exists after the drop"), gate);
        assertTrue(gate.contains("'idx_a_person_p09'"),
                "a leftover that survives must fail the run too: " + gate);
    }

    @Test
    @DisplayName("identifiers and comment text are quoted, not concatenated raw")
    void quotesIdentifiersAndLiterals() {
        DropTarget target = base(1);
        target.setIndexRelkind("I");
        target.setIndexComment(MARKER);
        // The owning-table guard compares this against schemaName + '.' + tableName, so it has
        // to agree with the plan below or that guard fires before the quoting is exercised.
        target.setIndexOwningTable("pu'blic.per\"son");
        attach(target.getLeaves().get(0), "child", MARKER);

        DropIndexPlan plan = plan().setSchemaName("pu'blic").setTableName("per\"son")
                .setIndexName("id\"x");

        String sql = sql(DropStatementBuilder.build(plan, target));
        assertTrue(sql.contains("DROP INDEX \"pu'blic\".\"id\"\"x\""), sql);
        assertTrue(sql.contains("'pu''blic'"), sql);
    }

    // ------------------------------------------------------------------ fixtures

    private static DropIndexPlan plan() {
        return new DropIndexPlan()
                .setSchemaName("public")
                .setTableName("person")
                .setIndexName("idx_a")
                .setConfirmExclusiveLock(true);
    }

    /** A partitioned table with {@code leaves} leaves and no indexes anywhere. */
    private static DropTarget base(int leaves) {
        DropTarget target = new DropTarget();
        target.setRootRelkind("p");
        target.setOriginalStatementTimeout("0");
        target.setOriginalLockTimeout("0");
        target.setIndexOwningTable("public.person");
        for (int i = 1; i <= leaves; i++) {
            leaf(target, String.format("person_p%02d", i));
        }
        return target;
    }

    /** A fully built, fully attached tree. */
    // ------------------------------------------------------------------ ownership evidence

    @Test
    @DisplayName("a parent with NO attached children is still ours if a marked leftover says so")
    void markedLeftoversAreEvidenceForTheTreeToo() {
        // The window this covers is not exotic: between CREATE INDEX ON ONLY and the FIRST
        // successful ATTACH there are zero attached children, and that window lasts as long as
        // leaf 1's CREATE INDEX CONCURRENTLY -- hours on a large partition. A run interrupted
        // there leaves exactly this state: the parent index, plus correctly marked child indexes
        // attached to nothing.
        //
        // Judging the tree on attached children alone refused it with "Nothing in the catalog says
        // this plugin built it" and told the operator to drop it by hand, while offering to drop
        // the very leftovers that carry the evidence, in the same changeset.
        DropTarget target = base(3);
        target.setIndexRelkind("I");
        target.setIndexValid(false);
        target.setIndexComment(null);          // create never stamps the parent, by design
        for (DropLeaf leaf : target.getLeaves()) {
            leaf.addIndex(new DropCandidateIndex(
                    IndexNaming.childIndexName("idx_a", leaf.getTableName()),
                    true, false, false, MARKER));
        }

        List<PlannedStatement> statements =
                DropStatementBuilder.build(plan().setConfirmExclusiveLock(true), target);

        assertEquals(3, countMatching(statements, "DROP INDEX CONCURRENTLY"),
                "the three unattached leftovers come off online");
        assertEquals(1, countMatching(statements, "DO $partitionctl$\nDECLARE\n  attempt"),
                "and the parent index itself is dropped: " + sql(statements));
    }

    @Test
    @DisplayName("an unmarked parent with unmarked leftovers is still refused")
    void unmarkedEverythingIsStillRefused() {
        // The control. Without it the test above would pass on a build that had simply stopped
        // checking ownership.
        DropTarget target = base(2);
        target.setIndexRelkind("I");
        target.setIndexComment(null);
        for (DropLeaf leaf : target.getLeaves()) {
            leaf.addIndex(new DropCandidateIndex(
                    IndexNaming.childIndexName("idx_a", leaf.getTableName()),
                    true, false, false, null));
        }

        PlanException e = assertThrows(PlanException.class,
                () -> DropStatementBuilder.build(plan().setConfirmExclusiveLock(true), target));
        assertTrue(e.getMessage().contains("Nothing in the catalog says this plugin built it"),
                e.getMessage());
        assertTrue(e.getMessage().contains("Nothing was executed"), e.getMessage());
    }

    @Test
    @DisplayName("a FOREIGN comment on the parent still overrides any amount of leftover evidence")
    void foreignParentCommentStillWins() {
        DropTarget target = base(2);
        target.setIndexRelkind("I");
        target.setIndexComment("CHG-4471 built by dba-team\nDO NOT DROP");
        for (DropLeaf leaf : target.getLeaves()) {
            leaf.addIndex(new DropCandidateIndex(
                    IndexNaming.childIndexName("idx_a", leaf.getTableName()),
                    true, false, false, MARKER));
        }

        PlanException e = assertThrows(PlanException.class,
                () -> DropStatementBuilder.build(plan().setConfirmExclusiveLock(true), target));
        assertTrue(e.getMessage().contains("written by something other than this plugin"),
                e.getMessage());
    }

    @Test
    @DisplayName("a foreign comment on ONE attached child refuses the drop, whatever the siblings say")
    void aForeignChildRefusesEvenWhenSiblingsAreOurs() {
        // The reported case: a security review built one index by hand, labelled it with a ticket
        // number, then ran create (which absorbed it) and drop. Eleven marked siblings said the
        // tree was ours, so the drop proceeded and the labelled index went from 1 to 0 with
        // BUILD SUCCESS and no warning. DROP INDEX on the parent takes every attached child with
        // it in one statement and PostgreSQL has no ALTER INDEX ... DETACH PARTITION, so there is
        // no way to spare one -- the only safe answer is to refuse the whole drop.
        DropTarget target = base(3);
        target.setIndexRelkind("I");
        target.setIndexValid(true);
        target.setIndexComment(null);
        attach(target.getLeaves().get(0),
                IndexNaming.childIndexName("idx_a", target.getLeaves().get(0).getTableName()), MARKER);
        attach(target.getLeaves().get(1),
                IndexNaming.childIndexName("idx_a", target.getLeaves().get(1).getTableName()), MARKER);
        attach(target.getLeaves().get(2),
                IndexNaming.childIndexName("idx_a", target.getLeaves().get(2).getTableName()),
                "DBA ticket OPS-4471 -- do not remove, backs the month-end reconciliation report");

        PlanException e = assertThrows(PlanException.class,
                () -> DropStatementBuilder.build(plan().setConfirmExclusiveLock(true), target));
        assertTrue(e.getMessage().contains("OPS-4471"), e.getMessage());
        assertTrue(e.getMessage().contains("written by something other than this plugin"),
                e.getMessage());
    }

    @Test
    @DisplayName("an ABSENT comment on a child is still fine: PostgreSQL names later children itself")
    void anUnmarkedChildIsStillAcceptable() {
        DropTarget target = base(3);
        target.setIndexRelkind("I");
        target.setIndexValid(true);
        target.setIndexComment(null);
        attach(target.getLeaves().get(0),
                IndexNaming.childIndexName("idx_a", target.getLeaves().get(0).getTableName()), MARKER);
        attach(target.getLeaves().get(1), "person_p02_address_idx", null);
        attach(target.getLeaves().get(2), "person_p03_address_idx", null);

        assertFalse(DropStatementBuilder.build(plan().setConfirmExclusiveLock(true), target).isEmpty());
    }

    private static DropTarget tree(int leaves, boolean marked) {
        DropTarget target = base(leaves);
        target.setIndexRelkind("I");
        target.setIndexValid(true);
        target.setIndexComment(marked ? MARKER : null);
        for (DropLeaf leaf : target.getLeaves()) {
            attach(leaf, IndexNaming.childIndexName("idx_a", leaf.getTableName()),
                    marked ? MARKER : null);
        }
        return target;
    }

    /** No parent index at all, just free-standing leftovers on the first {@code n} leaves. */
    private static DropTarget leftoversOnly(int n) {
        DropTarget target = base(n);
        target.setIndexRelkind(null);
        target.setIndexOwningTable(null);
        for (DropLeaf leaf : target.getLeaves()) {
            leaf.addIndex(new DropCandidateIndex(
                    IndexNaming.childIndexName("idx_a", leaf.getTableName()),
                    false, false, false, MARKER));
        }
        return target;
    }

    private static DropLeaf leaf(DropTarget target, String name) {
        DropLeaf leaf = new DropLeaf("public", name);
        leaf.setChildIndexName(IndexNaming.childIndexName("idx_a", name));
        target.addLeaf(leaf);
        return leaf;
    }

    private static void attach(DropLeaf leaf, String indexName, String comment) {
        leaf.addIndex(new DropCandidateIndex(indexName, true, true, true, comment));
    }

    // ------------------------------------------------------------------ helpers

    private static List<String> statementsOf(List<PlannedStatement> statements) {
        java.util.List<String> sql = new java.util.ArrayList<String>();
        for (PlannedStatement statement : statements) {
            sql.add(statement.getSql());
        }
        return sql;
    }

    private static String sql(List<PlannedStatement> statements) {
        StringBuilder sb = new StringBuilder();
        for (PlannedStatement statement : statements) {
            sb.append(statement.toSql()).append(";\n");
        }
        return sb.toString();
    }

    private static int countMatching(List<PlannedStatement> statements, String needle) {
        int n = 0;
        for (PlannedStatement statement : statements) {
            if (statement.getSql().contains(needle)) {
                n++;
            }
        }
        return n;
    }

    private static int indexOfFirst(List<String> sql, String needle) {
        for (int i = 0; i < sql.size(); i++) {
            if (sql.get(i).contains(needle)) {
                return i;
            }
        }
        throw new AssertionError("not found: " + needle + " in " + sql);
    }
}
