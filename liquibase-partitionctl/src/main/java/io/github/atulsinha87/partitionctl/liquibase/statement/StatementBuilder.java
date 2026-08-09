package io.github.atulsinha87.partitionctl.liquibase.statement;

import io.github.atulsinha87.partitionctl.liquibase.catalog.Identifiers;
import io.github.atulsinha87.partitionctl.liquibase.catalog.LeafIndex;
import io.github.atulsinha87.partitionctl.liquibase.catalog.LeafPartition;
import io.github.atulsinha87.partitionctl.liquibase.catalog.OwnershipMarker;
import io.github.atulsinha87.partitionctl.liquibase.catalog.TreeState;

import java.util.ArrayList;
import java.util.List;

/**
 * Turns a discovered {@link TreeState} into the statement list.
 *
 * <p>Pure: same inputs, same output, no I/O. That is what makes the interesting cases
 * unit-testable without a database, and it is required anyway because
 * {@code generateStatements} is called about seven times per update.
 *
 * <h2>The defect this class exists to prevent</h2>
 * An interrupted {@code CREATE INDEX CONCURRENTLY} leaves behind an <b>invalid</b> child
 * index bearing exactly the name the naming convention generates. A discovery that asks
 * only "does an index with this name exist" sees "yes", skips the rebuild, and emits only
 * the {@code ATTACH} — and PostgreSQL <b>accepts attaching an invalid index without
 * complaint</b>. The parent is then permanently {@code indisvalid = f}, every leaf is
 * attached so no future {@code ATTACH} can ever validate it, the build reports success, and
 * re-running cannot repair it.
 *
 * <p>So: an invalid conventionally-named child is treated as <b>absent</b> — dropped and
 * rebuilt, never attached.
 *
 * <h2>Resume</h2>
 * There is no state store. A failed changeset is not written to DATABASECHANGELOG at all,
 * so re-running retries it, and this planner emits only the work that is still outstanding.
 * Coverage is decided from {@code pg_inherits} against the parent index OID, never from the
 * child index name, because PostgreSQL names the child indexes it creates for partitions
 * added later.
 *
 * <h2>Timeouts</h2>
 * One statement per line, including every {@code SET} — nothing is clubbed into a shared
 * query string, because multiple statements in one string run in an implicit transaction
 * and {@code CREATE INDEX CONCURRENTLY} refuses to run inside one.
 *
 * <h2>Index shape: the parent and every child must agree</h2>
 * {@code unique}, {@code using} and {@code where} are rendered by one method,
 * {@link #createIndexSql}, for the parent {@code ON ONLY} statement and for every leaf's
 * concurrent build alike. That is a correctness requirement, not tidiness. Measured on 17.10,
 * each of these produces the same refusal:
 * <pre>
 * UNIQUE parent, non-unique child  ->  ERROR: cannot attach index "..." as a partition of "..."
 * WHERE k &gt; 3 parent, WHERE k &gt; 5 child   DETAIL: The index definitions do not match.
 * gin parent, btree child
 * </pre>
 * A tree that gets that far has a parent stuck at {@code indisvalid = f} and no way forward,
 * so the two statements are generated from one place and cannot drift.
 */
public final class StatementBuilder {

    private final CreateIndexPlan plan;
    private final TreeState state;
    private final List<PlannedStatement> out = new ArrayList<PlannedStatement>();

    private boolean lockTimeoutTouched;

    public StatementBuilder(CreateIndexPlan plan, TreeState state) {
        this.plan = plan;
        this.state = state;
    }

    public static List<PlannedStatement> build(CreateIndexPlan plan, TreeState state) {
        return new StatementBuilder(plan, state).build();
    }

