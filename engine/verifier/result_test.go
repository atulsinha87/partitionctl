package verifier

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

func TestStatusValid(t *testing.T) {
	for _, s := range []Status{StatusPass, StatusFail, StatusError} {
		if !s.Valid() {
			t.Fatalf("%q should be valid", s)
		}
		if s.String() != string(s) {
			t.Fatalf("String() = %q, want %q", s.String(), string(s))
		}
	}
	for _, s := range []Status{"", "PASS", "ok", "failed"} {
		if Status(s).Valid() {
			t.Fatalf("%q should not be valid", s)
		}
	}
}

func TestReportCounts(t *testing.T) {
	var r Report
	if c := r.Counts(); c != (Counts{}) {
		t.Fatalf("empty report counts = %+v", c)
	}
	if !r.Passed() {
		t.Fatal("an empty report should pass vacuously")
	}

	r.Add(Result{Check: protocol.CheckIndexValid, Status: StatusPass})
	r.Add(Result{Check: protocol.CheckIndexAttached, Status: StatusFail, Reason: "not attached"})
	r.Add(Result{Check: protocol.CheckLeafIndexCount, Status: StatusPass})
	r.Add(Result{Check: protocol.CheckParentIndexValid, Status: StatusError, Reason: "unreachable"})

	want := Counts{Total: 4, Passed: 2, Failed: 1, Errored: 1}
	if got := r.Counts(); got != want {
		t.Fatalf("counts = %+v, want %+v", got, want)
	}
	if r.Passed() {
		t.Fatal("report with a failure should not pass")
	}
	if got := len(r.Failures()); got != 2 {
		t.Fatalf("Failures() returned %d, want 2", got)
	}
	if !strings.Contains(r.Summary(), "4 checks: 2 passed, 1 failed, 1 errored") {
		t.Fatalf("Summary() = %q", r.Summary())
	}
}

// TestReportErrPrefersTheUnevaluatableCheck is the distinction that keeps an
// unreachable database from being reported as a broken index. A failure
// alongside an error is not a trustworthy verdict, so the cause wins.
func TestReportErrPrefersTheUnevaluatableCheck(t *testing.T) {
	cause := errors.New("connection refused")
	var r Report
	r.Add(Result{Check: protocol.CheckIndexValid, Status: StatusFail, Reason: "invalid"})
	r.Add(Result{Check: protocol.CheckIndexAttached, Status: StatusError, Reason: "unreadable", err: cause})

	err := r.Err()
	if !errors.Is(err, cause) {
		t.Fatalf("Err() = %v, want it to wrap %v", err, cause)
	}
	if errors.Is(err, protocol.ErrVerificationFailed) {
		t.Fatal("an unevaluatable check must not be reported as verification failure")
	}
	if code := protocol.ExitCodeFor(err); code != protocol.ExitFailure {
		t.Fatalf("exit code = %d, want %d", code, protocol.ExitFailure)
	}
}

func TestReportErrOnAnErrorWithNoCause(t *testing.T) {
	var r Report
	r.Add(Result{Check: protocol.CheckIndexValid, Status: StatusError, Reason: "no catalog"})
	err := r.Err()
	if !errors.Is(err, protocol.ErrFailure) {
		t.Fatalf("Err() = %v, want ErrFailure", err)
	}
	if !strings.Contains(err.Error(), "no catalog") {
		t.Fatalf("Err() = %q, want it to carry the reason", err)
	}
}

// TestReportErrMapsFailureToExit14 is the FR-CLI-13 / AC-26 binding for
// verification failure.
func TestReportErrMapsFailureToExit14(t *testing.T) {
	var r Report
	r.Add(Result{Check: protocol.CheckIndexValid, Status: StatusPass})
	r.Add(Result{
		Check:   protocol.CheckIndexAttached,
		Status:  StatusFail,
		Reason:  `index "public.a" is not attached`,
		Message: "run partitionctl resume migration.plan",
	})

	err := r.Err()
	if !errors.Is(err, protocol.ErrVerificationFailed) {
		t.Fatalf("Err() = %v, want ErrVerificationFailed", err)
	}
	if code := protocol.ExitCodeFor(err); code != protocol.ExitVerificationFailed {
		t.Fatalf("exit code = %d, want 14", code)
	}
	// FR-LB-6: the message must name what to do about it.
	if !strings.Contains(err.Error(), "partitionctl resume migration.plan") {
		t.Fatalf("Err() = %q, want it to carry the check's operator message", err)
	}
	if !strings.Contains(err.Error(), "1 of 2 checks failed") {
		t.Fatalf("Err() = %q, want it to state the tally", err)
	}
}

func TestReportErrNilWhenEverythingPasses(t *testing.T) {
	var r Report
	r.Add(Result{Check: protocol.CheckIndexValid, Status: StatusPass})
	if err := r.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
}

