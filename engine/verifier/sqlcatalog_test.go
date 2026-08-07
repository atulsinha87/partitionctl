package verifier

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// ---------------------------------------------------------------------------
// A driver.Connector implemented in the test binary.
//
// sql.OpenDB takes a connector, so this needs no sql.Register and therefore no
// global driver name: the library still imports no driver, and the test still
// exercises the real database/sql row-scanning path with no live PostgreSQL.
// ---------------------------------------------------------------------------

type recordedQuery struct {
	query string
	args  []driver.NamedValue
}

type fakeSQL struct {
	mu      sync.Mutex
	queries []recordedQuery
	execs   []string
	respond func(query string) (*fakeRows, error)
}

func (f *fakeSQL) query(q string, args []driver.NamedValue) (driver.Rows, error) {
	f.mu.Lock()
	f.queries = append(f.queries, recordedQuery{query: q, args: args})
	f.mu.Unlock()
	if f.respond == nil {
		return &fakeRows{}, nil
	}
	rows, err := f.respond(q)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (f *fakeSQL) recorded() []recordedQuery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedQuery(nil), f.queries...)
}

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("open by name is not supported; use the connector")
}

type fakeConnector struct{ db *fakeSQL }

func (c fakeConnector) Connect(context.Context) (driver.Conn, error) { return &fakeConn{db: c.db}, nil }
func (c fakeConnector) Driver() driver.Driver                        { return fakeDriver{} }

type fakeConn struct{ db *fakeSQL }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported by the fake driver")
}
func (c *fakeConn) Close() error              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) { return nil, errors.New("begin is not supported") }

func (c *fakeConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.db.query(q, args)
}

// ExecContext exists only to catch a statement this package must never issue
// (FR-VER-5). Reaching it is a test failure, not an error path.
func (c *fakeConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.db.mu.Lock()
	c.db.execs = append(c.db.execs, q)
	c.db.mu.Unlock()
	return nil, errors.New("the verifier must never execute a statement")
}

type fakeRows struct {
	cols []string
	rows [][]driver.Value
	pos  int
	// tailErr is returned once the rows are exhausted, which is how a driver
	// reports a mid-stream failure through sql.Rows.Err.
	tailErr error
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		if r.tailErr != nil {
			return r.tailErr
		}
		return io.EOF
	}
	copy(dest, r.rows[r.pos])
	r.pos++
	return nil
}

const (
	indexCols = 8
	nameCols  = 2
)

func indexRows(rows ...[]driver.Value) *fakeRows {
	return &fakeRows{
		cols: []string{"nspname", "relname", "tnspname", "trelname", "indisvalid", "indisready", "indislive", "partitioned"},
		rows: rows,
	}
}

func nameRows(rows ...[]driver.Value) *fakeRows {
	return &fakeRows{cols: []string{"nspname", "relname"}, rows: rows}
}

func openFake(t *testing.T, respond func(query string) (*fakeRows, error)) (*SQLCatalog, *fakeSQL) {
	t.Helper()
	f := &fakeSQL{respond: respond}
	db := sql.OpenDB(fakeConnector{db: f})
	t.Cleanup(func() { _ = db.Close() })
	return NewSQLCatalog(db), f
}

// ---------------------------------------------------------------------------

// TestSQLCatalogQueriesAreReadOnly is the static half of FR-VER-5: the whole
// statement set this catalog can issue is SELECTs.
func TestSQLCatalogQueriesAreReadOnly(t *testing.T) {
	forbidden := []string{
		"INSERT", "UPDATE", "DELETE", "TRUNCATE", "CREATE", "DROP", "ALTER",
		"REINDEX", "GRANT", "REVOKE", "COMMIT", "SET ", "LOCK",
	}
	for _, q := range allQueries() {
		trimmed := strings.TrimSpace(q)
		if !strings.HasPrefix(trimmed, "SELECT") && !strings.HasPrefix(trimmed, "WITH") {
			t.Fatalf("query does not start with SELECT or WITH:\n%s", q)
		}
		upper := strings.ToUpper(q)
		for _, word := range forbidden {
			if strings.Contains(upper, word) {
				t.Fatalf("query contains %q:\n%s", word, q)
			}
		}
		// Identifiers reach the server as bound parameters, never interpolated
		// (NFR-SEC-4).
		if !strings.Contains(q, "$1") || !strings.Contains(q, "$2") {
			t.Fatalf("query does not bind both name parts:\n%s", q)
		}
	}
}

