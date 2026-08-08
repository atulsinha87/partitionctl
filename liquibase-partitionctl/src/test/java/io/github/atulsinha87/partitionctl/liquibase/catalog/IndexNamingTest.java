package io.github.atulsinha87.partitionctl.liquibase.catalog;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * PostgreSQL truncates identifiers to 63 bytes silently. Resume works by re-discovering the
 * name a previous run generated, so generating 70 bytes and letting the server keep 63 would
 * make every re-run rebuild an index that already exists. These tests sit on the boundary.
 */
class IndexNamingTest {

    private static String repeat(char c, int n) {
        char[] chars = new char[n];
        Arrays.fill(chars, c);
        return new String(chars);
    }

    // ---------------------------------------------------------------- byte boundary

    @Test
    @DisplayName("62 bytes: one under the limit, untouched")
    void sixtyTwoBytes() {
        // "i" + "_" + 60 = 62
        String name = IndexNaming.childIndexName("i", repeat('a', 60));
        assertEquals(62, IndexNaming.byteLength(name));
        assertEquals("i_" + repeat('a', 60), name);
    }

    @Test
    @DisplayName("63 bytes: exactly the limit, untouched")
    void sixtyThreeBytes() {
        String name = IndexNaming.childIndexName("i", repeat('a', 61));
        assertEquals(63, IndexNaming.byteLength(name));
        assertEquals("i_" + repeat('a', 61), name);
    }

    @Test
    @DisplayName("64 bytes: one over the limit, clipped to 63")
    void sixtyFourBytes() {
        String name = IndexNaming.childIndexName("i", repeat('a', 62));
        assertEquals(63, IndexNaming.byteLength(name));
        assertEquals("i_" + repeat('a', 61), name);
    }

    @Test
    @DisplayName("far over the limit is still exactly 63 bytes")
    void wellOverTheLimit() {
        String name = IndexNaming.childIndexName("idx_a_rather_long_index_name_indeed",
                "orders_partition_for_the_year_2024_month_01_region_emea");
        assertEquals(63, IndexNaming.byteLength(name));
    }

    // ---------------------------------------------------------------- multi-byte

    @Test
    @DisplayName("byte-aware, not character-aware: 21 three-byte characters are 63 bytes, not 21")
    void countsBytesNotCharacters() {
        String threeByteChars = repeat('€', 30);   // EURO SIGN, 3 bytes each = 90 bytes
        assertEquals(90, IndexNaming.byteLength(threeByteChars));

        String name = IndexNaming.clipToNameDataLen(threeByteChars);
        assertEquals(63, IndexNaming.byteLength(name));
        assertEquals(21, name.length(), "63 bytes / 3 bytes per character");
    }

    @Test
    @DisplayName("a character straddling byte 63 is dropped whole, never split")
    void neverSplitsAMultiByteSequence() {
        // "i_" (2) + 60 ascii (62 total), then EURO occupies bytes 62,63,64.
        // A naive cut at 63 would keep the first byte of the EURO and produce invalid UTF-8.
        String base = "i_" + repeat('a', 60) + "€";
        assertEquals(65, IndexNaming.byteLength(base));

        String clipped = IndexNaming.clipToNameDataLen(base);
        assertEquals(62, IndexNaming.byteLength(clipped), "backed off to the character boundary");
        assertEquals("i_" + repeat('a', 60), clipped);
        assertFalse(clipped.contains("�"), "no replacement character, so nothing was split");
    }

    @Test
    @DisplayName("a multi-byte character that ends exactly on byte 63 is kept")
    void keepsACharacterEndingOnTheBoundary() {
        // "i_" (2) + 58 ascii = 60 bytes, EURO occupies 60,61,62, then ascii from 63 on.
        String base = "i_" + repeat('a', 58) + "€" + repeat('b', 20);
        String clipped = IndexNaming.clipToNameDataLen(base);
        assertEquals(63, IndexNaming.byteLength(clipped));
        assertTrue(clipped.endsWith("€"), "actual: " + clipped);
    }

    @Test
    @DisplayName("four-byte characters (astral plane) are handled too")
    void fourByteCharacters() {
        String emoji = repeat('a', 0) + new String(Character.toChars(0x1F600)); // 4 bytes
        String base = "i_" + repeat('a', 59) + emoji;     // 2 + 59 = 61, emoji at 61..64
        assertEquals(65, IndexNaming.byteLength(base));
        String clipped = IndexNaming.clipToNameDataLen(base);
        assertEquals(61, IndexNaming.byteLength(clipped));
        assertEquals("i_" + repeat('a', 59), clipped);
    }

    // ---------------------------------------------------------------- determinism

    @Test
    @DisplayName("same inputs always produce the same name -- resume depends on it")
    void deterministic() {
        for (int i = 0; i < 100; i++) {
            assertEquals(IndexNaming.childIndexName("idx_orders", "orders_2024_01"),
                    IndexNaming.childIndexName("idx_orders", "orders_2024_01"));
        }
    }

    // ---------------------------------------------------------------- collisions

    @Test
    @DisplayName("truncation collision fails loudly at plan time, naming both leaves")
    void collisionFailsLoudly() {
        String indexName = repeat('i', 40);
        List<LeafPartition> leaves = new ArrayList<LeafPartition>();
        leaves.add(new LeafPartition("public", repeat('p', 30) + "_alpha"));
        leaves.add(new LeafPartition("public", repeat('p', 30) + "_beta"));

        IndexNamingException thrown = assertThrows(IndexNamingException.class,
                () -> IndexNaming.assignChildIndexNames(indexName, leaves));

        String message = thrown.getMessage();
        assertTrue(message.contains("collide"), message);
        assertTrue(message.contains("_alpha"), message);
        assertTrue(message.contains("_beta"), message);
        assertTrue(message.contains("Nothing was executed"), message);
        assertTrue(message.contains("63"), message);
    }

    @Test
    @DisplayName("names that differ within 63 bytes do not collide")
    void noCollisionWhenDistinctWithinTheLimit() {
        List<LeafPartition> leaves = new ArrayList<LeafPartition>();
        leaves.add(new LeafPartition("public", "orders_2024_01"));
        leaves.add(new LeafPartition("public", "orders_2024_02"));

        IndexNaming.assignChildIndexNames("idx_orders_created_at", leaves);

        assertEquals("idx_orders_created_at_orders_2024_01", leaves.get(0).getChildIndexName());
        assertEquals("idx_orders_created_at_orders_2024_02", leaves.get(1).getChildIndexName());
    }

    @Test
    @DisplayName("identical truncated names in DIFFERENT schemas are not a collision")
    void differentSchemasDoNotCollide() {
        // Index names are unique per schema, and a partitioned table's leaves may live in
        // different schemas, so the child index goes in the leaf's own schema.
        List<LeafPartition> leaves = new ArrayList<LeafPartition>();
        leaves.add(new LeafPartition("shard_a", "orders_p01"));
        leaves.add(new LeafPartition("shard_b", "orders_p01"));

        IndexNaming.assignChildIndexNames("idx_orders", leaves);

        assertEquals("idx_orders_orders_p01", leaves.get(0).getChildIndexName());
        assertEquals("idx_orders_orders_p01", leaves.get(1).getChildIndexName());
    }
}