// TestReportJSONSchema pins the field schema `verify --json` emits (FR-CLI-15,
// NFR-OBS-2). A consumer branches on these names.
func TestReportJSONSchema(t *testing.T) {
	index := protocol.NewObjectName("public", "orders_created_at_idx_orders_2026_01")
	parent := protocol.NewObjectName("public", "orders_created_at_idx")
	var r Report
	r.Add(Result{
		Check:       protocol.CheckIndexAttached,
		Status:      StatusFail,
		Reason:      "not attached",
		NodeID:      "verify-leaf-1",
		Index:       &index,
		ParentIndex: &parent,
		Expected:    "attached to public.orders_created_at_idx",
		Actual:      "unattached",
		Message:     "resume the run",
		err:         errors.New("must not be serialized"),
	})

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, key := range []string{"passed", "total", "failed", "errored", "results"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("report JSON is missing %q: %s", key, b)
		}
	}
	if got["passed"] != false || got["total"] != float64(1) || got["failed"] != float64(1) {
		t.Fatalf("tallies wrong: %s", b)
	}
	res := got["results"].([]any)[0].(map[string]any)
	for _, key := range []string{"check", "status", "reason", "node_id", "index", "parent_index", "expected", "actual", "message"} {
		if _, ok := res[key]; !ok {
			t.Fatalf("result JSON is missing %q: %s", key, b)
		}
	}
	if strings.Contains(string(b), "must not be serialized") {
		t.Fatalf("the internal cause leaked into JSON: %s", b)
	}
}

func TestReportJSONRoundTrip(t *testing.T) {
	var r Report
	r.Add(Result{Check: protocol.CheckIndexValid, Status: StatusPass, Reason: "ok"})
	r.Add(Result{Check: protocol.CheckIndexAbsent, Status: StatusFail, Reason: "still there"})

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Report
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(back.Results) != 2 || back.Counts() != r.Counts() {
		t.Fatalf("round trip lost information: %+v", back)
	}

	// The tallies are recomputed on the way in, so a document whose summary
	// disagrees with its results cannot make a failing report look passing.
	doctored := `{"passed":true,"total":0,"failed":0,"errored":0,` +
		`"results":[{"check":"index_valid","status":"fail","reason":"broken"}]}`
	var forged Report
	if err := json.Unmarshal([]byte(doctored), &forged); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if forged.Passed() {
		t.Fatal("a doctored summary was believed over the results")
	}
}

func TestReportMarshalsAnEmptyResultsArray(t *testing.T) {
	b, err := json.Marshal(Report{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"results":[]`) {
		t.Fatalf("empty report should emit an array, not null: %s", b)
	}
}

func TestIndexStateFlags(t *testing.T) {
	st := IndexState{Valid: true, Ready: false, Live: true}
	if got := st.Flags(); got != "indisvalid=true indisready=false indislive=true" {
		t.Fatalf("Flags() = %q", got)
	}
	if st.Usable() {
		t.Fatal("an index with indisready=false is not usable")
	}
	if !(IndexState{Valid: true, Ready: true, Live: true}).Usable() {
		t.Fatal("all three true should be usable")
	}
	if usableFlags != (IndexState{Valid: true, Ready: true, Live: true}).Flags() {
		t.Fatalf("usableFlags %q does not match Flags()", usableFlags)
	}
}

func TestSameObject(t *testing.T) {
	tests := []struct {
		want, got protocol.ObjectName
		same      bool
	}{
		{protocol.NewObjectName("public", "a"), protocol.NewObjectName("public", "a"), true},
		{protocol.NewObjectName("public", "a"), protocol.NewObjectName("app", "a"), false},
		{protocol.NewObjectName("", "a"), protocol.NewObjectName("public", "a"), true},
		{protocol.NewObjectName("public", "a"), protocol.NewObjectName("", "a"), true},
		{protocol.NewObjectName("public", "a"), protocol.NewObjectName("public", "b"), false},
	}
	for _, tc := range tests {
		if got := sameObject(tc.want, tc.got); got != tc.same {
			t.Fatalf("sameObject(%v, %v) = %t, want %t", tc.want, tc.got, got, tc.same)
		}
	}
}

func TestResultPassedAndErr(t *testing.T) {
	pass := Result{Check: protocol.CheckIndexValid, Status: StatusPass}
	if !pass.Passed() || pass.Err() != nil {
		t.Fatalf("pass result = %+v", pass)
	}
	cause := errors.New("unreadable")
	bad := Result{Check: protocol.CheckIndexValid, Status: StatusError, err: cause}
	if bad.Passed() {
		t.Fatal("an error result must not report as passed")
	}
	if !errors.Is(bad.Err(), cause) {
		t.Fatalf("Err() = %v, want %v", bad.Err(), cause)
	}
	if (Result{Status: StatusFail}).Passed() {
		t.Fatal("a failed result must not report as passed")
	}
}

func TestReportUnmarshalRejectsMalformedJSON(t *testing.T) {
	var r Report
	if err := json.Unmarshal([]byte(`{"results": "not an array"}`), &r); err == nil {
		t.Fatal("malformed report JSON was accepted")
	}
}
