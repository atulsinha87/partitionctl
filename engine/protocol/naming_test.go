package protocol

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChildIndexNameShortPath(t *testing.T) {
	tests := []struct {
		parent    string
		partition string
		want      string
	}{
		{"orders_created_at_idx", "orders_2026_03", "orders_created_at_idx_orders_2026_03"},
		{"i", "p", "i_p"},
		{strings.Repeat("a", 31), strings.Repeat("b", 31), strings.Repeat("a", 31) + "_" + strings.Repeat("b", 31)},
	}
	for _, tc := range tests {
		got := ChildIndexName(tc.parent, tc.partition)
		if got != tc.want {
			t.Errorf("ChildIndexName(%q, %q) = %q, want %q", tc.parent, tc.partition, got, tc.want)
		}
		if len(got) > MaxIdentifierBytes {
			t.Errorf("ChildIndexName(%q, %q) = %q is %d bytes", tc.parent, tc.partition, got, len(got))
		}
	}
}

// The boundary between the readable path and the hashed path must be exactly
// MaxIdentifierBytes.
func TestChildIndexNameLengthBoundary(t *testing.T) {
	parent := strings.Repeat("a", 31)
	for _, partLen := range []int{30, 31, 32, 33} {
		partition := strings.Repeat("b", partLen)
		natural := parent + "_" + partition
		got := ChildIndexName(parent, partition)
		if len(got) > MaxIdentifierBytes {
			t.Fatalf("partition length %d: name %q is %d bytes", partLen, got, len(got))
		}
		if len(natural) <= MaxIdentifierBytes && got != natural {
			t.Fatalf("partition length %d: natural name %q fits but got %q", partLen, natural, got)
		}
		if len(natural) > MaxIdentifierBytes && got == natural {
			t.Fatalf("partition length %d: name was not truncated", partLen)
		}
	}
}

func TestChildIndexNameIsDeterministic(t *testing.T) {
	parent := strings.Repeat("parent_index_", 6)
	partition := strings.Repeat("partition_name_", 6)
	first := ChildIndexName(parent, partition)
	for i := 0; i < 1000; i++ {
		if got := ChildIndexName(parent, partition); got != first {
			t.Fatalf("iteration %d: %q != %q", i, got, first)
		}
	}
}

// The tag is pinned so that a change to the hashing is caught. A plan written
// by an earlier build records these names; re-deriving a different one at
// execution time would break resume correlation (FR-PLAN-11).
func TestChildIndexNameGolden(t *testing.T) {
	tests := []struct {
		parent    string
		partition string
		want      string
	}{
		{
			parent:    "orders_created_at_idx",
			partition: "orders_2026_03",
			want:      "orders_created_at_idx_orders_2026_03",
		},
		{
			parent:    "very_long_parent_index_name_for_a_partitioned_table_orders",
			partition: "orders_2026_03_01_shard_07",
			want:      "very_long_parent_index_name_for_a_partitioned_tabl_sdhfgdpzhyuv",
		},
	}
	for _, tc := range tests {
		if got := ChildIndexName(tc.parent, tc.partition); got != tc.want {
			t.Errorf("ChildIndexName(%q, %q)\n got %q\nwant %q", tc.parent, tc.partition, got, tc.want)
		}
	}
}

