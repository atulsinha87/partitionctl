package io.github.atulsinha87.partitionctl.liquibase.catalog;

/**
 * The {@code COMMENT ON INDEX} ownership marker, and the rule for recognising it.
 *
 * <h2>What it is for</h2>
 * {@code <dropPartitionedTableIndex>} destroys a whole index tree with one line of XML. The
 * marker is how the drop answers "did this plugin build the thing I am about to delete?", so
 * that a changelog cannot be pointed at an index somebody else created and quietly remove it.
 *
 * <h2>It is EVIDENCE, not authorisation</h2>
 * Say this plainly rather than implying a security boundary: any user who can
 * {@code COMMENT ON INDEX} can write this string by hand, and any user who can run this
 * changeset can already run {@code DROP INDEX} directly. The marker stops an <em>accident</em>
 * — a copied changelog, a typo'd {@code indexName}, a name that happens to match an index the
 * DBA team built — not an adversary. The second half of the guard,
 * {@code confirmExclusiveLock}, answers a different question that the marker cannot: not "is
 * this ours" but "do you accept locking the entire table". Both are required and neither
 * substitutes for the other.
 *
 * <h2>Format</h2>
 * <pre>
 * partitionctl owner=liquibase parent=idx_personaddress table=public.person changeset=db/changelog.xml::7::alice
 * </pre>
 * Recognition is a <b>prefix match on {@link #PREFIX} alone</b>. The tail is written for a
 * human reading {@code \d+} output and is never parsed, so adding a field to it later cannot
 * make an already-stamped index unrecognisable.
 *
 * <h2>Deliberately does NOT recognise the Go CLI's marker</h2>
 * The Go CLI stamps {@code partitionctl:v1:{...json...}}, which does not match this prefix.
 * That is intended: the two products are independent by decision and do not interoperate. This extension refuses to drop a tree the Go CLI built, and says so.
 */
public final class OwnershipMarker {

    /** The recognised prefix. Everything after it is human-readable and never parsed. */
    public static final String PREFIX = "partitionctl owner=liquibase";

    private OwnershipMarker() {
    }

    /**
     * The marker text for a child index built by {@code createPartitionedTableIndex}.
     *
     * <p>Provided here so that the create change and the drop change cannot drift: one class
     * writes the string, the same class recognises it.
     *
     * @param parentIndexName the partitioned index the child belongs to
     * @param schemaName      schema of the partitioned table
     * @param tableName       the partitioned table
     * @param changeSetId     Liquibase's {@code path::id::author}, or null to omit the field
     */
    public static String forChildIndex(String parentIndexName, String schemaName,
                                       String tableName, String changeSetId) {
        StringBuilder marker = new StringBuilder(PREFIX);
        marker.append(" parent=").append(parentIndexName);
        marker.append(" table=").append(schemaName).append('.').append(tableName);
        if (changeSetId != null && !changeSetId.trim().isEmpty()) {
            marker.append(" changeset=").append(changeSetId.trim());
        }
        return marker.toString();
    }

    /**
     * True when this comment was written by this extension.
     *
     * <p>A bare prefix match would also accept {@code "partitionctl owner=liquibasesomething"},
     * so the prefix must be either the whole comment or followed by a space.
     */
    public static boolean isOurs(String comment) {
        if (comment == null) {
            return false;
        }
        String trimmed = comment.trim();
        return trimmed.equals(PREFIX) || trimmed.startsWith(PREFIX + " ");
    }

    /**
     * True when a comment is present, is not ours, and is therefore somebody else's — a DBA's
     * change-ticket note, or the Go CLI's {@code partitionctl:v1:} marker. Distinguished from
     * "no comment at all" because the two mean different things to the drop: an absent comment
     * is merely missing evidence, a foreign comment is a positive signal to keep hands off.
     */
    public static boolean isForeign(String comment) {
        return comment != null && !comment.trim().isEmpty() && !isOurs(comment);
    }

    /** {@code COMMENT ON INDEX <qualified> IS '<marker>'}, correctly escaped. */
    public static String commentStatement(String schemaName, String indexName, String marker) {
        return "COMMENT ON INDEX " + Identifiers.qualified(schemaName, indexName)
                + " IS " + Identifiers.literal(marker);
    }
}
