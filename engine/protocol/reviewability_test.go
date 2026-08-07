package protocol

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestEncodePlanDoesNotEscapeTheReviewedSQLFragments is FR-PLANFILE-1 read
// against what the plan file is *for*.
//
// IndexColumn.Expression and IndexDefinition.Where are the only two fields that
// are not identifier-quoted: the renderer emits them into DDL byte for byte,
// and the documented defence is that a human reviews them in the plan file
// (G2, T2). encoding/json escapes <, > and & by default, so `status <> 'done'`
// reaches the reviewer as `status <> 'done'`, which makes a hostile
// fragment harder to spot in exactly the diff that is supposed to catch it.
//
// canonical.go already goes to explicit trouble to avoid this for the digest.
// The artifact and the canonical form should agree on presentation.
func TestEncodePlanDoesNotEscapeTheReviewedSQLFragments(t *testing.T) {
	where := `status <> 'done' AND note = 'a''b'`
	expression := `lower(email) < 'z' AND x > 1`

	p := planWithPredicate(t, where, expression)
	data, err := EncodePlan(p)
	if err != nil {
		t.Fatalf("EncodePlan: %v", err)
	}

	for _, want := range []string{where, expression} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("the plan file does not contain %q literally.\nA reviewer sees:\n%s", want, data)
		}
	}
	// The escape sequences are built rather than written literally, so the
	// assertion cannot be defeated by this file's own escaping.
	for _, r := range []rune{'<', '>', '&'} {
		escaped := fmt.Sprintf(`\u%04x`, r)
		if bytes.Contains(data, []byte(escaped)) {
			t.Errorf("the plan file contains the HTML escape %s for %q, so a reviewer does not see "+
				"the SQL that will run:\n%s", escaped, r, data)
		}
	}
}

// TestEncodedPlanStillRoundTrips: the presentation change must not weaken the
// digest or the round-trip check that backs AC-2.
func TestEncodedPlanStillRoundTrips(t *testing.T) {
	p := planWithPredicate(t, `status <> 'done'`, `lower(email) < 'z'`)
	data, err := EncodePlan(p)
	if err != nil {
		t.Fatalf("EncodePlan: %v", err)
	}
	back, err := DecodePlan(data)
	if err != nil {
		t.Fatalf("DecodePlan: %v", err)
	}
	if err := back.VerifyDigest(); err != nil {
		t.Fatalf("VerifyDigest after a round trip: %v", err)
	}
	if back.Digest != p.Digest {
		t.Errorf("digest changed across the round trip: %s -> %s", p.Digest, back.Digest)
	}
}

// TestDecodePlanReportsTheVersionBeforeTheContents is NFR-COMPAT-3.
//
// A plan written by a newer binary will pair a newer format version with node
// kinds this build does not know. Unmarshalling first means the operator is
// told their plan is corrupt ("unknown node kind") when the real answer is that
// their binary is too old, which is the wrong diagnosis on precisely the
// upgrade path this requirement exists to make survivable.
func TestDecodePlanReportsTheVersionBeforeTheContents(t *testing.T) {
	future := `{
  "format_version": 99,
  "plan_id": "plan-future",
  "operation": "create-index",
  "nodes": [{"id": "n1", "kind": "index.rewrite_v2", "params": {}}]
}`
	_, err := DecodePlan([]byte(future))
	if err == nil {
		t.Fatal("a plan declaring format version 99 was accepted")
	}
	if got := KindOf(err); got != KindUnsupportedFormatVersion {
		t.Fatalf("error kind = %q, want %q; the operator is told the plan is malformed "+
			"rather than that this binary is too old (NFR-COMPAT-3).\nerror: %v",
			got, KindUnsupportedFormatVersion, err)
	}
	if !strings.Contains(err.Error(), "format version") {
		t.Errorf("the message does not mention the format version: %v", err)
	}
}

// TestValidateRequiresATopologyFingerprint is FR-PLANFILE-4: "The plan SHALL
// contain a topology_fingerprint".
//
// Unenforced, a plan with no fingerprint validates, seals and encodes, and
// under --allow-drift it executes with only a warning, issuing DDL against a
// catalog whose shape was never bound to the artifact. That defeats T8 and T9.
// The CLI's own plan path always sets it, so this is reachable through the
// exported API rather than through today's binary, which is exactly what a
// contract package should refuse.
func TestValidateRequiresATopologyFingerprint(t *testing.T) {
	p := planWithPredicate(t, "", "")
	p.TopologyFingerprint = ""
	if err := p.Validate(); err == nil {
		t.Fatal("a plan with no topology fingerprint validated (FR-PLANFILE-4)")
	}

	p.TopologyFingerprint = "not-a-fingerprint"
	if err := p.Validate(); err == nil {
		t.Fatal("a plan whose fingerprint carries no algorithm prefix validated")
	}
}

// planWithPredicate builds a sealed one-node create plan carrying the two
// operator-authored SQL fragments.
func planWithPredicate(t *testing.T, where, expression string) *Plan {
	t.Helper()
	col := IndexColumn{Name: "email"}
	if expression != "" {
		col = IndexColumn{Expression: expression}
	}
	index := NewObjectName("public", "orders_idx")
	p := &Plan{
		FormatVersion:       PlanFormatVersion,
		PlanID:              "plan-review",
		Operation:           OpCreateIndex,
		CreatedAt:           NewTimestamp(time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)),
		TopologyFingerprint: FingerprintPrefix + strings.Repeat("a", 64),
		Target: Target{
			Database: "appdb",
			Table:    NewObjectName("public", "orders"),
			Index:    &index,
		},
		Nodes: []Node{{
			ID:   "create:p1",
			Kind: KindIndexCreateConcurrently,
			Params: &CreateConcurrentlyParams{
				Partition: NewObjectName("public", "orders_p1"),
				Index:     NewObjectName("public", "orders_idx_p1"),
				Definition: IndexDefinition{
					Method:  "btree",
					Columns: []IndexColumn{col},
					Where:   where,
				},
			},
		}},
	}
	if err := p.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return p
}
