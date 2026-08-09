package io.github.atulsinha87.partitionctl.liquibase.statement;

import io.github.atulsinha87.partitionctl.liquibase.catalog.DropCandidateIndex;
import io.github.atulsinha87.partitionctl.liquibase.catalog.DropLeaf;
import io.github.atulsinha87.partitionctl.liquibase.catalog.DropTarget;
import io.github.atulsinha87.partitionctl.liquibase.catalog.Identifiers;
import io.github.atulsinha87.partitionctl.liquibase.catalog.OwnershipMarker;

import java.util.ArrayList;
import java.util.List;

/**
 * Turns a discovered {@link DropTarget} into the statement list for
 * {@code <dropPartitionedTableIndex>}.
 *
 * <p>Pure: same inputs, same output, no I/O. Required, because {@code generateStatements} is
 * called about seven times per update.
 *
 * <h2>Two paths, because PostgreSQL has two situations</h2>
 * <table>
 *   <caption>How each of the two shapes is removed, and at what lock cost</caption>
 *   <tr><th></th><th>attached tree</th><th>free-standing leftover</th></tr>
 *   <tr><td>what it is</td>
 *       <td>the partitioned index plus every child index that inherits from it</td>
 *       <td>an index left on one leaf by an interrupted build, inheriting from nothing</td></tr>
 *   <tr><td>how it comes off</td>
 *       <td>{@code DROP INDEX} on the parent — AccessExclusiveLock on the parent table and
 *           <em>every</em> leaf</td>
 *       <td>{@code DROP INDEX CONCURRENTLY} — ShareUpdateExclusiveLock, online</td></tr>
 * </table>
 *
 * <p>All re-confirmed against PostgreSQL 17.10 while writing this class:
 * <pre>
 * DROP INDEX CONCURRENTLY &lt;partitioned parent&gt;  ERROR: cannot drop partitioned index "idx_probe" concurrently
 * DROP INDEX &lt;attached child&gt;                   ERROR: cannot drop index idx_probe_probe_p01
 *                                                     because index idx_probe requires it
 * DROP INDEX CONCURRENTLY inside a DO block      ERROR: DROP INDEX CONCURRENTLY cannot be
 *                                                     executed from a function
 * ALTER INDEX ... DETACH PARTITION               ERROR: syntax error at or near "DETACH"
 * </pre>
 * So there is no online path for an attached tree, and no way to peel a child off one first.
 * That is not a limitation of this code; it is the shape of the server.
 *
 * <h2>Order: leftovers first, then the tree</h2>
 * The concurrent drops are the cheap, online, individually-resumable half. Doing them first
 * means a failure at the exclusive step leaves strictly less work behind than it found, and the
 * re-run — a failed changeset is never recorded, so Liquibase retries it — re-discovers and
 * emits only the exclusive drop.
 *
 * <h2>Both guards are required and neither replaces the other</h2>
 * The ownership marker answers <em>is this ours</em>. {@code confirmExclusiveLock} answers
 * <em>do you accept locking the whole table</em>. An index can be unmistakably ours and still
 * be catastrophic to drop at 09:00 on a Monday.
 */
public final class DropStatementBuilder {

    /** First backoff after a failed exclusive attempt, in seconds. Doubles each time. */
    private static final int FIRST_BACKOFF_SECONDS = 1;

    private final DropIndexPlan plan;
    private final DropTarget target;
    private final List<PlannedStatement> out = new ArrayList<PlannedStatement>();

    private boolean lockTimeoutTouched;
    private boolean statementTimeoutOverridden;

    public DropStatementBuilder(DropIndexPlan plan, DropTarget target) {
        this.plan = plan;
        this.target = target;
    }

    public static List<PlannedStatement> build(DropIndexPlan plan, DropTarget target) {
        return new DropStatementBuilder(plan, target).build();
    }

