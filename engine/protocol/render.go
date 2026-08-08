package protocol

import (
	"sort"
	"strings"
)

// Render produces the statement a node issues, from the node's structured
// parameters alone.
//
// It lives in engine/protocol because there is exactly one renderer and three
// kinds of caller: the executor, which sends what this returns; the operation
// planners, which write [Preview] of it into [Node.RenderedSQL]; and `render`,
// which prints it as a runbook. A second implementation anywhere would be a
// second answer to "what will this run do", which is the one question the
// reviewed artifact exists to answer.
//
// [Node.RenderedSQL] is never read here: it is a human preview in the reviewed
// artifact, and re-rendering is what keeps it off the injection surface
// (FR-PLANFILE-7, T2). Every identifier goes through [QuoteIdentifier]; the two
// operator-authored SQL fragments, [IndexColumn.Expression] and
// [IndexDefinition.Where], are emitted verbatim by design and are covered by
// plan-file review.
//
// It returns an empty string and a nil error for the three kinds that issue no
// DDL. Callers should gate on [NodeKind.IssuesDDL].
func Render(n *Node) (string, error) {
	if n == nil {
		return "", ErrInvalidPlan.Detailf("cannot render a nil node")
	}
	switch n.Kind {
	case KindCatalogAssert, KindIndexVerify, KindWait:
		return "", nil

	case KindIndexCreateParentInvalid:
		p, err := renderParams[*CreateParentInvalidParams](n)
		if err != nil {
			return "", err
		}
		return renderCreateIndex(n, createIndexForm{
			Index:      p.Index,
			Table:      p.Parent,
			Only:       true,
			Definition: p.Definition,
		})

	case KindIndexCreateConcurrently:
		p, err := renderParams[*CreateConcurrentlyParams](n)
		if err != nil {
			return "", err
		}
		return renderCreateIndex(n, createIndexForm{
			Index:        p.Index,
			Table:        p.Partition,
			Concurrently: true,
			Definition:   p.Definition,
		})

	case KindIndexAttach:
		p, err := renderParams[*AttachParams](n)
		if err != nil {
			return "", err
		}
		return "ALTER INDEX " + p.ParentIndex.Quoted() + " ATTACH PARTITION " + p.ChildIndex.Quoted(), nil

	case KindIndexDropConcurrently:
		p, err := renderParams[*DropConcurrentlyParams](n)
		if err != nil {
			return "", err
		}
		return "DROP INDEX CONCURRENTLY " + p.Index.Quoted(), nil

	case KindIndexReindexConcurrently:
		p, err := renderParams[*ReindexConcurrentlyParams](n)
		if err != nil {
			return "", err
		}
		return "REINDEX INDEX CONCURRENTLY " + p.Index.Quoted(), nil

	case KindIndexDropPartitioned:
		p, err := renderParams[*DropPartitionedParams](n)
		if err != nil {
			return "", err
		}
		// No CASCADE and no CONCURRENTLY. The statement cascades to every
		// attached child index on its own, and PostgreSQL rejects the
		// concurrent form on a partitioned index outright (TRD §7.2.10). No
		// IF EXISTS either: an index that is already gone is a topology
		// question the planner answers, not something to paper over mid-run.
		return "DROP INDEX " + p.Index.Quoted(), nil
	}
	return "", ErrUnknownNodeKind.Detailf("node %q: %q", n.ID, n.Kind)
}

// Preview is the reviewer-facing form of [Render]: the same statement,
// terminated, so the artifact reads as something an operator could paste.
//
// It is deliberately the *same* function underneath. A preview that could drift
// from the statement would make plan review a fiction, which is why an
// operation planner never assembles DDL text of its own; the only thing an
// operation adds on top of this is a leading comment, for the context a
// statement cannot carry (a lock level, a blast radius).
//
// A node that issues no DDL previews as the empty string, and a node this
// renderer cannot render previews as the empty string too: a preview is not a
// place to surface an error, and [Node.Validate] has already rejected the plans
// where that could happen.
func Preview(n *Node) string {
	sql, err := Render(n)
	if err != nil || sql == "" {
		return ""
	}
	return sql + ";"
}

// createIndexForm is the shape shared by the two CREATE INDEX forms.
type createIndexForm struct {
	Index        ObjectName
	Table        ObjectName
	Only         bool
	Concurrently bool
	Definition   IndexDefinition
}

