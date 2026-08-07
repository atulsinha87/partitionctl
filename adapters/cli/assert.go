package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/atulsinha/partitionctl/engine/executor"
	"github.com/atulsinha/partitionctl/engine/planner"
	"github.com/atulsinha/partitionctl/engine/protocol"
)

// assertEvaluator evaluates the plan-time preconditions a catalog.assert node
// carries, against the live catalog, at dispatch time.
//
// # Why this lives in the CLI rather than in engine/verifier
//
// engine/verifier owns *end-state* assertions and refuses these on purpose: a
// [protocol.AssertionKind] is a statement about topology, strategy, depth,
// ownership and name availability, which are planner concerns, not "is the
// index healthy?" (verifier package doc). The read surface they need is
// [planner.CatalogReader], not [verifier.Catalog]. So the evaluator is here,
// where both packages are already in scope, and it re-uses the planner's reader
// rather than inventing a third catalog interface.
//
// # Why they are re-evaluated at all
//
// The plan recorded these as assertions rather than as plan-time-only checks so
// that a plan reviewed on Monday and executed on Thursday fails loudly if the
// table was re-partitioned as HASH in between, with the same exit code the
// planner would have used (AC-11, AC-12, AC-26). A false assertion is terminal
// and is never retried: it is false again a second later.
type assertEvaluator struct {
	reader     planner.CatalogReader
	provenance provenanceLookup

	relations map[protocol.ObjectName]planner.Relation
	trees     map[uint32][]planner.TreeEntry
	roles     map[string]map[uint32]planner.RoleMembership

	// acting caches CURRENT_USER for this session.
	acting string
}

// newAssertEvaluator builds an evaluator over a read-only catalog reader.
func newAssertEvaluator(r planner.CatalogReader, prov provenanceLookup) *assertEvaluator {
	return &assertEvaluator{
		reader:     r,
		provenance: prov,
		relations:  make(map[protocol.ObjectName]planner.Relation),
		trees:      make(map[uint32][]planner.TreeEntry),
		roles:      make(map[string]map[uint32]planner.RoleMembership),
	}
}

// Evaluate returns exactly one result per assertion, in order.
//
// A catalog read that fails is an error rather than a false assertion: "the
// database is unreachable" and "your table is HASH partitioned" are different
// operational facts, and only the second should halt a run with exit 15.
func (e *assertEvaluator) Evaluate(ctx context.Context, assertions []protocol.Assertion) ([]executor.CheckResult, error) {
	out := make([]executor.CheckResult, len(assertions))
	for i, a := range assertions {
		res, err := e.one(ctx, a)
		if err != nil {
			return nil, err
		}
		out[i] = res
	}
	return out, nil
}

