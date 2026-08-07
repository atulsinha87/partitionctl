package createindex

import (
	"fmt"
	"sort"
	"strings"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// The functions in this file produce [protocol.Node.RenderedSQL]: a
// NON-AUTHORITATIVE human preview for the reviewer of the plan file
// (FR-PLANFILE-7). Nothing here is sent to a server. The executor ignores every
// string this file produces and re-renders from the node's structured params,
// which is what keeps rendered_sql off the injection surface (T2).
//
// Identifiers still reach these strings only through [protocol.QuoteIdentifier]
// and [protocol.ObjectName.Quoted] (NFR-SEC-4), because a runbook an operator
// pastes into psql has to be correct too.

// renderCreateParentInvalid previews CREATE INDEX <index> ON ONLY <parent>.
//
// The index name is deliberately unqualified: PostgreSQL forbids a schema on
// the index name in CREATE INDEX, because the index is always created in its
// table's schema.
func renderCreateParentInvalid(p *protocol.CreateParentInvalidParams) string {
	var b strings.Builder
	b.WriteString("CREATE ")
	if p.Definition.Unique {
		b.WriteString("UNIQUE ")
	}
	b.WriteString("INDEX ")
	b.WriteString(protocol.QuoteIdentifier(p.Index.Name))
	b.WriteString(" ON ONLY ")
	b.WriteString(p.Parent.Quoted())
	b.WriteString(renderDefinition(p.Definition))
	b.WriteString(";")
	return b.String()
}

// renderCreateConcurrently previews CREATE INDEX CONCURRENTLY on one leaf.
func renderCreateConcurrently(p *protocol.CreateConcurrentlyParams) string {
	var b strings.Builder
	b.WriteString("CREATE ")
	if p.Definition.Unique {
		b.WriteString("UNIQUE ")
	}
	b.WriteString("INDEX CONCURRENTLY ")
	b.WriteString(protocol.QuoteIdentifier(p.Index.Name))
	b.WriteString(" ON ")
	b.WriteString(p.Partition.Quoted())
	b.WriteString(renderDefinition(p.Definition))
	b.WriteString(";")
	return b.String()
}

// withMarkerPreview appends the ownership marker the executor writes after the
// node's statement, so the artifact a human approves shows every statement the
// run will issue rather than all but one (G2, FR-PLANFILE-7).
//
// The run and plan fields read "pending" because neither exists yet: a plan is
// not bound to a run until `execute` opens one, and the digest cannot name
// itself. The executor substitutes the real values; this string is never sent
// anywhere.
func withMarkerPreview(n protocol.Node) protocol.Node {
	stmt, ok, err := protocol.RenderMarkerStatement(&n, protocol.Marker{
		Run: "pending", Plan: "pending", Op: string(protocol.OpCreateIndex), At: "pending",
	}, protocol.Marker{}, protocol.MarkerAbsent)
	if err != nil || !ok {
		return n
	}
	n.RenderedSQL += "\n" + stmt + ";"
	return n
}

// renderAttach previews ALTER INDEX <parent> ATTACH PARTITION <child>.
func renderAttach(p *protocol.AttachParams) string {
	return "ALTER INDEX " + p.ParentIndex.Quoted() +
		" ATTACH PARTITION " + p.ChildIndex.Quoted() + ";"
}

// renderDropConcurrently previews DROP INDEX CONCURRENTLY on an unattached leaf
// index.
func renderDropConcurrently(p *protocol.DropConcurrentlyParams) string {
	return "DROP INDEX CONCURRENTLY " + p.Index.Quoted() + ";"
}

// renderDefinition renders everything after the indexed relation: the access
// method, the key columns, INCLUDE, WITH, TABLESPACE and WHERE.
func renderDefinition(d protocol.IndexDefinition) string {
	var b strings.Builder
	if d.Method != "" {
		b.WriteString(" USING ")
		b.WriteString(protocol.QuoteIdentifier(d.Method))
	}
	b.WriteString(" (")
	for i, c := range d.Columns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(renderColumn(c))
	}
	b.WriteString(")")
	if len(d.Include) > 0 {
		b.WriteString(" INCLUDE (")
		for i, c := range d.Include {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(protocol.QuoteIdentifier(c))
		}
		b.WriteString(")")
	}
	if len(d.StorageParams) > 0 {
		keys := make([]string, 0, len(d.StorageParams))
		for k := range d.StorageParams {
			keys = append(keys, k)
		}
		// Sorted, because Go map iteration order is random and rendered_sql is
		// part of the plan body the digest covers.
		sort.Strings(keys)
		b.WriteString(" WITH (")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(protocol.QuoteIdentifier(k))
			b.WriteString(" = ")
			b.WriteString(quoteLiteral(d.StorageParams[k]))
		}
		b.WriteString(")")
	}
	if d.Tablespace != "" {
		b.WriteString(" TABLESPACE ")
		b.WriteString(protocol.QuoteIdentifier(d.Tablespace))
	}
	if d.Where != "" {
		b.WriteString(" WHERE ")
		b.WriteString(d.Where)
	}
	return b.String()
}

// renderColumn renders one key column: a quoted name or an operator-authored
// expression, then collation, operator class and ordering.
func renderColumn(c protocol.IndexColumn) string {
	var b strings.Builder
	if c.Name != "" {
		b.WriteString(protocol.QuoteIdentifier(c.Name))
	} else {
		// An index expression is operator-authored SQL that cannot be
		// structured without a SQL parser. It is parenthesised, not quoted,
		// and is covered by plan-file review (see the protocol package's trust
		// boundary note).
		b.WriteString("(")
		b.WriteString(c.Expression)
		b.WriteString(")")
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
	return b.String()
}

// quoteLiteral renders s as a single-quoted SQL string literal. Storage
// parameter values are literals, not identifiers.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// renderAssertComment previews the precondition node. It is a comment, because
// catalog.assert issues no statement.
func renderAssertComment(n int) string {
	return fmt.Sprintf(
		"-- catalog.assert: %d preconditions (relkind, strategy, depth, no DEFAULT partition, role membership, index name available)",
		n)
}

// renderLeafVerifyComment previews a leaf chain's verification.
func renderLeafVerifyComment(child protocol.ObjectName) string {
	return "-- verify " + child.Quoted() + ": indisvalid AND indisready AND indislive"
}

// renderFinalVerifyComment previews the terminal verification.
func renderFinalVerifyComment(parentIndex protocol.ObjectName, leaves int) string {
	return fmt.Sprintf("-- verify %s: indisvalid, and %d leaf index(es) attached",
		parentIndex.Quoted(), leaves)
}

// renderWaitComment previews a pacing pause.
func renderWaitComment(seconds int) string {
	return fmt.Sprintf("-- wait %ds: planner-emitted pacing between leaf index builds", seconds)
}