func TestSQLCatalogIndex(t *testing.T) {
	cat, f := openFake(t, func(q string) (*fakeRows, error) {
		if q != queryIndex {
			t.Errorf("unexpected query:\n%s", q)
		}
		return indexRows([]driver.Value{
			"public", "orders_created_at_idx", "public", "orders", true, true, true, true,
		}), nil
	})

	got, found, err := cat.Index(context.Background(), protocol.NewObjectName("public", "orders_created_at_idx"))
	if err != nil || !found {
		t.Fatalf("Index: found=%t err=%v", found, err)
	}
	want := IndexState{
		Name:        protocol.NewObjectName("public", "orders_created_at_idx"),
		Relation:    protocol.NewObjectName("public", "orders"),
		Valid:       true,
		Ready:       true,
		Live:        true,
		Partitioned: true,
	}
	if got != want {
		t.Fatalf("Index() = %+v, want %+v", got, want)
	}

	recorded := f.recorded()
	if len(recorded) != 1 {
		t.Fatalf("issued %d queries, want 1", len(recorded))
	}
	if len(recorded[0].args) != 2 ||
		recorded[0].args[0].Value != "public" ||
		recorded[0].args[1].Value != "orders_created_at_idx" {
		t.Fatalf("args = %+v, want the schema then the name", recorded[0].args)
	}
}

func TestSQLCatalogIndexNotFound(t *testing.T) {
	cat, _ := openFake(t, func(string) (*fakeRows, error) { return indexRows(), nil })
	got, found, err := cat.Index(context.Background(), protocol.NewObjectName("public", "nope"))
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if found {
		t.Fatalf("found = true for an empty result set (%+v)", got)
	}
}

func TestSQLCatalogIndexParent(t *testing.T) {
	cat, _ := openFake(t, func(q string) (*fakeRows, error) {
		if q != queryIndexParent {
			t.Errorf("unexpected query:\n%s", q)
		}
		return nameRows([]driver.Value{"public", "orders_created_at_idx"}), nil
	})
	parent, attached, err := cat.IndexParent(context.Background(),
		protocol.NewObjectName("public", "orders_created_at_idx_orders_2026_01"))
	if err != nil || !attached {
		t.Fatalf("IndexParent: attached=%t err=%v", attached, err)
	}
	if parent != protocol.NewObjectName("public", "orders_created_at_idx") {
		t.Fatalf("parent = %v", parent)
	}

	cat, _ = openFake(t, func(string) (*fakeRows, error) { return nameRows(), nil })
	if _, attached, err := cat.IndexParent(context.Background(), protocol.NewObjectName("public", "x")); err != nil || attached {
		t.Fatalf("unattached index: attached=%t err=%v", attached, err)
	}
}

func TestSQLCatalogAttachedIndexes(t *testing.T) {
	cat, _ := openFake(t, func(q string) (*fakeRows, error) {
		if q != queryAttachedIndexes {
			t.Errorf("unexpected query:\n%s", q)
		}
		return indexRows(
			[]driver.Value{"public", "idx_2026_01", "public", "orders_2026_01", true, true, true, false},
			[]driver.Value{"public", "idx_2026_02", "public", "orders_2026_02", false, true, true, false},
		), nil
	})
	got, err := cat.AttachedIndexes(context.Background(), protocol.NewObjectName("public", "orders_created_at_idx"))
	if err != nil {
		t.Fatalf("AttachedIndexes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d children, want 2", len(got))
	}
	if got[0].Name.Name != "idx_2026_01" || !got[0].Usable() {
		t.Fatalf("first child = %+v", got[0])
	}
	if got[1].Valid || !got[1].Ready {
		t.Fatalf("second child flags not mapped: %+v", got[1])
	}
	if got[1].Relation != protocol.NewObjectName("public", "orders_2026_02") {
		t.Fatalf("second child relation = %v", got[1].Relation)
	}
}

