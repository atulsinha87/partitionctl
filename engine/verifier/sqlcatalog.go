package verifier

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// Queryer is the only thing [SQLCatalog] is allowed to hold: the query half of
// database/sql and nothing else. *sql.DB, *sql.Conn and *sql.Tx all satisfy it.
//
// The narrowness is the point. FR-VER-5 requires `verify` to issue no DDL, and
// an interface without ExecContext makes that a compile-time property rather
// than a review comment.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// SQLCatalog reads the PostgreSQL system catalogs through database/sql.
//
// It imports no driver: the caller registers one and passes an open handle,
// which is what keeps every package below the CLI unit-testable with no live
// PostgreSQL (HANDOFF §3).
//
// Transaction scope belongs to the caller. Passing a *sql.DB runs each query on
// its own snapshot, which is fine for a single assertion; passing a transaction
// opened with sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead}
// gives a whole report one consistent snapshot, which is what a Liquibase gate
// or a multi-check `verify` wants.
type SQLCatalog struct {
	q Queryer
}

// NewSQLCatalog returns a catalog reading through q.
func NewSQLCatalog(q Queryer) *SQLCatalog { return &SQLCatalog{q: q} }

var _ Catalog = (*SQLCatalog)(nil)

// relationFilter matches pg_class by name, resolving an unqualified name
// through search_path exactly as PostgreSQL would. $1 is the schema, empty for
// unqualified; $2 is the object name. Both are bound parameters, never
// interpolated (NFR-SEC-4).
const relationFilter = `%[1]s.relname = $2
	  AND (($1 = '' AND pg_catalog.pg_table_is_visible(%[1]s.oid)) OR %[2]s.nspname = $1)`

// indexStateColumns is the projection every index-returning query shares, in
// the order scanIndexStates expects.
const indexStateColumns = `n.nspname, c.relname, tn.nspname, t.relname,
	       i.indisvalid, i.indisready, i.indislive, c.relkind = 'I'`

// indexStateJoins resolves an index in pg_class (aliased c) to its namespace,
// its pg_index row and the table it is on.
const indexStateJoins = `JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	JOIN pg_catalog.pg_index i ON i.indexrelid = c.oid
	JOIN pg_catalog.pg_class t ON t.oid = i.indrelid
	JOIN pg_catalog.pg_namespace tn ON tn.oid = t.relnamespace`

