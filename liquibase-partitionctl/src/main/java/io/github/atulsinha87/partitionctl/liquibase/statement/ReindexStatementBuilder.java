package io.github.atulsinha87.partitionctl.liquibase.statement;

import io.github.atulsinha87.partitionctl.liquibase.catalog.Identifiers;
import io.github.atulsinha87.partitionctl.liquibase.catalog.LeafIndex;
import io.github.atulsinha87.partitionctl.liquibase.catalog.LeafPartition;
import io.github.atulsinha87.partitionctl.liquibase.catalog.Leftovers;
import io.github.atulsinha87.partitionctl.liquibase.catalog.TreeState;

import java.util.ArrayList;
import java.util.List;

/**
 * Turns a discovered {@link TreeState} into the statement list for
 * {@code <ext:reindexPartitionedTableIndex>}. Pure: same inputs, same output, no I/O.
 *
 * <h2>Why per leaf, when the parent-level command works</h2>
 * {@code REINDEX INDEX CONCURRENTLY <partitioned index>} is legal and safe on 14.23 and 17.10 —
 * PostgreSQL loops the partitions itself, one per transaction, at
 * {@code ShareUpdateExclusiveLock}. The TRD's claim that it is rejected is simply wrong. So this
 * class has to earn per-leaf, and the reason the Go product gives — "per-leaf buys resume" —
 * <b>does not transfer to Liquibase</b>. It is worth being exact about why, because the wrong
 * answer here is the kind that ships.
 *
 * <h3>Resume is not the reason, because resume is impossible</h3>
 * "Was reindexed" is not catalog-observable. After a successful rebuild the index has a new
 * {@code relfilenode} and is otherwise identical to the stale one it replaced — same name, same
 * OID, same {@code indisvalid}, same {@code pg_inherits} row. There is nowhere to record that it
 * happened either: a failed changeset is not written to DATABASECHANGELOG at all, which is what
 * makes a re-run retry, and there is no state store. So on a re-run, a leaf reindexed by the
 * previous attempt looks exactly like one that was never reached, and gets rebuilt again.
 *
 * <p>The Go product does get resume, but from its plan file recording which nodes completed, not
 * from per-leaf statements. Take that away and per-leaf and parent-level converge: measured on
 * 17.10, cancelling a parent-level {@code REINDEX INDEX CONCURRENTLY} across four partitions left
 * three with new relfilenodes, the fourth untouched with a {@code _ccnew} beside it — the same
 * partial progress, kept the same way, that driving the leaves ourselves produces.
 *
 * <h3>What per leaf does buy, all of it measured</h3>
 * <ol>
 *   <li><b>The one resume signal that does exist.</b> A surviving {@code _ccold} <em>is</em>
 *       evidence that a specific leaf was rebuilt: PostgreSQL only creates that name after the
 *       swap has already put the new index in place. This planner drops it and skips the leaf.
 *       The parent-level command has no way to skip anything. Narrow — {@code _ccold} survives
 *       only if the run died in the window between the swap and the drop — but it is real, and
 *       it is the only true statement of the form "this leaf is already fresh" available.</li>
 *   <li><b>Pacing.</b> {@code paceSeconds} between leaves. PostgreSQL's internal loop has no gap
 *       and no way to insert one; on a 400-partition table that is the difference between a
 *       reindex you can run during business hours and one you cannot.</li>
 *   <li><b>Leftover cleanup in the right order.</b> Measured: a parent-level reindex run with a
 *       {@code _ccnew} already on one leaf left it exactly where it was — PostgreSQL never
 *       cleans these up. Someone has to, per leaf, and the drop has to precede that leaf's
 *       rebuild or the next attempt just makes a {@code _ccnew1} beside it.</li>
 *   <li><b>Statement-level timeouts and a readable plan.</b> One statement per leaf means
 *       {@code lock_timeout} applies per leaf and {@code liquibase updateSQL} reads as
 *       "leaf 7 of 400", instead of one opaque line that runs for hours.</li>
 * </ol>
 *
 * <p><b>The honest consequence:</b> on a re-run after an interruption this rebuilds every leaf
 * that has no {@code _ccold} beside it, including leaves the previous attempt already finished.
 * That is wasted work, not damage — {@code REINDEX INDEX CONCURRENTLY} on a fresh index is safe
 * and idempotent — but an adopter watching a 400-partition reindex restart from the top should
 * know it is not a bug.
 *
 * <h2>Why the guards are in SQL, not here</h2>
 * {@code generateStatements} is called more than once per update and is not required to be called
 * only at execution time, so a changelog that creates the index in one changeset and reindexes it
 * in the next can ask this planner about an index that does not exist yet. Throwing then would
 * fail an update that was about to be perfectly correct. So a tree that is not ready produces no
 * work and no exception — only the gate, which diagnoses at execution time, when the answer can
 * be trusted, and which is the same gate a real run ends with.
 *
 * <p>Verified live: {@code createPartitionedTableIndex} followed by
 * {@code reindexPartitionedTableIndex} in one changelog against an empty tree ran both, and the
 * server log shows all six {@code REINDEX INDEX CONCURRENTLY} statements issued against the index
 * the previous changeset had just built.
 *
 * <h2>Limitation: multi-level partitioning is refused, not attempted</h2>
 * When a partition is itself partitioned, the parent index's {@code pg_inherits} children are the
 * intermediate <em>partitioned</em> indexes ({@code relkind = 'I'}), not the leaf indexes.
 * Discovery reads coverage from the direct children only, so on such a tree it sees every leaf as
 * uncovered. Measured on a two-level table: four leaves, two direct children. Rather than guess,
 * the gate names the shape and points at {@code REINDEX INDEX CONCURRENTLY <parent>}, which was
 * verified to rebuild all four grandchildren in one command. Nothing is executed.
 */
