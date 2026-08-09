package planner

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// TestSentinelExitCodes is AC-26 for the planner's share of the failure
// classes: each named class produces its distinct exit code.
func TestSentinelExitCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want protocol.ExitCode
		kind protocol.ErrorKind
	}{
		{"relation not found", ErrRelationNotFound, protocol.ExitFailure, KindRelationNotFound},
		{"index not found", ErrIndexNotFound, protocol.ExitFailure, KindIndexNotFound},
		{"ambiguous relation", ErrAmbiguousRelation, protocol.ExitFailure, KindAmbiguousRelation},
		{"catalog unavailable", ErrCatalogUnavailable, protocol.ExitFailure, KindCatalogUnavailable},
		{"unsupported server version", ErrUnsupportedServerVersion, protocol.ExitFailure, KindUnsupportedServerVersion},
		{"invalid specification", ErrInvalidSpecification, protocol.ExitFailure, KindInvalidSpecification},
		{"not read only", ErrNotReadOnly, protocol.ExitFailure, KindNotReadOnly},
		{"foreign invalid index", ErrForeignInvalidIndex, protocol.ExitAuthorizationUnsatisfied, KindForeignInvalidIndex},
		{"topology rejection", &TopologyError{Code: CodeHashStrategy}, protocol.ExitUnsupportedTopology, protocol.KindUnsupportedTopology},
		{"privilege refusal", &PrivilegeError{Role: "migrator"}, protocol.ExitInsufficientPrivilege, protocol.KindInsufficientPrivilege},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := protocol.ExitCodeFor(tc.err); got != tc.want {
				t.Errorf("exit code = %d, want %d", got, tc.want)
			}
			if got := protocol.KindOf(tc.err); got != tc.kind {
				t.Errorf("kind = %q, want %q", got, tc.kind)
			}
		})
	}
}

// TestSentinelKindsAreDistinct: two classes sharing a kind would make them
// indistinguishable in logs and to errors.Is.
func TestSentinelKindsAreDistinct(t *testing.T) {
	kinds := []protocol.ErrorKind{
		KindRelationNotFound, KindIndexNotFound, KindAmbiguousRelation,
		KindCatalogUnavailable, KindUnsupportedServerVersion,
		KindInvalidSpecification, KindNotReadOnly, KindForeignInvalidIndex,
	}
	seen := map[protocol.ErrorKind]bool{}
	for _, k := range kinds {
		if seen[k] {
			t.Errorf("duplicate error kind %q", k)
		}
		seen[k] = true
	}
	// And none of them collides with a protocol kind.
	for _, k := range kinds {
		for _, p := range []protocol.ErrorKind{
			protocol.KindGeneric, protocol.KindDigestMismatch, protocol.KindTopologyDrift,
			protocol.KindLockHeld, protocol.KindAuthorizationUnsatisfied,
			protocol.KindVerificationFailed, protocol.KindUnsupportedTopology,
			protocol.KindInsufficientPrivilege, protocol.KindInvalidPlan,
		} {
			if k == p {
				t.Errorf("planner kind %q collides with a protocol kind", k)
			}
		}
	}
}

// TestSentinelsSurviveDetailf: a derived error must still match its sentinel,
// or every call site has to keep the bare value around.
func TestSentinelsSurviveDetailf(t *testing.T) {
	for _, sentinel := range []*protocol.Error{
		ErrRelationNotFound, ErrIndexNotFound, ErrAmbiguousRelation,
		ErrCatalogUnavailable, ErrUnsupportedServerVersion,
		ErrInvalidSpecification, ErrNotReadOnly, ErrForeignInvalidIndex,
	} {
		derived := sentinel.Detailf("detail %d", 1)
		if !errors.Is(derived, sentinel) {
			t.Errorf("%v lost its identity through Detailf", sentinel)
		}
		if protocol.ExitCodeFor(derived) != sentinel.Code {
			t.Errorf("%v lost its exit code through Detailf", sentinel)
		}
		wrapped := fmt.Errorf("context: %w", derived)
		if !errors.Is(wrapped, sentinel) {
			t.Errorf("%v lost its identity through fmt.Errorf", sentinel)
		}
		cause := errors.New("underlying")
		if !errors.Is(sentinel.Wrap(cause), cause) {
			t.Errorf("%v dropped its cause", sentinel)
		}
	}
}

func TestTopologyErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		err  *TopologyError
		want []string
	}{
		{
			name: "code only",
			err:  &TopologyError{Code: CodeHashStrategy},
			want: []string{"TOPO_HASH_STRATEGY"},
		},
		{
			name: "relation and detail",
			err: &TopologyError{
				Code: CodeDefaultPartition, Relation: "public.orders_default",
				Detail: "a DEFAULT partition is present",
			},
			want: []string{"TOPO_DEFAULT_PARTITION", "public.orders_default", "DEFAULT partition"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.err.Error()
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not contain %q", msg, want)
				}
			}
		})
	}
}

// TestTopologyErrorMatchingIsNotTransitive guards the exact bug the two-way
// matcher exists to avoid: HASH and DEFAULT must not satisfy errors.Is against
// each other just because they share a class (FR-PLAN-3).
func TestTopologyErrorMatchingIsNotTransitive(t *testing.T) {
	all := AllTopologyCodes()
	for _, a := range all {
		errA := &TopologyError{Code: a}
		if !errors.Is(errA, protocol.ErrUnsupportedTopology) {
			t.Errorf("%q does not match the class sentinel", a)
		}
		if !errors.Is(errA, &TopologyError{Code: a}) {
			t.Errorf("%q does not match itself", a)
		}
		for _, b := range all {
			if a == b {
				continue
			}
			if errors.Is(errA, &TopologyError{Code: b}) {
				t.Errorf("%q matched %q", a, b)
			}
		}
		// A topology rejection is not a privilege refusal, and vice versa.
		if errors.Is(errA, protocol.ErrInsufficientPrivilege) {
			t.Errorf("%q matched the privilege sentinel", a)
		}
		if errors.Is(errA, ErrCatalogUnavailable) {
			t.Errorf("%q matched an unrelated sentinel", a)
		}
	}

	priv := &PrivilegeError{Role: "migrator"}
	if !errors.Is(priv, protocol.ErrInsufficientPrivilege) {
		t.Error("a privilege refusal does not match its class sentinel")
	}
	if errors.Is(priv, protocol.ErrUnsupportedTopology) {
		t.Error("a privilege refusal matched the topology sentinel")
	}
	if errors.Is(priv, &TopologyError{}) {
		t.Error("a privilege refusal matched a topology target")
	}
}

func TestTopologyCodeOfIgnoresOtherErrors(t *testing.T) {
	for _, err := range []error{
		nil,
		errors.New("plain"),
		ErrCatalogUnavailable,
		&PrivilegeError{Role: "migrator"},
		protocol.ErrUnsupportedTopology,
	} {
		if _, ok := TopologyCodeOf(err); ok {
			t.Errorf("TopologyCodeOf(%v) claimed a code", err)
		}
	}
	wrapped := fmt.Errorf("planning: %w", &TopologyError{Code: CodeMultiLevel})
	code, ok := TopologyCodeOf(wrapped)
	if !ok || code != CodeMultiLevel {
		t.Errorf("TopologyCodeOf through a wrapper = %q, %v", code, ok)
	}
}

func TestTopologyCodeValid(t *testing.T) {
	for _, c := range AllTopologyCodes() {
		if !c.Valid() {
			t.Errorf("%q is not Valid", c)
		}
		if c.String() != string(c) {
			t.Errorf("String() = %q", c.String())
		}
	}
	if TopologyCode("TOPO_MADE_UP").Valid() {
		t.Error("an unknown code reported Valid")
	}
	if TopologyCode("").Valid() {
		t.Error("the empty code reported Valid")
	}
}

func TestErrorMatchersRejectUnrelatedTargets(t *testing.T) {
	topo := &TopologyError{Code: CodeHashStrategy}
	priv := &PrivilegeError{Role: "migrator"}
	plain := errors.New("plain")

	if errors.Is(topo, plain) {
		t.Error("a topology rejection matched a plain error")
	}
	if errors.Is(priv, plain) {
		t.Error("a privilege refusal matched a plain error")
	}

	// errors.As against a type neither of them can produce must not be
	// hijacked by the As methods.
	var other *PrivilegeError
	if errors.As(topo, &other) {
		t.Error("a topology rejection was extracted as a *PrivilegeError")
	}
	var te *TopologyError
	if errors.As(priv, &te) {
		t.Error("a privilege refusal was extracted as a *TopologyError")
	}
	if !errors.As(topo, &te) || te.Code != CodeHashStrategy {
		t.Error("a topology rejection could not be extracted as itself")
	}
	if !errors.As(priv, &other) || other.Role != "migrator" {
		t.Error("a privilege refusal could not be extracted as itself")
	}
}
