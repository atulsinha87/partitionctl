package verifier

import (
	"encoding/json"
	"fmt"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// Status is the outcome of one assertion.
//
// There are three, not two. A false assertion and an unevaluatable assertion are
// different operational facts with different exit codes, and an operator reading
// `verify --json` needs to tell them apart: "your index is broken" and "I could
// not reach your database" call for different actions.
type Status string

// The three outcomes.
const (
	// StatusPass means the catalog was read and the assertion holds.
	StatusPass Status = "pass"
	// StatusFail means the catalog was read and the assertion is false. This is
	// the verification failure of FR-VER-1…4, exit 14.
	StatusFail Status = "fail"
	// StatusError means the assertion could not be evaluated: the catalog read
	// failed, or the check itself was malformed. It is not a verification
	// verdict.
	StatusError Status = "error"
)

// Valid reports whether s is one of the three outcomes.
func (s Status) Valid() bool {
	switch s {
	case StatusPass, StatusFail, StatusError:
		return true
	}
	return false
}

func (s Status) String() string { return string(s) }

// Result is one assertion's outcome, individually reportable (FR-CLI-14).
//
// Reason is always populated, on pass as well as on fail, because the pass text
// is what makes a `verify` transcript readable and what a reviewer checks
// against the plan.
type Result struct {
	// Check names the assertion (FR-VER-1…4, FR-DROP-7, FR-REIDX-6).
	Check protocol.VerifyCheckKind `json:"check"`
	// Status is the outcome.
	Status Status `json:"status"`
	// Reason is the operator-facing explanation, always set.
	Reason string `json:"reason"`
	// NodeID is the index.verify node this check came from, when the check was
	// read out of a plan.
	NodeID protocol.NodeID `json:"node_id,omitempty"`
	// Index is the index under test, where the check has one.
	Index *protocol.ObjectName `json:"index,omitempty"`
	// ParentIndex is the partitioned index, where the check has one.
	ParentIndex *protocol.ObjectName `json:"parent_index,omitempty"`
	// Relation is the table, where the check has one.
	Relation *protocol.ObjectName `json:"relation,omitempty"`
	// Expected is the asserted state, in the same vocabulary as Actual.
	Expected string `json:"expected,omitempty"`
	// Actual is the observed state.
	Actual string `json:"actual,omitempty"`
	// Message is the planner-supplied operator message from the check, carried
	// through untouched.
	Message string `json:"message,omitempty"`

	// err is the cause behind a StatusError result. It is not serialized: JSON
	// output carries Reason, which is the same information in a form a log
	// pipeline can consume.
	err error
}

// Passed reports whether the assertion holds.
func (r Result) Passed() bool { return r.Status == StatusPass }

// Err returns the cause behind a [StatusError] result, or nil.
func (r Result) Err() error { return r.err }

// Counts summarizes a [Report].
type Counts struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Errored int `json:"errored"`
}

// Report is an ordered set of results. Order is the order the checks were given,
// which makes two runs over the same plan produce byte-identical output.
type Report struct {
	// Results are the individual outcomes, in evaluation order.
	Results []Result `json:"results"`
}

// Add appends a result.
func (r *Report) Add(res Result) { r.Results = append(r.Results, res) }

// Counts tallies the results.
func (r Report) Counts() Counts {
	c := Counts{Total: len(r.Results)}
	for _, res := range r.Results {
		switch res.Status {
		case StatusPass:
			c.Passed++
		case StatusFail:
			c.Failed++
		default:
			c.Errored++
		}
	}
	return c
}

// Passed reports whether every result passed.
//
// An empty report passes vacuously. That is the honest answer — nothing was
// asserted, so nothing is false — but it is a trap for a caller that expected
// checks to exist, so consult Counts().Total before treating a pass as
// meaningful.
func (r Report) Passed() bool {
	for _, res := range r.Results {
		if res.Status != StatusPass {
			return false
		}
	}
	return true
}

// Failures returns the results that did not pass, errors included, in order.
func (r Report) Failures() []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Status != StatusPass {
			out = append(out, res)
		}
	}
	return out
}

// Summary is a one-line tally for CLI output and for a Liquibase gate's failure
// message (FR-LB-6).
func (r Report) Summary() string {
	c := r.Counts()
	return fmt.Sprintf("%d checks: %d passed, %d failed, %d errored", c.Total, c.Passed, c.Failed, c.Errored)
}

// Err maps the report to the error the caller should return, and therefore to a
// process exit code (FR-CLI-13, AC-26).
//
// An unevaluatable check wins over a false one: if the catalog could not be read
// then the failures alongside it are not trustworthy verdicts, so the underlying
// cause is returned with its own exit code rather than being reported as
// verification failure. Otherwise a false assertion yields
// [protocol.ErrVerificationFailed], exit 14. A fully passing report returns nil.
func (r Report) Err() error {
	for _, res := range r.Results {
		if res.Status != StatusError {
			continue
		}
		if res.err != nil {
			return fmt.Errorf("check %s could not be evaluated: %w", res.Check, res.err)
		}
		return protocol.ErrFailure.Detailf("check %s could not be evaluated: %s", res.Check, res.Reason)
	}
	c := r.Counts()
	if c.Failed == 0 {
		return nil
	}
	first := r.Failures()[0]
	detail := first.Reason
	if first.Message != "" {
		detail += ": " + first.Message
	}
	return protocol.ErrVerificationFailed.Detailf("%d of %d checks failed; first: %s", c.Failed, c.Total, detail)
}

// reportJSON is Report's wire shape. The tallies are computed on the way out so
// that `verify --json` and a log consumer never have to derive them, and are
// ignored on the way in so the two directions cannot disagree (FR-CLI-15).
type reportJSON struct {
	Passed  bool     `json:"passed"`
	Total   int      `json:"total"`
	Failed  int      `json:"failed"`
	Errored int      `json:"errored"`
	Results []Result `json:"results"`
}

// MarshalJSON emits the stable field schema for `verify --json`.
func (r Report) MarshalJSON() ([]byte, error) {
	c := r.Counts()
	out := reportJSON{
		Passed:  r.Passed(),
		Total:   c.Total,
		Failed:  c.Failed,
		Errored: c.Errored,
		Results: r.Results,
	}
	if out.Results == nil {
		out.Results = []Result{}
	}
	return json.Marshal(out)
}

// UnmarshalJSON reads a report back, recomputing the tallies from the results
// rather than trusting the ones in the document.
func (r *Report) UnmarshalJSON(data []byte) error {
	var in reportJSON
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	r.Results = in.Results
	return nil
}
