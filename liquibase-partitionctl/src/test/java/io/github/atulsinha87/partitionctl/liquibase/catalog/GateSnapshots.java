package io.github.atulsinha87.partitionctl.liquibase.catalog;

/**
 * Builds {@link GateSnapshot} fixtures for the precondition tests.
 *
 * <p>It lives in this package, in test sources, because {@code GateSnapshot} and {@code GateLeaf}
 * keep their mutators package-private: only {@link GateInspection} should be able to assemble one
 * from outside a test. The alternative — widening those setters to public so a test in another
 * package could reach them — would make a snapshot look like something callers are meant to
 * construct.
 *
 * <p>Every fixture here mirrors a state that was produced against a live PostgreSQL 17.10, not an
 * imagined one. The interesting ones are named for what produced them.
 */
public final class GateSnapshots {

    private final GateSnapshot snapshot = new GateSnapshot();

    private GateSnapshots() {
    }

    /** An empty snapshot: no table of that name, no index of that name. */
    public static GateSnapshots none() {
        return new GateSnapshots();
    }

    /** A partitioned table exists. */
    public static GateSnapshots partitionedTable() {
        GateSnapshots builder = new GateSnapshots();
        builder.snapshot.setRootRelkind("p");
        return builder;
    }

    /** An ordinary table exists under the gated name. */
    public static GateSnapshots ordinaryTable() {
        GateSnapshots builder = new GateSnapshots();
        builder.snapshot.setRootRelkind("r");
        return builder;
    }

    /** A partitioned index ({@code relkind='I'}) on {@code onTable}, with the three flags. */
    public GateSnapshots partitionedIndex(String onTable, boolean valid, boolean ready,
                                          boolean live) {
        snapshot.setNamedIndexExists(true);
        snapshot.setNamedIndexRelkind("I");
        snapshot.setNamedIndexOnTable(onTable);
        snapshot.setNamedIndexValid(valid);
        snapshot.setNamedIndexReady(ready);
        snapshot.setNamedIndexLive(live);
        return this;
    }

    /** A healthy partitioned index on {@code onTable}. */
    public GateSnapshots partitionedIndex(String onTable) {
        return partitionedIndex(onTable, true, true, true);
    }

    /** An ordinary index ({@code relkind='i'}) under the gated index name. */
    public GateSnapshots ordinaryIndex(String onTable) {
        snapshot.setNamedIndexExists(true);
        snapshot.setNamedIndexRelkind("i");
        snapshot.setNamedIndexOnTable(onTable);
        snapshot.setNamedIndexValid(true);
        snapshot.setNamedIndexReady(true);
        snapshot.setNamedIndexLive(true);
        return this;
    }

    /** A leaf with no indexes at all. */
    public GateSnapshots bareLeaf(String schema, String name) {
        snapshot.addLeaf(new GateLeaf(schema, name));
        return this;
    }

    /** A leaf whose covering index is healthy. */
    public GateSnapshots coveredLeaf(String schema, String name, String indexName) {
        return leafWithIndex(schema, name, indexName, true, true, true, true);
    }

    /** A leaf carrying one index, with every flag stated explicitly. */
    public GateSnapshots leafWithIndex(String schema, String name, String indexName,
                                       boolean valid, boolean ready, boolean live,
                                       boolean covering) {
        GateLeaf leaf = new GateLeaf(schema, name);
        leaf.addIndex(new GateIndex(indexName, valid, ready, live, covering, covering));
        snapshot.addLeaf(leaf);
        return this;
    }

    /** Adds one more index to the most recently added leaf. */
    public GateSnapshots andIndex(String indexName, boolean valid, boolean ready, boolean live,
                                  boolean covering) {
        GateLeaf last = snapshot.getLeaves().get(snapshot.getLeaves().size() - 1);
        last.addIndex(new GateIndex(indexName, valid, ready, live, covering, covering));
        return this;
    }

    /**
     * The state a cancelled {@code REINDEX INDEX CONCURRENTLY} leaves: an unattached leftover
     * beside a healthy covering index. Measured flags — {@code indisvalid=f}, {@code indislive=t};
     * {@code indisready} is f for a rebuild killed mid-build and t for one killed after it.
     */
    public GateSnapshots andLeftover(String indexName, boolean ready) {
        return andIndex(indexName, false, ready, true, false);
    }

    public GateSnapshot build() {
        return snapshot;
    }
}