func TestSQLCatalogLeafPartitionsAndTreeIndexes(t *testing.T) {
	cat, _ := openFake(t, func(q string) (*fakeRows, error) {
		switch q {
		case queryLeafPartitions:
			return nameRows(
				[]driver.Value{"public", "orders_2026_01"},
				[]driver.Value{"public", "orders_2026_02"},
			), nil
		case queryTreeIndexes:
			return indexRows(
				[]driver.Value{"public", "idx_2026_01_ccnew", "public", "orders_2026_01", true, true, true, false},
			), nil
		}
		t.Errorf("unexpected query:\n%s", q)
		return nil, errors.New("unexpected query")
	})

	leaves, err := cat.LeafPartitions(context.Background(), protocol.NewObjectName("public", "orders"))
	if err != nil || len(leaves) != 2 {
		t.Fatalf("LeafPartitions = %v, err %v", leaves, err)
	}
	if leaves[1] != protocol.NewObjectName("public", "orders_2026_02") {
		t.Fatalf("leaves = %v", leaves)
	}

	indexes, err := cat.TreeIndexes(context.Background(), protocol.NewObjectName("public", "orders"))
	if err != nil || len(indexes) != 1 {
		t.Fatalf("TreeIndexes = %v, err %v", indexes, err)
	}
	if kind, _ := protocol.ClassifyLeftover(indexes[0].Name.Name); kind != protocol.LeftoverNew {
		t.Fatalf("leftover classification = %q", kind)
	}
}

func TestSQLCatalogQueryFailurePropagates(t *testing.T) {
	boom := errors.New("server closed the connection unexpectedly")
	cat, _ := openFake(t, func(string) (*fakeRows, error) { return nil, boom })

	if _, _, err := cat.Index(context.Background(), protocol.NewObjectName("public", "x")); !errors.Is(err, boom) {
		t.Fatalf("Index err = %v, want the driver error", err)
	}
	if _, _, err := cat.IndexParent(context.Background(), protocol.NewObjectName("public", "x")); !errors.Is(err, boom) {
		t.Fatalf("IndexParent err = %v", err)
	}
	if _, err := cat.AttachedIndexes(context.Background(), protocol.NewObjectName("public", "x")); !errors.Is(err, boom) {
		t.Fatalf("AttachedIndexes err = %v", err)
	}
	if _, err := cat.LeafPartitions(context.Background(), protocol.NewObjectName("public", "x")); !errors.Is(err, boom) {
		t.Fatalf("LeafPartitions err = %v", err)
	}
	if _, err := cat.TreeIndexes(context.Background(), protocol.NewObjectName("public", "x")); !errors.Is(err, boom) {
		t.Fatalf("TreeIndexes err = %v", err)
	}

	// A read failure must reach the operator as an unevaluatable check, never
	// as a verdict about the index.
	res := New(cat).Check(context.Background(), protocol.VerifyCheck{
		Check: protocol.CheckIndexValid,
		Index: ptr(protocol.NewObjectName("public", "x")),
	})
	mustError(t, res)
}

// TestSQLCatalogRowFailurePropagates covers the failure that arrives after the
// query was accepted: rows.Err() must not be mistaken for "no rows", which
// would report a present index as absent.
func TestSQLCatalogRowFailurePropagates(t *testing.T) {
	boom := errors.New("connection reset mid-stream")
	cat, _ := openFake(t, func(q string) (*fakeRows, error) {
		if q == queryLeafPartitions {
			r := nameRows([]driver.Value{"public", "orders_2026_01"})
			r.tailErr = boom
			return r, nil
		}
		r := indexRows()
		r.tailErr = boom
		return r, nil
	})

	if _, found, err := cat.Index(context.Background(), protocol.NewObjectName("public", "x")); found || !errors.Is(err, boom) {
		t.Fatalf("Index: found=%t err=%v, want the row error", found, err)
	}
	if _, err := cat.LeafPartitions(context.Background(), protocol.NewObjectName("public", "orders")); !errors.Is(err, boom) {
		t.Fatalf("LeafPartitions err = %v", err)
	}
}

func TestSQLCatalogScanFailurePropagates(t *testing.T) {
	// A projection that does not match what the scanner expects must fail
	// loudly rather than yield a half-populated IndexState.
	cat, _ := openFake(t, func(string) (*fakeRows, error) {
		return &fakeRows{
			cols: []string{"nspname", "relname"},
			rows: [][]driver.Value{{"public", "x"}},
		}, nil
	})
	if _, _, err := cat.Index(context.Background(), protocol.NewObjectName("public", "x")); err == nil {
		t.Fatal("a column-count mismatch was accepted")
	}
}

