package io.github.atulsinha87.partitionctl.liquibase.precondition;

import io.github.atulsinha87.partitionctl.liquibase.catalog.GateIndex;
import io.github.atulsinha87.partitionctl.liquibase.catalog.GateLeaf;
import io.github.atulsinha87.partitionctl.liquibase.catalog.GateSnapshot;

import java.util.ArrayList;
import java.util.List;

/**
 * {@code <ext:partitionctlIndexGate schema="..." table="..." index="..."/>}
 *
 * <p>Passes when a partitioned index of that name exists on that table and <b>every leaf
 * partition</b> carries a child index that is attached to it and is valid, ready and live.
 *
 * <pre>{@code
 * <changeSet author="x" id="9">
 *   <preConditions onFail="HALT">
 *     <ext:partitionctlIndexGate schema="public" table="person" index="idx_personaddress"/>
 *   </preConditions>
 *   <sql>ALTER TABLE person ADD CONSTRAINT ... </sql>
 * </changeSet>
 * }</pre>
 *
 * <p>Useful independently of the changes in this jar: it lets a different changeset assert an
 * index is really usable before depending on it, including when the index was built out of band
 * by the Go CLI or by hand.
 *
 * <h2>What "exists" is not</h2>
 * An earlier version of this class checked {@code pg_indexes} for the name and nothing else.
 * Measured on 17.10, that check passes on a tree where {@code CREATE INDEX ON ONLY} has run and
 * <em>not one</em> of six leaves is attached — the parent is {@code indisvalid = f} and covers
 * nothing, and {@code pg_indexes} lists it anyway. A gate whose whole purpose is "safe to depend
 * on" cannot answer that question from the index's name.
 *
 * <h2>{@code requireValidParent}, and why it defaults to false</h2>
 * A parent index can be left permanently {@code indisvalid = f} — attach an invalid child and no
 * later {@code ATTACH} can ever validate it, and no {@code REINDEX} variant clears it either
 * (limitation L3, re-measured on 17.10). But that flag <b>does not affect query planning</b>: the
 * planner uses the leaf indexes, and partitions added later are still covered. So the default is
 * to pass on leaves alone. Hard-failing by default would turn one unrepairable flag into a
 * deployment that fails forever with no path forward — the same severity split the changes use,
 * where an invalid leaf is fatal and an invalid parent is a warning.
 *
 * <p>Set {@code requireValidParent="true"} when the tree is meant to be pristine and you would
 * rather stop than proceed. The adopter chooses strictness; the default chooses recoverability.
 */
public class IndexGatePrecondition extends PartitionedIndexPrecondition {

    // MUST have an identically named attribute in partitionctl.xsd. A mismatch binds to null
    // silently: requireValidParent would then read as "not set" and the strictness an adopter
    // asked for would quietly not happen. PreconditionXsdBindingTest enforces the match.
    private Boolean requireValidParent;

    /**
     * When true, the gate additionally requires {@code pg_index.indisvalid} on the partitioned
     * parent itself. Default false — see the class comment.
     */
    public Boolean getRequireValidParent() {
        return requireValidParent;
    }

    public void setRequireValidParent(Boolean requireValidParent) {
        this.requireValidParent = requireValidParent;
    }

    @Override
    public String getName() {
        return "partitionctlIndexGate";
    }

    @Override
    protected void evaluate(GateSnapshot snapshot, List<String> failures) {
        if (!requireLeafPartitions(snapshot, failures)) {
            return;
        }
        requireHealthyTree(snapshot, failures, Boolean.TRUE.equals(requireValidParent),
                getSchemaName(), getTableName(), getIndexName());
    }

    /**
     * The tree assertion shared with {@link ReindexGatePrecondition}: the named index is a
     * partitioned index on the named table, and every leaf is covered by a usable child index.
     *
     * @return true when the tree checks out, so a caller can go on to look at leftovers
     */
    static boolean requireHealthyTree(GateSnapshot snapshot, List<String> failures,
                                      boolean requireValidParent, String schema, String table,
                                      String index) {
        if (!snapshot.isNamedIndexExists()) {
            failures.add("no index named " + schema + "." + index + " exists.");
            return false;
        }
        if (!snapshot.namedIndexIsPartitioned()) {
            failures.add(schema + "." + index + " is an ordinary index (pg_class.relkind = '"
                    + snapshot.getNamedIndexRelkind() + "', expected 'I'), not a partitioned "
                    + "index. For an ordinary index use Liquibase's own <indexExists> "
                    + "precondition.");
            return false;
        }
        String onTable = snapshot.getNamedIndexOnTable();
        if (onTable != null && !onTable.equals(schema + "." + table)) {
            // Index names are unique per schema but say nothing about which table they belong
            // to, so without this a changeset naming the right index and the wrong table would
            // gate on an index it has no relationship with, and pass.
            failures.add(schema + "." + index + " is a partitioned index on " + onTable
                    + ", not on the " + schema + "." + table + " named by this gate.");
            return false;
        }
        if (!snapshot.isNamedIndexLive() || !snapshot.isNamedIndexReady()) {
            failures.add("the partitioned index " + schema + "." + index + " is not usable "
                    + "(indisready=" + snapshot.isNamedIndexReady()
                    + ", indislive=" + snapshot.isNamedIndexLive()
                    + "); indislive=false means a DROP INDEX CONCURRENTLY is in progress.");
            return false;
        }

        List<String> uncovered = new ArrayList<String>();
        List<String> unusable = new ArrayList<String>();
        for (GateLeaf leaf : snapshot.getLeaves()) {
            GateIndex covering = leaf.getCoveringIndex();
            if (covering == null) {
                uncovered.add(leaf.toString());
            } else if (!covering.isUsable()) {
                unusable.add(leaf + " (" + covering.getIndexName() + ": "
                        + covering.describeFlags() + ")");
            }
        }

        int leaves = snapshot.getLeaves().size();
        if (!uncovered.isEmpty()) {
            failures.add(uncovered.size() + " of " + leaves + " leaf partition(s) have no child "
                    + "index attached to " + schema + "." + index + ": "
                    + summarise(sorted(uncovered)) + ".");
        }
        if (!unusable.isEmpty()) {
            failures.add(unusable.size() + " of " + leaves + " leaf partition(s) are covered by an "
                    + "index that is not usable: " + summarise(sorted(unusable)) + ".");
        }
        if (!uncovered.isEmpty() || !unusable.isEmpty()) {
            return false;
        }

        if (requireValidParent && !snapshot.isNamedIndexValid()) {
            failures.add("every one of the " + leaves + " leaf partition(s) is covered and usable, "
                    + "but the partitioned index " + schema + "." + index + " itself is "
                    + "indisvalid=false and requireValidParent=\"true\" was set. This flag does "
                    + "not affect query planning and cannot be cleared by any REINDEX variant "
                    + "(limitation L3); only dropping and rebuilding the index clears it.");
            return false;
        }
        return true;
    }
}