    public List<PlannedStatement> build() {
        guardTargetTable();
        guardNamedRelation();

        List<DropLeaf> orphanLeaves = target.leavesWithOrphans();
        boolean tree = target.isPartitionedIndexPresent();

        if (!tree && orphanLeaves.isEmpty()) {
            // The common steady state once the drop has succeeded once. Emit the gate and
            // nothing else, so a re-run is a single fast catalog read that says so out loud.
            emit(verifySql(orphanLeaves), "nothing to drop: no partitioned index "
                    + qualifiedIndex() + " and no leftover child indexes");
            return out;
        }

        // Refuse before emitting anything. Everything below this line is all-or-nothing:
        // a refusal leaves the database exactly as it was found.
        guardOwnership(tree, orphanLeaves);
        guardExclusiveLockConfirmed(tree);

        emitOrphanDrops(orphanLeaves);
        if (tree) {
            emitExclusiveTreeDrop();
        }
        restoreLockTimeout();

        emit(verifySql(orphanLeaves), "verifying " + qualifiedIndex()
                + " and every leftover child index are gone");
        return out;
    }

    // ------------------------------------------------------------------ guards

    private void guardTargetTable() {
        String kind = target.getRootRelkind();
        if (kind == null) {
            throw new PlanException("partitionctl: table " + plan.getSchemaName() + "."
                    + plan.getTableName() + " does not exist, so its partition tree cannot be "
                    + "inspected and nothing can be dropped safely. Nothing was executed. "
                    + "If the table was dropped deliberately, its indexes went with it and this "
                    + "changeset can be removed.");
        }
        if (!"p".equals(kind)) {
            throw new PlanException("partitionctl: " + plan.getSchemaName() + "."
                    + plan.getTableName() + " is not a partitioned table (pg_class.relkind = '"
                    + kind + "', expected 'p'). Use Liquibase's own <dropIndex> for an index on "
                    + "an ordinary table. Nothing was executed.");
        }
    }

    private void guardNamedRelation() {
        String kind = target.getIndexRelkind();
        if (kind == null) {
            return; // Nothing of that name exists. Leftovers may still, and are handled below.
        }
        if ("i".equals(kind)) {
            throw new PlanException("partitionctl: " + qualifiedIndex() + " is an ordinary index "
                    + "(pg_class.relkind = 'i'), not a partitioned index. dropPartitionedTableIndex "
                    + "only drops the partitioned index that <createPartitionedTableIndex> builds. "
                    + "Use Liquibase's own <dropIndex> for this one. Nothing was executed.");
        }
        if (!"I".equals(kind)) {
            throw new PlanException("partitionctl: " + qualifiedIndex() + " is not an index at all "
                    + "(pg_class.relkind = '" + kind + "'). Nothing was executed.");
        }
        String owner = target.getIndexOwningTable();
        String expected = plan.getSchemaName() + "." + plan.getTableName();
        if (owner != null && !owner.equals(expected)) {
            // Index names are unique per schema but carry no hint of which table they belong to.
            // Without this check a changeset naming the right index and the wrong table would
            // drop the right index anyway, and the operator's mistake would never surface.
            throw new PlanException("partitionctl: " + qualifiedIndex() + " is a partitioned index "
                    + "on " + owner + ", not on the " + expected + " named by this changeset. "
                    + "Refusing to drop an index on a table the changeset did not name. "
                    + "Nothing was executed.");
        }
    }

    /**
     * The marker check.
     *
     * <p>Evidence, deliberately not authorisation: anyone who can run this changeset can already
     * run {@code DROP INDEX} by hand, and anyone who can write a comment can forge the marker.
     * What it stops is the accident — a copied changelog, a typo'd {@code indexName}, a
     * collision with an index the DBA team built under the same name — and those are the ways an
     * index actually gets deleted by mistake.
     *
     * <p>The tree and the leftovers are judged separately because they die by different
     * statements. For the tree the evidence may come from the parent's own comment <em>or</em>
     * from any attached child's, because PostgreSQL creates and names the child index itself
     * when a partition is attached to an already-indexed table, and those children carry no
     * comment. Requiring every child to be marked would refuse to drop trees this plugin
     * demonstrably built.
     */
    private void guardOwnership(boolean tree, List<DropLeaf> orphanLeaves) {
        if (tree) {
            guardTreeOwnership(orphanLeaves);
        }
        guardOrphanOwnership(orphanLeaves);
    }