func (e *assertEvaluator) one(ctx context.Context, a protocol.Assertion) (executor.CheckResult, error) {
	r := executor.CheckResult{Name: string(a.Assertion), FailureCode: a.FailureCode}
	fail := func(format string, args ...any) (executor.CheckResult, error) {
		r.Passed = false
		r.Detail = fmt.Sprintf(format, args...)
		return r, nil
	}
	pass := func(format string, args ...any) (executor.CheckResult, error) {
		r.Passed = true
		r.Detail = fmt.Sprintf(format, args...)
		return r, nil
	}

	switch a.Assertion {
	case protocol.AssertRelationIsPartitioned:
		rel, err := e.relation(ctx, a.Relation)
		if err != nil {
			return e.resolve(r, err)
		}
		if rel.Kind != planner.RelKindPartitionedTable {
			return fail("%s has relkind %q, not 'p'; it is not a partitioned table", rel.Name, rel.Kind)
		}
		return pass("%s is a partitioned table", rel.Name)

	case protocol.AssertPartitionStrategy:
		rel, err := e.relation(ctx, a.Relation)
		if err != nil {
			return e.resolve(r, err)
		}
		strategy, err := e.reader.PartitionStrategy(ctx, rel.OID)
		if err != nil {
			return r, err
		}
		for _, want := range a.Expected {
			if string(strategy) == want {
				return pass("%s is %s partitioned", rel.Name, strategy)
			}
		}
		return fail("%s is %s partitioned; the plan requires one of %v (FR-PLAN-3)",
			rel.Name, strategy, a.Expected)

	case protocol.AssertPartitionDepth:
		rel, err := e.relation(ctx, a.Relation)
		if err != nil {
			return e.resolve(r, err)
		}
		entries, err := e.tree(ctx, rel.OID)
		if err != nil {
			return r, err
		}
		depth := 0
		for _, t := range entries {
			if t.Level > depth {
				depth = t.Level
			}
		}
		want := 1
		if len(a.Expected) > 0 {
			n, convErr := strconv.Atoi(a.Expected[0])
			if convErr != nil {
				return fail("assertion carries a non-numeric expected depth %q", a.Expected[0])
			}
			want = n
		}
		if depth != want {
			return fail("the partition tree rooted at %s is %d level(s) deep; the plan requires exactly %d (FR-PLAN-2)",
				rel.Name, depth, want)
		}
		return pass("the partition tree rooted at %s is %d level deep", rel.Name, depth)

	case protocol.AssertNoDefaultPartition:
		rel, err := e.relation(ctx, a.Relation)
		if err != nil {
			return e.resolve(r, err)
		}
		entries, err := e.tree(ctx, rel.OID)
		if err != nil {
			return r, err
		}
		for _, t := range entries {
			if t.OID == rel.OID {
				continue
			}
			if t.IsDefault || planner.IsDefaultBound(t.PartitionBound) {
				return fail("%s has a DEFAULT partition, %s; v0.1 rejects it (FR-PLAN-3)", rel.Name, t.Name)
			}
		}
		return pass("%s has no DEFAULT partition", rel.Name)

	case protocol.AssertRoleMembership:
		if a.Role == "" {
			return fail("assertion names no role")
		}
		rel, err := e.relation(ctx, a.Relation)
		if err != nil {
			return e.resolve(r, err)
		}
		// The question is about the role that will issue the DDL, which is the
		// connected one, not the one the plan was built under. A plan is
		// routinely reviewed on a workstation as one role and executed in CI as
		// another, and re-checking the planned role would confirm a membership
		// nobody is about to rely on: the run then builds for hours and fails
		// at the first partition the connected role cannot touch, which is
		// precisely what TRD §11.2 says this check exists to prevent.
		acting, err := e.actingRole(ctx)
		if err != nil {
			return r, err
		}
		memberships, err := e.membership(ctx, acting, rel.OwnerOID)
		if err != nil {
			return r, err
		}
		m, known := memberships[rel.OwnerOID]
		if !known {
			return fail("the owning role of %s (oid %d) does not exist in pg_roles", rel.Name, rel.OwnerOID)
		}
		if !m.IsMember {
			if acting != a.Role {
				return fail("the connected role %q does not have the privileges of %q, which owns %s. "+
					"The plan was built as %q, which does; executing a reviewed plan as a different "+
					"role is what this check exists to catch (FR-PLAN-10, AC-12)",
					acting, m.OwnerName, rel.Name, a.Role)
			}
			return fail("role %q does not have the privileges of %q, which owns %s (FR-PLAN-10, AC-12)",
				acting, m.OwnerName, rel.Name)
		}
		if acting != a.Role {
			return pass("the connected role %q has the privileges of %q, which owns %s "+
				"(the plan was built as %q)", acting, m.OwnerName, rel.Name, a.Role)
		}
		return pass("role %q has the privileges of %q, which owns %s", acting, m.OwnerName, rel.Name)

	case protocol.AssertIndexNameAvailable:
		if a.Index == nil {
			return fail("assertion names no index")
		}
		idx, err := e.reader.LookupIndex(ctx, *a.Index)
		if err != nil {
			if errors.Is(err, planner.ErrIndexNotFound) {
				return pass("%s is free", a.Index)
			}
			return r, err
		}
		if idx.Condition().Usable() {
			// A usable index under the target name is the converged case: the
			// build already happened. It is not a name clash.
			return pass("%s already exists and is valid", idx.Name)
		}
		has, err := e.provenance.HasProvenance(ctx, idx.Name)
		if err != nil {
			return r, err
		}
		if !has {
			return fail("%s already exists and is %s, and PartitionCTL has no provenance record proving it "+
				"created it; an in-progress build belonging to something else is never adopted "+
				"(FR-PLAN-7, AC-6, NFR-REL-3)", idx.Name, idx.Condition())
		}
		return pass("%s is an in-progress PartitionCTL build with provenance", idx.Name)

	case protocol.AssertIndexExists:
		if a.Index == nil {
			return fail("assertion names no index")
		}
		idx, err := e.reader.LookupIndex(ctx, *a.Index)
		if err != nil {
			if errors.Is(err, planner.ErrIndexNotFound) {
				return fail("%s does not exist (FR-DROP-1)", a.Index)
			}
			return r, err
		}
		return pass("%s exists", idx.Name)

	case protocol.AssertIndexIsPartitioned:
		if a.Index == nil {
			return fail("assertion names no index")
		}
		idx, err := e.reader.LookupIndex(ctx, *a.Index)
		if err != nil {
			if errors.Is(err, planner.ErrIndexNotFound) {
				return fail("%s does not exist (FR-DROP-1)", a.Index)
			}
			return r, err
		}
		if idx.Kind != planner.RelKindPartitionedIndex {
			return fail("%s has relkind %q, not 'I'; it is not a partitioned index (FR-DROP-1)", idx.Name, idx.Kind)
		}
		if a.Relation != nil && idx.Table != *a.Relation {
			return fail("%s is on %s, not on %s (FR-DROP-1)", idx.Name, idx.Table, a.Relation)
		}
		return pass("%s is a partitioned index on %s", idx.Name, idx.Table)

	case protocol.AssertIndexNotConstraintBacked:
		if a.Index == nil {
			return fail("assertion names no index")
		}
		idx, err := e.reader.LookupIndex(ctx, *a.Index)
		if err != nil {
			if errors.Is(err, planner.ErrIndexNotFound) {
				return fail("%s does not exist", a.Index)
			}
			return r, err
		}
		if idx.ConstraintBacked() {
			return fail("%s backs constraint %q (contype %q); drop it with "+
				"ALTER TABLE %s DROP CONSTRAINT %s instead (FR-DROP-2, AC-14)",
				idx.Name, idx.ConstraintName, idx.ConstraintType,
				idx.Table.Quoted(), protocol.QuoteIdentifier(idx.ConstraintName))
		}
		return pass("%s backs no UNIQUE, PRIMARY KEY or EXCLUDE constraint", idx.Name)

	case protocol.AssertLeavesAttached:
		if a.Index == nil {
			return fail("assertion names no index")
		}
		parent, err := e.reader.LookupIndex(ctx, *a.Index)
		if err != nil {
			if errors.Is(err, planner.ErrIndexNotFound) {
				return fail("%s does not exist", a.Index)
			}
			return r, err
		}
		rel, err := e.relation(ctx, a.Relation)
		if err != nil {
			return e.resolve(r, err)
		}
		entries, err := e.tree(ctx, rel.OID)
		if err != nil {
			return r, err
		}
		oids := make([]uint32, 0, len(entries))
		leaves := 0
		for _, t := range entries {
			oids = append(oids, t.OID)
			if t.IsLeaf {
				leaves++
			}
		}
		idxs, err := e.reader.IndexesOnRelations(ctx, oids)
		if err != nil {
			return r, err
		}
		attached := 0
		for _, i := range idxs {
			if i.AttachedTo(parent.OID) {
				attached++
			}
		}
		if attached != leaves {
			return fail("%s has %d attached leaf index(es) but %s has %d leaf partition(s)",
				parent.Name, attached, rel.Name, leaves)
		}
		return pass("every one of %s's %d leaf partitions carries an index attached to %s",
			rel.Name, leaves, parent.Name)
	}

	return fail("unknown assertion kind %q", a.Assertion)
}

