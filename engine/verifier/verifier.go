package verifier

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// maxNamedLeftovers caps how many leftover index names one failure reason
// lists. A tree that has accumulated hundreds is a story the operator needs to
// read, not scroll past.
const maxNamedLeftovers = 8

// Verifier evaluates catalog assertions. It is stateless apart from its
// [Catalog], so one value is safe for concurrent use if the Catalog is.
type Verifier struct {
	cat Catalog
}

// New returns a Verifier reading through cat. cat must not be nil; a nil
// Catalog produces [StatusError] results rather than a panic, so a
// misconfigured CLI reports a cause instead of crashing.
func New(cat Catalog) *Verifier { return &Verifier{cat: cat} }

// Check evaluates one assertion and reports it. It issues no DDL (FR-VER-5).
//
// A false assertion is [StatusFail]. A malformed check, a nil catalog, a
// cancelled context or a failed catalog read is [StatusError]: those are not
// verdicts about the database.
func (v *Verifier) Check(ctx context.Context, c protocol.VerifyCheck) Result {
	res := Result{
		Check:       c.Check,
		Index:       cloneName(c.Index),
		ParentIndex: cloneName(c.ParentIndex),
		Relation:    cloneName(c.Relation),
		Message:     c.Message,
	}
	if v == nil || v.cat == nil {
		return res.errorf(protocol.ErrFailure.Detailf("verifier has no catalog"),
			"verifier has no catalog to read")
	}
	// The check's own shape is validated before anything is read, so a plan that
	// names a check without its required arguments reports that, rather than
	// reporting a confusing assertion failure derived from a nil name.
	if err := c.Validate(); err != nil {
		return res.errorf(protocol.ErrInvalidPlan.Detailf("%v", err),
			"check is malformed: %v", err)
	}
	if err := ctx.Err(); err != nil {
		return res.errorf(err, "context ended before the check ran: %v", err)
	}

	switch c.Check {
	case protocol.CheckIndexValid:
		return v.checkIndexValid(ctx, res, *c.Index)
	case protocol.CheckIndexAttached:
		return v.checkIndexAttached(ctx, res, *c.Index, *c.ParentIndex)
	case protocol.CheckParentIndexValid:
		return v.checkParentIndexValid(ctx, res, *c.ParentIndex)
	case protocol.CheckLeafIndexCount:
		return v.checkLeafIndexCount(ctx, res, *c.ParentIndex, *c.ExpectedCount)
	case protocol.CheckIndexAbsent:
		return v.checkIndexAbsent(ctx, res, *c.Index)
	case protocol.CheckNoLeftoverIndexes:
		return v.checkNoLeftoverIndexes(ctx, res, *c.Relation)
	}
	// Unreachable while VerifyCheck.Validate rejects unknown kinds, but the
	// vocabulary is a versioned contract and this package must not silently pass
	// a check it does not understand.
	return res.errorf(protocol.ErrInvalidPlan.Detailf("unknown check %q", c.Check),
		"check %q is not one this verifier understands", c.Check)
}

// Verify evaluates every check in params, in order, reporting each one
// (FR-CLI-14). All checks run even after one fails, because the operator wants
// the whole picture, not the first symptom.
func (v *Verifier) Verify(ctx context.Context, params *protocol.VerifyParams) Report {
	var r Report
	if params == nil {
		r.Add(Result{
			Check:  "",
			Status: StatusError,
			Reason: "no verify parameters supplied",
			err:    protocol.ErrInvalidPlan.Detailf("nil verify params"),
		})
		return r
	}
	for _, c := range params.Checks {
		r.Add(v.Check(ctx, c))
	}
	return r
}

// VerifyNode evaluates one index.verify node. It returns an error only when the
// node is not an index.verify node or its params are the wrong type; a failed
// assertion is in the Report, reachable with [Report.Err].
func (v *Verifier) VerifyNode(ctx context.Context, n *protocol.Node) (Report, error) {
	if n == nil {
		return Report{}, protocol.ErrInvalidPlan.Detailf("nil node")
	}
	if n.Kind != protocol.KindIndexVerify {
		return Report{}, protocol.ErrInvalidPlan.Detailf(
			"node %q has kind %q; the verifier evaluates %q nodes", n.ID, n.Kind, protocol.KindIndexVerify)
	}
	params, ok := n.Params.(*protocol.VerifyParams)
	if !ok {
		return Report{}, protocol.ErrInvalidPlan.Detailf(
			"node %q has kind %q but params of another type", n.ID, n.Kind)
	}
	r := v.Verify(ctx, params)
	for i := range r.Results {
		r.Results[i].NodeID = n.ID
	}
	return r, nil
}

