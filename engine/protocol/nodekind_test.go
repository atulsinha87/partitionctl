package protocol

import (
	"errors"
	"testing"
)

// TRD §7.2.2, transcribed. The table is the engine contract, so it is asserted
// row by row rather than derived from the implementation.
func TestNodeKindTable(t *testing.T) {
	tests := []struct {
		kind             NodeKind
		destructive      bool
		issuesDDL        bool
		outsideTx        bool
		allowsStmtTimout bool
		lock             LockLevel
	}{
		{KindCatalogAssert, false, false, false, true, LockNone},
		// ShareLock, measured, not the TRD's ShareUpdateExclusive: this is the
		// one kind whose lock blocks application writes through the parent.
		{KindIndexCreateParentInvalid, false, true, false, true, LockShare},
		{KindIndexCreateConcurrently, false, true, true, false, LockShareUpdateExclusive},
		{KindIndexAttach, false, true, false, true, LockShareUpdateExclusive},
		{KindIndexVerify, false, false, false, true, LockNone},
		{KindWait, false, false, false, true, LockNone},
		{KindIndexDropConcurrently, true, true, true, false, LockShareUpdateExclusive},
		{KindIndexReindexConcurrently, false, true, true, false, LockShareUpdateExclusive},
		{KindIndexDropPartitioned, true, true, false, true, LockAccessExclusive},
	}

	if len(tests) != len(AllNodeKinds()) {
		t.Fatalf("the table covers %d kinds, the vocabulary has %d", len(tests), len(AllNodeKinds()))
	}

	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			if !tc.kind.Valid() {
				t.Fatal("kind is not valid")
			}
			if got := tc.kind.IsDestructive(); got != tc.destructive {
				t.Errorf("IsDestructive() = %v, want %v", got, tc.destructive)
			}
			if got := tc.kind.IssuesDDL(); got != tc.issuesDDL {
				t.Errorf("IssuesDDL() = %v, want %v", got, tc.issuesDDL)
			}
			if got := tc.kind.MustRunOutsideTransaction(); got != tc.outsideTx {
				t.Errorf("MustRunOutsideTransaction() = %v, want %v", got, tc.outsideTx)
			}
			if got := tc.kind.AllowsStatementTimeout(); got != tc.allowsStmtTimout {
				t.Errorf("AllowsStatementTimeout() = %v, want %v", got, tc.allowsStmtTimout)
			}
			if got := tc.kind.LockLevel(); got != tc.lock {
				t.Errorf("LockLevel() = %q, want %q", got, tc.lock)
			}
			if got := tc.kind.Retryable(); got != tc.issuesDDL {
				t.Errorf("Retryable() = %v, want %v", got, tc.issuesDDL)
			}
		})
	}
}

func TestNodeVocabularyIsExactlyNine(t *testing.T) {
	kinds := AllNodeKinds()
	if len(kinds) != 9 {
		t.Fatalf("TRD §7.2.2 defines nine node kinds, got %d: %v", len(kinds), kinds)
	}
	seen := make(map[NodeKind]bool, len(kinds))
	for _, k := range kinds {
		if seen[k] {
			t.Errorf("kind %s appears twice", k)
		}
		seen[k] = true
	}
}

// "There is deliberately no barrier kind" (TRD §7.2.2). A barrier is a node
// with N incoming edges, which the edge set already expresses.
func TestNoBarrierKind(t *testing.T) {
	for _, name := range []NodeKind{"barrier", "graph.barrier", "sync", "join"} {
		if name.Valid() {
			t.Errorf("%s is in the vocabulary; it should not be", name)
		}
	}
}

func TestExactlyTwoKindsAreDestructive(t *testing.T) {
	var destructive []NodeKind
	for _, k := range AllNodeKinds() {
		if k.IsDestructive() {
			destructive = append(destructive, k)
		}
	}
	if len(destructive) != 2 {
		t.Fatalf("expected two destructive kinds, got %v", destructive)
	}
}

// index.drop_partitioned is the only kind that takes AccessExclusiveLock
// (TRD §7.2.2, §7.2.10). AC-8 gates the release on this.
func TestOnlyOneKindTakesAccessExclusive(t *testing.T) {
	var heavy []NodeKind
	for _, k := range AllNodeKinds() {
		if k.LockLevel() == LockAccessExclusive {
			heavy = append(heavy, k)
		}
	}
	if len(heavy) != 1 || heavy[0] != KindIndexDropPartitioned {
		t.Fatalf("AccessExclusive kinds = %v, want [%s]", heavy, KindIndexDropPartitioned)
	}
}

// FR-EXEC-6: every CONCURRENTLY form must be issued outside a transaction
// block, and only those.
func TestConcurrentKindsRunOutsideTransactions(t *testing.T) {
	want := map[NodeKind]bool{
		KindIndexCreateConcurrently:  true,
		KindIndexDropConcurrently:    true,
		KindIndexReindexConcurrently: true,
	}
	for _, k := range AllNodeKinds() {
		if got := k.MustRunOutsideTransaction(); got != want[k] {
			t.Errorf("%s.MustRunOutsideTransaction() = %v, want %v", k, got, want[k])
		}
	}
}