    /**
     * @param orphanLeaves the free-standing leftovers found in the same discovery. A marker on one
     *                     of those counts as evidence for the tree too — see below.
     */
    private void guardTreeOwnership(List<DropLeaf> orphanLeaves) {
        String comment = target.getIndexComment();
        List<DropCandidateIndex> children = target.attachedChildren();

        if (OwnershipMarker.isForeign(comment)) {
            throw new PlanException("partitionctl: refusing to drop " + qualifiedIndex()
                    + ". Its COMMENT was written by something other than this plugin: \""
                    + oneLine(comment) + "\". A comment that is not a partitionctl marker is a "
                    + "positive signal to keep hands off, not merely missing evidence. "
                    + blastRadius() + " Nothing was executed. Drop it by hand if you are sure.");
        }
        if (OwnershipMarker.isOurs(comment)) {
            return;
        }

        int markedChildren = 0;
        String foreignChild = null;
        for (DropCandidateIndex child : children) {
            if (OwnershipMarker.isOurs(child.getComment())) {
                markedChildren++;
            } else if (foreignChild == null && OwnershipMarker.isForeign(child.getComment())) {
                foreignChild = child.getIndexName() + ": \"" + oneLine(child.getComment()) + "\"";
            }
        }

        // A foreign comment on ANY attached child refuses the whole drop, for exactly the reason
        // stated above for the parent: a comment that is not ours is a positive signal to keep
        // hands off. This check must precede the markedChildren test, because DROP INDEX on the
        // parent destroys every attached child in one statement and there is no way to spare one
        // -- PostgreSQL has no ALTER INDEX ... DETACH PARTITION.
        //
        // Before this guard, one marked sibling authorised destroying the whole tree. Measured:
        // a hand-built index labelled 'DBA ticket OPS-4471 -- do not remove' was dropped with
        // BUILD SUCCESS and no warning, because eleven siblings the plugin had built said the
        // tree was ours. The count went 1 -> 0.
        //
        // An ABSENT comment stays acceptable: PostgreSQL names and creates child indexes itself
        // when a partition is attached later, and those carry no comment. Only a present, foreign
        // one refuses.
        if (foreignChild != null) {
            throw new PlanException("partitionctl: refusing to drop " + qualifiedIndex()
                    + ". One of its attached child indexes carries a COMMENT written by something "
                    + "other than this plugin -- " + foreignChild + ". Dropping the partitioned "
                    + "index destroys every attached child with it in the same statement, and "
                    + "PostgreSQL offers no way to detach one first, so that index would go too. "
                    + blastRadius() + " Nothing was executed. Remove the comment if the index is "
                    + "in fact ours to drop, or drop the tree by hand if you are sure.");
        }
        if (markedChildren > 0) {
            return;
        }

        // A marked free-standing leftover is evidence too, and without this the one state the
        // create change is most likely to leave behind is unrecoverable through the tool.
        // Between CREATE INDEX ON ONLY and the FIRST successful ATTACH there are zero attached
        // children -- a window as long as leaf 1's CREATE INDEX CONCURRENTLY, so hours on a large
        // partition. A run interrupted in that window leaves the parent plus correctly marked,
        // unattached child indexes, and judging the tree on attached children alone refused it
        // with "Nothing in the catalog says this plugin built it" while pointing at leftovers the
        // same changeset was willing to drop in the same run.
        int markedOrphans = 0;
        for (DropLeaf leaf : orphanLeaves) {
            if (OwnershipMarker.isOurs(leaf.getOrphan().getComment())) {
                markedOrphans++;
            }
        }
        if (markedOrphans > 0) {
            return;
        }

        throw new PlanException("partitionctl: refusing to drop " + qualifiedIndex()
                + ". Nothing in the catalog says this plugin built it: the partitioned index "
                + "itself carries " + (comment == null ? "no comment" : "an empty comment")
                + ", none of its " + children.size() + " attached child index(es) carries a "
                + "partitionctl marker, and neither does any of the " + orphanLeaves.size()
                + " unattached leftover(s)"
                + ". createPartitionedTableIndex stamps every child index it builds with a COMMENT "
                + "beginning \"" + OwnershipMarker.PREFIX + "\". " + blastRadius()
                + " Nothing was executed. If this index was built by hand, or by the partitionctl "
                + "Go CLI -- a separate product that writes a different marker and does not "
                + "interoperate with this one -- drop it with plain SQL instead.");
    }