// VerifyPlan evaluates every index.verify node in a plan and reports each check
// individually (FR-VER-5, FR-CLI-14). It issues no DDL and mutates nothing.
//
// Nodes are visited in [protocol.Plan.TopologicalOrder], the order the executor
// would have run them in, so a `verify` transcript reads in the same sequence as
// the run it is checking. A plan with no index.verify nodes yields an empty,
// vacuously passing report; see [Report.Passed].
//
// catalog.assert nodes are deliberately skipped: their predicates are plan-time
// preconditions about topology and privilege, not end-state assertions.
func (v *Verifier) VerifyPlan(ctx context.Context, p *protocol.Plan) (Report, error) {
	if p == nil {
		return Report{}, protocol.ErrInvalidPlan.Detailf("nil plan")
	}
	order, err := p.TopologicalOrder()
	if err != nil {
		return Report{}, err
	}
	var r Report
	for _, id := range order {
		n, ok := p.NodeByID(id)
		if !ok || n.Kind != protocol.KindIndexVerify {
			continue
		}
		sub, err := v.VerifyNode(ctx, n)
		if err != nil {
			return Report{}, err
		}
		r.Results = append(r.Results, sub.Results...)
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// The assertions
// ---------------------------------------------------------------------------

// checkIndexValid is FR-VER-1: indisvalid AND indisready AND indislive.
func (v *Verifier) checkIndexValid(ctx context.Context, res Result, index protocol.ObjectName) Result {
	st, found, err := v.cat.Index(ctx, index)
	if err != nil {
		return res.errorf(err, "could not read index %q: %v", index.String(), err)
	}
	res.Expected = usableFlags
	if !found {
		res.Actual = "absent"
		return res.failf("index %q does not exist", index.String())
	}
	res.Actual = st.Flags()
	if !st.Usable() {
		return res.failf("index %q is not usable: %s", index.String(), st.Flags())
	}
	return res.passf("index %q is valid, ready and live", index.String())
}

// checkIndexAttached is FR-VER-2: the parent-child index relationship exists in
// pg_inherits.
func (v *Verifier) checkIndexAttached(ctx context.Context, res Result, child, parent protocol.ObjectName) Result {
	got, attached, err := v.cat.IndexParent(ctx, child)
	if err != nil {
		return res.errorf(err, "could not read pg_inherits for index %q: %v", child.String(), err)
	}
	res.Expected = "attached to " + parent.String()
	if !attached {
		// Distinguish "exists but unattached" from "does not exist". Both fail,
		// but they call for different repairs: the first needs an attach, the
		// second needs the whole build. This costs one extra read, and only on
		// the failure path.
		if _, found, ferr := v.cat.Index(ctx, child); ferr == nil && !found {
			res.Actual = "absent"
			return res.failf("index %q does not exist, so it is not attached to %q",
				child.String(), parent.String())
		}
		res.Actual = "unattached"
		return res.failf("index %q is not attached to any partitioned index in pg_inherits; expected %q",
			child.String(), parent.String())
	}
	res.Actual = "attached to " + got.String()
	if !sameObject(parent, got) {
		return res.failf("index %q is attached to %q, not to %q",
			child.String(), got.String(), parent.String())
	}
	return res.passf("index %q is attached to %q in pg_inherits", child.String(), parent.String())
}

// checkParentIndexValid is FR-VER-3: the parent index is indisvalid after the
// final attach. PostgreSQL sets it automatically when the last child attaches;
// no statement does it, which is why this assertion is the only proof the
// choreography completed.
//
// It asserts indisvalid alone, as FR-VER-3 states. indisready and indislive are
// reported in Actual for diagnosis but do not decide the verdict.
func (v *Verifier) checkParentIndexValid(ctx context.Context, res Result, parent protocol.ObjectName) Result {
	st, found, err := v.cat.Index(ctx, parent)
	if err != nil {
		return res.errorf(err, "could not read parent index %q: %v", parent.String(), err)
	}
	res.Expected = "indisvalid=true"
	if !found {
		res.Actual = "absent"
		return res.failf("parent index %q does not exist", parent.String())
	}
	res.Actual = st.Flags()
	if !st.Valid {
		return res.failf(
			"parent index %q is not valid (%s); PostgreSQL marks it valid automatically when the final child index attaches",
			parent.String(), st.Flags())
	}
	return res.passf("parent index %q is valid", parent.String())
}

// checkLeafIndexCount is FR-VER-4: the leaf index count equals the discovered
// leaf partition count.
//
// It counts children in pg_inherits, not indexes that merely exist on the
// leaves. An index built on a leaf but never attached contributes nothing to the
// parent's validity, so counting it would report a half-finished build as
// complete.
func (v *Verifier) checkLeafIndexCount(ctx context.Context, res Result, parent protocol.ObjectName, want int) Result {
	res.Expected = fmt.Sprintf("%d attached leaf indexes", want)
	// Existence is probed first so that a missing parent index reports itself
	// rather than surfacing as "0 attached, expected N", which reads like a
	// stalled build instead of an absent one.
	if _, found, err := v.cat.Index(ctx, parent); err != nil {
		return res.errorf(err, "could not read parent index %q: %v", parent.String(), err)
	} else if !found {
		res.Actual = "parent index absent"
		return res.failf("parent index %q does not exist, so it has no attached leaf indexes (expected %d)",
			parent.String(), want)
	}
	children, err := v.cat.AttachedIndexes(ctx, parent)
	if err != nil {
		return res.errorf(err, "could not read pg_inherits for parent index %q: %v", parent.String(), err)
	}
	got := len(children)
	res.Actual = fmt.Sprintf("%d attached leaf indexes", got)
	if got != want {
		return res.failf("parent index %q has %d attached leaf indexes, expected %d",
			parent.String(), got, want)
	}
	return res.passf("parent index %q has %d attached leaf indexes, matching the expected %d",
		parent.String(), got, want)
}

// checkIndexAbsent is FR-DROP-7: the index is gone from pg_index.
func (v *Verifier) checkIndexAbsent(ctx context.Context, res Result, index protocol.ObjectName) Result {
	st, found, err := v.cat.Index(ctx, index)
	if err != nil {
		return res.errorf(err, "could not read index %q: %v", index.String(), err)
	}
	res.Expected = "absent"
	if found {
		res.Actual = "present, " + st.Flags()
		return res.failf("index %q is still present in pg_index", index.String())
	}
	res.Actual = "absent"
	return res.passf("index %q is absent from pg_index", index.String())
}

// checkNoLeftoverIndexes is the catalog half of FR-REIDX-6: no _ccnew/_ccold
// index remains anywhere on the tree.
//
// Classification is [protocol.ClassifyLeftover], so the disambiguating integer
// PostgreSQL appends (_ccnew1, _ccold2) is matched as a pattern rather than as a
// literal suffix. Reporting a leftover is never authorization to drop it:
// AuthLeftover additionally requires recorded reindex-run history (FR-AUTH-3,
// INV-7, AC-19), which this package does not read.
func (v *Verifier) checkNoLeftoverIndexes(ctx context.Context, res Result, relation protocol.ObjectName) Result {
	indexes, err := v.cat.TreeIndexes(ctx, relation)
	if err != nil {
		return res.errorf(err, "could not read the indexes of %q and its partitions: %v", relation.String(), err)
	}
	type leftover struct {
		name string
		kind protocol.LeftoverKind
	}
	var found []leftover
	for _, ix := range indexes {
		if kind, _ := protocol.ClassifyLeftover(ix.Name.Name); kind != protocol.LeftoverNone {
			found = append(found, leftover{name: ix.Name.String(), kind: kind})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].name < found[j].name })

	res.Expected = "no _ccnew/_ccold indexes"
	res.Actual = fmt.Sprintf("%d leftover indexes", len(found))
	if len(found) == 0 {
		return res.passf("relation %q and its partitions carry no _ccnew/_ccold leftover indexes", relation.String())
	}
	named := found
	suffix := ""
	if len(named) > maxNamedLeftovers {
		named = named[:maxNamedLeftovers]
		suffix = fmt.Sprintf(", and %d more", len(found)-maxNamedLeftovers)
	}
	parts := make([]string, 0, len(named))
	for _, l := range named {
		parts = append(parts, fmt.Sprintf("%q (%s)", l.name, l.kind))
	}
	return res.failf("relation %q and its partitions carry %d REINDEX CONCURRENTLY leftover indexes: %s%s",
		relation.String(), len(found), strings.Join(parts, ", "), suffix)
}

// ---------------------------------------------------------------------------
// Result builders
// ---------------------------------------------------------------------------

func (r Result) passf(format string, args ...any) Result {
	r.Status = StatusPass
	r.Reason = fmt.Sprintf(format, args...)
	return r
}

func (r Result) failf(format string, args ...any) Result {
	r.Status = StatusFail
	r.Reason = fmt.Sprintf(format, args...)
	return r
}

func (r Result) errorf(cause error, format string, args ...any) Result {
	r.Status = StatusError
	r.Reason = fmt.Sprintf(format, args...)
	r.err = cause
	return r
}