    public List<PlannedStatement> build() {
        guardTargetIsPartitioned();

        String parentQualified = Identifiers.qualified(plan.getSchemaName(), plan.getIndexName());

        if (!state.isParentIndexExists()) {
            int group = out.size();
            setLockTimeout(plan.getAttachLockTimeout());
            emit(createIndexSql(plan.getIndexName(), true,
                            Identifiers.qualified(plan.getSchemaName(), plan.getTableName()), true),
                    "parent index " + plan.getSchemaName() + "." + plan.getIndexName()
                            + " ON ONLY -- deliberately INVALID until the final ATTACH validates it");
            progressOn(group, "parent index " + plan.getSchemaName() + "." + plan.getIndexName()
                    + " ON ONLY -- invalid until the final ATTACH");
        }

        List<LeafPartition> leaves = state.getLeaves();
        int total = leaves.size();
        int ordinal = 0;

        for (LeafPartition leaf : leaves) {
            ordinal++;
            String position = "leaf " + ordinal + " of " + total + " (" + leaf + ")";
            String at = at(ordinal, total, leaf);
            String child = leaf.getChildIndexName();
            String childQualified = Identifiers.qualified(leaf.getSchemaName(), child);
            int group = out.size();

            LeafIndex covering = leaf.getCoveringIndex();
            if (covering != null) {
                if (covering.isValid()) {
                    // Already covered, whatever the index is called. Nothing to do, ever.
                    continue;
                }
                // Covered by an INVALID, already-ATTACHED index. It cannot be dropped
                // (PostgreSQL: "cannot drop index ... because index ... requires it") and
                // PostgreSQL 17 has no ALTER INDEX ... DETACH PARTITION. The one repair that
                // works, and works without an exclusive lock, is REINDEX INDEX CONCURRENTLY.
                setStatementTimeoutUnbounded();
                setLockTimeout(plan.getLockTimeout());
                emit("REINDEX INDEX CONCURRENTLY "
                                + Identifiers.qualified(leaf.getSchemaName(), covering.getIndexName()),
                        position + ": repairing INVALID attached child index "
                                + covering.getIndexName() + " in place (no exclusive lock)");
                restoreStatementTimeout();
                progressOn(group, at + "  repair attached INVALID child (REINDEX CONCURRENTLY)");
                pace();
                continue;
            }

            LeafIndex named = leaf.getConventionallyNamedIndex();

            if (named != null && named.isAttachedToAnyParent()) {
                // Attached, but not to our parent -- otherwise it would be the covering index.
                throw new PlanException("partitionctl: cannot build "
                        + parentQualified + " over " + leaf + ": an index named \"" + child
                        + "\" already exists there and is attached to a DIFFERENT partitioned index. "
                        + "PostgreSQL 17 has no ALTER INDEX ... DETACH PARTITION, so it can be neither "
                        + "reused nor removed. Nothing was executed. Choose a different indexName.");
            }

            String action;
            if (named != null && !named.isValid()) {
                // The whole point. An invalid leftover is ABSENT, not "already there".
                //
                // DROP INDEX CONCURRENTLY waits for every transaction that could still be
                // using the index, exactly as CREATE INDEX CONCURRENTLY does, so it needs
                // statement_timeout lifted for the same reason. Measured on 17.10 against a
                // table with one open writer transaction: the drop waited 9.5s and succeeded
                // with statement_timeout = 0, and was cancelled at 197ms under an adopter
                // statement_timeout of 200ms. Leaving it bounded would make THIS path -- the
                // one that defends against the invalid-child defect -- fail on a busy table,
                // and since a failed changeset is never recorded, fail identically on every
                // re-run, forever.
                setStatementTimeoutUnbounded();
                setLockTimeout(plan.getLockTimeout());
                emit("DROP INDEX CONCURRENTLY " + childQualified,
                        position + ": dropping INVALID leftover " + child
                                + " from an interrupted CREATE INDEX CONCURRENTLY");
                restoreStatementTimeout();
                emitBuild(leaf, position, "rebuilding");
                action = "drop INVALID leftover, rebuild + attach";
            } else if (named == null) {
                emitBuild(leaf, position, "building");
                action = "build + attach";
            } else {
                // A valid, unattached index already carries the conventional name. Only the
                // ATTACH is outstanding -- an interrupted run that died between the two.
                action = "attach (child index already built)";
            }

            setLockTimeout(plan.getAttachLockTimeout());
            emit("ALTER INDEX " + parentQualified + " ATTACH PARTITION " + childQualified,
                    position + ": attaching " + child + " to " + plan.getIndexName());

            progressOn(group, at + "  " + action);
            pace();
        }

        restoreLockTimeout();

        int group = out.size();
        emit(verifySql(), "verifying every leaf of " + plan.getSchemaName() + "." + plan.getTableName()
                + " is covered by a VALID child index of " + plan.getIndexName());
        progressOn(group, "verifying " + plan.getSchemaName() + "." + plan.getIndexName()
                + " over " + total + " leaf partition(s)");

        noteIfParentAlreadyInvalid();
        return out;
    }

