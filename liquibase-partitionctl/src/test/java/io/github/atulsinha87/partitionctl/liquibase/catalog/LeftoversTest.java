package io.github.atulsinha87.partitionctl.liquibase.catalog;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNull;

/**
 * Every expectation here was taken from a live PostgreSQL 17.10, not from the documentation.
 * The name arithmetic in particular: this is where the Go product has a standing HIGH defect
 * from assuming the leftover is simply {@code base + "_ccnew"}.
 */
class LeftoversTest {

    @Test
    @DisplayName("the plain forms, as produced live")
    void plainForms() {
        // measured: cancelling REINDEX INDEX CONCURRENTLY public.e5_p01_address_idx left exactly this
        assertEquals(Leftovers.Kind.CCNEW,
                Leftovers.classify("e5_p01_address_idx_ccnew", "e5_p01_address_idx"));
        // measured: parking the reindex in the post-swap wait left exactly this
        assertEquals(Leftovers.Kind.CCOLD,
                Leftovers.classify("e13_p01_address_idx_ccold", "e13_p01_address_idx"));
    }

    @Test
    @DisplayName("PostgreSQL's disambiguating integer is part of the label, so it eats one more byte")
    void disambiguatedSuffixes() {
        assertEquals(Leftovers.Kind.CCNEW, Leftovers.classify("idx_ccnew1", "idx"));
        assertEquals(Leftovers.Kind.CCOLD, Leftovers.classify("idx_ccold27", "idx"));

        // 63-byte base, label "ccnew1" -> overhead 7 -> 56 bytes of base survive, not 57.
        String base = repeat('a', 63);
        assertEquals(Leftovers.Kind.CCNEW, Leftovers.classify(repeat('a', 56) + "_ccnew1", base));
        assertNull(Leftovers.classify(repeat('a', 57) + "_ccnew1", base));
    }

    @Test
    @DisplayName("a 63-byte base is truncated to 57 bytes, exactly as measured")
    void nameDataLenTruncation() {
        // Measured on 17.10:
        //   CREATE INDEX idx_e6_aaaa...aaaa (63 bytes) ON public.e6_p01 (address);
        //   cancelled REINDEX INDEX CONCURRENTLY -> idx_e6_aaaa...aa_ccnew, also 63 bytes,
        //   whose base part is 57 bytes.
        String base = "idx_e6_" + repeat('a', 56);
        String leftover = "idx_e6_" + repeat('a', 50) + "_ccnew";
        assertEquals(63, base.length());
        assertEquals(63, leftover.length());
        assertEquals(Leftovers.Kind.CCNEW, Leftovers.classify(leftover, base));

        // The naive rule -- base + "_ccnew" -- names an index that does not and cannot exist.
        assertNull(Leftovers.classify(base + "_ccnew", base));
    }

    @Test
    @DisplayName("a leftover of a DIFFERENT index on the same partition is not ours")
    void otherFamiliesAreNotClaimed() {
        assertNull(Leftovers.classify("orders_customer_idx_ccnew", "orders_created_idx"));
        // shares a prefix but is not the truncation of it
        assertNull(Leftovers.classify("idx_a_ccnew", "idx_ab"));
    }

    @Test
    @DisplayName("an ordinary index that merely ends in _ccnew is not claimed for the wrong base")
    void notEveryCcnewIsALeftover() {
        assertNull(Leftovers.classify("my_ccnew", "something_else"));
        assertNull(Leftovers.classify("idx_ccnewish", "idx"));
        assertNull(Leftovers.classify("idx", "idx"));
    }

    @Test
    @DisplayName("truncation is byte-aware and never splits a multi-byte character")
    void multiByteTruncation() {
        // 20 three-byte characters = 60 bytes, then two more = 66. Clipping to 57 bytes must
        // drop whole characters: 19 characters, 57 bytes exactly.
        String base = repeat('日', 22);
        assertEquals(66, utf8Length(base));
        String clipped = Leftovers.clipToBytes(base, 57);
        assertEquals(57, utf8Length(clipped));
        assertEquals(19, clipped.length());
        assertEquals(Leftovers.Kind.CCOLD, Leftovers.classify(clipped + "_ccold", base));
    }

    @Test
    @DisplayName("clipping stops on a character boundary rather than mid-sequence")
    void clipNeverSplitsACharacter() {
        String base = repeat('日', 5); // 15 bytes
        // 7 bytes would land inside the third character, so only two survive: 6 bytes.
        String clipped = Leftovers.clipToBytes(base, 7);
        assertEquals(2, clipped.length());
        assertEquals(6, utf8Length(clipped));
    }

    @Test
    @DisplayName("nulls are not leftovers")
    void nullsAreSafe() {
        assertNull(Leftovers.classify(null, "idx"));
        assertNull(Leftovers.classify("idx_ccnew", null));
    }

    private static String repeat(char c, int times) {
        StringBuilder sb = new StringBuilder();
        for (int i = 0; i < times; i++) {
            sb.append(c);
        }
        return sb.toString();
    }

    private static int utf8Length(String s) {
        try {
            return s.getBytes("UTF-8").length;
        } catch (java.io.UnsupportedEncodingException e) {
            throw new IllegalStateException(e);
        }
    }
}
