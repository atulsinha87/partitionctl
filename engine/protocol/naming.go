package protocol

import (
	"crypto/sha256"
	"encoding/base32"
	"sort"
	"strings"
	"unicode/utf8"
)

// childNameDomain separates child-index-name hashing from every other SHA-256
// in the system.
const childNameDomain = "partitionctl.childindex.v1"

// childNameTagLen is the length in characters of the disambiguating tag
// appended to a truncated child index name.
//
// Twelve base32 characters carry 60 bits. Across 1,000 partitions the birthday
// probability of any collision is about 4e-13, and [ChildIndexNames] turns even
// that into a plan-time error rather than a silent overwrite.
const childNameTagLen = 12

// childNameBase32 is RFC 4648 base32 without padding. The output is
// uppercased by the encoder and lowercased here, so the tag is stable across
// platforms and locales.
var childNameBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// ChildIndexName generates the name of the leaf index for one partition, as a
// deterministic pure function of the parent index name and the partition name
// (FR-PLAN-11).
//
// PostgreSQL-generated names are never relied upon, because they cannot be
// correlated on resume: the tool would be unable to tell its own half-built
// index from anyone else's. The planner calls this once, records the result in
// the plan, and the executor uses the recorded value verbatim. It is never
// re-derived at execution time (FR-PLAN-13).
//
// The natural name is parent_partition. When that exceeds PostgreSQL's
// [MaxIdentifierBytes], it is truncated on a rune boundary and a 60-bit hash of
// the *untruncated* inputs is appended, so two partitions that share a long
// prefix still get different names (FR-PLAN-13).
//
// A natural name that already ends in something shaped like the tag is tagged
// as well, even though it fits. Otherwise the two forms would share an image:
// a partition could be named after exactly what some longer partition truncates
// to, and the two would generate byte-identical child index names. PostgreSQL
// accepts both partitions, so the planner would refuse a legal tree, and since
// the tag is a pure function of public inputs the colliding name is computable
// offline by anyone who can name a partition. Tagging both makes the truncated
// form structurally unreachable from the natural one.
//
// The function is total. Inputs are assumed to be valid PostgreSQL identifiers;
// invalid UTF-8 is normalized to U+FFFD in the readable part of the name, which
// keeps the result a legal identifier without changing its determinism.
func ChildIndexName(parentIndex, partition string) string {
	natural := parentIndex + "_" + partition
	if len(natural) <= MaxIdentifierBytes && utf8.ValidString(natural) && !looksTagged(natural) {
		return natural
	}
	tag := childIndexTag(parentIndex, partition)
	budget := MaxIdentifierBytes - 1 - len(tag)
	if budget < 0 {
		budget = 0
	}
	return truncateUTF8(natural, budget) + "_" + tag
}

// ChildIndexNames generates a name for every partition and proves the set is
// collision-free, which is the guarantee FR-PLAN-13 actually needs. It returns
// names positionally aligned with partitions, or an error matching
// [ErrNameCollision].
func ChildIndexNames(parentIndex string, partitions []string) ([]string, error) {
	names := make([]string, len(partitions))
	seen := make(map[string]int, len(partitions))
	for i, p := range partitions {
		name := ChildIndexName(parentIndex, p)
		if j, dup := seen[name]; dup {
			return nil, ErrNameCollision.Detailf(
				"partitions %q and %q both map to child index name %q under parent index %q",
				partitions[j], p, name, parentIndex)
		}
		seen[name] = i
		names[i] = name
	}
	return names, nil
}

// ChildIndexNamesQualified generates the child index name for every partition
// and proves the set is collision-free, returning schema-qualified names
// positionally aligned with leaves (FR-PLAN-11, FR-PLAN-13).
//
// The collision proof is applied per schema, which is where a collision can
// actually happen: PostgreSQL resolves CREATE INDEX CONCURRENTLY <name> ON
// <leaf> in the leaf's schema, so two partitions in different schemas cannot
// collide even when their bare names generate the same index name. Proving it
// globally would reject a legal tree, because
// `CREATE TABLE archive.p1 PARTITION OF public.orders` is permitted alongside
// `public.p1`.
//
// This is the single generator both the plan path and the resume path use, so
// the two cannot disagree about what a collision is (AC-4).
func ChildIndexNamesQualified(parentIndex string, leaves []ObjectName) ([]ObjectName, error) {
	bySchema := make(map[string][]string)
	positions := make(map[string][]int)
	schemas := make([]string, 0, 4)
	for i, l := range leaves {
		if _, seen := bySchema[l.Schema]; !seen {
			schemas = append(schemas, l.Schema)
		}
		bySchema[l.Schema] = append(bySchema[l.Schema], l.Name)
		positions[l.Schema] = append(positions[l.Schema], i)
	}
	sort.Strings(schemas)

	out := make([]ObjectName, len(leaves))
	for _, s := range schemas {
		names, err := ChildIndexNames(parentIndex, bySchema[s])
		if err != nil {
			return nil, err
		}
		for k, pos := range positions[s] {
			out[pos] = NewObjectName(s, names[k])
		}
	}
	return out, nil
}

