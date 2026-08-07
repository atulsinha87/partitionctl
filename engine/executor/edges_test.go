package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// ---------------------------------------------------------------------------
// State store failures. A store that misbehaves must halt the run, never be
// worked around: the checkpoint is the only proof of where the executor is.
// ---------------------------------------------------------------------------

func TestStoreFailuresHaltTheRun(t *testing.T) {
	boom := errors.New("state store unreachable")

	tests := []struct {
		name    string
		inject  func(*fakeStore)
		wantDDL bool
	}{
		{"reading node states", func(s *fakeStore) { s.failStates = boom }, false},
		{"reading the cancellation flag", func(s *fakeStore) { s.failCancel = boom }, false},
		{"appending an audit event", func(s *fakeStore) { s.failAudit = boom }, false},
		// The claim is only read when a destructive node finds no marker, so
		// this one needs a plan that contains one.
		{"reading a claim", func(s *fakeStore) { s.failClaims = boom }, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			tc.inject(h.store)

			plan := createChainPlan(t)
			if tc.name == "reading a claim" {
				plan = cleanupPlan(t, provenanceAuth(t, "public.orders_created_at_idx_orders_2026_03"))
			}
			_, err := h.run(t, plan)
			if err == nil {
				t.Fatal("expected the run to halt")
			}
			if !errors.Is(err, boom) {
				t.Fatalf("error %v does not wrap the store failure", err)
			}
			if !tc.wantDDL && h.sql.execCount() != 0 {
				t.Fatalf("%d statements ran despite a broken state store", h.sql.execCount())
			}
		})
	}
}

func TestAuthorizationRecordFailureHaltsBeforeTheDrop(t *testing.T) {
	h := newHarness()
	const index = "public.orders_created_at_idx_orders_2026_03"
	h.catalog.mark(t, obj(t, index), "run-0")
	h.store.failAuthz = errors.New("cannot record authorization")

	_, err := h.run(t, cleanupPlan(t, provenanceAuth(t, index)))
	if !errors.Is(err, ErrCheckpointFailed) {
		t.Fatalf("error = %v, want ErrCheckpointFailed", err)
	}
	if h.sql.execCount() != 0 {
		t.Fatal("a destructive statement ran without its justification recorded (FR-AUTH-6)")
	}
}

func TestUnknownStoredStateIsRefused(t *testing.T) {
	h := newHarness()
	h.store.seed("n1", protocol.NodeState("BANANA"), 0)

	_, err := h.run(t, createChainPlan(t))
	if !errors.Is(err, ErrInvalidRun) {
		t.Fatalf("error = %v, want ErrInvalidRun", err)
	}
}

func TestRunRefusesAPlanMutatedAfterSealing(t *testing.T) {
	h := newHarness()
	plan := createChainPlan(t)
	// The kind of edit a digest exists to catch (T1, AC-2): change what a node
	// will do, leave the digest alone.
	p := plan.Nodes[4].Params.(*protocol.AttachParams)
	p.ChildIndex = obj(t, "public.somebody_elses_index")

	_, err := h.run(t, plan)
	if !errors.Is(err, protocol.ErrDigestMismatch) {
		t.Fatalf("error = %v, want ErrDigestMismatch", err)
	}
	if got := protocol.ExitCodeFor(err); got != protocol.ExitDigestMismatch {
		t.Fatalf("exit code = %d, want 10", got)
	}
	if h.sql.execCount() != 0 {
		t.Fatal("statements ran against a tampered plan")
	}
}

func TestRunRejectsANilPlan(t *testing.T) {
	h := newHarness()
	if _, err := h.executor(t).Run(context.Background(), "run-1", nil); !errors.Is(err, protocol.ErrInvalidPlan) {
		t.Fatalf("error = %v, want ErrInvalidPlan", err)
	}
}

// ---------------------------------------------------------------------------
// Dispatch-level defenses
// ---------------------------------------------------------------------------

// The vocabulary is closed. A kind outside it must fail loudly at dispatch
// rather than fall through to a silent no-op, which is what makes adding one a
// versioned engine change (TRD §7.2.2).
func TestDispatchRefusesAKindOutsideTheVocabulary(t *testing.T) {
	h := newHarness()
	e := h.executor(t)

	unknown := protocol.Node{ID: "n", Kind: protocol.NodeKind("index.teleport")}
	err := e.dispatch(context.Background(), "run-1", nil, &unknown, AuthorizationDecision{})
	if !errors.Is(err, protocol.ErrUnknownNodeKind) {
		t.Fatalf("error = %v, want ErrUnknownNodeKind", err)
	}
}

// Preflight is where an unimplementable plan is refused, before any statement
// runs. This build implements the whole vocabulary, so the only way in is a
// kind the protocol does not know.
func TestPreflightRefusesAnUnsupportedKindBeforeAnyStatement(t *testing.T) {
	h := newHarness()
	e := h.executor(t)
	plan := createChainPlan(t)
	plan.Nodes = append(plan.Nodes, protocol.Node{ID: "nX", Kind: protocol.NodeKind("index.teleport")})

	if err := e.preflight(plan); !errors.Is(err, ErrUnsupportedNodeKind) {
		t.Fatalf("error = %v, want ErrUnsupportedNodeKind", err)
	}
	if h.sql.execCount() != 0 {
		t.Fatal("preflight issued a statement")
	}
}

