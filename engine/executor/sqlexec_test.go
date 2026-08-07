package executor

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// ---------------------------------------------------------------------------
// A minimal in-process driver, so DBExecutor is testable without PostgreSQL.
// The library imports no driver; this one exists only for this test file.
// ---------------------------------------------------------------------------

type recordingDriver struct {
	mu      sync.Mutex
	queries []string
	begins  int
	failOn  string
}

func (d *recordingDriver) Open(string) (driver.Conn, error) { return &recordingConn{d: d}, nil }

func (d *recordingDriver) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.queries))
	copy(out, d.queries)
	return out
}

type recordingConn struct{ d *recordingDriver }

func (c *recordingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("not implemented")
}
func (c *recordingConn) Close() error { return nil }

func (c *recordingConn) Begin() (driver.Tx, error) {
	c.d.mu.Lock()
	c.d.begins++
	c.d.mu.Unlock()
	return nil, errors.New("this executor never opens a transaction")
}

func (c *recordingConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.d.mu.Lock()
	c.d.queries = append(c.d.queries, query)
	fail := c.d.failOn
	c.d.mu.Unlock()
	if fail != "" && query == fail {
		return nil, &pgErr{code: "42501", msg: "permission denied"}
	}
	return driver.RowsAffected(0), nil
}

// driverSeq keeps each test's driver registration unique; database/sql panics
// on a duplicate name and offers no way to unregister.
var driverSeq atomic.Int64

func newTestDB(t *testing.T) (*sql.DB, *recordingDriver) {
	t.Helper()
	d := &recordingDriver{}
	name := fmt.Sprintf("partitionctl-test-%d", driverSeq.Add(1))
	sql.Register(name, d)
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, d
}

func TestDBExecutorAppliesSessionSettingsThenTheStatement(t *testing.T) {
	db, rec := newTestDB(t)
	x := NewDBExecutor(db)

	err := x.Exec(context.Background(), Statement{
		NodeID:   "n3",
		Kind:     protocol.KindIndexCreateConcurrently,
		SQL:      `CREATE INDEX CONCURRENTLY "i" ON "public"."p" ("a")`,
		Settings: SessionSettings{LockTimeout: 3 * time.Second, StatementTimeout: 0},

		MustRunOutsideTransaction: true,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	want := []string{
		"SET lock_timeout = 3000",
		// Zero means disabled, which is what FR-EXEC-5 requires for CIC.
		"SET statement_timeout = 0",
		`CREATE INDEX CONCURRENTLY "i" ON "public"."p" ("a")`,
	}
	if got := rec.seen(); !reflect.DeepEqual(got, want) {
		t.Fatalf("statements =\n  %v\nwant\n  %v", got, want)
	}
	if rec.begins != 0 {
		t.Fatalf("the executor opened %d transactions; CONCURRENTLY forms may not run in one (FR-EXEC-6)", rec.begins)
	}
}

func TestDBExecutorRoundsSubMillisecondTimeoutsUp(t *testing.T) {
	db, rec := newTestDB(t)
	x := NewDBExecutor(db)

	if err := x.Exec(context.Background(), Statement{
		SQL:      "SELECT 1",
		Settings: SessionSettings{LockTimeout: 100 * time.Microsecond, StatementTimeout: 90 * time.Second},
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := rec.seen()
	// Rounding down would silently disable the timeout, which is the one
	// outcome this setting must never produce by accident.
	if got[0] != "SET lock_timeout = 1" {
		t.Fatalf("lock_timeout statement = %q, want 1ms", got[0])
	}
	if got[1] != "SET statement_timeout = 90000" {
		t.Fatalf("statement_timeout statement = %q", got[1])
	}
}

func TestDBExecutorSurfacesTheDriverError(t *testing.T) {
	db, rec := newTestDB(t)
	rec.failOn = "CREATE INDEX"
	x := NewDBExecutor(db)

	err := x.Exec(context.Background(), Statement{
		SQL:      "CREATE INDEX",
		Settings: SessionSettings{LockTimeout: time.Second},
	})
	if err == nil {
		t.Fatal("expected the driver error to surface")
	}
	// And it stays classifiable: the SQLSTATE survives database/sql.
	if got := Classify(err); got.SQLState != "42501" || got.Class != ClassTerminal {
		t.Fatalf("Classify = %+v, want terminal 42501", got)
	}
}

func TestDBExecutorWithoutAHandle(t *testing.T) {
	var x *DBExecutor
	if err := x.Exec(context.Background(), Statement{SQL: "SELECT 1"}); !errors.Is(err, ErrMissingPort) {
		t.Fatalf("error = %v, want ErrMissingPort", err)
	}
}
