package io.github.atulsinha87.partitionctl.liquibase.catalog;

import java.io.UnsupportedEncodingException;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Deterministic child-index names.
 *
 * <p>Child index names are <em>generated</em>, never read back from PostgreSQL, because
 * a name PostgreSQL chose for itself cannot be correlated with a leaf on a later run.
 * The whole resume design is: re-run, re-discover, and recognise the names a previous
 * attempt generated. So the function must be pure and stable.
 *
 * <h2>NAMEDATALEN</h2>
 * PostgreSQL identifiers are capped at {@code NAMEDATALEN - 1} = <b>63 bytes</b>, and the
 * server truncates silently — no error, no warning. If this class generated a 70-byte
 * name, PostgreSQL would store 63, and the next run would look for the 70-byte form,
 * fail to find it, and rebuild an index that already exists. Every run. Forever.
 *
 * <p>So truncation happens here, at exactly 63 <b>bytes</b> of UTF-8, and it never splits
 * a multi-byte sequence — matching {@code pg_mbcliplen} / {@code truncate_identifier} in
 * the server. Character-aware truncation would be wrong: a 40-character name of 3-byte
 * characters is 120 bytes.
 *
 * <h2>Truncation alone is not enough</h2>
 * Two leaves that share a long prefix truncate to the same 63 bytes. That is not exotic: a table
 * named {@code order_product_v2} with date-range partitions overflows on any descriptive index
 * name, and every leaf collapses to the same child name. Until 0.1.4 this threw and nothing ran.
 *
 * <p>So an overflowing name is disambiguated instead: the readable part is clipped to make room,
 * and a short tag derived from the <em>untruncated</em> inputs is appended. Two leaves that differ
 * anywhere differ in the tag, so the result is collision-free by construction rather than by luck,
 * and it stays a pure function of (indexName, leafTableName) — which is what the resume design
 * needs. {@link IndexNamingException} remains for the case the tag cannot fix.
 *
 * <p>A name that fits but already <em>looks</em> tagged is tagged too. Otherwise a leaf could be
 * named exactly what some longer leaf truncates to, and the two would generate the same child
 * name — the collision this is meant to remove, reachable by naming a partition.
 *
 * <p>Changing a generated name does not disturb an index already built: coverage is decided from
 * {@code pg_inherits} ({@link LeafPartition#getCoveringIndex()}), never from the name. The name
 * only identifies an <em>unattached</em> child left by an interrupted run.
 */
public final class IndexNaming {

    /** {@code NAMEDATALEN - 1}. The number of bytes PostgreSQL keeps of an identifier. */
    public static final int NAMEDATALEN_BYTES = 63;

    /** Characters of base32 in the disambiguating tag. 12 gives 60 bits. */
    static final int TAG_LENGTH = 12;

    /** Domain separator, so this hash cannot collide with any other use of SHA-256 here. */
    private static final String TAG_DOMAIN = "partitionctl.childindex.v1";

    private static final String BASE32_ALPHABET = "abcdefghijklmnopqrstuvwxyz234567";

    private static final String UTF8 = "UTF-8";

    private IndexNaming() {
    }

    /**
     * The child index name for one leaf: {@code <indexName>_<leafTableName>}, clipped to
     * 63 UTF-8 bytes on a character boundary.
     */
    public static String childIndexName(String indexName, String leafTableName) {
        if (indexName == null || indexName.isEmpty()) {
            throw new IllegalArgumentException("indexName must not be empty");
        }
        if (leafTableName == null || leafTableName.isEmpty()) {
            throw new IllegalArgumentException("leafTableName must not be empty");
        }
        String natural = indexName + "_" + leafTableName;
        if (byteLength(natural) <= NAMEDATALEN_BYTES && !looksTagged(natural)) {
            return natural;
        }
        String tag = childIndexTag(indexName, leafTableName);
        int budget = NAMEDATALEN_BYTES - 1 - tag.length();
        if (budget < 0) {
            budget = 0;
        }
        return clipToBytes(natural, budget) + "_" + tag;
    }

    /**
     * The disambiguating suffix: {@value #TAG_LENGTH} characters of lowercase RFC 4648 base32 over
     * a domain-separated SHA-256 of the untruncated inputs.
     *
     * <p>Base32 rather than hex because its alphabet excludes {@code 0 1 8 9}, so an ordinary name
     * ending in a date — {@code ..._20260601} — cannot be mistaken for a tag by
     * {@link #looksTagged(String)}. Inputs are length-prefixed so that ("ab","c") and ("a","bc")
     * cannot hash alike.
     */
    static String childIndexTag(String indexName, String leafTableName) {
        try {
            MessageDigest digest = MessageDigest.getInstance("SHA-256");
            writeLengthPrefixed(digest, TAG_DOMAIN);
            writeLengthPrefixed(digest, indexName);
            writeLengthPrefixed(digest, leafTableName);
            return base32(digest.digest(), TAG_LENGTH);
        } catch (NoSuchAlgorithmException e) {
            // SHA-256 is required of every Java SE implementation.
            throw new IllegalStateException("SHA-256 unavailable", e);
        }
    }

    /**
     * Whether a name already ends in something shaped like a tag: an underscore followed by
     * exactly {@value #TAG_LENGTH} lowercase base32 characters.
     */
    static boolean looksTagged(String s) {
        if (s.length() < TAG_LENGTH + 1) {
            return false;
        }
        if (s.charAt(s.length() - TAG_LENGTH - 1) != '_') {
            return false;
        }
        for (int i = s.length() - TAG_LENGTH; i < s.length(); i++) {
            char c = s.charAt(i);
            if ((c < 'a' || c > 'z') && (c < '2' || c > '7')) {
                return false;
            }
        }
        return true;
    }

    private static void writeLengthPrefixed(MessageDigest digest, String value) {
        byte[] bytes = utf8(value);
        digest.update((byte) (bytes.length >>> 24));
        digest.update((byte) (bytes.length >>> 16));
        digest.update((byte) (bytes.length >>> 8));
        digest.update((byte) bytes.length);
        digest.update(bytes);
    }

    /** Lowercase RFC 4648 base32 of the first bytes of {@code data}, truncated to {@code chars}. */
    private static String base32(byte[] data, int chars) {
        StringBuilder out = new StringBuilder(chars);
        int buffer = 0;
        int bits = 0;
        for (int i = 0; i < data.length && out.length() < chars; i++) {
            buffer = (buffer << 8) | (data[i] & 0xFF);
            bits += 8;
            while (bits >= 5 && out.length() < chars) {
                bits -= 5;
                out.append(BASE32_ALPHABET.charAt((buffer >>> bits) & 0x1F));
            }
        }
        return out.toString();
    }

    /** Like {@link #clipToNameDataLen(String)} but to an arbitrary byte budget. */
    private static String clipToBytes(String identifier, int maxBytes) {
        byte[] bytes = utf8(identifier);
        if (bytes.length <= maxBytes) {
            return identifier;
        }
        int end = maxBytes;
        while (end > 0 && (bytes[end] & 0xC0) == 0x80) {
            end--;
        }
        return decode(bytes, end);
    }

    /**
     * Clips an identifier to {@link #NAMEDATALEN_BYTES} bytes of UTF-8 without splitting a
     * multi-byte character. Identifiers already within the limit are returned unchanged.
     */
    public static String clipToNameDataLen(String identifier) {
        byte[] bytes = utf8(identifier);
        if (bytes.length <= NAMEDATALEN_BYTES) {
            return identifier;
        }
        // bytes[NAMEDATALEN_BYTES] is the first byte we are NOT keeping. If it is a UTF-8
        // continuation byte (10xxxxxx) the character it belongs to straddles the boundary,
        // so walk back to the start of that character and drop it whole.
        int end = NAMEDATALEN_BYTES;
        while (end > 0 && (bytes[end] & 0xC0) == 0x80) {
            end--;
        }
        return decode(bytes, end);
    }

    /** Length of {@code s} in UTF-8 bytes. */
    public static int byteLength(String s) {
        return utf8(s).length;
    }

    /**
     * Assigns {@link LeafPartition#getChildIndexName()} for every leaf, and fails loudly if
     * two leaves in the same schema end up with the same name after truncation.
     *
     * @throws IndexNamingException on collision, naming both leaves and the shared name
     */
    public static void assignChildIndexNames(String indexName, List<LeafPartition> leaves) {
        // key is schema + '\0' + name, because index names are unique per schema, and a
        // partitioned table's leaves are allowed to live in different schemas.
        Map<String, LeafPartition> taken = new LinkedHashMap<String, LeafPartition>();
        List<String> collisions = new ArrayList<String>();

        for (LeafPartition leaf : leaves) {
            String child = childIndexName(indexName, leaf.getTableName());
            leaf.setChildIndexName(child);

            String key = leaf.getSchemaName() + "\0" + child;
            LeafPartition previous = taken.put(key, leaf);
            if (previous != null) {
                collisions.add("\"" + child + "\" would name the child index of BOTH "
                        + previous.getSchemaName() + "." + previous.getTableName()
                        + " AND " + leaf.getSchemaName() + "." + leaf.getTableName());
            }
        }

        if (!collisions.isEmpty()) {
            StringBuilder message = new StringBuilder();
            message.append("partitionctl: child index names collide after truncation to ")
                    .append(NAMEDATALEN_BYTES)
                    .append(" bytes (PostgreSQL's NAMEDATALEN limit). ");
            for (int i = 0; i < collisions.size(); i++) {
                if (i > 0) {
                    message.append("; ");
                }
                message.append(collisions.get(i));
            }
            message.append(". Nothing was executed. Shorten indexName=\"")
                    .append(indexName)
                    .append("\" (currently ")
                    .append(byteLength(indexName))
                    .append(" bytes) so that indexName + '_' + the longest leaf table name ")
                    .append("stays distinct within ")
                    .append(NAMEDATALEN_BYTES)
                    .append(" bytes.");
            throw new IndexNamingException(message.toString());
        }
    }

    private static byte[] utf8(String s) {
        try {
            return s.getBytes(UTF8);
        } catch (UnsupportedEncodingException e) {
            throw new IllegalStateException("UTF-8 is required by the JVM spec", e);
        }
    }

    private static String decode(byte[] bytes, int length) {
        try {
            return new String(bytes, 0, length, UTF8);
        } catch (UnsupportedEncodingException e) {
            throw new IllegalStateException("UTF-8 is required by the JVM spec", e);
        }
    }
}