    private void guardOrphanOwnership(List<DropLeaf> orphanLeaves) {
        List<String> unmarked = new ArrayList<String>();
        for (DropLeaf leaf : orphanLeaves) {
            DropCandidateIndex orphan = leaf.getOrphan();
            if (!OwnershipMarker.isOurs(orphan.getComment())) {
                unmarked.add(Identifiers.qualified(leaf.getSchemaName(), orphan.getIndexName())
                        + (orphan.getComment() == null ? " (no comment)"
                           : " (comment: \"" + oneLine(orphan.getComment()) + "\")"));
            }
        }
        if (unmarked.isEmpty()) {
            return;
        }
        throw new PlanException("partitionctl: refusing to drop " + unmarked.size()
                + " unattached index(es) that carry no partitionctl marker: " + join(unmarked)
                + ". They are named exactly what createPartitionedTableIndex would name the child "
                + "indexes of " + qualifiedIndex() + ", but nothing says this plugin built them. "
                + "Nothing was executed -- including the rest of this changeset, because one "
                + "changeset is one intent. If they are leftovers from a createPartitionedTableIndex "
                + "run that was interrupted between the CREATE INDEX CONCURRENTLY and the COMMENT "
                + "that stamps it, re-running createPartitionedTableIndex will drop and rebuild "
                + "them. Otherwise remove them by hand with DROP INDEX CONCURRENTLY.");
    }

    private void guardExclusiveLockConfirmed(boolean tree) {
        if (!tree || plan.isConfirmExclusiveLock()) {
            return;
        }
        // Deliberately a plan-time check and not a required XSD attribute. Required-always would
        // make the attribute meaningless on the runs where no exclusive lock is taken at all --
        // the leftovers-only path is fully online -- and would fail a no-op re-run that has
        // nothing left to drop.
        throw new PlanException("partitionctl: dropPartitionedTableIndex requires "
                + "confirmExclusiveLock=\"true\" before it will drop " + qualifiedIndex() + ". "
                + blastRadius()
                + " PostgreSQL offers no online alternative: DROP INDEX CONCURRENTLY is rejected "
                + "on a partitioned index, an attached child index cannot be dropped on its own, "
                + "and there is no ALTER INDEX ... DETACH PARTITION to separate one first. "
                + "A changeset that names a single index does not look like it stalls an entire "
                + "table, so the acknowledgement is explicit. Nothing was executed. Add "
                + "confirmExclusiveLock=\"true\" to the <dropPartitionedTableIndex> element, "
                + "ideally in a maintenance window.");
    }

    private String blastRadius() {
        int leaves = target.getLeaves().size();
        int children = target.attachedChildren().size();
        return "DROP INDEX on " + qualifiedIndex() + " takes an AccessExclusiveLock on "
                + plan.getSchemaName() + "." + plan.getTableName() + " and on all " + leaves
                + " leaf partition(s) at once, and destroys the " + children
                + " attached child index(es) with it; every read and every write against any "
                + "partition of the table blocks until it completes, and a queued exclusive "
                + "request blocks everything behind it (measured: a plain SELECT that conflicted "
                + "with nothing waited 8 seconds).";
    }

    // ------------------------------------------------------------------ statements

    private void emitOrphanDrops(List<DropLeaf> orphanLeaves) {
        int total = orphanLeaves.size();
        int ordinal = 0;
        for (DropLeaf leaf : orphanLeaves) {
            ordinal++;
            int group = out.size();
            DropCandidateIndex orphan = leaf.getOrphan();
            // DROP INDEX CONCURRENTLY waits for every transaction that could still be using the
            // index, exactly as CREATE INDEX CONCURRENTLY does, so it needs statement_timeout
            // lifted for the same reason. Measured on 17.10 against a table with one open writer
            // transaction: cancelled at 197ms under an adopter statement_timeout of 200ms,
            // succeeded after 9.5s with it lifted.
            setStatementTimeoutUnbounded();
            setLockTimeout(plan.getLockTimeout());
            emit("DROP INDEX CONCURRENTLY "
                            + Identifiers.qualified(leaf.getSchemaName(), orphan.getIndexName()),
                    "leftover " + ordinal + " of " + total + " (" + leaf + "): dropping "
                            + orphan.getIndexName() + " online -- unattached"
                            + (orphan.isValid() ? "" : " and INVALID")
                            + ", so ShareUpdateExclusiveLock is enough");
            restoreStatementTimeout();
            progressOn(group, position(ordinal, total, leaf.toString())
                    + "  DROP INDEX CONCURRENTLY " + orphan.getIndexName() + " (unattached leftover)");
        }
    }