func TestChildIndexNameAlwaysFits(t *testing.T) {
	inputs := []struct{ parent, partition string }{
		{"", ""},
		{"a", ""},
		{"", "a"},
		{strings.Repeat("x", 63), strings.Repeat("y", 63)},
		{strings.Repeat("x", 200), strings.Repeat("y", 200)},
		{strings.Repeat("é", 60), strings.Repeat("ü", 60)},
		{"🐘🐘🐘🐘🐘🐘🐘🐘🐘🐘🐘🐘🐘🐘🐘🐘", "🦣🦣🦣🦣🦣🦣🦣🦣🦣🦣🦣🦣🦣🦣🦣🦣"},
		{"bad\xffbytes" + strings.Repeat("z", 60), "more\xfebytes" + strings.Repeat("w", 60)},
	}
	for _, in := range inputs {
		got := ChildIndexName(in.parent, in.partition)
		if len(got) > MaxIdentifierBytes {
			t.Errorf("ChildIndexName(%q, %q) = %q is %d bytes", in.parent, in.partition, got, len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("ChildIndexName(%q, %q) produced invalid UTF-8: %q", in.parent, in.partition, got)
		}
	}
}

// FR-PLAN-13's real requirement: 1,000 partitions must not collide, even when
// their names are adversarially similar. All three families below share a long
// prefix that survives truncation, so only the hash tag distinguishes them.
func TestChildIndexNameCollisionResistance(t *testing.T) {
	const n = 1000
	parent := "orders_multi_tenant_created_at_covering_index_v3"
	prefix := strings.Repeat("orders_archive_partition_", 3)

	families := map[string]func(i int) string{
		"suffix varies at the end":       func(i int) string { return fmt.Sprintf("%s%04d", prefix, i) },
		"suffix varies past truncation":  func(i int) string { return fmt.Sprintf("%s_%04d_tail", prefix, i) },
		"differs only in the last chars": func(i int) string { return prefix + strings.Repeat("z", 20) + fmt.Sprintf("%04d", i) },
	}

	for name, gen := range families {
		t.Run(name, func(t *testing.T) {
			partitions := make([]string, n)
			for i := range partitions {
				partitions[i] = gen(i)
			}
			names, err := ChildIndexNames(parent, partitions)
			if err != nil {
				t.Fatalf("ChildIndexNames: %v", err)
			}
			seen := make(map[string]string, n)
			truncated := 0
			for i, got := range names {
				if len(got) > MaxIdentifierBytes {
					t.Fatalf("%q is %d bytes", got, len(got))
				}
				if got != parent+"_"+partitions[i] {
					truncated++
				}
				if prev, dup := seen[got]; dup {
					t.Fatalf("collision: %q and %q both yield %q", prev, partitions[i], got)
				}
				seen[got] = partitions[i]
			}
			if truncated != n {
				t.Fatalf("expected all %d names to be truncated, only %d were", n, truncated)
			}
		})
	}
}

// Length framing is what stops ("a_b","c") and ("a","b_c") hashing alike.
func TestChildIndexTagIsInjectiveOverTheInputPair(t *testing.T) {
	pad := strings.Repeat("q", 60)
	a := childIndexTag("a_b"+pad, "c")
	b := childIndexTag("a"+pad, "b_c")
	if a == b {
		t.Fatalf("tags collide across a shifted separator: %q", a)
	}
	if childIndexTag("x", "y") == childIndexTag("y", "x") {
		t.Fatal("tag is symmetric in its arguments")
	}
}

func TestChildIndexNamesDetectsCollisions(t *testing.T) {
	// Duplicate partition names are the one way a collision is reachable in
	// practice, and the check must catch it rather than silently overwrite.
	_, err := ChildIndexNames("idx", []string{"p1", "p2", "p1"})
	if !errors.Is(err, ErrNameCollision) {
		t.Fatalf("error %v is not ErrNameCollision", err)
	}
	if code := ExitCodeFor(err); code != ExitFailure {
		t.Fatalf("exit code %d, want %d", code, ExitFailure)
	}

	names, err := ChildIndexNames("idx", []string{"p1", "p2"})
	if err != nil {
		t.Fatalf("ChildIndexNames: %v", err)
	}
	if len(names) != 2 || names[0] != "idx_p1" || names[1] != "idx_p2" {
		t.Fatalf("got %v", names)
	}
}

func TestChildIndexNamesIsPositional(t *testing.T) {
	partitions := []string{"p1", strings.Repeat("long", 30), "p3"}
	names, err := ChildIndexNames("idx", partitions)
	if err != nil {
		t.Fatalf("ChildIndexNames: %v", err)
	}
	if len(names) != len(partitions) {
		t.Fatalf("got %d names for %d partitions", len(names), len(partitions))
	}
	for i, p := range partitions {
		if names[i] != ChildIndexName("idx", p) {
			t.Errorf("index %d: %q != %q", i, names[i], ChildIndexName("idx", p))
		}
	}
}

func TestTruncateUTF8NeverSplitsARune(t *testing.T) {
	s := "aé🐘b"
	for n := 0; n <= len(s)+2; n++ {
		got := truncateUTF8(s, n)
		if len(got) > n {
			t.Fatalf("truncateUTF8(%q, %d) = %q is %d bytes", s, n, got, len(got))
		}
		if !utf8.ValidString(got) {
			t.Fatalf("truncateUTF8(%q, %d) = %q is not valid UTF-8", s, n, got)
		}
		if !strings.HasPrefix(s, got) {
			t.Fatalf("truncateUTF8(%q, %d) = %q is not a prefix", s, n, got)
		}
	}
}

func TestClassifyLeftover(t *testing.T) {
	tests := []struct {
		in       string
		wantKind LeftoverKind
		wantBase string
	}{
		{"orders_idx", LeftoverNone, "orders_idx"},
		{"orders_idx_ccnew", LeftoverNew, "orders_idx"},
		{"orders_idx_ccold", LeftoverOld, "orders_idx"},
		{"orders_idx_ccnew1", LeftoverNew, "orders_idx"},
		{"orders_idx_ccold2", LeftoverOld, "orders_idx"},
		{"orders_idx_ccnew123", LeftoverNew, "orders_idx"},
		// Not a leftover: the suffix is not anchored at the end.
		{"orders_ccnew_idx", LeftoverNone, "orders_ccnew_idx"},
		// Not a leftover: no underscore before cc.
		{"ordersccnew", LeftoverNone, "ordersccnew"},
		// A base name that legitimately ends in digits.
		{"orders_2026_ccnew", LeftoverNew, "orders_2026"},
	}
	for _, tc := range tests {
		kind, base := ClassifyLeftover(tc.in)
		if kind != tc.wantKind || base != tc.wantBase {
			t.Errorf("ClassifyLeftover(%q) = (%q, %q), want (%q, %q)", tc.in, kind, base, tc.wantKind, tc.wantBase)
		}
	}
}

// TestTruncatedNamesCannotCollideWithNaturalOnes is FR-PLAN-13's second half:
// the planner must "truncate deterministically and disambiguate".
//
// The disambiguation was incomplete. A name that fits was returned unchanged,
// and a name that overflowed became prefix + "_" + tag, so the tagged form was
// reachable by the untagged one: take the truncated name a long partition
// produces, name a partition after its suffix, and the two generate identical
// child index names. PostgreSQL accepts both partitions; `plan` refused the
// tree with exit 1 and produced no artifact.
//
// The tag is a pure function of public inputs, so the colliding name can be
// computed offline by anyone who can name a partition.
func TestTruncatedNamesCannotCollideWithNaturalOnes(t *testing.T) {
	const parent = "idx"

	// A overflows and is truncated with a tag.
	long := strings.Repeat("a", 80)
	nameA := ChildIndexName(parent, long)
	if len(nameA) > MaxIdentifierBytes {
		t.Fatalf("%q is %d bytes, over the limit", nameA, len(nameA))
	}

	// B is named after exactly what A truncated to, and fits on its own.
	short := strings.TrimPrefix(nameA, parent+"_")
	nameB := ChildIndexName(parent, short)

	if nameA == nameB {
		t.Fatalf("partitions %q and %q both generate %q; a truncated name is reachable "+
			"by a natural one, so a legal partition set is refused (FR-PLAN-13)",
			long, short, nameA)
	}
	if len(nameB) > MaxIdentifierBytes {
		t.Errorf("%q is %d bytes, over the limit", nameB, len(nameB))
	}

	// And the set-level generator accepts the pair, which is what `plan` calls.
	names, err := ChildIndexNames(parent, []string{long, short})
	if err != nil {
		t.Fatalf("ChildIndexNames refused a legal partition set: %v", err)
	}
	if names[0] == names[1] {
		t.Fatalf("ChildIndexNames returned two identical names: %q", names[0])
	}
}

// TestChildIndexNameStaysStableForOrdinaryNames guards the fix from being a
// rename of everything: a normal partition name must still produce the readable
// natural form.
func TestChildIndexNameStaysStableForOrdinaryNames(t *testing.T) {
	cases := map[string]string{
		"orders_2026_01": "orders_created_at_idx_orders_2026_01",
		"p1":             "orders_created_at_idx_p1",
	}
	for partition, want := range cases {
		if got := ChildIndexName("orders_created_at_idx", partition); got != want {
			t.Errorf("ChildIndexName(_, %q) = %q, want %q", partition, got, want)
		}
	}
}