// FR-EXEC-5: no finite statement_timeout on the long index builds.
func TestLongBuildsForbidAFiniteStatementTimeout(t *testing.T) {
	if KindIndexCreateConcurrently.AllowsStatementTimeout() {
		t.Error("index.create_concurrently must not carry a finite statement_timeout (FR-EXEC-5)")
	}
	if KindIndexReindexConcurrently.AllowsStatementTimeout() {
		t.Error("index.reindex_concurrently must not carry a finite statement_timeout")
	}
	// A concurrent drop waits for every transaction that can still see the
	// index. That wait is legitimately unbounded, and killing it at 30s leaves
	// the index indislive = false, which is a state the planner then has to
	// recover from. adapters/cli's resume cleanup already issues its own drop
	// with no finite statement_timeout and says why; the two paths must agree
	// about the same statement.
	if KindIndexDropConcurrently.AllowsStatementTimeout() {
		t.Error("index.drop_concurrently must not carry a finite statement_timeout (FR-EXEC-5)")
	}
	if !KindIndexAttach.AllowsStatementTimeout() {
		t.Error("a catalog-only statement may carry a statement_timeout")
	}
}

func TestCheckNodeKind(t *testing.T) {
	for _, k := range AllNodeKinds() {
		if err := CheckNodeKind(k); err != nil {
			t.Errorf("CheckNodeKind(%s): %v", k, err)
		}
	}
	err := CheckNodeKind("index.rebuild")
	if !errors.Is(err, ErrUnknownNodeKind) {
		t.Fatalf("error %v is not ErrUnknownNodeKind", err)
	}
	if ExitCodeFor(err) != ExitFailure {
		t.Fatalf("exit code %d", ExitCodeFor(err))
	}
}

func TestAllNodeKindsReturnsACopy(t *testing.T) {
	kinds := AllNodeKinds()
	kinds[0] = "mutated"
	if AllNodeKinds()[0] != KindCatalogAssert {
		t.Fatal("AllNodeKinds returned the package-level slice")
	}
}

func TestAuthorizationModes(t *testing.T) {
	modes := AllAuthorizationModes()
	if len(modes) != 3 {
		t.Fatalf("TRD §7.2.9 defines three modes, got %d", len(modes))
	}
	for _, m := range modes {
		if !m.Valid() {
			t.Errorf("%s is not valid", m)
		}
	}
	for _, m := range []AuthorizationMode{"", "naming", "trust_me", "PROVENANCE"} {
		if m.Valid() {
			t.Errorf("%q reported valid", m)
		}
	}

	modes[0] = "mutated"
	if AllAuthorizationModes()[0] != AuthProvenance {
		t.Fatal("AllAuthorizationModes returned the package-level slice")
	}
}

// FR-AUTH-7 / INV-7: leftover needs run history for a relation, so a leftover
// authorization without a relation is meaningless and must be refused.
func TestLeftoverAuthorizationRequiresARelation(t *testing.T) {
	idx := NewObjectName("public", "orders_idx_ccnew")
	rel := NewObjectName("public", "orders_2026_01")

	if err := (&Authorization{Mode: AuthLeftover, Object: idx}).Validate(); err == nil {
		t.Fatal("leftover authorization without a relation was accepted")
	}
	if err := (&Authorization{Mode: AuthLeftover, Object: idx, Relation: &rel}).Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// FR-AUTH-4: explicit is the operator's stated intent, so it must name the
// confirmation flag that carries it.
func TestExplicitAuthorizationRequiresAConfirmationFlag(t *testing.T) {
	idx := NewObjectName("public", "orders_idx")

	if err := (&Authorization{Mode: AuthExplicit, Object: idx}).Validate(); err == nil {
		t.Fatal("explicit authorization without a confirmation flag was accepted")
	}
	err := (&Authorization{Mode: AuthExplicit, Object: idx, RequiredConfirmation: ConfirmExclusiveLock}).Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestAuthorizationValidate(t *testing.T) {
	idx := NewObjectName("public", "orders_idx")
	bad := NewObjectName("public", "a\x00b")

	if err := (*Authorization)(nil).Validate(); err == nil {
		t.Error("a nil authorization was accepted")
	}
	if err := (&Authorization{Mode: "guess", Object: idx}).Validate(); err == nil {
		t.Error("an unknown mode was accepted")
	}
	if err := (&Authorization{Mode: AuthProvenance}).Validate(); err == nil {
		t.Error("an authorization with no object was accepted")
	}
	if err := (&Authorization{Mode: AuthProvenance, Object: bad}).Validate(); err == nil {
		t.Error("an unquotable object was accepted")
	}
	if err := (&Authorization{Mode: AuthProvenance, Object: idx}).Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}