    /**
     * {@code [ 3/12] public.orders_2024_03}, the ordinal right-aligned so the column lines up.
     * Same shape as the create and reindex progress lines, but counting leftovers rather than
     * leaves — a drop only ever touches the partitions that still carry one.
     */
    private static String position(int ordinal, int total, String leaf) {
        String max = String.valueOf(total);
        StringBuilder n = new StringBuilder(String.valueOf(ordinal));
        while (n.length() < max.length()) {
            n.insert(0, ' ');
        }
        return "[" + n + "/" + max + "] " + leaf;
    }

    private void progressOn(int group, String text) {
        if (group < out.size()) {
            out.get(group).setProgress(text);
        }
    }

    /**
     * <h2>Why {@code statement_timeout} is set to a finite budget here and not to 0</h2>
     * {@code lock_timeout} bounds <b>one lock acquisition</b>, not the statement.
     * {@code DROP INDEX} on a partitioned parent takes AccessExclusiveLock on the parent table,
     * then on each leaf in turn, <b>holding everything it already has</b> while it waits for the
     * next — so the waits add up. Measured on 17.10, 8-partition table, the drop blocked on the
     * last leaf and sampled from a third session:
     * <pre>
     * dropper | lk               | AccessExclusiveLock | t
     * dropper | lk_p1 .. lk_p7   | AccessExclusiveLock | t   (7 rows)
     * dropper | idx_lk           | AccessExclusiveLock | t
     * dropper | lk_p8            | AccessExclusiveLock | f   &lt;- waiting, holding the other nine
     * </pre>
     * With two contended leaves and {@code lock_timeout='5s'} — the shipped default — one attempt
     * took {@code Time: 6876.232 ms}. Nearly seven seconds of table-wide exclusive lock from a
     * knob documented as capping it at five, and on a 400-partition table the ceiling for a single
     * attempt is 401 × {@code exclusiveLockTimeout}. Setting {@code statement_timeout = 0} removed
     * the only bound that was not per-lock.
     *
     * <p>A finite {@code statement_timeout} is a real ceiling on the whole loop, and it is safe:
     * the DO block is one transaction, so a cancel rolls the whole thing back. Measured — after a
     * cancel mid-loop the parent index and all 8 children were still present and the retrying
     * session held no locks at all. The changeset then fails loudly, nothing is written to
     * DATABASECHANGELOG, and re-running retries. Nothing is lost.
     *
     * <p>The default is generous because the uncontended case is nowhere near it: the same
     * 8-partition drop with no blocker took {@code Time: 21.561 ms}. Reaching the budget means
     * real, sustained contention, which is exactly when giving up and retrying later is right.
     */
    private void emitExclusiveTreeDrop() {
        int group = out.size();
        setLockTimeout(plan.getExclusiveLockTimeout());
        emit("SET statement_timeout = " + Identifiers.literal(plan.getExclusiveTotalTimeout()), null);
        statementTimeoutOverridden = true;
        emit(exclusiveDropSql(), "dropping the attached tree: " + qualifiedIndex() + " and its "
                + target.attachedChildren().size() + " attached child index(es), under "
                + "AccessExclusiveLock, retrying up to " + plan.getExclusiveRetries()
                + " times with backoff, the whole loop capped at "
                + plan.getExclusiveTotalTimeout());
        restoreStatementTimeoutAfterExclusiveDrop();
        progressOn(group, "DROP INDEX " + qualifiedIndex() + " + "
                + target.attachedChildren().size() + " attached child index(es), under "
                + "AccessExclusiveLock on the table and every leaf");
    }

    /**
     * Puts back whatever the session came in with. Unlike the concurrent paths this always emits,
     * because the exclusive block always sets a value — including when the adopter's own
     * {@code statement_timeout} was PostgreSQL's default of 0.
     */
    private void restoreStatementTimeoutAfterExclusiveDrop() {
        if (!statementTimeoutOverridden) {
            return;
        }
        String original = target.getOriginalStatementTimeout();
        emit("SET statement_timeout = " + Identifiers.literal(original == null ? "0" : original),
                "restoring the session's incoming statement_timeout");
    }

