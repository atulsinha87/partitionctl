package planner

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// ---------------------------------------------------------------------------
// An in-memory database/sql driver.
//
// The point is coverage of the scan path. Column order, type conversions and
// row iteration are the only places SQLCatalog can be wrong in a way that
// compiles, and none of them is exercised by a test that stops at the
// interface. The driver is stdlib only and needs no server: the library still
// imports no driver, because this file is a test.
// ---------------------------------------------------------------------------

type stubResult struct {
	cols []string
	rows [][]driver.Value
	err  error
	// midErr, when set, is returned by Rows.Next after every row has been
	// handed over. It is how the rows.Err() path is reached: a connection that
	// dies partway through a 1,000-partition result must not look like a short
	// but complete answer.
	midErr error
}

type stubDB struct {
	mu        sync.Mutex
	responses map[string]stubResult
	queries   []stubQuery
	txOpts    []driver.TxOptions
	beginErr  error
}

type stubQuery struct {
	sql  string
	args []driver.Value
}

func newStubDB() *stubDB { return &stubDB{responses: map[string]stubResult{}} }

func (s *stubDB) respond(query string, cols []string, rows ...[]driver.Value) *stubDB {
	s.responses[query] = stubResult{cols: cols, rows: rows}
	return s
}

func (s *stubDB) fail(query string, err error) *stubDB {
	s.responses[query] = stubResult{err: err}
	return s
}

func (s *stubDB) failMidStream(query string, cols []string, err error, rows ...[]driver.Value) *stubDB {
	s.responses[query] = stubResult{cols: cols, rows: rows, midErr: err}
	return s
}

func (s *stubDB) recorded() []stubQuery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubQuery(nil), s.queries...)
}

var (
	stubRegistry = struct {
		sync.Mutex
		n  int
		db map[string]*stubDB
	}{db: map[string]*stubDB{}}
)

func init() { sql.Register("planner-stub", stubDriver{}) }

