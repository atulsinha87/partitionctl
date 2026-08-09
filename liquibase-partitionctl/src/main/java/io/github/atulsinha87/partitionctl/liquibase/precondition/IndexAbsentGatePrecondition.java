package io.github.atulsinha87.partitionctl.liquibase.precondition;

import io.github.atulsinha87.partitionctl.liquibase.catalog.GateIndex;
import io.github.atulsinha87.partitionctl.liquibase.catalog.GateLeaf;
import io.github.atulsinha87.partitionctl.liquibase.catalog.GateSnapshot;
import io.github.atulsinha87.partitionctl.liquibase.catalog.IndexNaming;
import io.github.atulsinha87.partitionctl.liquibase.catalog.Leftovers;

import java.util.ArrayList;
import java.util.List;

/**
 * {@code <ext:partitionctlIndexAbsentGate schema="..." table="..." index="..."/>}
 *
 * <p>The inverse of {@link IndexGatePrecondition}. Passes when the partitioned index is gone
 * <b>and nothing it left behind is still there</b>: no partitioned index of that name anywhere in
 * the schema, and no orphaned child index on any leaf.
 *
 * <pre>{@code
 * <changeSet author="x" id="10">
 *   <preConditions onFail="HALT">
 *     <ext:partitionctlIndexAbsentGate schema="public" table="person" index="idx_personaddress"/>
 *   </preConditions>
 *   <ext:createPartitionedTableIndex .../>
 * </changeSet>
 * }</pre>
 *
 * <h2>Why this is the one gate that reads names</h2>
 * The other two answer "is this leaf covered" from {@code pg_inherits}, because PostgreSQL names
 * child indexes itself when a partition is attached to an already-indexed table, so a name-based
 * answer is wrong. Here the parent index is supposed to be <em>gone</em> — and when it goes, so
 * does every {@code pg_inherits} row that pointed at it. Measured on 17.10: {@code DROP INDEX} on
 * the parent removes the attached children with it, but a free-standing leftover from an
 * interrupted {@code CREATE INDEX CONCURRENTLY} was never attached to anything and survives
 * untouched. Its name is the only evidence left that it belongs to this index.
 *
 * <p>So this gate looks for the names {@link IndexNaming} generates — the same names
 * {@code createPartitionedTableIndex} would build — plus the {@code _ccnew} / {@code _ccold}
 * forms {@code REINDEX INDEX CONCURRENTLY} derives from them. An index that merely happens to
 * share a leaf is not implicated.
 *
 * <h2>Why leftovers matter to a gate about absence</h2>
 * An orphaned child index is not inert. It is maintained on every INSERT into that leaf, it takes
 * disk, and — because it bears exactly the name the convention generates — the next
 * {@code createPartitionedTableIndex} has to recognise it and rebuild it rather than trust it.
 * "The index is gone" is a weaker and less useful assertion than "the tree is clean", and a gate
 * that only checked the parent would pass over a table still carrying six orphans.
 */
public class IndexAbsentGatePrecondition extends PartitionedIndexPrecondition {

    @Override
    public String getName() {
        return "partitionctlIndexAbsentGate";
    }

    @Override
    protected void evaluate(GateSnapshot snapshot, List<String> failures) {
        if (!requireLeafPartitions(snapshot, failures)) {
            return;
        }

        String schema = getSchemaName();
        String index = getIndexName();

        if (snapshot.isNamedIndexExists()) {
            String kind = snapshot.namedIndexIsPartitioned()
                    ? "a partitioned index" : "an ordinary index";
            failures.add(schema + "." + index + " still exists: " + kind
                    + " (pg_class.relkind = '" + snapshot.getNamedIndexRelkind() + "') on "
                    + snapshot.getNamedIndexOnTable() + ".");
        }

        List<String> orphans = new ArrayList<String>();
        for (GateLeaf leaf : snapshot.getLeaves()) {
            String conventional = IndexNaming.childIndexName(index, leaf.getTableName());
            for (GateIndex candidate : leaf.getIndexes()) {
                // Only UNATTACHED indexes are orphans. An index of the convention name that is
                // still attached to something belongs to a live tree -- either the parent this
                // gate is complaining about above, in which case saying it a second time as a
                // "free-standing leftover" would be both noise and false, or some other
                // partitioned index, which is none of this gate's business. Nothing that was
                // ever attached can be a leftover anyway: an interrupted CREATE INDEX
                // CONCURRENTLY never reached its ATTACH, and a _ccnew / _ccold is never attached
                // at all (measured, attached = 0 in both states).
                if (candidate.isAttachedToAnyParent()) {
                    continue;
                }
                String name = candidate.getIndexName();
                if (name.equals(conventional)) {
                    orphans.add(leaf.getSchemaName() + "." + name);
                } else if (Leftovers.classify(name, conventional) != null) {
                    orphans.add(leaf.getSchemaName() + "." + name + " (a "
                            + Leftovers.classify(name, conventional)
                            + " leftover of an interrupted REINDEX)");
                }
            }
        }

        if (!orphans.isEmpty()) {
            failures.add(orphans.size() + " child index(es) of " + schema + "." + index
                    + " are still on the leaf partitions: " + summarise(sorted(orphans))
                    + ". These are free-standing — dropping the partitioned parent does not "
                    + "remove an index that was never attached to it — and each is still "
                    + "maintained on every write to its partition. Remove them with "
                    + "DROP INDEX CONCURRENTLY, or with <ext:dropPartitionedTableIndex>, which "
                    + "cleans up leftovers as well as the tree.");
        }
    }
}