// resolve turns a relation-lookup failure into either a false assertion or a
// read error. A relation that is not there is a verdict; anything else is not.
func (e *assertEvaluator) resolve(r executor.CheckResult, err error) (executor.CheckResult, error) {
	if errors.Is(err, planner.ErrRelationNotFound) {
		r.Passed = false
		r.Detail = err.Error()
		return r, nil
	}
	return r, err
}

func (e *assertEvaluator) relation(ctx context.Context, name *protocol.ObjectName) (planner.Relation, error) {
	if name == nil {
		return planner.Relation{}, planner.ErrRelationNotFound.Detailf("assertion names no relation")
	}
	if rel, ok := e.relations[*name]; ok {
		return rel, nil
	}
	rel, err := e.reader.LookupRelation(ctx, *name)
	if err != nil {
		return planner.Relation{}, err
	}
	e.relations[*name] = rel
	return rel, nil
}

func (e *assertEvaluator) tree(ctx context.Context, rootOID uint32) ([]planner.TreeEntry, error) {
	if t, ok := e.trees[rootOID]; ok {
		return t, nil
	}
	t, err := e.reader.PartitionTree(ctx, rootOID)
	if err != nil {
		return nil, err
	}
	e.trees[rootOID] = t
	return t, nil
}

// actingRole is the role this session is connected as, read once and cached.
//
// It is the role that will actually issue the DDL, which is the only role whose
// privileges decide whether the run can finish.
func (e *assertEvaluator) actingRole(ctx context.Context) (string, error) {
	if e.acting != "" {
		return e.acting, nil
	}
	role, err := e.reader.CurrentRole(ctx)
	if err != nil {
		return "", err
	}
	e.acting = role
	return role, nil
}

func (e *assertEvaluator) membership(ctx context.Context, role string, ownerOID uint32) (map[uint32]planner.RoleMembership, error) {
	cached, ok := e.roles[role]
	if ok {
		if _, have := cached[ownerOID]; have {
			return cached, nil
		}
	}
	fresh, err := e.reader.RoleMemberships(ctx, role, []uint32{ownerOID})
	if err != nil {
		return nil, err
	}
	if cached == nil {
		cached = make(map[uint32]planner.RoleMembership, len(fresh))
		e.roles[role] = cached
	}
	for k, v := range fresh {
		cached[k] = v
	}
	return cached, nil
}