// openStub registers s under a fresh DSN and returns a *sql.DB bound to it.
func openStub(t *testing.T, s *stubDB) *sql.DB {
	t.Helper()
	stubRegistry.Lock()
	stubRegistry.n++
	dsn := "stub" + strconv.Itoa(stubRegistry.n)
	stubRegistry.db[dsn] = s
	stubRegistry.Unlock()

	db, err := sql.Open("planner-stub", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

type stubDriver struct{}

func (stubDriver) Open(dsn string) (driver.Conn, error) {
	stubRegistry.Lock()
	defer stubRegistry.Unlock()
	s, ok := stubRegistry.db[dsn]
	if !ok {
		return nil, fmt.Errorf("no stub registered for dsn %q", dsn)
	}
	return &stubConn{db: s}, nil
}

type stubConn struct{ db *stubDB }

func (c *stubConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("stub driver does not prepare; QueryerContext is implemented")
}
func (c *stubConn) Close() error { return nil }
func (c *stubConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *stubConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.db.mu.Lock()
	c.db.txOpts = append(c.db.txOpts, opts)
	err := c.db.beginErr
	c.db.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return stubTx{}, nil
}

func (c *stubConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	vals := make([]driver.Value, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	c.db.mu.Lock()
	c.db.queries = append(c.db.queries, stubQuery{sql: query, args: vals})
	res, ok := c.db.responses[query]
	c.db.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("stub has no response for query:\n%s", query)
	}
	if res.err != nil {
		return nil, res.err
	}
	return &stubRows{cols: res.cols, rows: res.rows, midErr: res.midErr}, nil
}

type stubTx struct{}

func (stubTx) Commit() error   { return nil }
func (stubTx) Rollback() error { return nil }

type stubRows struct {
	cols   []string
	rows   [][]driver.Value
	midErr error
	i      int
}

func (r *stubRows) Columns() []string { return r.cols }
func (r *stubRows) Close() error      { return nil }
func (r *stubRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		if r.midErr != nil {
			return r.midErr
		}
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

// ---------------------------------------------------------------------------
// Column vocabularies, one per query. Names are for readability only; the
// count is what database/sql enforces against the scan.
// ---------------------------------------------------------------------------

var (
	relationCols = []string{"oid", "schema", "name", "relkind", "owner_oid", "owner", "relpages", "parent_oid", "bound"}
	treeCols     = []string{"oid", "level", "isleaf", "schema", "name", "relkind", "owner_oid", "owner", "relpages", "parent_oid", "bound"}
	indexCols    = []string{"oid", "schema", "name", "relkind", "owner_oid", "relpages", "table_oid", "table_schema", "table_name",
		"indisvalid", "indisready", "indislive", "indisunique", "indisprimary", "indisexclusion",
		"parent_index_oid", "conname", "contype"}
	roleCols = []string{"oid", "rolname", "is_member"}
)

func relationRow(oid int64, schema, name, kind string, owner int64, ownerName string, pages, parent int64, bound string) []driver.Value {
	return []driver.Value{oid, schema, name, kind, owner, ownerName, pages, parent, bound}
}

func treeRow(oid int64, level int64, isLeaf bool, schema, name, kind string, owner int64, ownerName string, pages, parent int64, bound string) []driver.Value {
	return []driver.Value{oid, level, isLeaf, schema, name, kind, owner, ownerName, pages, parent, bound}
}

func indexRow(oid int64, schema, name, kind string, owner, pages, tableOID int64, tableSchema, tableName string,
	valid, ready, live, unique, primary, exclusion bool, parentIdx int64, conname, contype string) []driver.Value {
	return []driver.Value{oid, schema, name, kind, owner, pages, tableOID, tableSchema, tableName,
		valid, ready, live, unique, primary, exclusion, parentIdx, conname, contype}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestBeginReadOnlyOpensTheRightTransaction(t *testing.T) {
	s := newStubDB()
	db := openStub(t, s)

	cat, release, err := BeginReadOnly(ctx(), db)
	if err != nil {
		t.Fatalf("BeginReadOnly: %v", err)
	}
	if cat == nil {
		t.Fatal("no catalog reader returned")
	}
	if err := release(); err != nil {
		t.Errorf("release: %v", err)
	}
	// Releasing twice must be harmless: the caller defers it and may also
	// call it on the success path.
	if err := release(); err != nil {
		t.Errorf("second release: %v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.txOpts) != 1 {
		t.Fatalf("BeginTx called %d times", len(s.txOpts))
	}
	opts := s.txOpts[0]
	if !opts.ReadOnly {
		t.Error("transaction is not READ ONLY; FR-PLAN-8 requires it")
	}
	if driver.IsolationLevel(sql.LevelRepeatableRead) != opts.Isolation {
		t.Errorf("isolation = %v, want REPEATABLE READ so the whole pass sees one snapshot", opts.Isolation)
	}
}

func TestBeginReadOnlyPropagatesFailure(t *testing.T) {
	s := newStubDB()
	s.beginErr = errors.New("too many connections")
	db := openStub(t, s)

	_, _, err := BeginReadOnly(ctx(), db)
	if !errors.Is(err, ErrCatalogUnavailable) {
		t.Fatalf("err = %v, want ErrCatalogUnavailable", err)
	}
}

func TestSQLCatalogScalars(t *testing.T) {
	s := newStubDB().
		respond(qReadOnly, []string{"ro"}, []driver.Value{true}).
		respond(qCurrentRole, []string{"role"}, []driver.Value{"migrator"}).
		respond(qCurrentDatabase, []string{"db"}, []driver.Value{"appdb"}).
		respond(qServerVersion, []string{"v"}, []driver.Value{int64(160004)})
	c := NewSQLCatalog(openStub(t, s))

	if err := c.AssertReadOnly(ctx()); err != nil {
		t.Errorf("AssertReadOnly: %v", err)
	}
	if got, err := c.CurrentRole(ctx()); err != nil || got != "migrator" {
		t.Errorf("CurrentRole = %q, %v", got, err)
	}
	if got, err := c.CurrentDatabase(ctx()); err != nil || got != "appdb" {
		t.Errorf("CurrentDatabase = %q, %v", got, err)
	}
	if got, err := c.ServerVersionNum(ctx()); err != nil || got != 160004 {
		t.Errorf("ServerVersionNum = %d, %v", got, err)
	}
}

func TestSQLCatalogAssertReadOnlyRefusesAWritableSession(t *testing.T) {
	s := newStubDB().respond(qReadOnly, []string{"ro"}, []driver.Value{false})
	c := NewSQLCatalog(openStub(t, s))

	err := c.AssertReadOnly(ctx())
	if !errors.Is(err, ErrNotReadOnly) {
		t.Fatalf("err = %v, want ErrNotReadOnly", err)
	}
	if !strings.Contains(err.Error(), "BeginReadOnly") {
		t.Errorf("message %q does not tell the caller what to do", err.Error())
	}
}

func TestSQLCatalogScalarFailures(t *testing.T) {
	boom := errors.New("connection reset")
	tests := []struct {
		name  string
		query string
		call  func(*SQLCatalog) error
	}{
		{"AssertReadOnly", qReadOnly, func(c *SQLCatalog) error { return c.AssertReadOnly(ctx()) }},
		{"CurrentRole", qCurrentRole, func(c *SQLCatalog) error { _, err := c.CurrentRole(ctx()); return err }},
		{"CurrentDatabase", qCurrentDatabase, func(c *SQLCatalog) error { _, err := c.CurrentDatabase(ctx()); return err }},
		{"ServerVersionNum", qServerVersion, func(c *SQLCatalog) error { _, err := c.ServerVersionNum(ctx()); return err }},
		{"LookupRelation", qLookupRelation, func(c *SQLCatalog) error {
			_, err := c.LookupRelation(ctx(), name("public", "orders"))
			return err
		}},
		{"PartitionTree", qPartitionTree, func(c *SQLCatalog) error { _, err := c.PartitionTree(ctx(), 100); return err }},
		{"PartitionStrategy", qPartitionStrategy, func(c *SQLCatalog) error {
			_, err := c.PartitionStrategy(ctx(), 100)
			return err
		}},
		{"IndexesOnRelations", qIndexesOnRelations, func(c *SQLCatalog) error {
			_, err := c.IndexesOnRelations(ctx(), []uint32{100})
			return err
		}},
		{"LookupIndex", qLookupIndex, func(c *SQLCatalog) error {
			_, err := c.LookupIndex(ctx(), name("public", "i"))
			return err
		}},
		{"RoleMemberships", qRoleMemberships, func(c *SQLCatalog) error {
			_, err := c.RoleMemberships(ctx(), "migrator", []uint32{10})
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubDB().fail(tc.query, boom)
			c := NewSQLCatalog(openStub(t, s))
			if err := tc.call(c); !errors.Is(err, ErrCatalogUnavailable) {
				t.Fatalf("err = %v, want ErrCatalogUnavailable", err)
			}
		})
	}
}

func TestSQLCatalogLookupRelation(t *testing.T) {
	tests := []struct {
		name    string
		rows    [][]driver.Value
		want    Relation
		wantErr error
	}{
		{
			name: "partitioned root",
			rows: [][]driver.Value{
				relationRow(100, "public", "orders", "p", 10, "app_owner", 0, 0, ""),
			},
			want: Relation{
				OID: 100, Name: name("public", "orders"), Kind: RelKindPartitionedTable,
				OwnerOID: 10, Owner: "app_owner",
			},
		},
		{
			name: "default partition is flagged from its bound",
			rows: [][]driver.Value{
				relationRow(201, "public", "orders_default", "r", 10, "app_owner", 42, 100, "DEFAULT"),
			},
			want: Relation{
				OID: 201, Name: name("public", "orders_default"), Kind: RelKindTable,
				OwnerOID: 10, Owner: "app_owner", RelPages: 42, ParentOID: 100,
				PartitionBound: "DEFAULT", IsDefault: true,
			},
		},
		{
			name:    "absent",
			rows:    nil,
			wantErr: ErrRelationNotFound,
		},
		{
			name: "ambiguous unqualified name",
			rows: [][]driver.Value{
				relationRow(100, "a", "orders", "p", 10, "app_owner", 0, 0, ""),
				relationRow(101, "b", "orders", "p", 10, "app_owner", 0, 0, ""),
			},
			wantErr: ErrAmbiguousRelation,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubDB().respond(qLookupRelation, relationCols, tc.rows...)
			c := NewSQLCatalog(openStub(t, s))

			got, err := c.LookupRelation(ctx(), name("public", "orders"))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LookupRelation: %v", err)
			}
			if got != tc.want {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestSQLCatalogLookupRelationBindsBothParts(t *testing.T) {
	s := newStubDB().respond(qLookupRelation, relationCols,
		relationRow(100, "public", "orders", "p", 10, "app_owner", 0, 0, ""))
	c := NewSQLCatalog(openStub(t, s))

	if _, err := c.LookupRelation(ctx(), name("public", "orders")); err != nil {
		t.Fatal(err)
	}
	q := s.recorded()
	if len(q) != 1 {
		t.Fatalf("recorded %d queries", len(q))
	}
	if len(q[0].args) != 2 || q[0].args[0] != "orders" || q[0].args[1] != "public" {
		t.Errorf("args = %v, want [orders public] bound as parameters, never interpolated", q[0].args)
	}
	if strings.Contains(q[0].sql, "orders") {
		t.Error("the relation name was interpolated into the SQL text")
	}
}

func TestSQLCatalogPartitionTree(t *testing.T) {
	s := newStubDB().respond(qPartitionTree, treeCols,
		treeRow(100, 0, false, "public", "orders", "p", 10, "app_owner", 0, 0, ""),
		treeRow(201, 1, true, "public", "orders_2026_01", "r", 10, "app_owner", 1000, 100, "FOR VALUES FROM ('a') TO ('b')"),
		treeRow(202, 1, true, "public", "orders_default", "r", 10, "app_owner", 5, 100, "DEFAULT"),
	)
	c := NewSQLCatalog(openStub(t, s))

	got, err := c.PartitionTree(ctx(), 100)
	if err != nil {
		t.Fatalf("PartitionTree: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3", len(got))
	}
	if got[0].Level != 0 || got[0].IsLeaf || got[0].Kind != RelKindPartitionedTable {
		t.Errorf("root entry = %+v", got[0])
	}
	if got[1].Level != 1 || !got[1].IsLeaf || got[1].RelPages != 1000 || got[1].ParentOID != 100 {
		t.Errorf("leaf entry = %+v", got[1])
	}
	if !got[2].IsDefault {
		t.Error("the DEFAULT partition was not flagged (FR-PLAN-3)")
	}

	q := s.recorded()
	if len(q[0].args) != 1 || q[0].args[0] != int64(100) {
		t.Errorf("args = %v, want the root OID bound as a parameter", q[0].args)
	}
	if !strings.Contains(q[0].sql, "pg_partition_tree") {
		t.Error("discovery does not go through pg_partition_tree (FR-PLAN-1)")
	}
}

func TestSQLCatalogPartitionStrategy(t *testing.T) {
	tests := []struct {
		name     string
		rows     [][]driver.Value
		want     protocol.PartitionStrategy
		wantCode TopologyCode
	}{
		{name: "range", rows: [][]driver.Value{{"r"}}, want: protocol.StrategyRange},
		{name: "list", rows: [][]driver.Value{{"l"}}, want: protocol.StrategyList},
		{name: "hash", rows: [][]driver.Value{{"h"}}, want: protocol.StrategyHash},
		{name: "no row means not partitioned", rows: nil, wantCode: CodeNotPartitioned},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubDB().respond(qPartitionStrategy, []string{"partstrat"}, tc.rows...)
			c := NewSQLCatalog(openStub(t, s))

			got, err := c.PartitionStrategy(ctx(), 100)
			if tc.wantCode != "" {
				code, ok := TopologyCodeOf(err)
				if !ok || code != tc.wantCode {
					t.Fatalf("err = %v, want code %q", err, tc.wantCode)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("PartitionStrategy = %q, %v", got, err)
			}
		})
	}
}

func TestSQLCatalogIndexes(t *testing.T) {
	s := newStubDB().respond(qIndexesOnRelations, indexCols,
		indexRow(900, "public", "orders_created_at_idx", "I", 10, 0, 100, "public", "orders",
			false, true, true, false, false, false, 0, "", ""),
		indexRow(901, "public", "orders_2026_01_pkey", "i", 10, 12, 201, "public", "orders_2026_01",
			true, true, true, true, true, false, 0, "orders_2026_01_pkey", "p"),
		indexRow(902, "public", "orders_created_at_idx_orders_2026_01", "i", 10, 34, 201, "public", "orders_2026_01",
			true, true, true, false, false, false, 900, "", ""),
	)
	c := NewSQLCatalog(openStub(t, s))

	got, err := c.IndexesOnRelations(ctx(), []uint32{100, 201})
	if err != nil {
		t.Fatalf("IndexesOnRelations: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d indexes, want 3", len(got))
	}

	parent := got[0]
	if parent.Kind != RelKindPartitionedIndex || parent.IsValid {
		t.Errorf("parent index = %+v; an ON ONLY parent is invalid for the whole build", parent)
	}
	if parent.Condition() != IndexInvalid {
		t.Errorf("parent Condition = %q, want %q", parent.Condition(), IndexInvalid)
	}

	pkey := got[1]
	if !pkey.ConstraintBacked() || pkey.ConstraintType != "p" {
		t.Errorf("primary key index = %+v; FR-DROP-2 needs the constraint reported", pkey)
	}

	child := got[2]
	if !child.AttachedTo(900) {
		t.Errorf("child index = %+v, want attached to the partitioned parent", child)
	}
	if child.RelPages != 34 || child.Table.String() != "public.orders_2026_01" {
		t.Errorf("child index = %+v", child)
	}

	q := s.recorded()
	if len(q[0].args) != 1 || q[0].args[0] != "100,201" {
		t.Errorf("args = %v, want one comma-joined OID list", q[0].args)
	}
}

func TestSQLCatalogIndexesEmptySetIssuesNoQuery(t *testing.T) {
	s := newStubDB()
	c := NewSQLCatalog(openStub(t, s))

	got, err := c.IndexesOnRelations(ctx(), nil)
	if err != nil || got != nil {
		t.Fatalf("IndexesOnRelations(nil) = %v, %v", got, err)
	}
	if len(s.recorded()) != 0 {
		t.Error("an empty relation set still hit the server")
	}
}

func TestSQLCatalogLookupIndex(t *testing.T) {
	tests := []struct {
		name    string
		rows    [][]driver.Value
		wantErr error
	}{
		{
			name: "found",
			rows: [][]driver.Value{
				indexRow(900, "public", "orders_created_at_idx", "I", 10, 0, 100, "public", "orders",
					true, true, true, false, false, false, 0, "", ""),
			},
		},
		{name: "absent", wantErr: ErrIndexNotFound},
		{
			name: "ambiguous",
			rows: [][]driver.Value{
				indexRow(900, "a", "i", "i", 10, 0, 100, "a", "t", true, true, true, false, false, false, 0, "", ""),
				indexRow(901, "b", "i", "i", 10, 0, 101, "b", "t", true, true, true, false, false, false, 0, "", ""),
			},
			wantErr: ErrAmbiguousRelation,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubDB().respond(qLookupIndex, indexCols, tc.rows...)
			c := NewSQLCatalog(openStub(t, s))

			got, err := c.LookupIndex(ctx(), name("public", "orders_created_at_idx"))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LookupIndex: %v", err)
			}
			if got.OID != 900 || got.Kind != RelKindPartitionedIndex {
				t.Errorf("got %+v", got)
			}
		})
	}
}

func TestSQLCatalogRoleMemberships(t *testing.T) {
	s := newStubDB().respond(qRoleMemberships, roleCols,
		[]driver.Value{int64(10), "app_owner", true},
		[]driver.Value{int64(11), "someone_else", false},
	)
	c := NewSQLCatalog(openStub(t, s))

	got, err := c.RoleMemberships(ctx(), "migrator", []uint32{10, 11, 12})
	if err != nil {
		t.Fatalf("RoleMemberships: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d memberships, want 2", len(got))
	}
	if !got[10].IsMember || got[10].OwnerName != "app_owner" {
		t.Errorf("owner 10 = %+v", got[10])
	}
	if got[11].IsMember {
		t.Errorf("owner 11 = %+v, want not a member", got[11])
	}
	// 12 has no pg_roles row and is simply absent; ValidateRoleMembership
	// treats that as a violation rather than a pass.
	if _, present := got[12]; present {
		t.Error("an owner with no pg_roles row appeared in the result")
	}

	q := s.recorded()
	if len(q[0].args) != 2 || q[0].args[0] != "migrator" || q[0].args[1] != "10,11,12" {
		t.Errorf("args = %v", q[0].args)
	}
	if !strings.Contains(q[0].sql, "pg_has_role") {
		t.Error("membership is not tested with pg_has_role")
	}
	if !strings.Contains(q[0].sql, "'USAGE'") {
		t.Error("membership must be the has-the-privileges-of test, not bare MEMBER")
	}
}

func TestSQLCatalogRoleMembershipsEmptySetIssuesNoQuery(t *testing.T) {
	s := newStubDB()
	c := NewSQLCatalog(openStub(t, s))

	got, err := c.RoleMemberships(ctx(), "migrator", nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("RoleMemberships(nil) = %v, %v", got, err)
	}
	if len(s.recorded()) != 0 {
		t.Error("an empty owner set still hit the server")
	}
}

// TestSQLCatalogTouchesOnlyAllowedCatalogs pins the read surface. Anything
// outside this list is either unavailable to a non-superuser or a dependency
// the design did not sign up for.
func TestSQLCatalogTouchesOnlyAllowedCatalogs(t *testing.T) {
	allowed := []string{
		"pg_partition_tree", "pg_class", "pg_index", "pg_inherits",
		"pg_constraint", "pg_namespace", "pg_roles", "pg_partitioned_table",
	}
	queries := map[string]string{
		"qReadOnly":           qReadOnly,
		"qCurrentRole":        qCurrentRole,
		"qCurrentDatabase":    qCurrentDatabase,
		"qServerVersion":      qServerVersion,
		"qLookupRelation":     qLookupRelation,
		"qPartitionTree":      qPartitionTree,
		"qPartitionStrategy":  qPartitionStrategy,
		"qIndexesOnRelations": qIndexesOnRelations,
		"qLookupIndex":        qLookupIndex,
		"qRoleMemberships":    qRoleMemberships,
	}
	for label, q := range queries {
		t.Run(label, func(t *testing.T) {
			// Every statement is a SELECT (FR-PLAN-8).
			if !strings.HasPrefix(strings.TrimSpace(q), "SELECT") {
				t.Errorf("query does not begin with SELECT:\n%s", q)
			}
			for _, forbidden := range []string{"INSERT", "UPDATE", "DELETE", "CREATE ", "DROP ", "ALTER "} {
				if strings.Contains(q, forbidden) {
					t.Errorf("query contains %q:\n%s", forbidden, q)
				}
			}
			// Any pg_ table reference must be on the allow-list.
			for _, token := range strings.FieldsFunc(q, func(r rune) bool {
				return !(r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
			}) {
				if !strings.HasPrefix(token, "pg_") {
					continue
				}
				switch token {
				// Catalog-inspection functions, not catalogs.
				case "pg_catalog", "pg_get_expr", "pg_table_is_visible", "pg_has_role", "pg_partition_tree":
					continue
				}
				ok := false
				for _, a := range allowed {
					if token == a {
						ok = true
						break
					}
				}
				if !ok {
					t.Errorf("query references %q, which is outside the allowed catalog surface", token)
				}
			}
		})
	}
}

func TestJoinOIDs(t *testing.T) {
	tests := []struct {
		in   []uint32
		want string
	}{
		{nil, ""},
		{[]uint32{}, ""},
		{[]uint32{16384}, "16384"},
		{[]uint32{1, 2, 3}, "1,2,3"},
		{[]uint32{4294967295}, "4294967295"},
	}
	for _, tc := range tests {
		if got := joinOIDs(tc.in); got != tc.want {
			t.Errorf("joinOIDs(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSQLCatalogSatisfiesTheHost wires the database/sql implementation through
// the host end to end, so the two halves are proven compatible without a
// server.
func TestSQLCatalogSatisfiesTheHost(t *testing.T) {
	s := newStubDB().
		respond(qReadOnly, []string{"ro"}, []driver.Value{true}).
		respond(qCurrentRole, []string{"r"}, []driver.Value{"migrator"}).
		respond(qCurrentDatabase, []string{"d"}, []driver.Value{"appdb"}).
		respond(qServerVersion, []string{"v"}, []driver.Value{int64(160004)}).
		respond(qLookupRelation, relationCols,
			relationRow(100, "public", "orders", "p", 10, "app_owner", 0, 0, "")).
		respond(qPartitionTree, treeCols,
			treeRow(100, 0, false, "public", "orders", "p", 10, "app_owner", 0, 0, ""),
			treeRow(201, 1, true, "public", "orders_2026_01", "r", 10, "app_owner", 131072, 100, "FOR VALUES FROM ('a') TO ('b')"),
			treeRow(202, 1, true, "public", "orders_2026_02", "r", 10, "app_owner", 262144, 100, "FOR VALUES FROM ('b') TO ('c')"),
		).
		respond(qPartitionStrategy, []string{"partstrat"}, []driver.Value{"r"}).
		respond(qRoleMemberships, roleCols, []driver.Value{int64(10), "app_owner", true})

	cat := NewSQLCatalog(openStub(t, s))
	h := &Host{Catalog: cat, NewPlanID: func() protocol.PlanID { return "plan-sql" }}

	out, err := h.Run(ctx(), &stubPlanner{op: protocol.OpCreateIndex}, createSpec())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Topology.LeafCount() != 2 {
		t.Errorf("LeafCount = %d, want 2", out.Topology.LeafCount())
	}
	if err := out.Plan.VerifyDigest(); err != nil {
		t.Errorf("plan is not sealed: %v", err)
	}
	// Estimates must have come from relpages, not from a constant.
	var buildSeconds []int
	for _, n := range out.Plan.Nodes {
		if n.Kind == protocol.KindIndexCreateConcurrently {
			buildSeconds = append(buildSeconds, n.EstimatedSeconds)
		}
	}
	if len(buildSeconds) != 2 || buildSeconds[1] <= buildSeconds[0] {
		t.Errorf("build estimates = %v; the larger partition must estimate longer (FR-PLAN-9)", buildSeconds)
	}
}

// TestSQLCatalogScanFailures: a row this binary cannot interpret must be an
// error, never a zero-valued Relation or Index that silently plans the wrong
// thing.
func TestSQLCatalogScanFailures(t *testing.T) {
	badRelation := []driver.Value{"not-an-oid", "public", "orders", "p", int64(10), "app_owner", int64(0), int64(0), ""}
	badIndex := []driver.Value{"not-an-oid", "public", "i", "i", int64(10), int64(0), int64(100), "public", "t",
		true, true, true, false, false, false, int64(0), "", ""}

	tests := []struct {
		name  string
		build func() *stubDB
		call  func(*SQLCatalog) error
	}{
		{
			name:  "relation",
			build: func() *stubDB { return newStubDB().respond(qLookupRelation, relationCols, badRelation) },
			call: func(c *SQLCatalog) error {
				_, err := c.LookupRelation(ctx(), name("public", "orders"))
				return err
			},
		},
		{
			name: "tree entry",
			build: func() *stubDB {
				return newStubDB().respond(qPartitionTree, treeCols,
					[]driver.Value{"not-an-oid", int64(0), false, "public", "orders", "p", int64(10), "app_owner", int64(0), int64(0), ""})
			},
			call: func(c *SQLCatalog) error { _, err := c.PartitionTree(ctx(), 100); return err },
		},
		{
			name:  "index",
			build: func() *stubDB { return newStubDB().respond(qIndexesOnRelations, indexCols, badIndex) },
			call: func(c *SQLCatalog) error {
				_, err := c.IndexesOnRelations(ctx(), []uint32{100})
				return err
			},
		},
		{
			name:  "index by name",
			build: func() *stubDB { return newStubDB().respond(qLookupIndex, indexCols, badIndex) },
			call:  func(c *SQLCatalog) error { _, err := c.LookupIndex(ctx(), name("public", "i")); return err },
		},
		{
			name: "role membership",
			build: func() *stubDB {
				return newStubDB().respond(qRoleMemberships, roleCols, []driver.Value{"not-an-oid", "app_owner", true})
			},
			call: func(c *SQLCatalog) error {
				_, err := c.RoleMemberships(ctx(), "migrator", []uint32{10})
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewSQLCatalog(openStub(t, tc.build()))
			if err := tc.call(c); !errors.Is(err, ErrCatalogUnavailable) {
				t.Fatalf("err = %v, want ErrCatalogUnavailable", err)
			}
		})
	}
}

// TestSQLCatalogMidStreamFailures: a result set that dies partway through must
// not look like a short but complete answer. A truncated partition list would
// produce a plan that indexes some of the table.
func TestSQLCatalogMidStreamFailures(t *testing.T) {
	boom := errors.New("connection reset mid-result")

	tests := []struct {
		name  string
		build func() *stubDB
		call  func(*SQLCatalog) error
	}{
		{
			name: "relation",
			build: func() *stubDB {
				return newStubDB().failMidStream(qLookupRelation, relationCols, boom,
					relationRow(100, "public", "orders", "p", 10, "app_owner", 0, 0, ""))
			},
			call: func(c *SQLCatalog) error {
				_, err := c.LookupRelation(ctx(), name("public", "orders"))
				return err
			},
		},
		{
			name: "partition tree",
			build: func() *stubDB {
				return newStubDB().failMidStream(qPartitionTree, treeCols, boom,
					treeRow(100, 0, false, "public", "orders", "p", 10, "app_owner", 0, 0, ""))
			},
			call: func(c *SQLCatalog) error { _, err := c.PartitionTree(ctx(), 100); return err },
		},
		{
			name: "indexes",
			build: func() *stubDB {
				return newStubDB().failMidStream(qIndexesOnRelations, indexCols, boom,
					indexRow(900, "public", "i", "i", 10, 0, 100, "public", "t",
						true, true, true, false, false, false, 0, "", ""))
			},
			call: func(c *SQLCatalog) error {
				_, err := c.IndexesOnRelations(ctx(), []uint32{100})
				return err
			},
		},
		{
			name: "index by name",
			build: func() *stubDB {
				return newStubDB().failMidStream(qLookupIndex, indexCols, boom,
					indexRow(900, "public", "i", "i", 10, 0, 100, "public", "t",
						true, true, true, false, false, false, 0, "", ""))
			},
			call: func(c *SQLCatalog) error { _, err := c.LookupIndex(ctx(), name("public", "i")); return err },
		},
		{
			name: "role memberships",
			build: func() *stubDB {
				return newStubDB().failMidStream(qRoleMemberships, roleCols, boom,
					[]driver.Value{int64(10), "app_owner", true})
			},
			call: func(c *SQLCatalog) error {
				_, err := c.RoleMemberships(ctx(), "migrator", []uint32{10})
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewSQLCatalog(openStub(t, tc.build()))
			if err := tc.call(c); !errors.Is(err, ErrCatalogUnavailable) {
				t.Fatalf("err = %v, want ErrCatalogUnavailable", err)
			}
		})
	}
}
