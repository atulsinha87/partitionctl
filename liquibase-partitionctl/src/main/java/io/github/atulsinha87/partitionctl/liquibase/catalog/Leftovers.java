package io.github.atulsinha87.partitionctl.liquibase.catalog;

import java.io.UnsupportedEncodingException;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * Recognises the transient indexes {@code REINDEX INDEX CONCURRENTLY} leaves behind when it is
 * interrupted, and says which of the two it is.
 *
 * <h2>The two leftovers mean opposite things</h2>
 * <table>
 *   <caption>measured on PostgreSQL 17.10</caption>
 *   <tr><th>leftover</th><th>state</th><th>what it proves</th></tr>
 *   <tr><td>{@code <base>_ccnew}</td><td>indisvalid=f, unattached</td>
 *       <td>the rebuild <b>failed</b>; the base index is untouched and still stale</td></tr>
 *   <tr><td>{@code <base>_ccold}</td><td>indisvalid=f, unattached</td>
 *       <td>the rebuild <b>succeeded</b>; the base index already holds the new relfilenode</td></tr>
 * </table>
 *
 * <p>Both were produced live. A {@code _ccnew} was made by cancelling a leaf rebuild; the base
 * index kept its relfilenode. A {@code _ccold} was made by parking an idle-in-transaction
 * {@code AccessShareLock} holder on the leaf so the reindex stalled in the post-swap wait; the
 * base index had already taken a <em>new</em> relfilenode and the {@code _ccold} carried the old
 * one. Their {@code pg_index} flags are otherwise identical — {@code indisvalid=f},
 * {@code indisready=t}, {@code indislive=t}, unattached — so <b>only the suffix distinguishes
 * them</b>, which is why this class exists.
 *
 * <h2>Why the name cannot simply be {@code base + "_ccnew"}</h2>
 * PostgreSQL builds the name with {@code makeObjectName}, which truncates the <em>base</em> so
 * that base + {@code '_'} + label fits in {@code NAMEDATALEN - 1} = 63 bytes. Measured: a
 * 63-byte index named {@code idx_e6_aaaa…aaaa} produced
 * {@code idx_e6_aaaa…aa_ccnew}, whose base part is 57 bytes, not 63.
 *
 * <p>So the base name is <b>not recoverable</b> from the leftover name — the Go product has a
 * standing HIGH defect from trying (issue #2). This class runs the derivation in the
 * only direction that is total: it takes the base name we already know from the catalog and asks
 * whether a candidate leftover is the name PostgreSQL <em>would</em> have derived from it.
 */
public final class Leftovers {

    /** {@code NAMEDATALEN - 1}, the number of bytes PostgreSQL keeps of an identifier. */
    public static final int NAMEDATALEN_BYTES = 63;

    /** Which of the two leftovers a name is. */
    public enum Kind {
        /** The rebuild failed. Drop it and reindex the leaf anyway. */
        CCNEW,
        /** The rebuild succeeded and only the old copy survived. Drop it and skip the leaf. */
        CCOLD
    }

    /**
     * {@code ChooseRelationName} tries the label {@code ccnew}, then {@code ccnew1},
     * {@code ccnew2}, … until the name is free, so the trailing digits are part of the label and
     * change how many bytes of the base survive.
     */
    private static final Pattern LEFTOVER = Pattern.compile("^(.*)_cc(new|old)([0-9]*)$");

    private static final String UTF8 = "UTF-8";

    private Leftovers() {
    }

    /**
     * Whether {@code leftoverName} is the leftover PostgreSQL would have created while
     * reindexing {@code baseName}, and if so which kind. Null when it is not ours — a leftover
     * of some other index on the same partition, or an ordinary index that merely ends in
     * {@code _ccnew}.
     */
    public static Kind classify(String leftoverName, String baseName) {
        if (leftoverName == null || baseName == null) {
            return null;
        }
        Matcher matcher = LEFTOVER.matcher(leftoverName);
        if (!matcher.matches()) {
            return null;
        }
        String prefix = matcher.group(1);
        // makeObjectName(): availchars = NAMEDATALEN - 1 - (strlen(label) + 1), and the label is
        // "cc" + kind + any disambiguating digits. All ASCII, so chars == bytes here.
        int labelBytes = 2 + matcher.group(2).length() + matcher.group(3).length();
        int available = NAMEDATALEN_BYTES - 1 - labelBytes;
        if (available <= 0) {
            return null;
        }
        if (!prefix.equals(clipToBytes(baseName, available))) {
            return null;
        }
        return "new".equals(matcher.group(2)) ? Kind.CCNEW : Kind.CCOLD;
    }

    /**
     * Clips to {@code maxBytes} UTF-8 bytes without splitting a multi-byte character, the way
     * {@code pg_mbcliplen} does. Byte-aware, not character-aware: a 40-character name of 3-byte
     * characters is 120 bytes.
     */
    public static String clipToBytes(String identifier, int maxBytes) {
        byte[] bytes = utf8(identifier);
        if (bytes.length <= maxBytes) {
            return identifier;
        }
        // bytes[maxBytes] is the first byte we are not keeping. If it is a UTF-8 continuation
        // byte (10xxxxxx) the character straddles the boundary, so drop that character whole.
        int end = maxBytes;
        while (end > 0 && (bytes[end] & 0xC0) == 0x80) {
            end--;
        }
        return decode(bytes, end);
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
