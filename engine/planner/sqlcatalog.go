package planner

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// Querier is the subset of database/sql the catalog reader uses. *sql.DB,
// *sql.Tx and *sql.Conn all satisfy it.
//
// Only the two read methods are here. There is no Exec, which is the structural
// form of FR-PLAN-8: the catalog reader cannot issue DDL because it holds
// nothing that could.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SQLCatalog is the database/sql implementation of [CatalogReader].
//
// It imports no driver. The caller registers one and hands over a *sql.DB, so
// the library stays offline-testable and the driver choice stays the
// application's.
//
// Every query reads only pg_partition_tree, pg_class, pg_index, pg_inherits,
// pg_constraint, pg_namespace, pg_roles and pg_partitioned_table. Every
// identifier that reaches the server does so as a bind parameter, never by
// interpolation; the one exception is a list of OIDs the planner itself read
// out of the catalog, which is passed as a single comma-joined text parameter
// and split server-side.
type SQLCatalog struct {
	q Querier
}

// NewSQLCatalog wraps any [Querier].
//
// Prefer [BeginReadOnly]: a plain *sql.DB runs each query in its own implicit
// transaction, so the partition tree, the index list and the role memberships
// can each come from a different catalog snapshot. A fingerprint computed
// across a torn view describes a topology that never existed.
func NewSQLCatalog(q Querier) *SQLCatalog { return &SQLCatalog{q: q} }

// BeginReadOnly opens the transaction the planner is meant to run in and
// returns a catalog reader bound to it, plus a release function the caller must
// defer.
//
// READ ONLY is FR-PLAN-8 enforced by the server rather than by convention: any
// DDL would be rejected. REPEATABLE READ gives the whole planning pass one
// catalog snapshot, so the topology fingerprint, the index inspection and the
// privilege check all describe the same instant.
//
// The release function rolls back. There is nothing to commit, and rolling back
// a read-only transaction is the cheapest way to say so.
func BeginReadOnly(ctx context.Context, db *sql.DB) (*SQLCatalog, func() error, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return nil, nil, ErrCatalogUnavailable.Wrap(err)
	}
	release := func() error {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			return err
		}
		return nil
	}
	return NewSQLCatalog(tx), release, nil
}

// AssertReadOnly proves the session cannot write (FR-PLAN-8). It implements
// [ReadOnlyAsserter], which [Host.Run] calls before planning.
func (c *SQLCatalog) AssertReadOnly(ctx context.Context) error {
	var ro bool
	err := c.q.QueryRowContext(ctx, qReadOnly).Scan(&ro)
	if err != nil {
		return ErrCatalogUnavailable.Wrap(err)
	}
	if !ro {
		return ErrNotReadOnly.Detailf(
			"transaction_read_only is off; build the catalog reader with planner.BeginReadOnly")
	}
	return nil
}

const qReadOnly = `SELECT pg_catalog.current_setting('transaction_read_only')::bool`

// CurrentRole returns the connected role.
func (c *SQLCatalog) CurrentRole(ctx context.Context) (string, error) {
	var role string
	if err := c.q.QueryRowContext(ctx, qCurrentRole).Scan(&role); err != nil {
		return "", ErrCatalogUnavailable.Wrap(err)
	}
	return role, nil
}

const qCurrentRole = `SELECT CURRENT_USER::text`

// CurrentDatabase returns the connected database name.
func (c *SQLCatalog) CurrentDatabase(ctx context.Context) (string, error) {
	var db string
	if err := c.q.QueryRowContext(ctx, qCurrentDatabase).Scan(&db); err != nil {
		return "", ErrCatalogUnavailable.Wrap(err)
	}
	return db, nil
}

const qCurrentDatabase = `SELECT pg_catalog.current_database()::text`

// ServerVersionNum returns server_version_num.
func (c *SQLCatalog) ServerVersionNum(ctx context.Context) (int, error) {
	var v int
	if err := c.q.QueryRowContext(ctx, qServerVersion).Scan(&v); err != nil {
		return 0, ErrCatalogUnavailable.Wrap(err)
	}
	return v, nil
}

const qServerVersion = `SELECT pg_catalog.current_setting('server_version_num')::int`

// qLookupRelation resolves a relation by name.
//
// An empty schema means "resolve through search_path", which is what
// pg_table_is_visible answers. It is the same resolution the server would apply
// to the bare name in a DDL statement, so a plan built from an unqualified name
// targets what the operator would have targeted by hand. The plan records the
// resolved schema-qualified name, so the ambiguity ends here.
const qLookupRelation = `
SELECT c.oid::int8,
       n.nspname::text,
       c.relname::text,
       c.relkind::text,
       c.relowner::int8,
       COALESCE(o.rolname, '')::text,
       c.relpages::int8,
       COALESCE(i.inhparent::oid::int8, 0),
       COALESCE(pg_catalog.pg_get_expr(c.relpartbound, c.oid), '')::text
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_roles o ON o.oid = c.relowner
LEFT JOIN pg_catalog.pg_inherits i ON i.inhrelid = c.oid
WHERE c.relname = $1
  AND (($2 = '' AND pg_catalog.pg_table_is_visible(c.oid)) OR n.nspname = $2)
ORDER BY n.nspname, c.oid`

