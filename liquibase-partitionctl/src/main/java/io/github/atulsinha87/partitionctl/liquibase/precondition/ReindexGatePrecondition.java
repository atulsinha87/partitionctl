package io.github.atulsinha87.partitionctl.liquibase.precondition;

import io.github.atulsinha87.partitionctl.liquibase.catalog.GateIndex;
import io.github.atulsinha87.partitionctl.liquibase.catalog.GateLeaf;
import io.github.atulsinha87.partitionctl.liquibase.catalog.GateSnapshot;
import io.github.atulsinha87.partitionctl.liquibase.catalog.Leftovers;

import java.util.ArrayList;
import java.util.List;

/**
 * {@code <ext:partitionctlReindexGate schema="..." table="..." index="..."/>}
 *
 * <p>Passes when the tree is healthy — everything {@link IndexGatePrecondition} asserts — and no
 * leaf carries a {@code _ccnew} or {@code _ccold} left behind by an interrupted
 * {@code REINDEX INDEX CONCURRENTLY}.
 *
 * <h2>It asserts catalog health, not run history</h2>
 * "This index was reindexed" is <b>not observable</b>. A rebuilt index has a new
 * {@code relfilenode} and is otherwise identical to the one it replaced: same name, same OID,
 * same {@code indisvalid}, same {@code pg_inherits} row. There is nowhere in the catalog that
 * records when the rebuild happened, and this extension keeps no state store of its own — resume
 * is re-discovery, by design. So there is no {@code since} attribute and there cannot be one. Any
 * gate claiming to answer "has it been reindexed lately" would be reading a value nobody wrote.
 *
 * <p>What <em>is</em> observable is whether a reindex left wreckage, and that is what this gate
 * is for: run it after {@code reindexPartitionedTableIndex} to confirm the rebuild finished
 * cleanly, or before a changeset that assumes it did.
 *
 * <h2>Why a leftover is worth halting for</h2>
 * The two leftovers mean opposite things and neither is harmless (both states produced live on
 * 17.10, see {@link Leftovers}):
 *
 * <ul>
 *   <li>{@code _ccnew} — the rebuild <b>failed</b>. The base index is untouched and still holds
 *       whatever bloat the reindex was meant to remove.</li>
 *   <li>{@code _ccold} — the rebuild <b>succeeded</b> and only the old copy survived the
 *       cleanup.</li>
 * </ul>
 *
 * <p>Either way an unusable index sits on the partition taking disk. PostgreSQL never removes
 * these on its own — measured: a parent-level {@code REINDEX INDEX CONCURRENTLY} run over a tree
 * with a {@code _ccnew} on one leaf left it exactly where it was.
 *
 * <h2>A leftover belonging to a different index is not a failure</h2>
 * Only leftovers derived from <em>this</em> index's per-leaf names count. A {@code _ccnew} beside
 * some other index on the same partition says nothing about this index's health, and may well be
 * another maintenance job's rebuild still legitimately running. Halting on it would let an
 * unrelated job block this changeset, with no repair path that this changeset owns.
 *
 * <h2>There is no {@code requireValidParent} here, on purpose</h2>
 * Preconditions inside one {@code <preConditions>} block are ANDed, so an adopter who wants the
 * parent's {@code indisvalid} checked too composes the two gates rather than setting a duplicate
 * attribute:
 *
 * <pre>{@code
 * <preConditions onFail="HALT">
 *   <ext:partitionctlIndexGate   schema="public" table="person" index="idx_p" requireValidParent="true"/>
 *   <ext:partitionctlReindexGate schema="public" table="person" index="idx_p"/>
 * </preConditions>
 * }</pre>
 */
public class ReindexGatePrecondition extends PartitionedIndexPrecondition {

    @Override
    public String getName() {
        return "partitionctlReindexGate";
    }

    @Override
    protected void evaluate(GateSnapshot snapshot, List<String> failures) {
        if (!requireLeafPartitions(snapshot, failures)) {
            return;
        }
        // A tree with uncovered or unusable leaves cannot be judged clean, and the leftover check
        // below needs each leaf's covering index to derive the leftover names from. requireValid-
        // Parent is false here: see the class comment on composing the two gates instead.
        if (!IndexGatePrecondition.requireHealthyTree(snapshot, failures, false,
                getSchemaName(), getTableName(), getIndexName())) {
            return;
        }

        List<String> leftovers = new ArrayList<String>();
        for (GateLeaf leaf : snapshot.getLeaves()) {
            GateIndex covering = leaf.getCoveringIndex();
            String base = covering.getIndexName();
            for (GateIndex candidate : leaf.getIndexes()) {
                if (candidate == covering) {
                    continue;
                }
                Leftovers.Kind kind = Leftovers.classify(candidate.getIndexName(), base);
                if (kind == null) {
                    continue;
                }
                leftovers.add(leaf.getSchemaName() + "." + candidate.getIndexName() + " ("
                        + (kind == Leftovers.Kind.CCNEW
                                ? "_ccnew: the rebuild of " + base + " FAILED and that index is "
                                        + "still the old copy"
                                : "_ccold: the rebuild of " + base + " SUCCEEDED and this is the "
                                        + "superseded copy")
                        + ")");
            }
        }

        if (!leftovers.isEmpty()) {
            failures.add(leftovers.size() + " leftover index(es) from an interrupted REINDEX "
                    + "INDEX CONCURRENTLY are still present: " + summarise(sorted(leftovers))
                    + ". PostgreSQL never removes these itself. Re-running "
                    + "<ext:reindexPartitionedTableIndex> drops each one before touching its "
                    + "leaf; a _ccold also tells it that leaf is already fresh and can be "
                    + "skipped.");
        }
    }
}