    /**
     * A parent found already invalid means an earlier run was interrupted, or a buggy one attached
     * an invalid child. Said up front, on the progress channel, because the gate's
     * {@code RAISE WARNING} at the end of the run is not surfaced by every host.
     *
     * <p>It rides on the first statement's progress line rather than being printed from
     * {@code generateStatements}, which must stay free of side effects: that method also runs
     * under {@code liquibase updateSQL}, where nothing executes. Measured on 4.33.0,
     * {@code ConsoleUIService.sendMessage} is a {@code System.out.println} and the CLI writes
     * update-sql to stdout when no {@code --output-file} is given, so a message printed from there
     * lands inside a redirected migration script. {@code ProgressSqlGenerator} already prints only
     * when {@code JdbcExecutor} is on the stack, so routing it here makes it silent under a preview
     * for free.
     */
    private void noteIfParentAlreadyInvalid() {
        if (out.isEmpty() || !state.isParentIndexExists() || state.isParentIndexValid()) {
            return;
        }
        PlannedStatement first = out.get(0);
        String note = plan.getSchemaName() + "." + plan.getIndexName() + " exists but is currently "
                + "indisvalid = false. That is expected mid-build; this run finishes the "
                + "outstanding leaves. If it is still false afterwards, an earlier run attached an "
                + "invalid child index and the flag is permanent -- the index remains usable.";
        first.setProgress(first.getProgress() == null ? note : note + "\n" + first.getProgress());
    }

    // ------------------------------------------------------------------ pieces

    private void guardTargetIsPartitioned() {
        String kind = state.getRootRelkind();
        if (kind == null) {
            throw new PlanException("partitionctl: table "
                    + plan.getSchemaName() + "." + plan.getTableName()
                    + " does not exist. Nothing was executed.");
        }
        if (!"p".equals(kind)) {
            throw new PlanException("partitionctl: "
                    + plan.getSchemaName() + "." + plan.getTableName()
                    + " is not a partitioned table (pg_class.relkind = '" + kind + "', expected 'p'). "
                    + "Use Liquibase's own <createIndex> for an ordinary table. Nothing was executed.");
        }
        guardSingleLevel();
        guardParentIndexBelongsToThisTable();
    }

