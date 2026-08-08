package io.github.atulsinha87.partitionctl.liquibase.catalog;

/**
 * Thrown at plan time when the deterministic child-index name for two different leaf
 * partitions in the same schema collides after truncation to PostgreSQL's 63-byte
 * identifier limit.
 *
 * <p>There is deliberately no clever disambiguation. A hash suffix or a counter would
 * make the name non-deterministic across runs, and the resume design depends entirely
 * on re-discovering the exact name a previous run generated.
 */
public class IndexNamingException extends RuntimeException {

    private static final long serialVersionUID = 1L;

    public IndexNamingException(String message) {
        super(message);
    }
}
