package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// authFixture builds a destructive node plus a plan for [Authorize] to judge.
type authFixture struct {
	store   *fakeStore
	catalog *fakeCatalog
	plan    *protocol.Plan
	node    *protocol.Node
}

func newAuthFixture(t *testing.T, auth *protocol.Authorization, index string) *authFixture {
	t.Helper()
	rec := &recorder{}
	plan := newPlan(t, dropNode(t, "nDrop", index, auth))
	n, _ := plan.NodeByID("nDrop")
	return &authFixture{
		store: newFakeStore(rec), catalog: newFakeCatalog(rec), plan: plan, node: n,
	}
}

func (f *authFixture) authorize(t *testing.T) AuthorizationDecision {
	t.Helper()
	d, err := Authorize(context.Background(), f.store, f.catalog, f.plan, f.node)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	return d
}

// Directive A.5.1, evaluated where it matters: at dispatch, against the live
// catalog, with the state store consulted only where a marker is missing.
func TestAuthorizeProvenanceMode(t *testing.T) {
	const index = "public.orders_created_at_idx_orders_2026_03"

	t.Run("satisfied by the marker on the object", func(t *testing.T) {
		f := newAuthFixture(t, provenanceAuth(t, index), index)
		f.catalog.mark(t, obj(t, index), "run-0")

		d := f.authorize(t)
		if !d.Satisfied {
			t.Fatalf("decision = %+v, want satisfied", d)
		}
		if d.Adopt {
			t.Fatal("a marked object does not need adopting")
		}
		if d.Evidence["source"] != "marker" || d.Evidence["marker_run"] != "run-0" {
			t.Fatalf("evidence = %v, want the creating run read off the object", d.Evidence)
		}
	})

	t.Run("unmarked with a live claim is adopted, then dropped", func(t *testing.T) {
		f := newAuthFixture(t, provenanceAuth(t, index), index)
		f.store.claim(obj(t, index), "run-crashed")

		d := f.authorize(t)
		if !d.Satisfied || !d.Adopt {
			t.Fatalf("decision = %+v, want satisfied and adopt", d)
		}
		if d.Evidence["source"] != "claim" || d.Evidence["claim_run"] != "run-crashed" {
			t.Fatalf("evidence = %v, want the claiming run", d.Evidence)
		}
	})

	t.Run("unmarked with no claim is refused", func(t *testing.T) {
		f := newAuthFixture(t, provenanceAuth(t, index), index)
		d := f.authorize(t)
		if d.Satisfied {
			t.Fatal("an INVALID index with no marker and no claim was authorized; AC-6 forbids it")
		}
		if d.Reason == "" {
			t.Fatal("a denial must carry a reason the operator can act on")
		}
	})

	t.Run("a human's comment is never overwritten and never dropped under", func(t *testing.T) {
		f := newAuthFixture(t, provenanceAuth(t, index), index)
		f.catalog.setComment(obj(t, index), "built by the DBA team, do not touch")
		// Even a live claim does not make somebody else's comment ours.
		f.store.claim(obj(t, index), "run-crashed")

		d := f.authorize(t)
		if d.Satisfied {
			t.Fatalf("an object carrying a foreign comment was authorized: %+v", d)
		}
	})

	t.Run("a marker from a newer binary halts", func(t *testing.T) {
		f := newAuthFixture(t, provenanceAuth(t, index), index)
		f.catalog.setComment(obj(t, index), `partitionctl:v9:{"run":"run-future"}`)

		d := f.authorize(t)
		if d.Satisfied {
			t.Fatal("a marker this binary cannot read was treated as ours")
		}
		if d.Reason == "" {
			t.Fatal("a denial must carry a reason")
		}
	})
}