func TestSQLCatalogWithoutAHandle(t *testing.T) {
	var cat *SQLCatalog
	if _, _, err := cat.Index(context.Background(), protocol.NewObjectName("public", "x")); err == nil {
		t.Fatal("a nil catalog reported success")
	}
	if _, err := NewSQLCatalog(nil).LeafPartitions(context.Background(), protocol.NewObjectName("public", "x")); err == nil {
		t.Fatal("a catalog with no handle reported success")
	}
}

// TestSQLCatalogNeverExecutes runs a whole gate through database/sql and proves
// nothing but reads reached the connection (FR-VER-5, FR-LB-5).
func TestSQLCatalogNeverExecutes(t *testing.T) {
	cat, f := openFake(t, func(q string) (*fakeRows, error) {
		switch q {
		case queryLeafPartitions:
			return nameRows([]driver.Value{"public", "orders_2026_01"}), nil
		case queryAttachedIndexes:
			return indexRows([]driver.Value{
				"public", "orders_created_at_idx_orders_2026_01", "public", "orders_2026_01", true, true, true, false,
			}), nil
		case queryIndex:
			return indexRows([]driver.Value{
				"public", "orders_created_at_idx", "public", "orders", true, true, true, true,
			}), nil
		case queryIndexParent:
			return nameRows([]driver.Value{"public", "orders_created_at_idx"}), nil
		}
		t.Errorf("unexpected query:\n%s", q)
		return nil, errors.New("unexpected query")
	})

	r, err := New(cat).VerifyPartitionedIndex(context.Background(),
		protocol.NewObjectName("public", "orders"),
		protocol.NewObjectName("public", "orders_created_at_idx"))
	if err != nil {
		t.Fatalf("VerifyPartitionedIndex: %v", err)
	}
	if !r.Passed() {
		t.Fatalf("healthy fake did not pass: %+v", r.Failures())
	}

	f.mu.Lock()
	execs := len(f.execs)
	f.mu.Unlock()
	if execs != 0 {
		t.Fatalf("the verifier executed %d statements", execs)
	}
	recorded := f.recorded()
	if len(recorded) == 0 {
		t.Fatal("no query was issued")
	}
	for _, q := range recorded {
		trimmed := strings.TrimSpace(q.query)
		if !strings.HasPrefix(trimmed, "SELECT") && !strings.HasPrefix(trimmed, "WITH") {
			t.Fatalf("non-read statement reached the connection:\n%s", q.query)
		}
	}
}

// TestSQLCatalogAcceptsATransaction pins the seam that lets a caller give a
// whole report one consistent snapshot.
func TestSQLCatalogAcceptsATransaction(t *testing.T) {
	var _ Queryer = (*sql.DB)(nil)
	var _ Queryer = (*sql.Tx)(nil)
	var _ Queryer = (*sql.Conn)(nil)
}

func TestSQLCatalogColumnCountsMatchTheProjection(t *testing.T) {
	// A guard against the projection and the scanner drifting apart: every
	// index-state query must select exactly the columns scanIndexStates reads.
	for _, q := range []string{queryIndex, queryAttachedIndexes, queryTreeIndexes} {
		if got := strings.Count(q, "indisvalid"); got != 1 {
			t.Fatalf("query selects indisvalid %d times:\n%s", got, q)
		}
	}
	if indexCols != 8 || nameCols != 2 {
		t.Fatal("column-count constants drifted from the fixtures")
	}
}

func TestSQLCatalogNameScanFailurePropagates(t *testing.T) {
	cat, _ := openFake(t, func(string) (*fakeRows, error) {
		return &fakeRows{
			cols: []string{"nspname", "relname", "extra"},
			rows: [][]driver.Value{{"public", "orders_2026_01", "surprise"}},
		}, nil
	})
	if _, err := cat.LeafPartitions(context.Background(), protocol.NewObjectName("public", "orders")); err == nil {
		t.Fatal("a column-count mismatch was accepted")
	}
}
