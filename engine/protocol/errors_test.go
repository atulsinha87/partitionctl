package protocol

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// AC-26 requires one test per exit code. Each row is that test: the sentinel a
// failure class raises must map to the code TRD §7.2.12 assigns it.
func TestExitCodeForEveryFailureClass(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ExitCode
	}{
		{"success", nil, ExitSuccess},
		{"generic failure", ErrFailure, ExitFailure},
		{"untyped failure", errors.New("something broke"), ExitFailure},
		{"plan digest mismatch", ErrDigestMismatch, ExitDigestMismatch},
		{"topology drift", ErrTopologyDrift, ExitTopologyDrift},
		{"advisory lock held", ErrLockHeld, ExitLockHeld},
		{"authorization unsatisfied", ErrAuthorizationUnsatisfied, ExitAuthorizationUnsatisfied},
		{"verification failed", ErrVerificationFailed, ExitVerificationFailed},
		{"unsupported topology", ErrUnsupportedTopology, ExitUnsupportedTopology},
		{"insufficient privilege", ErrInsufficientPrivilege, ExitInsufficientPrivilege},
		{"unsupported format version", ErrUnsupportedFormatVersion, ExitFailure},
		{"invalid plan", ErrInvalidPlan, ExitFailure},
		{"unknown node kind", ErrUnknownNodeKind, ExitFailure},
		{"invalid transition", ErrInvalidTransition, ExitFailure},
		{"invalid identifier", ErrInvalidIdentifier, ExitFailure},
		{"name collision", ErrNameCollision, ExitFailure},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCodeFor(tc.err); got != tc.want {
				t.Fatalf("ExitCodeFor(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// Every code in the contract must be produced by at least one sentinel, so the
// CLI cannot advertise a code nothing raises.
func TestEveryExitCodeIsReachable(t *testing.T) {
	sentinels := []*Error{
		ErrFailure, ErrDigestMismatch, ErrTopologyDrift, ErrLockHeld,
		ErrAuthorizationUnsatisfied, ErrVerificationFailed, ErrUnsupportedTopology,
		ErrInsufficientPrivilege,
	}
	produced := map[ExitCode]bool{ExitSuccess: true}
	for _, s := range sentinels {
		produced[s.Code] = true
	}
	for _, code := range AllExitCodes() {
		if !produced[code] {
			t.Errorf("exit code %d is in the contract but no sentinel produces it", code)
		}
	}
	if len(AllExitCodes()) != 9 {
		t.Errorf("TRD §7.2.12 defines 9 exit codes, got %d", len(AllExitCodes()))
	}
}

// Each sentinel must be distinguishable from every other, or errors.Is is
// useless for branching.
func TestSentinelsAreDistinct(t *testing.T) {
	sentinels := map[string]*Error{
		"failure":               ErrFailure,
		"digest":                ErrDigestMismatch,
		"drift":                 ErrTopologyDrift,
		"lock":                  ErrLockHeld,
		"authorization":         ErrAuthorizationUnsatisfied,
		"verification":          ErrVerificationFailed,
		"topology":              ErrUnsupportedTopology,
		"privilege":             ErrInsufficientPrivilege,
		"format":                ErrUnsupportedFormatVersion,
		"plan":                  ErrInvalidPlan,
		"kind":                  ErrUnknownNodeKind,
		"transition":            ErrInvalidTransition,
		"identifier":            ErrInvalidIdentifier,
		"collision":             ErrNameCollision,
		"duplicate placeholder": ErrFailure,
	}
	delete(sentinels, "duplicate placeholder")

	for aName, a := range sentinels {
		for bName, b := range sentinels {
			if aName == bName {
				if !errors.Is(a, b) {
					t.Errorf("%s does not match itself", aName)
				}
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("%s matches %s", aName, bName)
			}
		}
	}
}

func TestErrorDetailfPreservesIdentity(t *testing.T) {
	err := ErrDigestMismatch.Detailf("plan %q: recorded %s", "p1", "sha256:abc")

	if !errors.Is(err, ErrDigestMismatch) {
		t.Error("Detailf lost the sentinel identity")
	}
	if errors.Is(err, ErrTopologyDrift) {
		t.Error("Detailf matched an unrelated sentinel")
	}
	if err.ExitCode() != ExitDigestMismatch {
		t.Errorf("ExitCode() = %d", err.ExitCode())
	}
	if !strings.Contains(err.Error(), "plan digest mismatch") {
		t.Errorf("message %q lost the sentinel text", err)
	}
	if !strings.Contains(err.Error(), `"p1"`) {
		t.Errorf("message %q lost the detail", err)
	}

	// Detailf must not mutate the sentinel.
	if ErrDigestMismatch.Msg != "plan digest mismatch" {
		t.Errorf("Detailf mutated the sentinel: %q", ErrDigestMismatch.Msg)
	}
	if strings.Contains(ErrDigestMismatch.Error(), "p1") {
		t.Error("Detailf mutated the sentinel's message")
	}
}

func TestErrorWrapPreservesBothEnds(t *testing.T) {
	cause := errors.New("connection reset by peer")
	err := ErrLockHeld.Wrap(cause)

	if !errors.Is(err, ErrLockHeld) {
		t.Error("Wrap lost the sentinel identity")
	}
	if !errors.Is(err, cause) {
		t.Error("Wrap lost the cause")
	}
	if !strings.Contains(err.Error(), cause.Error()) {
		t.Errorf("message %q does not include the cause", err)
	}
	if ExitCodeFor(err) != ExitLockHeld {
		t.Errorf("ExitCodeFor = %d", ExitCodeFor(err))
	}
	if ErrLockHeld.Err != nil {
		t.Error("Wrap mutated the sentinel")
	}
}

// Errors are routinely wrapped by callers with fmt.Errorf; the exit code must
// still be recoverable from deep in the chain.
func TestExitCodeSurvivesWrapping(t *testing.T) {
	err := error(ErrUnsupportedTopology.Detailf("HASH partitioning"))
	for i := 0; i < 5; i++ {
		err = fmt.Errorf("layer %d: %w", i, err)
	}
	if got := ExitCodeFor(err); got != ExitUnsupportedTopology {
		t.Fatalf("ExitCodeFor = %d, want %d", got, ExitUnsupportedTopology)
	}
	if !errors.Is(err, ErrUnsupportedTopology) {
		t.Fatal("errors.Is failed through five layers")
	}
	if got := KindOf(err); got != KindUnsupportedTopology {
		t.Fatalf("KindOf = %q", got)
	}
}

func TestKindOf(t *testing.T) {
	if got := KindOf(nil); got != "" {
		t.Errorf("KindOf(nil) = %q", got)
	}
	if got := KindOf(errors.New("plain")); got != KindGeneric {
		t.Errorf("KindOf(plain) = %q", got)
	}
	if got := KindOf(ErrVerificationFailed); got != KindVerificationFailed {
		t.Errorf("KindOf = %q", got)
	}
}

func TestErrorImplementsExitCoder(t *testing.T) {
	var _ ExitCoder = (*Error)(nil)
	var _ error = (*Error)(nil)
}

// A custom ExitCoder from another package must also be honoured, so the engine
// packages are not forced to import protocol's Error type to set a code.
func TestExitCodeForHonoursForeignExitCoders(t *testing.T) {
	if got := ExitCodeFor(foreignError{}); got != ExitVerificationFailed {
		t.Fatalf("ExitCodeFor = %d, want %d", got, ExitVerificationFailed)
	}
}

type foreignError struct{}

func (foreignError) Error() string      { return "foreign" }
func (foreignError) ExitCode() ExitCode { return ExitVerificationFailed }
