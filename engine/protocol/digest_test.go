package protocol

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// goldenDigest pins the digest of samplePlan. Its purpose is to fail if the
// canonicalization ever changes, which is the only way to detect a break in the
// "identical across processes and across runs" guarantee from inside one
// process: a value computed by an earlier build is compared against this one.
//
// It is updated only alongside a deliberate, reviewed change to the plan format
// or its canonicalization, and the format version in the body must move with
// it. The current value covers format version 2, which added Plan.Topology so a
// drift refusal can name what changed (AC-3). The previous value,
// sha256:48ca9d94c56e7826a5de0d1e11268fcd1a2d19ffeb22332992e5031ddb03ddd8,
// covered version 1.
const goldenDigest = "sha256:e9206372b7b3bd254bf295697bf4ce478da15eb8357e208febbe5ca9b2c011ed"

func TestDigestGolden(t *testing.T) {
	p := samplePlan(t)
	got, err := p.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	if got != goldenDigest {
		t.Fatalf("digest drifted.\n got %s\nwant %s\n\ncanonical body:\n%s",
			got, goldenDigest, mustCanonicalBody(t, p))
	}
}

func TestDigestIsStableAcrossRepeatedComputation(t *testing.T) {
	p := samplePlan(t)
	first, err := p.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	for i := 0; i < 200; i++ {
		got, err := p.ComputeDigest()
		if err != nil {
			t.Fatalf("ComputeDigest: %v", err)
		}
		if got != first {
			t.Fatalf("iteration %d: digest changed: %s != %s", i, got, first)
		}
	}
}

// Map iteration order in Go is randomized per range statement. Rebuilding the
// same plan repeatedly and inserting map keys in different orders must not move
// the digest.
func TestDigestIgnoresMapInsertionOrder(t *testing.T) {
	keys := []string{"fillfactor", "deduplicate_items", "vacuum_truncate", "autovacuum_enabled", "log_autovacuum_min_duration", "toast_tuple_target"}
	vals := []string{"90", "on", "false", "true", "0", "2040"}

	build := func(order []int) *Plan {
		params := make(map[string]string, len(keys))
		for _, i := range order {
			params[keys[i]] = vals[i]
		}
		idx := NewObjectName("public", "orders_created_at_idx")
		return &Plan{
			FormatVersion: PlanFormatVersion,
			PlanID:        "p",
			Operation:     OpCreateIndex,
			Target:        Target{Table: NewObjectName("public", "orders")},
			CreatedAt:     NewTimestamp(time.Unix(0, 0)),
			Nodes: []Node{{
				ID:   "n",
				Kind: KindIndexCreateParentInvalid,
				Params: &CreateParentInvalidParams{
					Parent: NewObjectName("public", "orders"),
					Index:  idx,
					Definition: IndexDefinition{
						Columns:       []IndexColumn{{Name: "created_at"}},
						StorageParams: params,
					},
				},
			}},
		}
	}

	forward := []int{0, 1, 2, 3, 4, 5}
	backward := []int{5, 4, 3, 2, 1, 0}
	shuffled := []int{3, 0, 5, 1, 4, 2}

	want, err := build(forward).ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	for _, order := range [][]int{forward, backward, shuffled} {
		// Recompute many times: each run reseeds Go's map iteration order.
		for i := 0; i < 50; i++ {
			got, err := build(order).ComputeDigest()
			if err != nil {
				t.Fatalf("ComputeDigest: %v", err)
			}
			if got != want {
				t.Fatalf("map order %v iteration %d: digest %s, want %s", order, i, got, want)
			}
		}
	}
}

// The same instant expressed in different zones, and with a monotonic clock
// reading attached, must produce one digest.
func TestDigestNormalizesTimeZones(t *testing.T) {
	instant := time.Date(2026, 8, 7, 12, 0, 0, 123456789, time.UTC)
	kolkata := time.FixedZone("IST", 5*3600+1800)
	newYork := time.FixedZone("EDT", -4*3600)

	build := func(ts time.Time) *Plan {
		p := samplePlan(t)
		p.CreatedAt = NewTimestamp(ts)
		p.Confirmations = []Confirmation{{Flag: "--yes", At: NewTimestamp(ts)}}
		return p
	}

	want, err := build(instant).ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	for name, ts := range map[string]time.Time{
		"utc":     instant,
		"kolkata": instant.In(kolkata),
		"newyork": instant.In(newYork),
		"local":   instant.Local(),
	} {
		got, err := build(ts).ComputeDigest()
		if err != nil {
			t.Fatalf("%s: ComputeDigest: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: digest %s, want %s", name, got, want)
		}
	}
}

