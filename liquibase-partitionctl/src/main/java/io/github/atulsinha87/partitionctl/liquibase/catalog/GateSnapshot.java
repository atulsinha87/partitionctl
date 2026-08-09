package io.github.atulsinha87.partitionctl.liquibase.catalog;

import java.util.ArrayList;
import java.util.Collections;
import java.util.List;

/** Everything one read-only gate query learned, in one consistent snapshot. */
public final class GateSnapshot {

    private String rootRelkind;
    private boolean namedIndexExists;
    private String namedIndexRelkind;
    private boolean namedIndexValid;
    private boolean namedIndexReady;
    private boolean namedIndexLive;
    private String namedIndexOnTable;
    private final List<GateLeaf> leaves = new ArrayList<GateLeaf>();

    /**
     * {@code pg_class.relkind} of the named table: {@code 'p'} partitioned, {@code 'r'} ordinary,
     * null when no relation of that name exists.
     */
    public String getRootRelkind() {
        return rootRelkind;
    }

    void setRootRelkind(String rootRelkind) {
        this.rootRelkind = rootRelkind;
    }

    /** True when an index of the gated name exists in the schema — on any table. */
    public boolean isNamedIndexExists() {
        return namedIndexExists;
    }

    void setNamedIndexExists(boolean namedIndexExists) {
        this.namedIndexExists = namedIndexExists;
    }

    /** {@code 'I'} for a partitioned index, {@code 'i'} for an ordinary one, null when absent. */
    public String getNamedIndexRelkind() {
        return namedIndexRelkind;
    }

    void setNamedIndexRelkind(String namedIndexRelkind) {
        this.namedIndexRelkind = namedIndexRelkind;
    }

    public boolean isNamedIndexValid() {
        return namedIndexValid;
    }

    void setNamedIndexValid(boolean namedIndexValid) {
        this.namedIndexValid = namedIndexValid;
    }

    public boolean isNamedIndexReady() {
        return namedIndexReady;
    }

    void setNamedIndexReady(boolean namedIndexReady) {
        this.namedIndexReady = namedIndexReady;
    }

    public boolean isNamedIndexLive() {
        return namedIndexLive;
    }

    void setNamedIndexLive(boolean namedIndexLive) {
        this.namedIndexLive = namedIndexLive;
    }

    /**
     * {@code schema.table} the named index actually sits on.
     *
     * <p>Index names are unique per schema but say nothing about which table they belong to, so a
     * changeset naming the right index and the wrong table would otherwise gate on an index it
     * has no relationship with.
     */
    public String getNamedIndexOnTable() {
        return namedIndexOnTable;
    }

    void setNamedIndexOnTable(String namedIndexOnTable) {
        this.namedIndexOnTable = namedIndexOnTable;
    }

    /** Every leaf of the named table, at any depth. Empty when the table does not exist. */
    public List<GateLeaf> getLeaves() {
        return Collections.unmodifiableList(leaves);
    }

    void addLeaf(GateLeaf leaf) {
        leaves.add(leaf);
    }

    /** True when a relation of the gated table name exists at all. */
    public boolean tableExists() {
        return rootRelkind != null;
    }

    /** True when the gated table is partitioned ({@code relkind = 'p'}). */
    public boolean tableIsPartitioned() {
        return "p".equals(rootRelkind);
    }

    /** True when the named index is a partitioned index ({@code relkind = 'I'}). */
    public boolean namedIndexIsPartitioned() {
        return "I".equals(namedIndexRelkind);
    }
}
