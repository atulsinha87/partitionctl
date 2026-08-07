package executor

import (
	"sort"
	"strings"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// Render produces the statement the executor will send for a node, from the
// node's structured parameters alone.
//
// [protocol.Node.RenderedSQL] is never read: it is a human preview in the
// reviewed artifact, and re-rendering is what keeps it off the injection
// surface (FR-PLANFILE-7, T2). Every identifier goes through
// [protocol.QuoteIdentifier]; the two operator-authored SQL fragments,
// [protocol.IndexColumn.Expression] and [protocol.IndexDefinition.Where], are
// emitted verbatim by design and are covered by plan-file review.
//
// It returns an empty string and a nil error for the three kinds that issue no
// DDL. Callers should gate on [protocol.NodeKind.IssuesDDL].
func Render(n *protocol.Node) (string, error) {
	if n == nil {
		return "", protocol.ErrInvalidPlan.Detailf("cannot render a nil node")
	}
	switch n.Kind {
	case protocol.KindCatalogAssert, protocol.KindIndexVerify, protocol.KindWait:
		return "", nil

	case protocol.KindIndexCreateParentInvalid:
		p, err := paramsOf[*protocol.CreateParentInvalidParams](n)
		if err != nil {
			return "", err
		}
		return renderCreateIndex(n, createIndex{
			Index:      p.Index,
			Table:      p.Parent,
			Only:       true,
			Definition: p.Definition,
		})

	case protocol.KindIndexCreateConcurrently:
		p, err := paramsOf[*protocol.CreateConcurrentlyParams](n)
		if err != nil {
			return "", err
		}
		return renderCreateIndex(n, createIndex{
			Index:        p.Index,
			Table:        p.Partition,
			Concurrently: true,
			Definition:   p.Definition,
		})

	case protocol.KindIndexAttach:
		p, err := paramsOf[*protocol.AttachParams](n)
		if err != nil {
			return "", err
		}
		return "ALTER INDEX " + p.ParentIndex.Quoted() + " ATTACH PARTITION " + p.ChildIndex.Quoted(), nil

	case protocol.KindIndexDropConcurrently:
		p, err := paramsOf[*protocol.DropConcurrentlyParams](n)
		if err != nil {
			return "", err
		}
		return "DROP INDEX CONCURRENTLY " + p.Index.Quoted(), nil

	case protocol.KindIndexReindexConcurrently, protocol.KindIndexDropPartitioned:
		return "", unsupportedKind(n)
	}
	return "", protocol.ErrUnknownNodeKind.Detailf("node %q: %q", n.ID, n.Kind)
}

// createIndex is the shape shared by the two CREATE INDEX forms.
type createIndex struct {
	Index        protocol.ObjectName
	Table        protocol.ObjectName
	Only         bool
	Concurrently bool
	Definition   protocol.IndexDefinition
}

// renderCreateIndex builds:
//
//	CREATE [UNIQUE] INDEX [CONCURRENTLY] <index> ON [ONLY] <table>
//	    [USING <method>] (<columns>) [INCLUDE (...)] [WITH (...)]
//	    [TABLESPACE <ts>] [WHERE <predicate>]
func renderCreateIndex(n *protocol.Node, c createIndex) (string, error) {
	// PostgreSQL puts a new index in its table's schema and rejects a
	// qualified name here, so the index name is rendered bare. A plan that asks
	// for a different schema is asking for something the server cannot do, and
	// saying so at render time is better than a confusing syntax error mid-run.
	if c.Index.Schema != "" && c.Index.Schema != c.Table.Schema {
		return "", protocol.ErrInvalidPlan.Detailf(
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
	b.WriteString(protocol.QuoteIdentifier(c.Index.Name))
	b.WriteString(" ON ")
	if c.Only {
		b.WriteString("ONLY ")
	}
	b.WriteString(c.Table.Quoted())
	if c.Definition.Method != "" {
		b.WriteString(" USING ")
		b.WriteString(protocol.QuoteIdentifier(c.Definition.Method))
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
			b.WriteString(protocol.QuoteIdentifier(col))
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
			b.WriteString(protocol.QuoteIdentifier(k))
			b.WriteString(" = ")
			b.WriteString(quoteLiteral(c.Definition.StorageParams[k]))
		}
		b.WriteString(")")
	}
	if c.Definition.Tablespace != "" {
		b.WriteString(" TABLESPACE ")
		b.WriteString(protocol.QuoteIdentifier(c.Definition.Tablespace))
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
func renderIndexColumn(b *strings.Builder, c protocol.IndexColumn) {
	if c.Expression != "" {
		b.WriteString("(")
		b.WriteString(c.Expression)
		b.WriteString(")")
	} else {
		b.WriteString(protocol.QuoteIdentifier(c.Name))
	}
	if c.Collation != "" {
		b.WriteString(" COLLATE ")
		b.WriteString(protocol.QuoteMaybeQualified(c.Collation))
	}
	if c.OpClass != "" {
		b.WriteString(" ")
		b.WriteString(protocol.QuoteMaybeQualified(c.OpClass))
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

// quoteLiteral renders a storage-parameter value as a single-quoted SQL
// literal. Values are operator-supplied strings, so doubling the quote is the
// whole escape: PostgreSQL processes no backslash escapes inside a standard
// string literal when standard_conforming_strings is on, which it is by default
// on every supported version.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// paramsOf recovers a node's concrete params type. [protocol.Node.Validate]
// already guarantees Params.Kind() == Kind, so a failure here means the plan
// bypassed validation.
func paramsOf[T protocol.NodeParams](n *protocol.Node) (T, error) {
	p, ok := n.Params.(T)
	if !ok {
		var zero T
		return zero, protocol.ErrInvalidPlan.Detailf(
			"node %q of kind %q carries params of type %T", n.ID, n.Kind, n.Params)
	}
	return p, nil
}