public final class ReindexStatementBuilder {

    private final ReindexIndexPlan plan;
    private final TreeState state;
    private final List<PlannedStatement> out = new ArrayList<PlannedStatement>();

    private boolean lockTimeoutTouched;

    public ReindexStatementBuilder(ReindexIndexPlan plan, TreeState state) {
        this.plan = plan;
        this.state = state;
    }

    public static List<PlannedStatement> build(ReindexIndexPlan plan, TreeState state) {
        return new ReindexStatementBuilder(plan, state).build();
    }

    /**
     * Whether the discovered tree is one this operation can act on: a partitioned table, with the
     * named partitioned index on it, covering every leaf. An <em>invalid</em> child does not
     * disqualify it — {@code REINDEX INDEX CONCURRENTLY} is exactly the repair for that, measured
     * on a child left {@code indisvalid=f, indisready=f} by a cancelled build.
     *
     * <p>Also the memoisation predicate: discovery is only worth caching once this holds, because
     * before it does the answer may just be "the earlier changeset has not run yet".
     */
    public static boolean readyToReindex(TreeState state) {
        if (!"p".equals(state.getRootRelkind()) || !state.isParentIndexExists()) {
            return false;
        }
        for (LeafPartition leaf : state.getLeaves()) {
            if (leaf.getCoveringIndex() == null) {
                return false;
            }
        }
        return true;
    }

    public List<PlannedStatement> build() {
        if (!readyToReindex(state)) {
            emit(gateSql(), "checking " + plan.getSchemaName() + "." + plan.getIndexName()
                    + " is a complete partitioned index before reindexing anything");
            return out;
        }

        List<LeafPartition> leaves = state.getLeaves();
        int total = leaves.size();
        int ordinal = 0;

        for (LeafPartition leaf : leaves) {
            ordinal++;
            String position = "leaf " + ordinal + " of " + total + " (" + leaf + ")";
            String at = StatementBuilder.at(ordinal, total, leaf);
            int group = out.size();
            LeafIndex covering = leaf.getCoveringIndex();

            List<LeafIndex> leftovers = leftoversOf(leaf, covering.getIndexName());
            boolean rebuildAlreadySucceeded = false;
            for (LeafIndex leftover : leftovers) {
                Leftovers.Kind kind = Leftovers.classify(leftover.getIndexName(), covering.getIndexName());
                if (kind == Leftovers.Kind.CCOLD && covering.isValid()) {
                    rebuildAlreadySucceeded = true;
                }
            }

            for (LeafIndex leftover : leftovers) {
                Leftovers.Kind kind = Leftovers.classify(leftover.getIndexName(), covering.getIndexName());
                emitDropLeftover(leaf, leftover, kind, position);
            }

            if (rebuildAlreadySucceeded) {
                // _ccold is the old copy, renamed by the swap that had ALREADY installed the new
                // index under the original name. Measured: the base index carried a new
                // relfilenode and the _ccold carried the old one. So this leaf is fresh.
                progressOn(group, at + "  skip -- _ccold proves the previous run rebuilt this leaf");
                continue;
            }

            emitReindex(leaf, covering, position);
            progressOn(group, at + (leftovers.isEmpty()
                    ? "  REINDEX CONCURRENTLY"
                    : "  drop " + leftovers.size() + " leftover(s), REINDEX CONCURRENTLY"));
            pace();
        }

        restoreLockTimeout();

        int group = out.size();
        emit(gateSql(), "verifying every leaf of " + plan.getSchemaName() + "." + plan.getTableName()
                + " is still covered by a VALID child index of " + plan.getIndexName());
        progressOn(group, "verifying " + plan.getSchemaName() + "." + plan.getIndexName()
                + " over " + total + " leaf partition(s)");

        return out;
    }

