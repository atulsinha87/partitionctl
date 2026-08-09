package protocol

import (
	"strings"
	"testing"
)

// Issue #2. Recovering a leftover's base index by trimming the _ccnew/_ccold
// suffix is wrong whenever PostgreSQL had to truncate, because makeObjectName
// truncates the BASE so that base + suffix fits in NAMEDATALEN-1 = 63 bytes.
//
// The three rows below are the ones measured on 17.10 by cancelling real
// REINDEX INDEX CONCURRENTLY runs. Row 1 is the case trimming got right; rows 2
// and 3 are the cases it got wrong, and are why the resolution has to run
// forwards from a candidate set instead.

func name(n int, lead string) string {
	if len(lead) > n {
		return lead[:n]
	}
	return lead + strings.Repeat("a", n-len(lead))
}

func TestDerivesLeftoverReproducesTheMeasuredTruncation(t *testing.T) {
	cases := []struct {
		what     string
		base     string
		leftover string
	}{
		{"57-byte base fits, no truncation", name(57, "idx_"), name(57, "idx_") + "_ccnew"},
		{"58-byte base truncated to 56, digit appended", name(58, "idx_"), name(56, "idx_") + "_ccnew1"},
		{"63-byte base truncated to 57", name(63, "idx_"), name(57, "idx_") + "_ccnew"},
	}
	for _, c := range cases {
		if len(c.leftover) != 63 {
			t.Fatalf("%s: test fixture is wrong, leftover is %d bytes not 63", c.what, len(c.leftover))
		}
		if !DerivesLeftover(c.base, c.leftover) {
			t.Errorf("%s: DerivesLeftover(%d-byte base, %q) = false, want true", c.what, len(c.base), c.leftover)
		}
	}
}

// The defect, stated as a test: for the two truncating rows, the trimmed stem
// is NOT the base. Anything treating it as one is reading a name that may
// belong to a different index.
func TestTrimmedStemIsNotTheBaseWhenTruncated(t *testing.T) {
	base := name(58, "idx_")
	leftover := name(56, "idx_") + "_ccnew1"

	_, stem := ClassifyLeftover(leftover)

	if stem == base {
		t.Fatal("fixture no longer exercises truncation")
	}
	if !strings.HasPrefix(base, stem) {
		t.Fatalf("stem %q should still be a prefix of the base %q", stem, base)
	}
}

func TestResolveLeftoverBaseFindsTheRealBaseAcrossTruncation(t *testing.T) {
	base := NewObjectName("app", name(58, "idx_"))
	leftover := NewObjectName("app", name(56, "idx_")+"_ccnew1")
	// A decoy that shares the truncated prefix but is a different index.
	unrelated := NewObjectName("app", "idx_unrelated")

	got, res := ResolveLeftoverBase(leftover, []ObjectName{unrelated, base})

	if res != LeftoverResolved {
		t.Fatalf("resolution = %v, want LeftoverResolved", res)
	}
	if got != base {
		t.Fatalf("resolved base = %v, want %v", got, base)
	}
}

// The forgery case from issue #2, consequence 3. Two indexes on the same
// relation both derive the leftover because truncation destroyed the bytes that
// told them apart. Picking either one would read an ownership marker off an
// object that may not be ours, so this must refuse to decide.
func TestResolveLeftoverBaseRefusesWhenTruncationMadeItAmbiguous(t *testing.T) {
	// Both bases must be longer than the 57-byte truncation budget and identical
	// up to it, so PostgreSQL would produce the same 63-byte leftover from either.
	// A base is itself capped at 63 bytes, so they differ only after byte 57.
	shared := name(57, "idx_shared_")
	a := NewObjectName("app", shared+"xxx")
	b := NewObjectName("app", shared+"yyy")
	leftover := NewObjectName("app", shared+"_ccnew")

	if !DerivesLeftover(a.Name, leftover.Name) || !DerivesLeftover(b.Name, leftover.Name) {
		t.Fatalf("fixture does not produce two derivations: %d/%d byte bases, leftover %d bytes",
			len(a.Name), len(b.Name), len(leftover.Name))
	}

	_, res := ResolveLeftoverBase(leftover, []ObjectName{a, b})

	if res != LeftoverAmbiguous {
		t.Fatalf("resolution = %v, want LeftoverAmbiguous: guessing here is the marker forgery", res)
	}
}

func TestResolveLeftoverBaseReportsUnresolvedWhenTheBaseIsGone(t *testing.T) {
	leftover := NewObjectName("app", "idx_orders_ccnew")
	_, res := ResolveLeftoverBase(leftover, []ObjectName{NewObjectName("app", "idx_something_else")})
	if res != LeftoverUnresolved {
		t.Fatalf("resolution = %v, want LeftoverUnresolved", res)
	}
}

func TestResolveLeftoverBaseIgnoresOtherSchemasAndOtherLeftovers(t *testing.T) {
	base := NewObjectName("app", "idx_orders")
	leftover := NewObjectName("app", "idx_orders_ccnew")

	got, res := ResolveLeftoverBase(leftover, []ObjectName{
		NewObjectName("other", "idx_orders"),     // right name, wrong schema
		NewObjectName("app", "idx_orders_ccold"), // a leftover is never a base
		base,
	})

	if res != LeftoverResolved || got != base {
		t.Fatalf("resolved %v/%v, want %v/LeftoverResolved", got, res, base)
	}
}

func TestResolveLeftoverBaseRejectsANameThatIsNotALeftover(t *testing.T) {
	_, res := ResolveLeftoverBase(NewObjectName("app", "idx_orders"), nil)
	if res != LeftoverNotAName {
		t.Fatalf("resolution = %v, want LeftoverNotAName", res)
	}
}

// The tool's own generator emits 63-byte names, so it produces the exposed case
// itself -- that is what makes this reachable rather than theoretical.
func TestChildIndexNameProducesTheExposedLength(t *testing.T) {
	got := ChildIndexName(name(50, "idx_orders_"), name(40, "orders_2024_01_"))
	if len(got) != MaxIdentifierBytes {
		t.Fatalf("ChildIndexName length = %d, want %d", len(got), MaxIdentifierBytes)
	}
}