func TestDigestNormalizesMonotonicClockReading(t *testing.T) {
	now := time.Now()
	stripped := now.Round(0)

	a := samplePlan(t)
	a.CreatedAt = NewTimestamp(now)
	b := samplePlan(t)
	b.CreatedAt = NewTimestamp(stripped)

	da, err := a.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	db, err := b.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	if da != db {
		t.Fatalf("monotonic reading changed the digest: %s != %s", da, db)
	}
}

// Unicode, HTML-sensitive characters and the JS line separators must survive
// canonicalization without the digest depending on any encoder's escaping mood.
func TestDigestHandlesUnicode(t *testing.T) {
	strs := []string{
		"plain",
		"<script>&amp;</script>",
		"emoji 🐘 partition",
		"line separator paragraph",
		"combining é vs precomposed é",
		"tab\tnewline\nquote\"backslash\\",
		"control\x00\x01\x1f",
		"кириллица",
		"中文分区",
	}
	seen := make(map[string]string, len(strs))
	for _, s := range strs {
		p := samplePlan(t)
		p.Nodes[0].RenderedSQL = s
		d, err := p.ComputeDigest()
		if err != nil {
			t.Fatalf("%q: ComputeDigest: %v", s, err)
		}
		// Stable under repetition.
		for i := 0; i < 20; i++ {
			again, err := p.ComputeDigest()
			if err != nil {
				t.Fatalf("%q: ComputeDigest: %v", s, err)
			}
			if again != d {
				t.Fatalf("%q: unstable digest", s)
			}
		}
		if prev, dup := seen[d]; dup {
			t.Fatalf("distinct strings %q and %q collide on digest %s", prev, s, d)
		}
		seen[d] = s

		// And it must survive a JSON round trip.
		encoded, err := EncodeSealed(t, p)
		if err != nil {
			t.Fatalf("%q: encode: %v", s, err)
		}
		back, err := DecodePlan(encoded)
		if err != nil {
			t.Fatalf("%q: DecodePlan: %v", s, err)
		}
		if err := back.VerifyDigest(); err != nil {
			t.Fatalf("%q: round trip broke the digest: %v", s, err)
		}
	}
}

// Invalid UTF-8 must normalize deterministically rather than producing two
// different digests for the same value.
func TestDigestNormalizesInvalidUTF8(t *testing.T) {
	p := samplePlan(t)
	p.Nodes[0].RenderedSQL = "bad \xff\xfe bytes"
	first, err := p.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}

	q := samplePlan(t)
	q.Nodes[0].RenderedSQL = "bad �� bytes"
	second, err := q.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	if first != second {
		t.Fatalf("invalid UTF-8 did not normalize to U+FFFD: %s != %s", first, second)
	}
}

func TestDigestExcludesItself(t *testing.T) {
	p := samplePlan(t)
	want, err := p.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	for _, d := range []string{"", "sha256:deadbeef", want, strings.Repeat("x", 71)} {
		p.Digest = d
		got, err := p.ComputeDigest()
		if err != nil {
			t.Fatalf("ComputeDigest: %v", err)
		}
		if got != want {
			t.Errorf("digest field %q leaked into the digest: %s, want %s", d, got, want)
		}
	}
	body, err := p.CanonicalBody()
	if err != nil {
		t.Fatalf("CanonicalBody: %v", err)
	}
	if strings.Contains(string(body), `"digest"`) {
		t.Errorf("canonical body still contains the digest key:\n%s", body)
	}
}