// INV-7 and FR-AUTH-3 as amended: authorization comes off the *base* index,
// never off the leftover, because a rebuild that failed before the swap leaves
// an unmarked _ccnew.
func TestAuthorizeLeftoverModeNeedsBothConditions(t *testing.T) {
	relation := "public.orders_2026_03"
	tests := []struct {
		name       string
		index      string
		markedBase string // the base index to mark, empty for none
		want       bool
	}{
		{"ccnew whose base is ours", "public.orders_idx_ccnew", "public.orders_idx", true},
		{"ccold whose base is ours", "public.orders_idx_ccold", "public.orders_idx", true},
		{"disambiguated suffix", "public.orders_idx_ccnew1", "public.orders_idx", true},
		// AC-19: an operator's own hand-rolled REINDEX leftover, on an index
		// this tool never built, is not ours to drop.
		{"ccnew whose base is unmarked", "public.orders_idx_ccnew", "", false},
		// A marked index does not license dropping an ordinary name.
		{"ordinary name", "public.orders_idx", "public.orders_idx", false},
		// Marking a *different* index proves nothing about this one.
		{"the wrong base is marked", "public.orders_idx_ccnew", "public.other_idx", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			auth := &protocol.Authorization{
				Mode:     protocol.AuthLeftover,
				Object:   obj(t, tc.index),
				Relation: objPtr(t, relation),
			}
			f := newAuthFixture(t, auth, tc.index)
			// The base must actually exist on the relation. Before issue #2 was
			// closed the base name was invented by trimming the leftover's
			// suffix, so these cases passed without any index being there --
			// which is precisely how a marker could be read off an unrelated
			// index. The candidate set now has to contain it.
			f.catalog.setIndexes(obj(t, relation),
				obj(t, "public.orders_idx"), obj(t, tc.index))
			if tc.markedBase != "" {
				f.catalog.mark(t, obj(t, tc.markedBase), "run-reindex-9")
			}
			d := f.authorize(t)
			if d.Satisfied != tc.want {
				t.Fatalf("satisfied = %v, want %v (reason: %s)", d.Satisfied, tc.want, d.Reason)
			}
			if tc.want && d.Evidence["base_index"] != "public.orders_idx" {
				t.Fatalf("evidence = %v, want the base index named", d.Evidence)
			}
		})
	}
}

// The leftover mode never reads the leftover's own comment. PostgreSQL's
// behaviour on _ccnew descriptions is unmeasured, and a rebuild that failed
// before the swap certainly leaves one unmarked.
func TestAuthorizeLeftoverIgnoresTheLeftoversOwnMarker(t *testing.T) {
	const leftover = "public.orders_idx_ccnew"
	auth := &protocol.Authorization{
		Mode:     protocol.AuthLeftover,
		Object:   obj(t, leftover),
		Relation: objPtr(t, "public.orders_2026_03"),
	}
	f := newAuthFixture(t, auth, leftover)
	// The leftover itself is marked; the base is not.
	f.catalog.mark(t, obj(t, leftover), "run-1")

	if d := f.authorize(t); d.Satisfied {
		t.Fatal("a marker on the leftover authorized the drop; the base is the only witness")
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
		rec := &recorder{}
		return &authFixture{
			store: newFakeStore(rec), catalog: newFakeCatalog(rec), plan: plan, node: n,
		}
	}

	t.Run("satisfied", func(t *testing.T) {
		f := build(t, true, index)
		d := f.authorize(t)
		if !d.Satisfied {
			t.Fatalf("decision = %+v, want satisfied", d)
		}
		if d.Evidence["confirmed_by"] != "operator" {
			t.Fatalf("evidence = %v, want the actor who confirmed", d.Evidence)
		}
	})

	t.Run("unsatisfied without the acknowledgement", func(t *testing.T) {
		f := build(t, false, index)
		if d := f.authorize(t); d.Satisfied {
			t.Fatal("a drop was authorized without --confirm-exclusive-lock")
		}
	})

	t.Run("unsatisfied when the plan names another object", func(t *testing.T) {
		f := build(t, true, "public.some_other_idx")
		if d := f.authorize(t); d.Satisfied {
			t.Fatal("the operator's intent covered a different index")
		}
	})

	// FR-DROP-2's reason for existing: drop-index has to work on an index
	// PartitionCTL never created, so the marker must play no part here.
	t.Run("an unmarked index is still droppable under explicit intent", func(t *testing.T) {
		f := build(t, true, index)
		if d := f.authorize(t); !d.Satisfied {
			t.Fatalf("explicit intent was overruled by the absence of a marker: %s", d.Reason)
		}
	})
}