    /** See {@code StatementBuilder.progressOn} — one line per partition, on its first statement. */
    private void progressOn(int group, String text) {
        if (group < out.size()) {
            out.get(group).setProgress(text);
        }
    }

    // ------------------------------------------------------------------ pieces

    /**
     * The leftovers on this leaf that belong to <em>this</em> index. Two guards, both load
     * bearing. The index must be invalid, and it must be attached to nothing — every
     * {@code _ccnew} and {@code _ccold} observed live was both, and requiring it means an
     * ordinary index that merely happens to end in {@code _ccnew} is never dropped. Attribution
     * is by {@link Leftovers#classify}, so a leftover of some other index family on the same
     * partition — a DBA's hand-run {@code REINDEX CONCURRENTLY}, possibly still in flight — is
     * left alone.
     */
    private List<LeafIndex> leftoversOf(LeafPartition leaf, String baseName) {
        List<LeafIndex> found = new ArrayList<LeafIndex>();
        for (LeafIndex index : leaf.getIndexes()) {
            if (index.isValid() || index.isAttachedToAnyParent()) {
                continue;
            }
            if (Leftovers.classify(index.getIndexName(), baseName) != null) {
                found.add(index);
            }
        }
        return found;
    }

    private void emitDropLeftover(LeafPartition leaf, LeafIndex leftover,
                                  Leftovers.Kind kind, String position) {
        String meaning = kind == Leftovers.Kind.CCOLD
                ? "the previous rebuild SUCCEEDED and only this old copy survived"
                : "the previous rebuild FAILED and left this behind";
        // DROP INDEX CONCURRENTLY waits for every transaction that could still be using the
        // index, exactly as the concurrent build does, so it needs statement_timeout lifted for
        // the same reason: under a finite adopter statement_timeout on a busy table it is
        // cancelled, the changeset fails, nothing is recorded, and every re-run fails identically.
        setStatementTimeoutUnbounded();
        setLockTimeout(plan.getLockTimeout());
        emit("DROP INDEX CONCURRENTLY "
                        + Identifiers.qualified(leaf.getSchemaName(), leftover.getIndexName()),
                position + ": dropping leftover " + leftover.getIndexName() + " -- " + meaning);
        restoreStatementTimeout();
    }

    private void emitReindex(LeafPartition leaf, LeafIndex covering, String position) {
        String note = covering.isValid()
                ? "rebuilding " + covering.getIndexName()
                : "rebuilding INVALID " + covering.getIndexName();
        // REINDEX INDEX CONCURRENTLY builds a whole second copy of the index and waits for
        // concurrent transactions between phases. A leaf can be terabytes, so statement_timeout
        // has to come off for it; the adopter's own value keeps protecting every other statement.
        setStatementTimeoutUnbounded();
        setLockTimeout(plan.getLockTimeout());
        emit("REINDEX INDEX CONCURRENTLY "
                        + Identifiers.qualified(leaf.getSchemaName(), covering.getIndexName()),
                position + ": " + note);
        restoreStatementTimeout();
    }

    /**
     * Measured: {@code SET statement_timeout='200ms'; SELECT pg_sleep(1)} is cancelled. A pace
     * longer than the adopter's own statement_timeout would therefore fail the changeset, which
     * is a silly way to lose a reindex, so the sleep gets the same bracket the real work gets.
     */
    private void pace() {
        Integer seconds = plan.getPaceSeconds();
        if (seconds == null || seconds <= 0) {
            return;
        }
        setStatementTimeoutUnbounded();
        emit("SELECT pg_sleep(" + seconds + ")", "pacing " + seconds + "s before the next leaf");
        restoreStatementTimeout();
    }

    private void setLockTimeout(String value) {
        lockTimeoutTouched = true;
        emit("SET lock_timeout = " + Identifiers.literal(value), null);
    }