    /**
     * Multi-level partitioning is refused, not attempted.
     *
     * <p>{@code ALTER INDEX ... ATTACH PARTITION} resolves the child against the parent table's
     * <em>direct</em> partitions. Measured on 17.10 with {@code ml} partitioned into {@code ml_a}
     * and {@code ml_b}, each partitioned again:
     * <pre>
     * CREATE INDEX idx_ml ON ONLY ml (v);
     * CREATE INDEX CONCURRENTLY idx_ml_ml_a1 ON ml_a1 (v);
     * ALTER INDEX public.idx_ml ATTACH PARTITION public.idx_ml_ml_a1;
     *   ERROR:  cannot attach index "idx_ml_ml_a1" as a partition of index "idx_ml"
     *   DETAIL:  Index "idx_ml_ml_a1" is not an index on any partition of table "ml".
     * </pre>
     * Without this guard that is a <b>permanent</b> failure: discovery enumerates the grandchildren
     * as leaves, the first ATTACH fails, no DATABASECHANGELOG row is written, and every re-run
     * repeats it — with {@code runAlways="true"} that halts the whole update on every deploy,
     * forever, leaving an invalid parent and an orphan child index behind. Refusing before the
     * first {@code CREATE INDEX ON ONLY} leaves no wreckage at all.
     *
     * <p>{@code ReindexStatementBuilder.gateSql()} already refuses the same shape; this is the
     * matching guard for the build path. Supporting it properly means an {@code ON ONLY} index per
     * intermediate level and an attach at each level, which nobody has asked for.
     */
    private void guardSingleLevel() {
        int intermediates = state.getIntermediatePartitionedCount();
        if (intermediates <= 0) {
            return;
        }
        throw new PlanException("partitionctl: " + plan.getSchemaName() + "." + plan.getTableName()
                + " is a MULTI-LEVEL partitioned table -- " + intermediates + " of its partition(s) "
                + "are themselves partitioned. ALTER INDEX ... ATTACH PARTITION only accepts an "
                + "index on a DIRECT partition of the table, so a child index built on a "
                + "grandchild partition can never be attached (PostgreSQL: \"is not an index on "
                + "any partition of table\"), and every re-run would fail identically. Nothing was "
                + "executed. Build the index with a plain CREATE INDEX on "
                + plan.getSchemaName() + "." + plan.getTableName() + " -- PostgreSQL walks the "
                + "whole tree itself, though it holds a ShareLock on the table while it does. "
                + "Or point one changeset at each lowest-level partitioned table.");
    }

    /**
     * The named index must belong to the named table.
     *
     * <p>Index names are unique per schema but carry no hint of which table they index. Without
     * this, a changeset naming an index that already exists on a <em>different</em> table adopts
     * it as the parent — discovery reports "parent exists", no {@code ON ONLY} statement is
     * emitted, and the first ATTACH fails with a PostgreSQL message that never mentions the wrong
     * table. Measured: {@code indexName="idx_wrongtable" tableName="wt_right"} against an
     * {@code idx_wrongtable} on {@code wt_other} left an orphan child index and failed the same
     * way on every re-run.
     *
     * <p>{@code DropStatementBuilder.guardNamedRelation()} and
     * {@code IndexGatePrecondition} both already refuse this; the create path was the one without it.
     */
    private void guardParentIndexBelongsToThisTable() {
        String owner = state.getParentIndexOwningTable();
        String expected = plan.getSchemaName() + "." + plan.getTableName();
        if (owner == null || owner.equals(expected)) {
            return;
        }
        throw new PlanException("partitionctl: " + plan.getSchemaName() + "." + plan.getIndexName()
                + " already exists as a partitioned index on " + owner + ", not on the " + expected
                + " named by this changeset. Refusing to extend an index that belongs to a table "
                + "the changeset did not name -- the child indexes could never attach to it. "
                + "Nothing was executed. Choose a different indexName, or correct tableName.");
    }

    private void emitBuild(LeafPartition leaf, String position, String verb) {
        setStatementTimeoutUnbounded();
        setLockTimeout(plan.getLockTimeout());
        emit(createIndexSql(leaf.getChildIndexName(), false,
                        Identifiers.qualified(leaf.getSchemaName(), leaf.getTableName()), false),
                position + ": " + verb + " child index " + leaf.getChildIndexName());
        restoreStatementTimeout();
        emitOwnershipMarker(leaf, position);
    }