var (
	// queryIndex reads one index's state. relkind is constrained to 'i' and 'I'
	// so a table sharing a name with no index does not masquerade as one.
	queryIndex = `SELECT ` + indexStateColumns + `
	FROM pg_catalog.pg_class c
	` + indexStateJoins + `
	WHERE ` + fmt.Sprintf(relationFilter, "c", "n") + `
	  AND c.relkind IN ('i', 'I')
	LIMIT 1`

	// queryIndexParent reads the partitioned index a leaf index is attached to
	// (FR-VER-2). An index has at most one parent in pg_inherits.
	queryIndexParent = `SELECT pn.nspname, p.relname
	FROM pg_catalog.pg_inherits h
	JOIN pg_catalog.pg_class c ON c.oid = h.inhrelid
	JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	JOIN pg_catalog.pg_class p ON p.oid = h.inhparent
	JOIN pg_catalog.pg_namespace pn ON pn.oid = p.relnamespace
	WHERE ` + fmt.Sprintf(relationFilter, "c", "n") + `
	  AND c.relkind IN ('i', 'I')
	LIMIT 1`

	// queryAttachedIndexes reads every child of a partitioned index, with state,
	// in one round trip. It is what FR-VER-4 counts.
	queryAttachedIndexes = `SELECT ` + indexStateColumns + `
	FROM pg_catalog.pg_inherits h
	JOIN pg_catalog.pg_class p ON p.oid = h.inhparent
	JOIN pg_catalog.pg_namespace pn ON pn.oid = p.relnamespace
	JOIN pg_catalog.pg_class c ON c.oid = h.inhrelid
	` + indexStateJoins + `
	WHERE ` + fmt.Sprintf(relationFilter, "p", "pn") + `
	  AND p.relkind = 'I'
	ORDER BY n.nspname, c.relname`

	// queryLeafPartitions discovers the leaf partitions via pg_partition_tree
	// (FR-PLAN-1: the tree is discovered, never parsed from DDL text). The
	// function returns a NULL row for a relation that is neither a partition nor
	// partitioned; the join on relid discards it, so a plain table yields no
	// leaves rather than an error.
	queryLeafPartitions = `WITH target AS (
	  SELECT c.oid AS relid
	  FROM pg_catalog.pg_class c
	  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	  WHERE ` + fmt.Sprintf(relationFilter, "c", "n") + `
	  LIMIT 1
	)
	SELECT n.nspname, c.relname
	FROM target
	CROSS JOIN LATERAL pg_catalog.pg_partition_tree(target.relid) pt
	JOIN pg_catalog.pg_class c ON c.oid = pt.relid
	JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	WHERE pt.isleaf
	ORDER BY n.nspname, c.relname`

	// queryTreeIndexes reads every index on the relation and on every partition
	// beneath it. The UNION includes the relation itself so the query is correct
	// for a plain table too, where pg_partition_tree returns nothing usable.
	queryTreeIndexes = `WITH target AS (
	  SELECT c.oid AS relid
	  FROM pg_catalog.pg_class c
	  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	  WHERE ` + fmt.Sprintf(relationFilter, "c", "n") + `
	  LIMIT 1
	), rels AS (
	  SELECT relid FROM target
	  UNION
	  SELECT pt.relid
	  FROM target
	  CROSS JOIN LATERAL pg_catalog.pg_partition_tree(target.relid) pt
	  WHERE pt.relid IS NOT NULL
	)
	SELECT ` + indexStateColumns + `
	FROM rels
	JOIN pg_catalog.pg_index i ON i.indrelid = rels.relid
	JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
	JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	JOIN pg_catalog.pg_class t ON t.oid = i.indrelid
	JOIN pg_catalog.pg_namespace tn ON tn.oid = t.relnamespace
	ORDER BY n.nspname, c.relname`

	// queryIndexComment reads the ownership marker PartitionCTL writes onto an
	// object it created. obj_description is the supported reader for
	// pg_description on a pg_class entry, and it returns NULL both for an index
	// with no comment and for a comment that was never set, which the caller
	// treats identically.
	queryIndexComment = `SELECT pg_catalog.obj_description(c.oid, 'pg_class')
	FROM pg_catalog.pg_class c
	JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	WHERE ` + fmt.Sprintf(relationFilter, "c", "n") + `
	  AND c.relkind IN ('i', 'I')
	LIMIT 1`
)

// allQueries is every statement this catalog can issue. It exists so a test can
// assert that the set is read-only (FR-VER-5).
func allQueries() []string {
	return []string{
		queryIndex, queryIndexParent, queryAttachedIndexes,
		queryLeafPartitions, queryTreeIndexes, queryIndexComment,
	}
}

// Index implements [Catalog].
func (c *SQLCatalog) Index(ctx context.Context, name protocol.ObjectName) (IndexState, bool, error) {
	states, err := c.queryIndexStates(ctx, queryIndex, name)
	if err != nil {
		return IndexState{}, false, err
	}
	if len(states) == 0 {
		return IndexState{}, false, nil
	}
	return states[0], true, nil
}

// IndexParent implements [Catalog].
func (c *SQLCatalog) IndexParent(ctx context.Context, child protocol.ObjectName) (protocol.ObjectName, bool, error) {
	names, err := c.queryNames(ctx, queryIndexParent, child)
	if err != nil {
		return protocol.ObjectName{}, false, err
	}
	if len(names) == 0 {
		return protocol.ObjectName{}, false, nil
	}
	return names[0], true, nil
}