    private void restoreLockTimeout() {
        if (lockTimeoutTouched && state.getOriginalLockTimeout() != null) {
            emit("SET lock_timeout = " + Identifiers.literal(state.getOriginalLockTimeout()),
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
            emit("SET statement_timeout = " + Identifiers.literal(state.getOriginalStatementTimeout()), null);
        }
    }

    private boolean statementTimeoutNeedsOverride() {
        String original = state.getOriginalStatementTimeout();
        return original != null && !"0".equals(original);
    }

    private void emit(String sql, String label) {
        out.add(new PlannedStatement(sql, label));
    }

    // ------------------------------------------------------------------ the gate

    /**
     * Emitted either as the whole plan (when the tree was not ready and the only honest thing to
     * do is diagnose at execution time) or as the last statement of a real run.
     *
     * <p>Hard-fails on the states that are both damaging and repairable: the table or index
     * missing, a leaf not covered, a child index left invalid. Only <b>warns</b> on a parent
     * stuck at {@code indisvalid = false}, because that flag is permanent once an invalid child
     * has been attached and clearing it needs a full drop and rebuild — measured, and reconfirmed
     * here: a parent-level {@code REINDEX INDEX CONCURRENTLY} over such a tree rebuilt both
     * children and left the parent's flag exactly as it found it. Hard-failing on it under
     * {@code runAlways="true"} would fail every deploy forever with no repair path.
     *
     * <p>Leftovers also only warn. Ones this operation could attribute have already been dropped;
     * the ones that remain belong to somebody else's index and may be a rebuild still in flight.
     */
    private String gateSql() {
        String schema = Identifiers.literal(plan.getSchemaName());
        String table = Identifiers.literal(plan.getTableName());
        String index = Identifiers.literal(plan.getIndexName());
        return ""
            + "DO $partitionctl$\n"
            + "DECLARE\n"
            + "  rootkind \"char\"; pidx oid; pvalid boolean;\n"
            + "  nleaf int; ncov int; ninvalid int; nleft int; nsub int;\n"
            + "BEGIN\n"
            + "  SELECT c.relkind INTO rootkind\n"
            + "    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace\n"
            + "   WHERE n.nspname = " + schema + " AND c.relname = " + table + ";\n"
            + "  IF rootkind IS NULL THEN\n"
            + "    RAISE EXCEPTION 'partitionctl: table %.% does not exist. Nothing was executed.',\n"
            + "      " + schema + ", " + table + ";\n"
            + "  END IF;\n"
            + "  IF rootkind <> 'p' THEN\n"
            + "    RAISE EXCEPTION 'partitionctl: %.% is not a partitioned table "
                    + "(pg_class.relkind = %, expected p). Use REINDEX INDEX CONCURRENTLY directly "
                    + "for an ordinary table. Nothing was executed.',\n"
            + "      " + schema + ", " + table + ", rootkind;\n"
            + "  END IF;\n"
            + "  SELECT c.oid, i.indisvalid INTO pidx, pvalid\n"
            + "    FROM pg_class c\n"
            + "    JOIN pg_namespace n ON n.oid = c.relnamespace\n"
            + "    JOIN pg_index i ON i.indexrelid = c.oid\n"
            + "   WHERE n.nspname = " + schema + " AND c.relname = " + index + " AND c.relkind = 'I';\n"
            + "  IF pidx IS NULL THEN\n"
            + "    RAISE EXCEPTION 'partitionctl: %.% is not a partitioned index on %.%. "
                    + "There is nothing to reindex; build it with <createPartitionedTableIndex> first.',\n"
            + "      " + schema + ", " + index + ", " + schema + ", " + table + ";\n"
            + "  END IF;\n"
            + "  SELECT count(*) INTO nsub\n"
            + "    FROM pg_inherits ii JOIN pg_class ic ON ic.oid = ii.inhrelid\n"
            + "   WHERE ii.inhparent = pidx AND ic.relkind = 'I';\n"
            + "  IF nsub > 0 THEN\n"
            + "    RAISE EXCEPTION 'partitionctl: %.% is a MULTI-LEVEL partitioned index -- % of its "
                    + "children are themselves partitioned indexes, not leaf indexes. partitionctl "
                    + "reads coverage from the direct children of the parent index only, so it cannot "
                    + "tell which leaf each index covers here and will not guess. Reindex this tree "
                    + "with REINDEX INDEX CONCURRENTLY %.% instead: PostgreSQL walks the whole tree "
                    + "itself, one partition per transaction, at ShareUpdateExclusiveLock. Nothing "
                    + "was executed.',\n"
            + "      " + schema + ", " + index + ", nsub, " + schema + ", " + index + ";\n"
            + "  END IF;\n"
            + "  WITH RECURSIVE tree AS (\n"
            + "    SELECT c.oid, c.relkind FROM pg_class c\n"
            + "      JOIN pg_namespace n ON n.oid = c.relnamespace\n"
            + "     WHERE n.nspname = " + schema + " AND c.relname = " + table + "\n"
            + "    UNION ALL\n"
            + "    SELECT c2.oid, c2.relkind FROM tree t\n"
            + "      JOIN pg_inherits i ON i.inhparent = t.oid AND NOT i.inhdetachpending\n"
            + "      JOIN pg_class c2 ON c2.oid = i.inhrelid)\n"
            + "  SELECT count(*) INTO nleaf FROM tree WHERE relkind = 'r';\n"
            + "  SELECT count(*), count(*) FILTER (WHERE NOT ix.indisvalid) INTO ncov, ninvalid\n"
            + "    FROM pg_inherits ii JOIN pg_index ix ON ix.indexrelid = ii.inhrelid\n"
            + "   WHERE ii.inhparent = pidx;\n"
            + "  IF ncov <> nleaf THEN\n"
            + "    RAISE EXCEPTION 'partitionctl: %.% covers only % of % leaf partitions, so there is "
                    + "no complete index to reindex. Build the missing children with "
                    + "<createPartitionedTableIndex> first; nothing was executed.',\n"
            + "      " + schema + ", " + index + ", ncov, nleaf;\n"
            + "  END IF;\n"
            + "  IF ninvalid > 0 THEN\n"
            + "    RAISE EXCEPTION 'partitionctl: % of % child indexes of %.% are INVALID and will "
                    + "not be used by the planner. Re-run to rebuild them.',\n"
            + "      ninvalid, ncov, " + schema + ", " + index + ";\n"
            + "  END IF;\n"
            + "  WITH RECURSIVE tree AS (\n"
            + "    SELECT c.oid, c.relkind FROM pg_class c\n"
            + "      JOIN pg_namespace n ON n.oid = c.relnamespace\n"
            + "     WHERE n.nspname = " + schema + " AND c.relname = " + table + "\n"
            + "    UNION ALL\n"
            + "    SELECT c2.oid, c2.relkind FROM tree t\n"
            + "      JOIN pg_inherits i ON i.inhparent = t.oid AND NOT i.inhdetachpending\n"
            + "      JOIN pg_class c2 ON c2.oid = i.inhrelid)\n"
            + "  SELECT count(*) INTO nleft\n"
            + "    FROM tree l\n"
            + "    JOIN pg_index ix ON ix.indrelid = l.oid\n"
            + "    JOIN pg_class ic ON ic.oid = ix.indexrelid\n"
            + "   WHERE l.relkind = 'r'\n"
            + "     AND ic.relname ~ '_cc(new|old)[0-9]*$'\n"
            + "     AND NOT ix.indisvalid\n"
            + "     AND NOT EXISTS (SELECT 1 FROM pg_inherits ii WHERE ii.inhrelid = ic.oid);\n"
            + "  IF nleft > 0 THEN\n"
            + "    RAISE WARNING 'partitionctl: % REINDEX CONCURRENTLY leftover index(es) remain on "
                    + "partitions of %.%. They do not belong to %, so they were left alone -- they may "
                    + "be another index''s rebuild, possibly still running. Remove a dead one with "
                    + "DROP INDEX CONCURRENTLY.',\n"
            + "      nleft, " + schema + ", " + table + ", " + index + ";\n"
            + "  END IF;\n"
            + "  IF NOT pvalid THEN\n"
            + "    RAISE WARNING 'partitionctl: %.% was reindexed (% of % leaves covered, none invalid) "
                    + "but the parent index itself is still marked indisvalid = false. That flag is "
                    + "permanent once an invalid child has been attached; no form of REINDEX clears it. "
                    + "It does not affect query planning or coverage of new partitions, only monitoring "
                    + "that reads it.',\n"
            + "      " + schema + ", " + index + ", ncov, nleaf;\n"
            + "  ELSE\n"
            + "    RAISE NOTICE 'partitionctl: %.% VALID, % of % leaf partitions covered and valid',\n"
            + "      " + schema + ", " + index + ", ncov, nleaf;\n"
            + "  END IF;\n"
            + "END\n"
            + "$partitionctl$";
    }
}