// Every field the plan carries must be covered: changing any one of them must
// move the digest.
func TestDigestCoversEveryField(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(p *Plan)
	}{
		{"format_version", func(p *Plan) { p.FormatVersion = 99 }},
		{"plan_id", func(p *Plan) { p.PlanID = "other" }},
		{"operation", func(p *Plan) { p.Operation = OpDropIndex }},
		{"target_database", func(p *Plan) { p.Target.Database = "other" }},
		{"target_table", func(p *Plan) { p.Target.Table.Name = "other" }},
		{"target_schema", func(p *Plan) { p.Target.Table.Schema = "other" }},
		{"target_index", func(p *Plan) { p.Target.Index = nil }},
		{"created_at", func(p *Plan) { p.CreatedAt = NewTimestamp(p.CreatedAt.Time.Add(time.Nanosecond)) }},
		{"confirmations", func(p *Plan) {
			p.Confirmations = []Confirmation{{Flag: ConfirmExclusiveLock, At: p.CreatedAt}}
		}},
		{"fingerprint", func(p *Plan) { p.TopologyFingerprint = "sha256:ff" }},
		{"node_id", func(p *Plan) { p.Nodes[0].ID = "renamed" }},
		{"node_kind", func(p *Plan) {
			// Kind and Params are welded together by Node.MarshalJSON, so the
			// only way to change a node's kind is to change both.
			p.Nodes[6].Kind = KindIndexVerify
			idx := NewObjectName("public", "orders_created_at_idx")
			p.Nodes[6].Params = &VerifyParams{Checks: []VerifyCheck{{Check: CheckParentIndexValid, ParentIndex: &idx}}}
		}},
		{"node_rendered_sql", func(p *Plan) { p.Nodes[0].RenderedSQL += " " }},
		{"node_estimate", func(p *Plan) { p.Nodes[3].EstimatedSeconds++ }},
		{"node_depends_on", func(p *Plan) { p.Nodes[3].DependsOn = nil }},
		{"node_order", func(p *Plan) { p.Nodes[0], p.Nodes[1] = p.Nodes[1], p.Nodes[0] }},
		{"authorization_mode", func(p *Plan) { p.Nodes[1].Authorization.Mode = AuthExplicit }},
		{"authorization_object", func(p *Plan) { p.Nodes[1].Authorization.Object.Name = "other" }},
		{"params_column", func(p *Plan) {
			p.Nodes[2].Params.(*CreateParentInvalidParams).Definition.Columns[0].Name = "updated_at"
		}},
		{"params_column_order", func(p *Plan) {
			cols := p.Nodes[2].Params.(*CreateParentInvalidParams).Definition.Columns
			cols[0], cols[1] = cols[1], cols[0]
		}},
		{"params_nulls_first", func(p *Plan) {
			p.Nodes[2].Params.(*CreateParentInvalidParams).Definition.Columns[0].NullsFirst = boolPtr(true)
		}},
		{"params_nulls_first_unset", func(p *Plan) {
			p.Nodes[2].Params.(*CreateParentInvalidParams).Definition.Columns[0].NullsFirst = nil
		}},
		{"params_storage_value", func(p *Plan) {
			p.Nodes[2].Params.(*CreateParentInvalidParams).Definition.StorageParams["fillfactor"] = "70"
		}},
		{"params_storage_key", func(p *Plan) {
			d := &p.Nodes[2].Params.(*CreateParentInvalidParams).Definition
			delete(d.StorageParams, "fillfactor")
			d.StorageParams["fill_factor"] = "90"
		}},
		{"params_where", func(p *Plan) {
			p.Nodes[2].Params.(*CreateParentInvalidParams).Definition.Where = "true"
		}},
		{"params_unique", func(p *Plan) {
			p.Nodes[2].Params.(*CreateParentInvalidParams).Definition.Unique = true
		}},
		{"params_wait", func(p *Plan) { p.Nodes[6].Params.(*WaitParams).Seconds = 31 }},
		{"params_expected_count", func(p *Plan) {
			p.Nodes[7].Params.(*VerifyParams).Checks[1].ExpectedCount = intPtr(2)
		}},
		{"params_drop_reason", func(p *Plan) { p.Nodes[1].Params.(*DropConcurrentlyParams).Reason = DropCCNew }},
	}

	base := samplePlan(t)
	want, err := base.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := samplePlan(t)
			tc.mutate(p)
			got, err := p.ComputeDigest()
			if err != nil {
				t.Fatalf("ComputeDigest: %v", err)
			}
			if got == want {
				t.Fatalf("mutating %s did not change the digest", tc.name)
			}
		})
	}
}

func TestSealAndVerifyDigest(t *testing.T) {
	p := samplePlan(t)
	if p.Digest == "" {
		t.Fatal("Seal left the digest empty")
	}
	if !strings.HasPrefix(p.Digest, DigestPrefix) {
		t.Fatalf("digest %q lacks the %q prefix", p.Digest, DigestPrefix)
	}
	if len(p.Digest) != len(DigestPrefix)+64 {
		t.Fatalf("digest %q is not %d hex characters", p.Digest, 64)
	}
	if err := p.VerifyDigest(); err != nil {
		t.Fatalf("VerifyDigest on a freshly sealed plan: %v", err)
	}
}

func TestVerifyDigestRejectsTampering(t *testing.T) {
	// AC-2 / T1: a plan edited after approval must refuse to run.
	p := samplePlan(t)
	p.Nodes[0].RenderedSQL = "-- edited after approval"

	err := p.VerifyDigest()
	if err == nil {
		t.Fatal("VerifyDigest accepted a tampered plan")
	}
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("error %v is not ErrDigestMismatch", err)
	}
	if code := ExitCodeFor(err); code != ExitDigestMismatch {
		t.Fatalf("exit code %d, want %d", code, ExitDigestMismatch)
	}
}