// renderCreateIndex builds:
//
//	CREATE [UNIQUE] INDEX [CONCURRENTLY] <index> ON [ONLY] <table>
//	    [USING <method>] (<columns>) [INCLUDE (...)] [WITH (...)]
//	    [TABLESPACE <ts>] [WHERE <predicate>]
func renderCreateIndex(n *Node, c createIndexForm) (string, error) {
	// PostgreSQL puts a new index in its table's schema and rejects a
	// qualified name here, so the index name is rendered bare. A plan that asks
	// for a different schema is asking for something the server cannot do, and
	// saying so at render time is better than a confusing syntax error mid-run.
	if c.Index.Schema != "" && c.Index.Schema != c.Table.Schema {
		return "", ErrInvalidPlan.Detailf(
			"node %q: index %s is in schema %q but its table %s is in schema %q; "+
				"PostgreSQL creates an index in its table's schema",
			n.ID, c.Index.Name, c.Index.Schema, c.Table.String(), c.Table.Schema)
	}

	var b strings.Builder
	b.WriteString("CREATE ")
	if c.Definition.Unique {
		b.WriteString("UNIQUE ")
	}
	b.WriteString("INDEX ")
	if c.Concurrently {
		b.WriteString("CONCURRENTLY ")
	}
	b.WriteString(QuoteIdentifier(c.Index.Name))
	b.WriteString(" ON ")
	if c.Only {
		b.WriteString("ONLY ")
	}
	b.WriteString(c.Table.Quoted())
	if c.Definition.Method != "" {
		b.WriteString(" USING ")
		b.WriteString(QuoteIdentifier(c.Definition.Method))
	}
	b.WriteString(" (")
	for i, col := range c.Definition.Columns {
		if i > 0 {
			b.WriteString(", ")
		}
		renderIndexColumn(&b, col)
	}
	b.WriteString(")")
	if len(c.Definition.Include) > 0 {
		b.WriteString(" INCLUDE (")
		for i, col := range c.Definition.Include {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(QuoteIdentifier(col))
		}
		b.WriteString(")")
	}
	if len(c.Definition.StorageParams) > 0 {
		b.WriteString(" WITH (")
		keys := make([]string, 0, len(c.Definition.StorageParams))
		for k := range c.Definition.StorageParams {
			keys = append(keys, k)
		}
		// Map order is not deterministic and the executor must render the same
		// bytes on every run, on every host.
		sort.Strings(keys)
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(QuoteIdentifier(k))
			b.WriteString(" = ")
			b.WriteString(QuoteLiteral(c.Definition.StorageParams[k]))
		}
		b.WriteString(")")
	}
	if c.Definition.Tablespace != "" {
		b.WriteString(" TABLESPACE ")
		b.WriteString(QuoteIdentifier(c.Definition.Tablespace))
	}
	if c.Definition.Where != "" {
		b.WriteString(" WHERE ")
		b.WriteString(c.Definition.Where)
	}
	return b.String(), nil
}

// renderIndexColumn writes one key column:
//
//	{ <name> | (<expression>) } [COLLATE <c>] [<opclass>] [DESC] [NULLS {FIRST|LAST}]
func renderIndexColumn(b *strings.Builder, c IndexColumn) {
	if c.Expression != "" {
		b.WriteString("(")
		b.WriteString(c.Expression)
		b.WriteString(")")
	} else {
		b.WriteString(QuoteIdentifier(c.Name))
	}
	if c.Collation != "" {
		b.WriteString(" COLLATE ")
		b.WriteString(QuoteMaybeQualified(c.Collation))
	}
	if c.OpClass != "" {
		b.WriteString(" ")
		b.WriteString(QuoteMaybeQualified(c.OpClass))
	}
	if c.Descending {
		b.WriteString(" DESC")
	}
	if c.NullsFirst != nil {
		if *c.NullsFirst {
			b.WriteString(" NULLS FIRST")
		} else {
			b.WriteString(" NULLS LAST")
		}
	}
}

// renderParams recovers a node's concrete params type. [Node.Validate] already
// guarantees Params.Kind() == Kind, so a failure here means the plan bypassed
// validation.
func renderParams[T NodeParams](n *Node) (T, error) {
	p, ok := n.Params.(T)
	if !ok {
		var zero T
		return zero, ErrInvalidPlan.Detailf(
			"node %q of kind %q carries params of type %T", n.ID, n.Kind, n.Params)
	}
	return p, nil
}
