package io.github.atulsinha87.partitionctl.liquibase.statement;

import io.github.atulsinha87.partitionctl.liquibase.catalog.Identifiers;
import io.github.atulsinha87.partitionctl.liquibase.catalog.LeafIndex;
import io.github.atulsinha87.partitionctl.liquibase.catalog.LeafPartition;
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
            setLockTimeout(plan.getAttachLockTimeout());
            emit("CREATE INDEX " + Identifiers.quote(plan.getIndexName())
                            + " ON ONLY " + Identifiers.qualified(plan.getSchemaName(), plan.getTableName())
                            + " (" + columnList() + ")",
                    "parent index " + plan.getSchemaName() + "." + plan.getIndexName()
                            + " ON ONLY -- deliberately INVALID until the final ATTACH validates it");
        }

        List<LeafPartition> leaves = state.getLeaves();
        int total = leaves.size();
        int ordinal = 0;

        for (LeafPartition leaf : leaves) {
            ordinal++;
            String position = "leaf " + ordinal + " of " + total + " (" + leaf + ")";
            String child = leaf.getChildIndexName();
            String childQualified = Identifiers.qualified(leaf.getSchemaName(), child);

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
            } else if (named == null) {
                emitBuild(leaf, position, "building");
            }

            setLockTimeout(plan.getAttachLockTimeout());
            emit("ALTER INDEX " + parentQualified + " ATTACH PARTITION " + childQualified,
                    position + ": attaching " + child + " to " + plan.getIndexName());

            pace();
        }

        restoreLockTimeout();

        emit(verifySql(), "verifying every leaf of " + plan.getSchemaName() + "." + plan.getTableName()
                + " is covered by a VALID child index of " + plan.getIndexName());

        return out;
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
    }

    private void emitBuild(LeafPartition leaf, String position, String verb) {
        setStatementTimeoutUnbounded();
        setLockTimeout(plan.getLockTimeout());
        emit("CREATE INDEX CONCURRENTLY " + Identifiers.quote(leaf.getChildIndexName())
                        + " ON " + Identifiers.qualified(leaf.getSchemaName(), leaf.getTableName())
                        + " (" + columnList() + ")",
                position + ": " + verb + " child index " + leaf.getChildIndexName());
        restoreStatementTimeout();
    }

    private void pace() {
        Integer seconds = plan.getPaceSeconds();
        if (seconds != null && seconds > 0) {
            emit("SELECT pg_sleep(" + seconds + ")", "pacing " + seconds + "s before the next leaf");
        }
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
            + "      JOIN pg_inherits i ON i.inhparent = t.oid\n"
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