func TestSupportedKindsIsTheWholeVocabulary(t *testing.T) {
	got := SupportedKinds()
	want := protocol.AllNodeKinds()
	if len(got) != len(want) {
		t.Fatalf("SupportedKinds has %d kinds, the vocabulary has %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position %d: %q, want %q (vocabulary order)", i, got[i], want[i])
		}
	}
}

func TestDestructiveObjectCoversBothDestructiveKinds(t *testing.T) {
	plan := dropPartitionedPlan(t)
	n, _ := plan.NodeByID("n1")
	got, err := destructiveObject(n)
	if err != nil {
		t.Fatalf("destructiveObject: %v", err)
	}
	if got.String() != "public.orders_created_at_idx" {
		t.Fatalf("destructiveObject = %s, want the partitioned index", got)
	}

	attach := node("n", protocol.KindIndexAttach, &protocol.AttachParams{
		ParentIndex: obj(t, "public.p"), ChildIndex: obj(t, "public.c"),
	})
	if _, err := destructiveObject(&attach); !errors.Is(err, protocol.ErrInvalidPlan) {
		t.Fatalf("error = %v, want ErrInvalidPlan for a non-destructive kind", err)
	}
}

func TestVerifierResultCountMismatchIsAFailure(t *testing.T) {
	h := newHarness()
	// A verifier that returns the wrong number of results cannot be paired with
	// its checks, so guessing would be worse than failing.
	h.catalog.verifyFn = func([]protocol.VerifyCheck) ([]CheckResult, error) { return nil, nil }

	_, err := h.run(t, createChainPlan(t))
	if !errors.Is(err, protocol.ErrVerificationFailed) {
		t.Fatalf("error = %v, want ErrVerificationFailed", err)
	}
}

func TestAssertionEvaluatorErrorFailsTheNode(t *testing.T) {
	h := newHarness()
	boom := errors.New("catalog unreachable")
	h.catalog.assertFn = func([]protocol.Assertion) ([]CheckResult, error) { return nil, boom }

	_, err := h.run(t, createChainPlan(t))
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the evaluator failure", err)
	}
	if h.sql.execCount() != 0 {
		t.Fatal("DDL ran after preconditions could not be evaluated")
	}
}

func TestAssertionResultCountMismatchIsAFailure(t *testing.T) {
	h := newHarness()
	h.catalog.assertFn = func(as []protocol.Assertion) ([]CheckResult, error) {
		return []CheckResult{{Passed: true}, {Passed: true}}, nil // one assertion, two results
	}

	_, err := h.run(t, createChainPlan(t))
	if !errors.Is(err, protocol.ErrVerificationFailed) {
		t.Fatalf("error = %v, want ErrVerificationFailed", err)
	}
}

func TestCheckResultFailureCodeOverridesThePlan(t *testing.T) {
	h := newHarness()
	// The planner asked for exit 15; the evaluator knows better at run time.
	h.catalog.assertFn = func(as []protocol.Assertion) ([]CheckResult, error) {
		return []CheckResult{{
			Passed:      false,
			Detail:      "role app_rw is not a member of orders_owner",
			FailureCode: protocol.ExitInsufficientPrivilege,
		}}, nil
	}

	_, err := h.run(t, createChainPlan(t))
	if got := protocol.ExitCodeFor(err); got != protocol.ExitInsufficientPrivilege {
		t.Fatalf("exit code = %d, want 16", got)
	}
}

// ---------------------------------------------------------------------------
// Exit-code mapping (AC-26)
// ---------------------------------------------------------------------------

func TestErrorForExitCodeCoversTheContract(t *testing.T) {
	tests := []struct {
		code protocol.ExitCode
		want *protocol.Error
	}{
		{protocol.ExitDigestMismatch, protocol.ErrDigestMismatch},
		{protocol.ExitTopologyDrift, protocol.ErrTopologyDrift},
		{protocol.ExitLockHeld, protocol.ErrLockHeld},
		{protocol.ExitAuthorizationUnsatisfied, protocol.ErrAuthorizationUnsatisfied},
		{protocol.ExitVerificationFailed, protocol.ErrVerificationFailed},
		{protocol.ExitUnsupportedTopology, protocol.ErrUnsupportedTopology},
		{protocol.ExitInsufficientPrivilege, protocol.ErrInsufficientPrivilege},
		{protocol.ExitFailure, protocol.ErrFailure},
		// Unset means "verification failed", per the documented meaning of
		// Assertion.FailureCode.
		{0, protocol.ErrVerificationFailed},
	}
	for _, tc := range tests {
		got := errorForExitCode(tc.code)
		if !errors.Is(got, tc.want) {
			t.Errorf("errorForExitCode(%d) = %v, want %v", tc.code, got, tc.want)
		}
		if tc.code != 0 && got.ExitCode() != tc.code {
			t.Errorf("errorForExitCode(%d) carries exit code %d", tc.code, got.ExitCode())
		}
	}
}

// ---------------------------------------------------------------------------
// SystemClock
// ---------------------------------------------------------------------------

func TestSystemClock(t *testing.T) {
	c := SystemClock{}
	before := c.Now()
	if err := c.Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if !c.Now().After(before) {
		t.Fatal("Now did not advance across a sleep")
	}
	if err := c.Sleep(context.Background(), 0); err != nil {
		t.Fatalf("a non-positive sleep must return immediately, got %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep on a cancelled context = %v, want context.Canceled", err)
	}
}

func TestRunIDString(t *testing.T) {
	if got := RunID("run-1").String(); got != "run-1" {
		t.Fatalf("RunID.String() = %q", got)
	}
}
