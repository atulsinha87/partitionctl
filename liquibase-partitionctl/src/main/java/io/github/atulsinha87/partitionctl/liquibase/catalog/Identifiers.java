package io.github.atulsinha87.partitionctl.liquibase.catalog;

/**
 * PostgreSQL identifier and literal quoting.
 *
 * <p>Everything this extension emits is built by string concatenation, because the
 * statements are DDL and DDL cannot be parameterised. Every identifier that reaches
 * SQL therefore goes through {@link #quote(String)} and every string value through
 * {@link #literal(String)}.
 */
public final class Identifiers {

    private Identifiers() {
    }

    /** Double-quotes an identifier, doubling any embedded double quote. */
    public static String quote(String identifier) {
        if (identifier == null) {
            throw new IllegalArgumentException("identifier must not be null");
        }
        return "\"" + identifier.replace("\"", "\"\"") + "\"";
    }

    /** Single-quotes a string literal, doubling any embedded single quote. */
    public static String literal(String value) {
        if (value == null) {
            return "NULL";
        }
        return "'" + value.replace("'", "''") + "'";
    }

    /** {@code schema.name}, both quoted. */
    public static String qualified(String schema, String name) {
        return quote(schema) + "." + quote(name);
    }

    /**
     * Wraps a PL/pgSQL body in a {@code DO} block, using a dollar-quote tag chosen so that it
     * cannot occur inside the body.
     *
     * <p>The tag used to be a fixed {@code $partitionctl$}. Identifiers reach these blocks as
     * literals, and {@link #literal(String)} doubles single quotes but leaves {@code $} alone,
     * so a schema, table or index name containing the text {@code $partitionctl$} closed the
     * block early and whatever followed became a statement in its own right. Through pgjdbc
     * that is a hard changeset failure rather than execution — the driver sends one Sync and
     * the server abandons the batch at the first error — but {@code liquibase updateSQL} writes
     * the same text into a migration script, and psql runs each statement separately, so there
     * the stacked statement does run.
     *
     * <p>Lengthening the tag until it is absent removes the possibility instead of bounding the
     * damage, and needs no validation rule for adopters to fall foul of.
     */
    public static String doBlock(String body) {
        String tag = "partitionctl";
        while (body.contains("$" + tag + "$")) {
            tag = tag + "_";
        }
        return "DO $" + tag + "$\n" + body + "$" + tag + "$";
    }
}
