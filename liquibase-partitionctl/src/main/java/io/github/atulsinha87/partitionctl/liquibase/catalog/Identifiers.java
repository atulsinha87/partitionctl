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
}