// LookupRelation resolves a name to catalog state.
func (c *SQLCatalog) LookupRelation(ctx context.Context, name protocol.ObjectName) (Relation, error) {
	rows, err := c.q.QueryContext(ctx, qLookupRelation, name.Name, name.Schema)
	if err != nil {
		return Relation{}, ErrCatalogUnavailable.Wrap(err)
	}
	defer rows.Close()

	var found []Relation
	for rows.Next() {
		r, err := scanRelation(rows)
		if err != nil {
			return Relation{}, err
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		return Relation{}, ErrCatalogUnavailable.Wrap(err)
	}
	switch len(found) {
	case 0:
		return Relation{}, ErrRelationNotFound.Detailf(
			"%s does not exist, or is not visible to the connected role", name.String())
	case 1:
		return found[0], nil
	default:
		return Relation{}, ErrAmbiguousRelation.Detailf(
			"%q resolves to %d relations; qualify it with a schema", name.String(), len(found))
	}
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRelation(s scanner) (Relation, error) {
	var (
		oid, ownerOID, relPages, parentOID int64
		schema, relName, relKind, owner    string
		bound                              string
	)
	if err := s.Scan(&oid, &schema, &relName, &relKind, &ownerOID, &owner, &relPages, &parentOID, &bound); err != nil {
		return Relation{}, ErrCatalogUnavailable.Wrap(err)
	}
	r := Relation{
		OID:            uint32(oid),
		Name:           protocol.NewObjectName(schema, relName),
		Kind:           RelKind(relKind),
		OwnerOID:       uint32(ownerOID),
		Owner:          owner,
		RelPages:       relPages,
		ParentOID:      uint32(parentOID),
		PartitionBound: bound,
	}
	r.IsDefault = IsDefaultBound(bound)
	return r, nil
}

// qPartitionTree is FR-PLAN-1: the partition tree comes from
// pg_partition_tree() and never from parsing DDL text.
//
// The ORDER BY is not cosmetic. Planning must be deterministic, so the row
// order must not depend on whatever the server felt like returning.
const qPartitionTree = `
SELECT t.relid::oid::int8,
       t.level::int,
       t.isleaf,
       n.nspname::text,
       c.relname::text,
       c.relkind::text,
       c.relowner::int8,
       COALESCE(o.rolname, '')::text,
       c.relpages::int8,
       COALESCE(t.parentrelid::oid::int8, 0),
       COALESCE(pg_catalog.pg_get_expr(c.relpartbound, c.oid), '')::text
FROM pg_catalog.pg_partition_tree($1::oid::regclass) AS t
JOIN pg_catalog.pg_class c ON c.oid = t.relid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_roles o ON o.oid = c.relowner
ORDER BY t.level, n.nspname, c.relname`

// PartitionTree returns every relation in the tree rooted at rootOID.
func (c *SQLCatalog) PartitionTree(ctx context.Context, rootOID uint32) ([]TreeEntry, error) {
	rows, err := c.q.QueryContext(ctx, qPartitionTree, int64(rootOID))
	if err != nil {
		return nil, ErrCatalogUnavailable.Wrap(err)
	}
	defer rows.Close()

	var out []TreeEntry
	for rows.Next() {
		var (
			oid, ownerOID, relPages, parentOID int64
			level                              int
			isLeaf                             bool
			schema, relName, relKind, owner    string
			bound                              string
		)
		if err := rows.Scan(&oid, &level, &isLeaf, &schema, &relName, &relKind,
			&ownerOID, &owner, &relPages, &parentOID, &bound); err != nil {
			return nil, ErrCatalogUnavailable.Wrap(err)
		}
		r := Relation{
			OID:            uint32(oid),
			Name:           protocol.NewObjectName(schema, relName),
			Kind:           RelKind(relKind),
			OwnerOID:       uint32(ownerOID),
			Owner:          owner,
			RelPages:       relPages,
			ParentOID:      uint32(parentOID),
			PartitionBound: bound,
		}
		r.IsDefault = IsDefaultBound(bound)
		out = append(out, TreeEntry{Relation: r, Level: level, IsLeaf: isLeaf})
	}
	if err := rows.Err(); err != nil {
		return nil, ErrCatalogUnavailable.Wrap(err)
	}
	return out, nil
}

const qPartitionStrategy = `
SELECT p.partstrat::text
FROM pg_catalog.pg_partitioned_table p
WHERE p.partrelid = $1::oid`

// PartitionStrategy returns the root's partitioning strategy.
func (c *SQLCatalog) PartitionStrategy(ctx context.Context, rootOID uint32) (protocol.PartitionStrategy, error) {
	var code string
	err := c.q.QueryRowContext(ctx, qPartitionStrategy, int64(rootOID)).Scan(&code)
	if errors.Is(err, sql.ErrNoRows) {
		return "", topologyErr(CodeNotPartitioned, "oid "+strconv.FormatUint(uint64(rootOID), 10),
			"the relation has no pg_partitioned_table row, so it is not partitioned")
	}
	if err != nil {
		return "", ErrCatalogUnavailable.Wrap(err)
	}
	return strategyFromCode(code)
}

// indexSelect is shared by the two index queries. It is one statement rather
// than one per relation, because 1,000 round trips would not fit inside
// NFR-PERF-1.
const indexSelect = `
SELECT ic.oid::int8,
       n.nspname::text,
       ic.relname::text,
       ic.relkind::text,
       ic.relowner::int8,
       ic.relpages::int8,
       i.indrelid::oid::int8,
       tn.nspname::text,
       tc.relname::text,
       i.indisvalid,
       i.indisready,
       i.indislive,
       i.indisunique,
       i.indisprimary,
       i.indisexclusion,
       COALESCE(inh.inhparent::oid::int8, 0),
       COALESCE(con.conname, '')::text,
       COALESCE(con.contype::text, '')::text
FROM pg_catalog.pg_index i
JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
JOIN pg_catalog.pg_namespace n ON n.oid = ic.relnamespace
JOIN pg_catalog.pg_class tc ON tc.oid = i.indrelid
JOIN pg_catalog.pg_namespace tn ON tn.oid = tc.relnamespace
LEFT JOIN pg_catalog.pg_inherits inh ON inh.inhrelid = i.indexrelid
LEFT JOIN pg_catalog.pg_constraint con
       ON con.conindid = i.indexrelid AND con.contype IN ('p', 'u', 'x')
`

// qIndexesOnRelations selects every index on a set of relations.
//
// The OID list arrives as one comma-joined text parameter and is split
// server-side. Passing an array would need a driver-specific type, and this
// package imports no driver; interpolating the list would put planner-built SQL
// on the injection surface for no gain. The values are uint32s this package
// read out of pg_class itself, so the text is a list of digits by construction.
const qIndexesOnRelations = indexSelect + `
WHERE i.indrelid = ANY (SELECT x::oid FROM unnest(string_to_array($1, ',')) AS x)
ORDER BY tn.nspname, tc.relname, n.nspname, ic.relname`

// IndexesOnRelations returns every index on the given relations.
func (c *SQLCatalog) IndexesOnRelations(ctx context.Context, tableOIDs []uint32) ([]Index, error) {
	if len(tableOIDs) == 0 {
		return nil, nil
	}
	rows, err := c.q.QueryContext(ctx, qIndexesOnRelations, joinOIDs(tableOIDs))
	if err != nil {
		return nil, ErrCatalogUnavailable.Wrap(err)
	}
	defer rows.Close()

	var out []Index
	for rows.Next() {
		idx, err := scanIndex(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, idx)
	}
	if err := rows.Err(); err != nil {
		return nil, ErrCatalogUnavailable.Wrap(err)
	}
	return out, nil
}

const qLookupIndex = indexSelect + `
WHERE ic.relname = $1
  AND (($2 = '' AND pg_catalog.pg_table_is_visible(ic.oid)) OR n.nspname = $2)
ORDER BY n.nspname, ic.oid`

// LookupIndex resolves an index name to catalog state.
func (c *SQLCatalog) LookupIndex(ctx context.Context, name protocol.ObjectName) (Index, error) {
	rows, err := c.q.QueryContext(ctx, qLookupIndex, name.Name, name.Schema)
	if err != nil {
		return Index{}, ErrCatalogUnavailable.Wrap(err)
	}
	defer rows.Close()

	var found []Index
	for rows.Next() {
		idx, err := scanIndex(rows)
		if err != nil {
			return Index{}, err
		}
		found = append(found, idx)
	}
	if err := rows.Err(); err != nil {
		return Index{}, ErrCatalogUnavailable.Wrap(err)
	}
	switch len(found) {
	case 0:
		return Index{}, ErrIndexNotFound.Detailf(
			"%s does not exist, or is not visible to the connected role", name.String())
	case 1:
		return found[0], nil
	default:
		return Index{}, ErrAmbiguousRelation.Detailf(
			"index %q resolves to %d indexes; qualify it with a schema", name.String(), len(found))
	}
}

// qIndexComment reads the ownership marker PartitionCTL writes onto an object
// it created. obj_description is the supported reader for pg_description on a
// pg_class entry; it returns NULL for an index with no comment, which the
// caller treats exactly as it treats an index that does not exist.
const qIndexComment = `
SELECT pg_catalog.obj_description(ic.oid, 'pg_class')
FROM pg_catalog.pg_class ic
JOIN pg_catalog.pg_namespace n ON n.oid = ic.relnamespace
WHERE ic.relname = $1
  AND (($2 = '' AND pg_catalog.pg_table_is_visible(ic.oid)) OR n.nspname = $2)
  AND ic.relkind IN ('i', 'I')
ORDER BY n.nspname, ic.oid
LIMIT 1`

// IndexComment implements [CatalogReader].
func (c *SQLCatalog) IndexComment(ctx context.Context, index protocol.ObjectName) (string, bool, error) {
	var comment sql.NullString
	err := c.q.QueryRowContext(ctx, qIndexComment, index.Name, index.Schema).Scan(&comment)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, ErrCatalogUnavailable.Wrap(err)
	}
	if !comment.Valid || comment.String == "" {
		return "", false, nil
	}
	return comment.String, true, nil
}

func scanIndex(s scanner) (Index, error) {
	var (
		oid, ownerOID, relPages, tableOID, parentIndexOID int64
		schema, indexName, relKind                        string
		tableSchema, tableName                            string
		isValid, isReady, isLive                          bool
		isUnique, isPrimary, isExclusion                  bool
		conName, conType                                  string
	)
	if err := s.Scan(&oid, &schema, &indexName, &relKind, &ownerOID, &relPages,
		&tableOID, &tableSchema, &tableName,
		&isValid, &isReady, &isLive, &isUnique, &isPrimary, &isExclusion,
		&parentIndexOID, &conName, &conType); err != nil {
		return Index{}, ErrCatalogUnavailable.Wrap(err)
	}
	return Index{
		OID:            uint32(oid),
		Name:           protocol.NewObjectName(schema, indexName),
		Kind:           RelKind(relKind),
		OwnerOID:       uint32(ownerOID),
		RelPages:       relPages,
		TableOID:       uint32(tableOID),
		Table:          protocol.NewObjectName(tableSchema, tableName),
		IsValid:        isValid,
		IsReady:        isReady,
		IsLive:         isLive,
		IsUnique:       isUnique,
		IsPrimary:      isPrimary,
		IsExclusion:    isExclusion,
		ParentIndexOID: uint32(parentIndexOID),
		ConstraintName: conName,
		ConstraintType: conType,
	}, nil
}

// qRoleMemberships answers FR-PLAN-10 for every distinct owning role in one
// query.
//
// pg_has_role(..., 'USAGE') is the "has the privileges of" test, which is the
// one PostgreSQL itself applies to an ownership check. 'MEMBER' would also pass
// a NOINHERIT membership that only SET ROLE could use, and PartitionCTL never
// issues SET ROLE. Superusers pass, because PostgreSQL treats them as holding
// every role.
const qRoleMemberships = `
SELECT r.oid::int8,
       r.rolname::text,
       pg_catalog.pg_has_role($1::name, r.oid, 'USAGE')
FROM pg_catalog.pg_roles r
WHERE r.oid = ANY (SELECT x::oid FROM unnest(string_to_array($2, ',')) AS x)
ORDER BY r.oid`

// RoleMemberships reports whether role holds the privileges of each owning role.
func (c *SQLCatalog) RoleMemberships(ctx context.Context, role string, ownerOIDs []uint32) (map[uint32]RoleMembership, error) {
	out := make(map[uint32]RoleMembership, len(ownerOIDs))
	if len(ownerOIDs) == 0 {
		return out, nil
	}
	rows, err := c.q.QueryContext(ctx, qRoleMemberships, role, joinOIDs(ownerOIDs))
	if err != nil {
		return nil, ErrCatalogUnavailable.Wrap(err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			oid      int64
			name     string
			isMember bool
		)
		if err := rows.Scan(&oid, &name, &isMember); err != nil {
			return nil, ErrCatalogUnavailable.Wrap(err)
		}
		out[uint32(oid)] = RoleMembership{
			OwnerOID:  uint32(oid),
			OwnerName: name,
			IsMember:  isMember,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, ErrCatalogUnavailable.Wrap(err)
	}
	return out, nil
}

// joinOIDs renders an OID set as the comma-joined text the queries split
// server-side.
func joinOIDs(oids []uint32) string {
	var b strings.Builder
	for i, oid := range oids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(uint64(oid), 10))
	}
	return b.String()
}

// compile-time proof that the database/sql implementation satisfies both
// interfaces the host uses.
var (
	_ CatalogReader    = (*SQLCatalog)(nil)
	_ ReadOnlyAsserter = (*SQLCatalog)(nil)
)
