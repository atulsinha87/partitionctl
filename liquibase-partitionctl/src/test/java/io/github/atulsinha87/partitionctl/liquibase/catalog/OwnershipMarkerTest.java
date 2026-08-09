package io.github.atulsinha87.partitionctl.liquibase.catalog;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

class OwnershipMarkerTest {

    @Test
    @DisplayName("the marker it writes is the marker it recognises")
    void roundTrips() {
        String marker = OwnershipMarker.forChildIndex(
                "idx_personaddress", "public", "person", "db/changelog.xml::7::alice");

        assertEquals("partitionctl owner=liquibase parent=idx_personaddress table=public.person "
                + "changeset=db/changelog.xml::7::alice", marker);
        assertTrue(OwnershipMarker.isOurs(marker));
        assertFalse(OwnershipMarker.isForeign(marker));
    }

    @Test
    @DisplayName("the changeset field is optional, and the marker still round-trips without it")
    void changesetIsOptional() {
        assertTrue(OwnershipMarker.isOurs(
                OwnershipMarker.forChildIndex("idx_a", "public", "person", null)));
        assertTrue(OwnershipMarker.isOurs(
                OwnershipMarker.forChildIndex("idx_a", "public", "person", "  ")));
    }

    @Test
    @DisplayName("recognition is prefix-only, so extending the tail later cannot orphan old indexes")
    void recognitionIsPrefixOnly() {
        assertTrue(OwnershipMarker.isOurs(OwnershipMarker.PREFIX));
        assertTrue(OwnershipMarker.isOurs(OwnershipMarker.PREFIX + " anything at all here"));
        assertTrue(OwnershipMarker.isOurs("  " + OwnershipMarker.PREFIX + " parent=x  "));
    }

    @Test
    @DisplayName("a longer word starting with the prefix is NOT ours")
    void prefixMustEndAtAWordBoundary() {
        assertFalse(OwnershipMarker.isOurs("partitionctl owner=liquibaseXYZ parent=idx_a"));
        assertTrue(OwnershipMarker.isForeign("partitionctl owner=liquibaseXYZ parent=idx_a"));
    }

    @Test
    @DisplayName("the Go CLI's marker is foreign: the two products do not interoperate")
    void theGoCliMarkerIsForeign() {
        String go = "partitionctl:v1:{\"run\":\"run-7\",\"op\":\"create-index\",\"role\":\"leaf\"}";
        assertFalse(OwnershipMarker.isOurs(go));
        assertTrue(OwnershipMarker.isForeign(go));
    }

    @Test
    @DisplayName("absent and foreign are different answers")
    void absentIsNotForeign() {
        assertFalse(OwnershipMarker.isOurs(null));
        assertFalse(OwnershipMarker.isForeign(null), "no comment is missing evidence, not a signal");
        assertFalse(OwnershipMarker.isForeign("   "), "an empty comment is not somebody's note");
        assertTrue(OwnershipMarker.isForeign("CHG-4471, do not drop"));
    }

    @Test
    @DisplayName("the COMMENT statement escapes both the identifier and the marker text")
    void commentStatementEscapes() {
        assertEquals("COMMENT ON INDEX \"pu'blic\".\"id\"\"x\" IS 'it''s ours'",
                OwnershipMarker.commentStatement("pu'blic", "id\"x", "it's ours"));
    }
}
