package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// authFixture builds a destructive node plus a plan for [Authorize] to judge.
type authFixture struct {
	store *fakeStore
	plan  *protocol.Plan
	node  *protocol.Node
}

func newAuthFixture(t *testing.T, auth *protocol.Authorization, index string) *authFixture {
	t.Helper()
	plan := newPlan(t, dropNode(t, "nDrop", index, auth))
	n, _ := plan.NodeByID("nDrop")
	return &authFixture{store: newFakeStore(&recorder{}), plan: plan, node: n}
}

func TestAuthorizeProvenanceMode(t *testing.T) {
	const index = "public.orders_created_at_idx_orders_2026_03"

	t.Run("satisfied by a committed record", func(t *testing.T) {
		f := newAuthFixture(t, provenanceAuth(t, index), index)
		f.store.provenance[index] = Provenance{
			RunID: "run-0", NodeID: "n3", Object: obj(t, index),
			ObjectKind: ObjectKindIndex, CreatedAt: time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC),
		}
		d, err := Authorize(context.Background(), f.store, f.plan, f.node)
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if !d.Satisfied {
			t.Fatalf("decision = %+v, want satisfied", d)
		}
		if d.Evidence["provenance_run_id"] != "run-0" {
			t.Fatalf("evidence = %v, want the creating run recorded", d.Evidence)
		}
	})

	t.Run("unsatisfied without a record", func(t *testing.T) {
		f := newAuthFixture(t, provenanceAuth(t, index), index)
		d, err := Authorize(context.Background(), f.store, f.plan, f.node)
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if d.Satisfied {
			t.Fatal("an INVALID index with no provenance was authorized; AC-6 forbids it")
		}
		if d.Reason == "" {
			t.Fatal("a denial must carry a reason the operator can act on")
		}
	})
}

func TestAuthorizeLeftoverModeNeedsBothConditions(t *testing.T) {
	relation := "public.orders_2026_03"
	tests := []struct {
		name       string
		index      string
		reindexRun bool
		want       bool
	}{
		{"ccnew with a recorded run", "public.orders_idx_ccnew", true, true},
		{"ccold with a recorded run", "public.orders_idx_ccold", true, true},
		{"disambiguated suffix", "public.orders_idx_ccnew1", true, true},
		// INV-7 and AC-19: naming alone is forgeable. An operator's own
		// hand-rolled REINDEX leftover is not ours to drop.
		{"ccnew with no recorded run", "public.orders_idx_ccnew", false, false},
		// A recorded run does not license dropping an ordinary index.
		{"ordinary name with a recorded run", "public.orders_idx", true, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			auth := &protocol.Authorization{
				Mode:     protocol.AuthLeftover,
				Object:   obj(t, tc.index),
				Relation: objPtr(t, relation),
			}
			f := newAuthFixture(t, auth, tc.index)
			if tc.reindexRun {
				f.store.reindexRuns[relation] = true
			}
			d, err := Authorize(context.Background(), f.store, f.plan, f.node)
			if err != nil {
				t.Fatalf("Authorize: %v", err)
			}
			if d.Satisfied != tc.want {
				t.Fatalf("satisfied = %v, want %v (reason: %s)", d.Satisfied, tc.want, d.Reason)
			}
			if tc.want && d.Evidence["reindex_run"] != "recorded" {
				t.Fatalf("evidence = %v, want both conditions recorded", d.Evidence)
			}
		})
	}
}

func TestAuthorizeExplicitModeNeedsTheConfirmationAndTheNamedObject(t *testing.T) {
	const index = "public.orders_created_at_idx"

	build := func(t *testing.T, confirm bool, targetIndex string) *authFixture {
		t.Helper()
		auth := &protocol.Authorization{
			Mode:                 protocol.AuthExplicit,
			Object:               obj(t, index),
			RequiredConfirmation: protocol.ConfirmExclusiveLock,
		}
		plan := newPlan(t, dropNode(t, "nDrop", index, auth))
		plan.Target.Index = objPtr(t, targetIndex)
		if confirm {
			plan.Confirmations = []protocol.Confirmation{{
				Flag:  protocol.ConfirmExclusiveLock,
				Actor: "operator",
				At:    protocol.NewTimestamp(time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)),
			}}
		}
		n, _ := plan.NodeByID("nDrop")
		return &authFixture{store: newFakeStore(&recorder{}), plan: plan, node: n}
	}

	t.Run("satisfied", func(t *testing.T) {
		f := build(t, true, index)
		d, err := Authorize(context.Background(), f.store, f.plan, f.node)
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if !d.Satisfied {
			t.Fatalf("decision = %+v, want satisfied", d)
		}
		if d.Evidence["confirmed_by"] != "operator" {
			t.Fatalf("evidence = %v, want the actor who confirmed", d.Evidence)
		}
	})

	t.Run("unsatisfied without the acknowledgement", func(t *testing.T) {
		f := build(t, false, index)
		d, _ := Authorize(context.Background(), f.store, f.plan, f.node)
		if d.Satisfied {
			t.Fatal("a drop was authorized without --confirm-exclusive-lock")
		}
	})

	t.Run("unsatisfied when the plan names another object", func(t *testing.T) {
		f := build(t, true, "public.some_other_idx")
		d, _ := Authorize(context.Background(), f.store, f.plan, f.node)
		if d.Satisfied {
			t.Fatal("the operator's intent covered a different index")
		}
	})
}