    /**
     * The {@code COMMENT ON INDEX} ownership marker, on its own line immediately after the build
     * — M4-PLAN §5.2.
     *
     * <p>It is what {@code <ext:dropPartitionedTableIndex>} reads to answer "did this plugin
     * build the tree I am about to destroy". Until this line existed the drop refused every tree
     * the create built, correctly, because there was no evidence.
     *
     * <p>No {@code SET} of its own. Measured on 17.10: {@code COMMENT ON INDEX} takes
     * {@code ShareUpdateExclusiveLock} on the index alone and nothing on the table, so it belongs
     * in the same bucket as the concurrent build that precedes it and inherits that
     * {@code lock_timeout}. It is a catalog write, so the adopter's own {@code statement_timeout}
     * is left in force — restored by {@code emitBuild} on the line above.
     *
     * <p>The marker is written before the {@code ATTACH}, not after, so that a run interrupted
     * between them leaves an index that is unmistakably ours. Measured: the comment survives
     * {@code ALTER INDEX ... ATTACH PARTITION} and both forms of
     * {@code REINDEX INDEX CONCURRENTLY} — leaf-level and parent-level — even though the
     * relfilenode changes each time. So a tree stays recognisable across a reindex.
     */
    private void emitOwnershipMarker(LeafPartition leaf, String position) {
        String marker = OwnershipMarker.forChildIndex(plan.getIndexName(), plan.getSchemaName(),
                plan.getTableName(), plan.getChangeSetId());
        emit(OwnershipMarker.commentStatement(leaf.getSchemaName(), leaf.getChildIndexName(), marker),
                position + ": marking " + leaf.getChildIndexName()
                        + " as built by partitionctl (evidence for dropPartitionedTableIndex)");
    }

    /**
     * The one method that renders a {@code CREATE INDEX}, used for the parent and for every leaf.
     *
     * <p>Sharing it is the point, not a tidiness preference: measured on 17.10, a child whose
     * uniqueness, access method or predicate differs from the parent's is rejected by
     * {@code ALTER INDEX ... ATTACH PARTITION} with "cannot attach index … The index definitions
     * do not match." Two call sites building the same SQL twice is exactly how that ships.
     */
    private String createIndexSql(String indexName, boolean onOnly, String target,
                                  boolean parent) {
        StringBuilder sql = new StringBuilder("CREATE ");
        if (plan.isUnique()) {
            sql.append("UNIQUE ");
        }
        sql.append("INDEX ");
        if (!parent) {
            sql.append("CONCURRENTLY ");
        }
        sql.append(Identifiers.quote(indexName)).append(" ON ");
        if (onOnly) {
            sql.append("ONLY ");
        }
        sql.append(target);
        if (notBlank(plan.getUsing())) {
            sql.append(" USING ").append(Identifiers.quote(plan.getUsing().trim()));
        }
        sql.append(" (").append(columnList()).append(")");
        if (notBlank(plan.getWhere())) {
            // Raw SQL, verbatim, at the end of the statement -- see CreateIndexPlan.getWhere().
            sql.append(" WHERE ").append(plan.getWhere().trim());
        }
        return sql.toString();
    }

    private static boolean notBlank(String value) {
        return value != null && !value.trim().isEmpty();
    }

    /** {@code [ 7/12] public.orders_2024_07}, the ordinal right-aligned so the column lines up. */
    static String at(int ordinal, int total, LeafPartition leaf) {
        String max = String.valueOf(total);
        StringBuilder position = new StringBuilder(String.valueOf(ordinal));
        while (position.length() < max.length()) {
            position.insert(0, ' ');
        }
        return "[" + position + "/" + max + "] " + leaf;
    }

    /**
     * Attaches the progress line to the first statement of a group, so it prints immediately
     * before that partition's work starts rather than after it finishes. A group with no
     * statements — a leaf that was already covered — gets no line, which is the honest report:
     * nothing happened to it.
     */
    private void progressOn(int group, String text) {
        if (group < out.size()) {
            out.get(group).setProgress(text);
        }
    }

    /**
     * The sleep gets the same {@code statement_timeout} bracket the real work gets.
     *
     * <p>Not cosmetic. Measured on 17.10 with the adopter's {@code statement_timeout} at 1s and
     * {@code paceSeconds="3"}: without the bracket, run 1 built and attached leaf 1 and then died
     * with {@code ERROR: canceling statement due to statement timeout}, writing no
     * DATABASECHANGELOG row; run 2 advanced exactly one more leaf and died identically. One leaf
     * of progress per red build, forever — and {@code paceSeconds} exists precisely for the large
     * partition counts where that is 400 failed deploys.
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

    /**
     * Only a concurrent index build needs {@code statement_timeout} lifted, and only for the
     * duration of that one statement. The adopter's own value keeps protecting everything
     * else. PostgreSQL's default is already 0, so in the common case nothing is emitted at all.
     */
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

