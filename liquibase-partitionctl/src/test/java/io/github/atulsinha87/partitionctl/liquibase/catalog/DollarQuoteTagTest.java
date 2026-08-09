package io.github.atulsinha87.partitionctl.liquibase.catalog;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * The {@code DO} block tag must not be terminable by anything the block contains.
 *
 * <p>Found by adversarial review. Identifiers reach these blocks as literals, and
 * {@link Identifiers#literal(String)} doubles single quotes but leaves {@code $} alone, so a
 * schema, table or index name containing the literal text {@code $partitionctl$} closed the
 * block early and whatever followed became a statement of its own.
 *
 * <p>The measured impact split by client, which is why this is worth a test rather than a note.
 * Through pgjdbc the payload is a hard changeset failure — one Sync, so the server abandons the
 * batch at the first error and the stacked statement never runs. Through psql it executed:
 * {@code DROP TABLE victim} took the row count from 1 to 0. {@code liquibase updateSQL} writes
 * this same text to a migration script, and "generate SQL, review, hand to the DBA" is a
 * workflow this plugin is built for, so the psql path is a real one.
 */
class DollarQuoteTagTest {

    @Test
    @DisplayName("the ordinary case still uses the plain tag")
    void plainBodyKeepsThePlainTag() {
        String block = Identifiers.doBlock("BEGIN\n  RAISE NOTICE 'hello';\nEND\n");

        assertTrue(block.startsWith("DO $partitionctl$\n"), block);
        assertTrue(block.endsWith("$partitionctl$"), block);
    }

    @Test
    @DisplayName("a body carrying the tag gets a longer one, so it cannot close the block early")
    void aBodyContainingTheTagLengthensIt() {
        String hostile = "BEGIN\n  RAISE NOTICE '" + Identifiers.literal("x$partitionctl$x")
                + "';\nEND\n";
        String block = Identifiers.doBlock(hostile);

        String tag = block.substring("DO ".length(), block.indexOf('\n'));
        assertFalse("$partitionctl$".equals(tag),
                "the tag is still the one the body contains, so the block closes early: " + block);
        assertTrue(block.endsWith(tag), block);
        assertEquals(2, occurrences(block, tag),
                "the chosen tag must appear exactly twice — once to open, once to close: " + block);
    }

    @Test
    @DisplayName("it keeps lengthening until the tag really is absent")
    void itLengthensPastSuccessiveCollisions() {
        String block = Identifiers.doBlock("$partitionctl$ $partitionctl_$ $partitionctl__$\n");

        String tag = block.substring("DO ".length(), block.indexOf('\n'));
        assertEquals("$partitionctl___$", tag, block);
        assertEquals(2, occurrences(block, tag), block);
    }

    private static int occurrences(String haystack, String needle) {
        int n = 0;
        for (int i = haystack.indexOf(needle); i >= 0; i = haystack.indexOf(needle, i + needle.length())) {
            n++;
        }
        return n;
    }
}