// AttachedIndexes implements [Catalog].
func (c *SQLCatalog) AttachedIndexes(ctx context.Context, parentIndex protocol.ObjectName) ([]IndexState, error) {
	return c.queryIndexStates(ctx, queryAttachedIndexes, parentIndex)
}

// LeafPartitions implements [Catalog].
func (c *SQLCatalog) LeafPartitions(ctx context.Context, table protocol.ObjectName) ([]protocol.ObjectName, error) {
	return c.queryNames(ctx, queryLeafPartitions, table)
}

// TreeIndexes implements [Catalog].
func (c *SQLCatalog) TreeIndexes(ctx context.Context, table protocol.ObjectName) ([]IndexState, error) {
	return c.queryIndexStates(ctx, queryTreeIndexes, table)
}

// IndexComment implements [Catalog].
func (c *SQLCatalog) IndexComment(ctx context.Context, index protocol.ObjectName) (string, bool, error) {
	if c == nil || c.q == nil {
		return "", false, protocol.ErrFailure.Detailf("sql catalog has no query handle")
	}
	rows, err := c.q.QueryContext(ctx, queryIndexComment, index.Schema, index.Name)
	if err != nil {
		return "", false, fmt.Errorf("catalog read for %q: %w", index.String(), err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", false, fmt.Errorf("catalog read for %q: %w", index.String(), err)
		}
		return "", false, nil
	}
	var comment sql.NullString
	if err := rows.Scan(&comment); err != nil {
		return "", false, fmt.Errorf("catalog read for %q: %w", index.String(), err)
	}
	if err := rows.Err(); err != nil {
		return "", false, fmt.Errorf("catalog read for %q: %w", index.String(), err)
	}
	if !comment.Valid || comment.String == "" {
		return "", false, nil
	}
	return comment.String, true, nil
}

func (c *SQLCatalog) queryIndexStates(ctx context.Context, query string, name protocol.ObjectName) ([]IndexState, error) {
	if c == nil || c.q == nil {
		return nil, protocol.ErrFailure.Detailf("sql catalog has no query handle")
	}
	rows, err := c.q.QueryContext(ctx, query, name.Schema, name.Name)
	if err != nil {
		return nil, fmt.Errorf("catalog read for %q: %w", name.String(), err)
	}
	defer rows.Close()

	var out []IndexState
	for rows.Next() {
		var (
			s          IndexState
			schema     string
			index      string
			relSchema  string
			relName    string
			partitiond bool
		)
		if err := rows.Scan(&schema, &index, &relSchema, &relName,
			&s.Valid, &s.Ready, &s.Live, &partitiond); err != nil {
			return nil, fmt.Errorf("catalog read for %q: %w", name.String(), err)
		}
		s.Name = protocol.NewObjectName(schema, index)
		s.Relation = protocol.NewObjectName(relSchema, relName)
		s.Partitioned = partitiond
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog read for %q: %w", name.String(), err)
	}
	return out, nil
}

func (c *SQLCatalog) queryNames(ctx context.Context, query string, name protocol.ObjectName) ([]protocol.ObjectName, error) {
	if c == nil || c.q == nil {
		return nil, protocol.ErrFailure.Detailf("sql catalog has no query handle")
	}
	rows, err := c.q.QueryContext(ctx, query, name.Schema, name.Name)
	if err != nil {
		return nil, fmt.Errorf("catalog read for %q: %w", name.String(), err)
	}
	defer rows.Close()

	var out []protocol.ObjectName
	for rows.Next() {
		var schema, object string
		if err := rows.Scan(&schema, &object); err != nil {
			return nil, fmt.Errorf("catalog read for %q: %w", name.String(), err)
		}
		out = append(out, protocol.NewObjectName(schema, object))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog read for %q: %w", name.String(), err)
	}
	return out, nil
}