func TestAuthorizeRequiresTheAuthorizationToNameWhatTheNodeDestroys(t *testing.T) {
	// The node drops A; the authorization vouches for B, and B is marked.
	f := newAuthFixture(t, provenanceAuth(t, "public.other_idx"), "public.orders_idx")
	f.catalog.mark(t, obj(t, "public.other_idx"), "run-0")

	d := f.authorize(t)
	if d.Satisfied {
		t.Fatal("an authorization for a different object let the drop through")
	}
	if d.Object.Name != "orders_idx" {
		t.Fatalf("decision object = %s, want the node's actual target", d.Object)
	}
}

func TestAuthorizeRejectsAMissingOrMalformedAuthorization(t *testing.T) {
	newStore := func() (*fakeStore, *fakeCatalog) {
		rec := &recorder{}
		return newFakeStore(rec), newFakeCatalog(rec)
	}

	t.Run("nil", func(t *testing.T) {
		// protocol.Node.Validate rejects this, so reaching Authorize means the
		// plan was hand-built. It must still deny rather than assume.
		n := node("nDrop", protocol.KindIndexDropConcurrently, &protocol.DropConcurrentlyParams{
			Index: obj(t, "public.i"),
		})
		store, cat := newStore()
		d, err := Authorize(context.Background(), store, cat, nil, &n)
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
		store, cat := newStore()
		d, err := Authorize(context.Background(), store, cat, nil, &n)
		if err != nil {
			t.Fatalf("Authorize: %v", err)
		}
		if d.Satisfied {
			t.Fatal("leftover mode was satisfied with no relation naming the leaf it sat on")
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
		store, cat := newStore()
		d, err := Authorize(context.Background(), store, cat, nil, &n)
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
	rec := &recorder{}
	_, err := Authorize(context.Background(), newFakeStore(rec), newFakeCatalog(rec), nil, &n)
	if !errors.Is(err, protocol.ErrInvalidPlan) {
		t.Fatalf("error = %v, want ErrInvalidPlan", err)
	}
}

// "Cannot decide" is not "denied": an outage must halt the run with the
// underlying error rather than be recorded as an authorization failure.
func TestAuthorizeSurfacesAReadFailureRatherThanDenying(t *testing.T) {
	const index = "public.orders_idx"
	boom := errors.New("catalog unreachable")

	t.Run("catalog", func(t *testing.T) {
		f := newAuthFixture(t, provenanceAuth(t, index), index)
		f.catalog.markerFn = func(protocol.ObjectName) (protocol.Marker, protocol.MarkerStatus, error) {
			return protocol.Marker{}, protocol.MarkerAbsent, boom
		}
		if _, err := Authorize(context.Background(), f.store, f.catalog, f.plan, f.node); !errors.Is(err, boom) {
			t.Fatalf("error = %v, want the catalog failure", err)
		}
	})

	t.Run("state store", func(t *testing.T) {
		f := newAuthFixture(t, provenanceAuth(t, index), index)
		f.store.failClaims = boom
		if _, err := Authorize(context.Background(), f.store, f.catalog, f.plan, f.node); !errors.Is(err, boom) {
			t.Fatalf("error = %v, want the store failure", err)
		}
	})

	t.Run("no catalog port at all", func(t *testing.T) {
		f := newAuthFixture(t, provenanceAuth(t, index), index)
		_, err := Authorize(context.Background(), f.store, nil, f.plan, f.node)
		if !errors.Is(err, ErrMissingPort) {
			t.Fatalf("error = %v, want ErrMissingPort; ownership cannot be read without a catalog", err)
		}
	})
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

	// The same plan succeeds once the object actually carries our marker.
	h2 := newHarness()
	h2.catalog.mark(t, obj(t, "public.orders_created_at_idx_orders_2026_03"), "run-0")
	if _, err := h2.run(t, plan); err != nil {
		t.Fatalf("Run with the marker present: %v", err)
	}
}

// Issue #2, consequence 3: the forgery this fix exists to prevent.
//
// PostgreSQL truncates the base to make room for _ccnew, so once a leftover
// fills all 63 bytes the bytes that told two indexes apart are gone. The old
// code trimmed the suffix and read the ownership marker off whatever carried
// the resulting name -- which could be an entirely different index. If that one
// happened to be marked, an unrelated object's marker authorized the drop.
//
// Resolution now runs forwards against the indexes that actually exist on the
// relation, and refuses when more than one derives the leftover.
func TestAuthorizeLeftoverRefusesWhenTruncationMakesTheBaseAmbiguous(t *testing.T) {
	// The exact shape of the forgery. The leftover fills all 63 bytes, so the
	// trimmed stem is 57. A DIFFERENT index happens to be named exactly that
	// stem, and it is one of ours, marked. The leftover's real base is a longer
	// name that truncates to the same 57 bytes.
	//
	// Old behaviour: trim to the stem, read the marker off whatever carries that
	// name -- the unrelated index -- find it ours, and authorize. That is an
	// ownership proof taken from the wrong object.
	stem := "idx_" + strings.Repeat("a", 53) // 57 bytes
	unrelatedButMarked := "public." + stem   // an ordinary index that merely has this name
	realBase := "public." + stem + "xxx"     // 60 bytes, truncates to the same stem
	leftover := "public." + stem + "_ccnew"  // 63 bytes

	auth := &protocol.Authorization{
		Mode:     protocol.AuthLeftover,
		Object:   obj(t, leftover),
		Relation: objPtr(t, "public.orders_2026_03"),
	}
	f := newAuthFixture(t, auth, leftover)
	f.catalog.setIndexes(obj(t, "public.orders_2026_03"),
		obj(t, unrelatedButMarked), obj(t, realBase), obj(t, leftover))
	f.catalog.mark(t, obj(t, unrelatedButMarked), "run-reindex-9")

	d := f.authorize(t)

	if d.Satisfied {
		t.Fatal("authorized a drop on a marker that may belong to a different index (issue #2)")
	}
	if !strings.Contains(d.Reason, "truncated") {
		t.Fatalf("the refusal must explain that truncation made it undecidable, got: %s", d.Reason)
	}
}

// The base is resolved even when PostgreSQL truncated it, so the ordinary case
// on long names still authorizes rather than halting for no reason.
func TestAuthorizeLeftoverResolvesATruncatedBase(t *testing.T) {
	base := "public.idx_" + strings.Repeat("b", 54)                 // 58-byte base name
	leftover := "public.idx_" + strings.Repeat("b", 52) + "_ccnew1" // truncated to 56 + suffix

	auth := &protocol.Authorization{
		Mode:     protocol.AuthLeftover,
		Object:   obj(t, leftover),
		Relation: objPtr(t, "public.orders_2026_03"),
	}
	f := newAuthFixture(t, auth, leftover)
	f.catalog.setIndexes(obj(t, "public.orders_2026_03"), obj(t, base), obj(t, leftover))
	f.catalog.mark(t, obj(t, base), "run-reindex-9")

	d := f.authorize(t)

	if !d.Satisfied {
		t.Fatalf("a truncated but unambiguous base was not resolved: %s", d.Reason)
	}
}