    /**
     * {@code DROP INDEX} on the parent, wrapped in a retry loop.
     *
     * <p>A loop is only expressible server-side: Liquibase executes a flat list of statements
     * and cannot branch on one failing. A {@code DO} block can, because plain {@code DROP INDEX}
     * — unlike the concurrent form — is legal inside a transaction.
     *
     * <p><b>The measurement that makes retrying safe.</b> A failed attempt takes
     * AccessExclusiveLock on some leaves before timing out on the next. If it kept them across
     * the backoff, the retry would stall the table for longer than a single attempt ever could,
     * and this design would be worse than no retry at all. Measured on 17.10 with a 2s
     * {@code lock_timeout} against a blocked partition, sampling {@code pg_locks} from a third
     * session during the backoff: the retrying session held <b>no locks whatsoever</b>. The
     * subtransaction that plpgsql opens for the {@code EXCEPTION} block releases every lock it
     * had taken when it rolls back. So the pressure between attempts really is zero.
     *
     * <p><b>What one attempt actually costs, corrected.</b> An earlier version of this comment
     * said each attempt was "{@code exclusiveLockTimeout} of pressure, then nothing". That is
     * wrong, and the error mattered. {@code lock_timeout} bounds a single lock acquisition, and
     * this statement takes one lock per leaf plus two, holding each while it waits for the next,
     * so the waits are <b>additive</b>: an attempt can hold the table for up to
     * {@code exclusiveLockTimeout × (leaves + 2)}. Measured, 8 leaves, two of them contended,
     * {@code lock_timeout='5s'}: {@code DROP INDEX / Time: 6876.232 ms}. The real ceiling is
     * {@code exclusiveTotalTimeout}, applied as {@code statement_timeout} around this whole block
     * — see {@link #emitExclusiveTreeDrop()}.
     */
    private String exclusiveDropSql() {
        String qualified = Identifiers.qualified(plan.getSchemaName(), plan.getIndexName());
        String schema = Identifiers.literal(plan.getSchemaName());
        String index = Identifiers.literal(plan.getIndexName());
        String table = Identifiers.literal(plan.getSchemaName() + "." + plan.getTableName());
        String timeout = Identifiers.literal(plan.getExclusiveLockTimeout());
        int retries = plan.getExclusiveRetries();
        int children = target.attachedChildren().size();
        int leaves = target.getLeaves().size();

        return Identifiers.doBlock(""
            + "DECLARE\n"
            + "  attempt int := 0;\n"
            + "  backoff numeric := " + FIRST_BACKOFF_SECONDS + ";\n"
            + "BEGIN\n"
            + "  LOOP\n"
            + "    attempt := attempt + 1;\n"
            + "    BEGIN\n"
            + "      DROP INDEX " + qualified + ";\n"
            + "      RAISE NOTICE 'partitionctl: dropped %.% and its % attached child index(es) "
                    + "on attempt % of %', " + schema + ", " + index + ", " + children
                    + ", attempt, " + retries + ";\n"
            + "      RETURN;\n"
            + "    EXCEPTION WHEN lock_not_available THEN\n"
            + "      IF attempt >= " + retries + " THEN\n"
            + "        RAISE EXCEPTION 'partitionctl: could not acquire AccessExclusiveLock on "
                    + "%.% and its % leaf partition(s) within % on any of % attempts. NOTHING was "
                    + "dropped and the table was never held: every failed attempt released each "
                    + "lock it had taken before backing off. Re-run when the table is quieter, or "
                    + "raise exclusiveLockTimeout / exclusiveRetries.', "
                    + schema + ", " + index + ", " + leaves + ", " + timeout + ", " + retries + ";\n"
            + "      END IF;\n"
            + "      RAISE NOTICE 'partitionctl: attempt % of % timed out after % waiting for "
                    + "AccessExclusiveLock on %; retrying in % second(s)', "
                    + "attempt, " + retries + ", " + timeout + ", " + table + ", backoff;\n"
            + "      PERFORM pg_sleep(backoff);\n"
            + "      backoff := backoff * 2;\n"
            + "    END;\n"
            + "  END LOOP;\n"
            + "END\n");
    }