    private String columnList() {
        StringBuilder sb = new StringBuilder();
        List<IndexColumn> columns = plan.getColumns();
        for (int i = 0; i < columns.size(); i++) {
            if (i > 0) {
                sb.append(", ");
            }
            sb.append(Identifiers.quote(columns.get(i).getName()));
            if (columns.get(i).isDescending()) {
                sb.append(" DESC");
            }
        }
        return sb.toString();
    }

    // ------------------------------------------------------------------ the gate

    /**
     * The last statement. Hard-fails the changeset — so Liquibase records nothing and a
     * re-run retries — if the parent index vanished, if any leaf is uncovered, or if any
     * child index is invalid.
     *
     * <p>An invalid <b>parent</b> only warns. Measured: a parent stuck at
     * {@code indisvalid = f} whose children are all valid does not stop the planner using the
     * leaf indexes and does not stop new partitions being covered, and nothing short of
     * dropping and rebuilding the whole tree clears the flag. Since {@code runAlways="true"}
     * is the documented default, hard-failing on it would fail every deploy forever with no
     * repair path. The damage that matters is an invalid or unattached <b>leaf</b>, and that
     * is repairable — so that is what fails.
     */
    private String verifySql() {
        String schema = Identifiers.literal(plan.getSchemaName());
        String table = Identifiers.literal(plan.getTableName());
        String index = Identifiers.literal(plan.getIndexName());
        return ""
            + "DO $partitionctl$\n"
            + "DECLARE\n"
            + "  pidx oid; pvalid boolean; nleaf int; ncov int; ninvalid int;\n"
            + "BEGIN\n"
            + "  SELECT c.oid, i.indisvalid INTO pidx, pvalid\n"
            + "    FROM pg_class c\n"
            + "    JOIN pg_namespace n ON n.oid = c.relnamespace\n"
            + "    JOIN pg_index i ON i.indexrelid = c.oid\n"
            + "   WHERE n.nspname = " + schema + " AND c.relname = " + index + " AND c.relkind = 'I';\n"
            + "  IF pidx IS NULL THEN\n"
            + "    RAISE EXCEPTION 'partitionctl: parent index %.% does not exist after the build',\n"
            + "      " + schema + ", " + index + ";\n"
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
            + "    RAISE EXCEPTION 'partitionctl: %.% covers only % of % leaf partitions. "
                    + "Re-run to build the rest; nothing already built is lost.',\n"
            + "      " + schema + ", " + index + ", ncov, nleaf;\n"
            + "  END IF;\n"
            + "  IF ninvalid > 0 THEN\n"
            + "    RAISE EXCEPTION 'partitionctl: % of % child indexes of %.% are INVALID and will "
                    + "not be used by the planner. Re-run to repair them with REINDEX INDEX CONCURRENTLY.',\n"
            + "      ninvalid, ncov, " + schema + ", " + index + ";\n"
            + "  END IF;\n"
            + "  IF NOT pvalid THEN\n"
            + "    RAISE WARNING 'partitionctl: %.% is usable (% of % leaves covered, none invalid) "
                    + "but the parent index itself is still marked indisvalid = false. That flag is "
                    + "permanent once an invalid child has been attached; it does not affect query "
                    + "planning or coverage of new partitions, only monitoring that reads it. "
                    + "Clearing it needs a drop and rebuild of the whole tree.',\n"
            + "      " + schema + ", " + index + ", ncov, nleaf;\n"
            + "  ELSE\n"
            + "    RAISE NOTICE 'partitionctl: %.% VALID, % of % leaf partitions covered',\n"
            + "      " + schema + ", " + index + ", ncov, nleaf;\n"
            + "  END IF;\n"
            + "END\n"
            + "$partitionctl$";
    }
}
