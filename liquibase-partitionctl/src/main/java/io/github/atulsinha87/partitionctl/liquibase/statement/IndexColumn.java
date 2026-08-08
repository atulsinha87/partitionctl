package io.github.atulsinha87.partitionctl.liquibase.statement;

/** One column of the index being built. Deliberately independent of Liquibase types. */
public final class IndexColumn {

    private final String name;
    private final boolean descending;

    public IndexColumn(String name, boolean descending) {
        this.name = name;
        this.descending = descending;
    }

    public String getName() {
        return name;
    }

    public boolean isDescending() {
        return descending;
    }

    @Override
    public String toString() {
        return descending ? name + " DESC" : name;
    }
}
