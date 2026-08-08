package io.github.atulsinha87.partitionctl.liquibase.statement;

/**
 * Thrown at plan time when the discovered catalog state cannot be turned into a safe
 * statement list. Nothing has been executed when this is thrown.
 */
public class PlanException extends RuntimeException {

    private static final long serialVersionUID = 1L;

    public PlanException(String message) {
        super(message);
    }
}
