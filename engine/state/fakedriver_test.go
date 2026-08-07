package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// A scripted database/sql driver, so the SQL store can be exercised end to end
// with no PostgreSQL anywhere.
//
// It lives in a _test file on purpose: the library imports no driver, which is
// what keeps the engine offline-testable (TRD §3). Registering one here does
// not change that, and it is what lets the tests assert on the exact SQL and
// arguments the store issues rather than on a mock's expectations.

var (
	fakeRegisterOnce sync.Once
	fakeRegistry     sync.Map // dsn -> *fakeDB
)

// call is one statement the store issued.
type call struct {
	Query string
	Args  []driver.Value
	Kind  string // "exec", "query", "begin", "commit", "rollback", "ping"
}

// reply is a scripted response, matched by substring against the query.
type reply struct {
	match   string
	columns []string
	rows    [][]driver.Value
	err     error
}

type fakeDB struct {
	mu      sync.Mutex
	calls   []call
	replies []reply
}

func (f *fakeDB) record(kind, query string, args []driver.NamedValue) {
	vals := make([]driver.Value, len(args))
	for i, a := range args {
		vals[i] = a.Value
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{Query: query, Args: vals, Kind: kind})
}

func (f *fakeDB) respond(query string) (*fakeRows, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.replies {
		if strings.Contains(query, r.match) {
			if r.err != nil {
				return nil, r.err
			}
			return &fakeRows{cols: r.columns, rows: r.rows}, nil
		}
	}
	return &fakeRows{}, nil
}

// Reply scripts a response for every query containing match.
func (f *fakeDB) Reply(match string, columns []string, rows ...[]driver.Value) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, reply{match: match, columns: columns, rows: rows})
}

// Fail scripts an error for every query containing match.
func (f *fakeDB) Fail(match string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, reply{match: match, err: err})
}

// Calls returns the statements issued so far.
func (f *fakeDB) Calls() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]call, len(f.calls))
	copy(out, f.calls)
	return out
}

// Queries returns just the SQL text of every statement issued.
func (f *fakeDB) Queries() []string {
	var out []string
	for _, c := range f.Calls() {
		if c.Kind == "exec" || c.Kind == "query" {
			out = append(out, c.Query)
		}
	}
	return out
}

// Find returns the first recorded call whose query contains match.
func (f *fakeDB) Find(match string) (call, bool) {
	for _, c := range f.Calls() {
		if strings.Contains(c.Query, match) {
			return c, true
		}
	}
	return call{}, false
}

// CountMatching returns how many recorded calls contain match.
func (f *fakeDB) CountMatching(match string) int {
	n := 0
	for _, c := range f.Calls() {
		if strings.Contains(c.Query, match) {
			n++
		}
	}
	return n
}

func (f *fakeDB) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

type fakeDriver struct{}

func (fakeDriver) Open(dsn string) (driver.Conn, error) {
	v, ok := fakeRegistry.Load(dsn)
	if !ok {
		return nil, fmt.Errorf("fake driver: no database registered for %q", dsn)
	}
	return &fakeConn{db: v.(*fakeDB)}, nil
}

type fakeConn struct{ db *fakeDB }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake driver: Prepare is not used; the store goes through the context API")
}
func (c *fakeConn) Close() error { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *fakeConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.db.record("begin", "BEGIN", nil)
	return &fakeTx{db: c.db}, nil
}

func (c *fakeConn) Ping(ctx context.Context) error {
	c.db.record("ping", "PING", nil)
	return nil
}

func (c *fakeConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.db.record("exec", query, args)
	if _, err := c.db.respond(query); err != nil {
		return nil, err
	}
	return driver.RowsAffected(1), nil
}

func (c *fakeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.db.record("query", query, args)
	rows, err := c.db.respond(query)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

type fakeTx struct{ db *fakeDB }

func (t *fakeTx) Commit() error   { t.db.record("commit", "COMMIT", nil); return nil }
func (t *fakeTx) Rollback() error { t.db.record("rollback", "ROLLBACK", nil); return nil }

type fakeRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

var (
	_ driver.Driver         = fakeDriver{}
	_ driver.Conn           = (*fakeConn)(nil)
	_ driver.ConnBeginTx    = (*fakeConn)(nil)
	_ driver.ExecerContext  = (*fakeConn)(nil)
	_ driver.QueryerContext = (*fakeConn)(nil)
	_ driver.Pinger         = (*fakeConn)(nil)
	_ driver.Rows           = (*fakeRows)(nil)
)

// openFakeDB registers a scripted database and returns it with an *sql.DB.
func openFakeDB(t *testing.T) (*sql.DB, *fakeDB) {
	t.Helper()
	fakeRegisterOnce.Do(func() { sql.Register("partitionctl-fake", fakeDriver{}) })

	fake := &fakeDB{}
	dsn := fmt.Sprintf("fake-%s-%p", t.Name(), fake)
	fakeRegistry.Store(dsn, fake)
	t.Cleanup(func() { fakeRegistry.Delete(dsn) })

	db, err := sql.Open("partitionctl-fake", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// One connection keeps the recorded call order deterministic.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db, fake
}

// openNilDB returns a *sql.DB that is never used, for tests that only inspect
// generated SQL.
func openNilDB(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := openFakeDB(t)
	return db
}