    /**
     * The last statement. Hard-fails the changeset — so Liquibase records nothing and a re-run
     * retries — if the partitioned index or any planned leftover is still there afterwards.
     *
     * <p>Unlike the create side there is no warn-only case: "still present" is always both wrong
     * and repairable by running again.
     */
    private String verifySql(List<DropLeaf> orphanLeaves) {
        String schema = Identifiers.literal(plan.getSchemaName());
        String index = Identifiers.literal(plan.getIndexName());

        StringBuilder sql = new StringBuilder();
        sql.append("DECLARE\n")
           .append("  nparent int; nleft int := 0;\n")
           .append("BEGIN\n")
           .append("  SELECT count(*) INTO nparent FROM pg_class c\n")
           .append("    JOIN pg_namespace n ON n.oid = c.relnamespace\n")
           .append("   WHERE n.nspname = ").append(schema).append(" AND c.relname = ")
           .append(index).append(" AND c.relkind = 'I';\n")
           .append("  IF nparent > 0 THEN\n")
           .append("    RAISE EXCEPTION 'partitionctl: partitioned index %.% still exists after "
                   + "the drop. Nothing is recorded in DATABASECHANGELOG, so re-running retries "
                   + "it.', ").append(schema).append(", ").append(index).append(";\n")
           .append("  END IF;\n");

        if (!orphanLeaves.isEmpty()) {
            sql.append("  SELECT count(*) INTO nleft\n")
               .append("    FROM (VALUES ");
            for (int i = 0; i < orphanLeaves.size(); i++) {
                DropLeaf leaf = orphanLeaves.get(i);
                if (i > 0) {
                    sql.append(", ");
                }
                sql.append('(').append(Identifiers.literal(leaf.getSchemaName())).append(", ")
                   .append(Identifiers.literal(leaf.getOrphan().getIndexName())).append(')');
            }
            sql.append(") AS want(s, i)\n")
               .append("    JOIN pg_class c ON c.relname = want.i\n")
               .append("    JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = want.s;\n")
               .append("  IF nleft > 0 THEN\n")
               .append("    RAISE EXCEPTION 'partitionctl: % leftover child index(es) of %.% are "
                       + "still present after the drop.', nleft, ")
               .append(schema).append(", ").append(index).append(";\n")
               .append("  END IF;\n");
        }

        sql.append("  RAISE NOTICE 'partitionctl: %.% is gone; % leftover child index(es) removed "
                   + "with it', ").append(schema).append(", ").append(index).append(", ")
           .append(orphanLeaves.size()).append(";\n")
           .append("END\n");
        return Identifiers.doBlock(sql.toString());
    }

    // ------------------------------------------------------------------ pieces

    private void setLockTimeout(String value) {
        lockTimeoutTouched = true;
        emit("SET lock_timeout = " + Identifiers.literal(value), null);
    }

    private void restoreLockTimeout() {
        if (lockTimeoutTouched && target.getOriginalLockTimeout() != null) {
            emit("SET lock_timeout = " + Identifiers.literal(target.getOriginalLockTimeout()),
                    "restoring the session's incoming lock_timeout");
        }
    }

    private void setStatementTimeoutUnbounded() {
        if (statementTimeoutNeedsOverride()) {
            emit("SET statement_timeout = 0", null);
        }
    }

    private void restoreStatementTimeout() {
        if (statementTimeoutNeedsOverride()) {
            emit("SET statement_timeout = "
                    + Identifiers.literal(target.getOriginalStatementTimeout()), null);
        }
    }

    private boolean statementTimeoutNeedsOverride() {
        String original = target.getOriginalStatementTimeout();
        return original != null && !"0".equals(original);
    }

    private void emit(String sql, String label) {
        out.add(new PlannedStatement(sql, label));
    }

    private String qualifiedIndex() {
        return plan.getSchemaName() + "." + plan.getIndexName();
    }

    /** Comments are free text; a newline in one would break the single-line refusal message. */
    private static String oneLine(String text) {
        return text == null ? "" : text.replace("\r", " ").replace("\n", " ").trim();
    }

    private static String join(List<String> parts) {
        StringBuilder sb = new StringBuilder();
        for (int i = 0; i < parts.size(); i++) {
            if (i > 0) {
                sb.append(", ");
            }
            sb.append(parts.get(i));
        }
        return sb.toString();
    }
}