// looksTagged reports whether s ends in the shape [childIndexTag] produces:
// an underscore followed by exactly [childNameTagLen] lowercase base32
// characters, at the very end of the name.
//
// It is the test that keeps the tagged and untagged images disjoint. False
// positives cost only an unnecessary tag on an unusual name; a false negative
// would reopen the collision, so the check is deliberately about shape rather
// than about whether the suffix is a *correct* tag for these inputs.
func looksTagged(s string) bool {
	if len(s) < childNameTagLen+1 {
		return false
	}
	if s[len(s)-childNameTagLen-1] != '_' {
		return false
	}
	for i := len(s) - childNameTagLen; i < len(s); i++ {
		c := s[i]
		// RFC 4648 base32, lowercased: a-z and 2-7.
		if (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

// childIndexTag is the disambiguating suffix: the first [childNameTagLen]
// lowercase base32 characters of a domain-separated, length-framed SHA-256 over
// both inputs.
//
// Length framing is what makes the tag injective over the input pair: without
// it, ("a_b", "c") and ("a", "b_c") would hash identically.
func childIndexTag(parentIndex, partition string) string {
	h := sha256.New()
	writeLengthPrefixed(h, []byte(childNameDomain))
	writeLengthPrefixed(h, []byte(parentIndex))
	writeLengthPrefixed(h, []byte(partition))
	encoded := childNameBase32.EncodeToString(h.Sum(nil))
	return strings.ToLower(encoded[:childNameTagLen])
}

// truncateUTF8 returns the longest prefix of s that is at most maxBytes long
// and does not split a rune. Invalid bytes are re-encoded as U+FFFD, so the
// result is always valid UTF-8.
func truncateUTF8(s string, maxBytes int) string {
	var b strings.Builder
	b.Grow(maxBytes)
	for _, r := range s {
		n := utf8.RuneLen(r)
		if n < 0 {
			n = utf8.RuneLen(utf8.RuneError)
		}
		if b.Len()+n > maxBytes {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// LeftoverSuffixes are the prefixes of the transient index names PostgreSQL
// leaves behind when REINDEX CONCURRENTLY fails. PostgreSQL appends a
// disambiguating integer when needed (_ccnew1, _ccold2), so detection matches a
// pattern, not a literal (TRD §7.2.11).
//
// Matching a name is never sufficient authorization to drop it: [AuthLeftover]
// additionally requires a recorded PartitionCTL reindex run for the relation
// (FR-AUTH-3, FR-AUTH-7, INV-7, AC-19).
const (
	LeftoverNewPrefix = "_ccnew"
	LeftoverOldPrefix = "_ccold"
)

// LeftoverKind classifies a REINDEX CONCURRENTLY leftover by its suffix.
type LeftoverKind string

// The leftover classes and their recovery meanings (TRD §7.2.11).
const (
	// LeftoverNone means the name is not a leftover.
	LeftoverNone LeftoverKind = ""
	// LeftoverNew means the rebuild failed and the original is intact: drop
	// it, then reindex that leaf again (FR-REIDX-3).
	LeftoverNew LeftoverKind = "ccnew"
	// LeftoverOld means the rebuild succeeded and the old copy could not be
	// dropped: drop it and treat the leaf as complete (FR-REIDX-4).
	LeftoverOld LeftoverKind = "ccold"
)

// ClassifyLeftover reports whether an index name is a REINDEX CONCURRENTLY
// leftover and, if so, which class it belongs to and what the base index name
// was.
//
// The suffix is matched as a pattern: "_ccnew" or "_ccold" followed by an
// optional run of decimal digits, anchored at the end of the name.
func ClassifyLeftover(indexName string) (kind LeftoverKind, base string) {
	trimmed := strings.TrimRight(indexName, "0123456789")
	switch {
	case strings.HasSuffix(trimmed, LeftoverNewPrefix):
		return LeftoverNew, strings.TrimSuffix(trimmed, LeftoverNewPrefix)
	case strings.HasSuffix(trimmed, LeftoverOldPrefix):
		return LeftoverOld, strings.TrimSuffix(trimmed, LeftoverOldPrefix)
	}
	return LeftoverNone, indexName
}
