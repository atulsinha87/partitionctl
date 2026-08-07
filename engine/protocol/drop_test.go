package protocol

import (
	"strings"
	"testing"
)

func ourMarker() Marker {
	return Marker{
		Run: "run-1", Plan: "sha256:aaa", Op: string(OpCreateIndex),
		Role: MarkerRoleLeaf, At: "2026-08-07T00:00:00Z",
	}
}

// The whole of directive A.5.1, as a table. Every row is a decision that ends
// in a DROP against a production catalog or in a refusal, so each one is
// asserted rather than sampled.
func TestDecideProvenanceDropTable(t *testing.T) {
	object := NewObjectName("public", "orders_idx_p1")
	cases := []struct {
		name     string
		status   MarkerStatus
		claim    string
		want     DropAction
		evidence map[string]string
	}{
		{
			name: "our marker, no claim: drop on the marker", status: MarkerOurs, want: DropAuthorized,
			evidence: map[string]string{"source": "marker", "marker_run": "run-1"},
		},
		{
			name: "our marker and a claim: still the marker", status: MarkerOurs, claim: "run-2",
			want: DropAuthorized, evidence: map[string]string{"source": "marker"},
		},
		{
			name: "no marker but a live claim: adopt then drop", status: MarkerAbsent, claim: "run-9",
			want: DropAdoptThenDrop, evidence: map[string]string{"source": "claim", "claim_run": "run-9"},
		},
		{name: "no marker and no claim: halt", status: MarkerAbsent, want: DropHalt},
		{name: "a human's comment: halt", status: MarkerForeign, want: DropHalt},
		{name: "a human's comment and a claim: still halt", status: MarkerForeign, claim: "run-9", want: DropHalt},
		{name: "a marker we cannot read: halt", status: MarkerUnreadable, want: DropHalt},
		{name: "a marker we cannot read, with a claim: still halt", status: MarkerUnreadable, claim: "run-9", want: DropHalt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := ProvenanceDropInput{Object: object, Status: c.status, ClaimRun: c.claim}
			if c.status == MarkerOurs {
				in.Marker = ourMarker()
			}
			got := DecideProvenanceDrop(in)
			if got.Action != c.want {
				t.Fatalf("Action = %s, want %s (reason: %s)", got.Action, c.want, got.Reason)
			}
			if got.Action == DropHalt {
				if got.Reason == "" {
					t.Fatal("a halt with no reason is not actionable")
				}
				if got.Satisfied() {
					t.Fatal("a halt reported itself satisfied")
				}
				return
			}
			for k, v := range c.evidence {
				if got.Evidence[k] != v {
					t.Fatalf("evidence[%q] = %q, want %q (full: %v)", k, got.Evidence[k], v, got.Evidence)
				}
			}
			if got.Evidence["object"] != object.String() {
				t.Fatalf("evidence does not name the object: %v", got.Evidence)
			}
		})
	}
}

// AC-6, stated as the attack it is: a completed run leaves nothing behind that
// can authorize destroying a same-named index somebody else created later.
func TestProvenanceDropRefusesASameNamedForeignIndex(t *testing.T) {
	got := DecideProvenanceDrop(ProvenanceDropInput{
		Object: NewObjectName("public", "orders_idx_p1"),
		Status: MarkerAbsent, // their index; nothing on it
		// No claim: the run that built ours completed, so every node is DONE.
	})
	if got.Action != DropHalt {
		t.Fatalf("Action = %s; a same-named foreign index must never be dropped (AC-6)", got.Action)
	}
	if !strings.Contains(got.Reason, "no run holds a live claim") {
		t.Fatalf("reason does not explain itself: %s", got.Reason)
	}
}

func TestDecideLeftoverDropTable(t *testing.T) {
	ccnew := NewObjectName("public", "orders_idx_p1_ccnew")
	ccold := NewObjectName("public", "orders_idx_p1_ccold3")
	plain := NewObjectName("public", "orders_idx_p1")

	cases := []struct {
		name       string
		object     ObjectName
		baseExists bool
		baseStatus MarkerStatus
		want       DropAction
	}{
		{"ccnew on a marked base", ccnew, true, MarkerOurs, DropAuthorized},
		{"ccold with a disambiguating integer", ccold, true, MarkerOurs, DropAuthorized},
		{"not a leftover name at all", plain, true, MarkerOurs, DropHalt},
		{"base does not exist", ccnew, false, MarkerAbsent, DropHalt},
		{"base is unmarked", ccnew, true, MarkerAbsent, DropHalt},
		{"base carries a human's comment", ccnew, true, MarkerForeign, DropHalt},
		{"base marker is unreadable", ccnew, true, MarkerUnreadable, DropHalt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := LeftoverDropInput{
				Object: c.object, BaseExists: c.baseExists, BaseStatus: c.baseStatus,
			}
			if c.baseStatus == MarkerOurs {
				in.BaseMarker = ourMarker()
			}
			got := DecideLeftoverDrop(in)
			if got.Action != c.want {
				t.Fatalf("Action = %s, want %s (reason: %s)", got.Action, c.want, got.Reason)
			}
			if got.Action == DropHalt && got.Reason == "" {
				t.Fatal("a halt with no reason is not actionable")
			}
			if got.Action == DropAuthorized && got.Evidence["base_index"] != "public.orders_idx_p1" {
				t.Fatalf("evidence does not name the base index: %v", got.Evidence)
			}
		})
	}
}

// AC-19: an operator's own hand-made _ccnew on an index PartitionCTL never
// touched is never dropped.
func TestLeftoverDropRefusesAHandMadeLeftover(t *testing.T) {
	got := DecideLeftoverDrop(LeftoverDropInput{
		Object:     NewObjectName("public", "their_index_ccnew"),
		BaseExists: true,
		BaseStatus: MarkerAbsent,
	})
	if got.Action != DropHalt {
		t.Fatalf("Action = %s, want halt (AC-19)", got.Action)
	}
}

func TestLeftoverBase(t *testing.T) {
	got, ok := LeftoverBase(NewObjectName("app", "idx_ccnew12"))
	if !ok || got != NewObjectName("app", "idx") {
		t.Fatalf("LeftoverBase = %v, %t", got, ok)
	}
	if _, ok := LeftoverBase(NewObjectName("app", "idx")); ok {
		t.Fatal("a plain name is not a leftover")
	}
}