func TestVerifyDigestRejectsUnsealedPlan(t *testing.T) {
	p := samplePlan(t)
	p.Digest = ""
	if err := p.VerifyDigest(); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("error %v is not ErrDigestMismatch", err)
	}
}

// The plan must survive the file round trip that FR-PLANFILE-1 describes: write
// it, read it back, and the digest still verifies.
func TestPlanRoundTripPreservesDigest(t *testing.T) {
	p := samplePlan(t)

	encoded, err := EncodePlan(p)
	if err != nil {
		t.Fatalf("EncodePlan: %v", err)
	}
	back, err := DecodePlan(encoded)
	if err != nil {
		t.Fatalf("DecodePlan: %v", err)
	}
	if err := back.VerifyDigest(); err != nil {
		t.Fatalf("VerifyDigest after round trip: %v", err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("Validate after round trip: %v", err)
	}

	// Re-encoding is a fixed point.
	again, err := EncodePlan(back)
	if err != nil {
		t.Fatalf("EncodePlan (second): %v", err)
	}
	if string(again) != string(encoded) {
		t.Fatalf("re-encoding is not a fixed point:\nfirst:\n%s\nsecond:\n%s", encoded, again)
	}
}

// Content the parser would silently drop must be refused, because the digest is
// computed over the parsed plan and so could not cover it.
func TestDecodePlanRefusesContentItWouldDrop(t *testing.T) {
	p := samplePlan(t)
	encoded, err := EncodePlan(p)
	if err != nil {
		t.Fatalf("EncodePlan: %v", err)
	}

	tests := []struct {
		name string
		old  string
		new  string
	}{
		{
			name: "unknown top-level field",
			old:  `"plan_id"`,
			new:  `"smuggled": "payload",` + "\n  " + `"plan_id"`,
		},
		{
			name: "unknown node field",
			old:  `"id": "assert"`,
			new:  `"id": "assert",` + "\n      " + `"run_this_too": "DROP TABLE orders"`,
		},
		{
			name: "unknown params field",
			old:  `"seconds": 30`,
			new:  `"seconds": 30,` + "\n        " + `"minutes": 5`,
		},
		{
			name: "duplicate key",
			old:  `"plan_id": "plan-01HZ"`,
			new:  `"plan_id": "plan-01HZ",` + "\n  " + `"plan_id": "plan-01HZ"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			injected := strings.Replace(string(encoded), tc.old, tc.new, 1)
			if injected == string(encoded) {
				t.Fatalf("fixture does not contain %q", tc.old)
			}
			if _, err := DecodePlan([]byte(injected)); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("DecodePlan accepted droppable content: %v", err)
			}
		})
	}
}

// Editing a value the parser *does* understand is caught by the digest instead.
func TestEditedValueTripsTheDigest(t *testing.T) {
	p := samplePlan(t)
	encoded, err := EncodePlan(p)
	if err != nil {
		t.Fatalf("EncodePlan: %v", err)
	}
	injected := strings.Replace(string(encoded), `"seconds": 30`, `"seconds": 31`, 1)
	if injected == string(encoded) {
		t.Fatal("fixture does not contain the wait duration")
	}
	back, err := DecodePlan([]byte(injected))
	if err != nil {
		t.Fatalf("DecodePlan: %v", err)
	}
	if err := back.VerifyDigest(); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("an edited value did not trip the digest: %v", err)
	}
	if code := ExitCodeFor(back.VerifyDigest()); code != ExitDigestMismatch {
		t.Fatalf("exit code %d, want %d", code, ExitDigestMismatch)
	}
}

func TestEncodePlanRefusesStaleDigest(t *testing.T) {
	p := samplePlan(t)
	p.Nodes[0].RenderedSQL = "changed after sealing"
	if _, err := EncodePlan(p); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("EncodePlan accepted a stale digest: %v", err)
	}
	if err := p.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if _, err := EncodePlan(p); err != nil {
		t.Fatalf("EncodePlan after resealing: %v", err)
	}
}

func mustCanonicalBody(t *testing.T, p *Plan) string {
	t.Helper()
	b, err := p.CanonicalBody()
	if err != nil {
		t.Fatalf("CanonicalBody: %v", err)
	}
	return string(b)
}

// EncodeSealed reseals p and encodes it, for tests that mutate a plan and then
// want a valid file.
func EncodeSealed(t *testing.T, p *Plan) ([]byte, error) {
	t.Helper()
	if err := p.Seal(); err != nil {
		return nil, err
	}
	return EncodePlan(p)
}