func TestAuthorizeRequiresTheAuthorizationToNameWhatTheNodeDestroys(t *testing.T) {
	// The node drops A; the authorization vouches for B, and B has provenance.
	f := newAuthFixture(t, provenanceAuth(t, "public.other_idx"), "public.orders_idx")
	f.store.provenance["public.other_idx"] = Provenance{RunID: "run-0", Object: obj(t, "public.other_idx")}

	d, err := Authorize(context.Background(), f.store, f.plan, f.node)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if d.Satisfied {
		t.Fatal("an authorization for a different object let the drop through")
	}
	if d.Object.Name != "orders_idx" {
		t.Fatalf("decision object = %s, want the node's actual target", d.Object)
	}
}

func TestAuthorizeRejectsAMissingOrMalformedAuthorization(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		// protocol.Node.Validate rejects this, so reaching Authorize means the
		// plan was hand-built. It must still deny rather than assume.
		n := node("nDrop", protocol.KindIndexDropConcurrently, &protocol.DropConcurrentlyParams{
			Index: obj(t, "public.i"),
		})
		d, err := Authorize(context.Background(), newFakeStore(&recorder{}), nil, &n)
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if d.Satisfied {
			t.Fatal("a node with no authorization was authorized")
		}
	})

	t.Run("leftover without a relation", func(t *testing.T) {
		auth := &protocol.Authorization{
			Mode:   protocol.AuthLeftover,
			Object: obj(t, "public.orders_idx_ccnew"),
		}
		n := node("nDrop", protocol.KindIndexDropConcurrently, &protocol.DropConcurrentlyParams{
			Index: obj(t, "public.orders_idx_ccnew"),
		})
		n.Authorization = auth
		d, err := Authorize(context.Background(), newFakeStore(&recorder{}), nil, &n)
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if d.Satisfied {
			t.Fatal("leftover mode was satisfied with no relation to resolve run history against")
		}
	})

	t.Run("unknown mode", func(t *testing.T) {
		auth := &protocol.Authorization{
			Mode:   protocol.AuthorizationMode("vibes"),
			Object: obj(t, "public.i"),
		}
		n := node("nDrop", protocol.KindIndexDropConcurrently, &protocol.DropConcurrentlyParams{
			Index: obj(t, "public.i"),
		})
		n.Authorization = auth
		d, err := Authorize(context.Background(), newFakeStore(&recorder{}), nil, &n)
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if d.Satisfied {
			t.Fatal("an unknown authorization mode was satisfied")
		}
	})
}

func TestAuthorizeRefusesANonDestructiveNode(t *testing.T) {
	n := node("n", protocol.KindIndexAttach, &protocol.AttachParams{
		ParentIndex: obj(t, "public.p"),
		ChildIndex:  obj(t, "public.c"),
	})
	if _, err := Authorize(context.Background(), newFakeStore(&recorder{}), nil, &n); !errors.Is(err, protocol.ErrInvalidPlan) {
		t.Fatalf("error = %v, want ErrInvalidPlan", err)
	}
}

func TestAuthorizeSurfacesAStoreFailureRatherThanDenying(t *testing.T) {
	// "Cannot decide" is not "denied": a store outage must halt the run with a
	// store error, not be recorded as an authorization failure.
	const index = "public.orders_idx"
	f := newAuthFixture(t, provenanceAuth(t, index), index)
	boom := errors.New("state store unreachable")
	failing := &failingAuthority{err: boom}

	if _, err := Authorize(context.Background(), failing, f.plan, f.node); !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the store failure", err)
	}
}

type failingAuthority struct{ err error }

func (f *failingAuthority) LookupProvenance(context.Context, protocol.ObjectName) (Provenance, bool, error) {
	return Provenance{}, false, f.err
}

func (f *failingAuthority) HasReindexRun(context.Context, protocol.ObjectName) (bool, error) {
	return false, f.err
}

func TestAuthorizationIsReEvaluatedAtDispatchNotTakenFromThePlan(t *testing.T) {
	// FR-AUTH-5: the plan asserts provenance and even carries an encouraging
	// note. Live state says otherwise, and live state wins.
	h := newHarness()
	auth := provenanceAuth(t, "public.orders_created_at_idx_orders_2026_03")
	auth.Note = "provenance was verified at plan time"
	plan := cleanupPlan(t, auth)

	_, err := h.run(t, plan)
	if !errors.Is(err, protocol.ErrAuthorizationUnsatisfied) {
		t.Fatalf("error = %v, want the plan's claim to be overruled by live state", err)
	}

	// The same plan succeeds once live state actually supports it.
	h2 := newHarness()
	h2.store.provenance["public.orders_created_at_idx_orders_2026_03"] = Provenance{
		RunID: "run-0", Object: obj(t, "public.orders_created_at_idx_orders_2026_03"),
	}
	if _, err := h2.run(t, plan); err != nil {
		t.Fatalf("Run with provenance present: %v", err)
	}
}
