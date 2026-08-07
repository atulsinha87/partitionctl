package executor

import (
	"context"
	"database/sql"
	"strconv"
	"time"
)

// DBExecutor is the database/sql implementation of [SQLExecutor].
//
// It imports no driver. The caller registers one and hands over an open
// *sql.DB, which is what keeps every package below the CLI offline-testable.
//
// Each statement runs on its own [sql.Conn] in autocommit, never inside an
// explicit transaction block, so every CONCURRENTLY form is legal (FR-EXEC-6).
// Taking a dedicated connection is what makes the session GUCs apply to the
// statement that follows them: on a pooled *sql.DB, a SET and the statement
// after it can land on different connections.
type DBExecutor struct {
	db *sql.DB
}

// NewDBExecutor wraps an open database handle.
func NewDBExecutor(db *sql.DB) *DBExecutor { return &DBExecutor{db: db} }

// Exec applies the session settings and runs the statement.
func (x *DBExecutor) Exec(ctx context.Context, stmt Statement) error {
	if x == nil || x.db == nil {
		return ErrMissingPort.Detailf("DBExecutor has no database handle")
	}
	conn, err := x.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	// lock_timeout is always finite for DDL, so nothing queues indefinitely
	// behind a long transaction (FR-EXEC-5).
	if err := setTimeout(ctx, conn, "lock_timeout", stmt.Settings.LockTimeout); err != nil {
		return err
	}
	// Zero means "disabled", which is exactly what index.create_concurrently
	// requires: it legitimately runs for hours.
	if err := setTimeout(ctx, conn, "statement_timeout", stmt.Settings.StatementTimeout); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, stmt.SQL)
	return err
}

// setTimeout issues SET <name> = <milliseconds>. The value is an integer this
// function computes, never operator input, so there is nothing to quote.
// A sub-millisecond positive duration rounds up to 1ms rather than down to
// "disabled", because 0 means something different and dangerous here.
func setTimeout(ctx context.Context, conn *sql.Conn, name string, d time.Duration) error {
	ms := int64(0)
	if d > 0 {
		ms = d.Milliseconds()
		if ms == 0 {
			ms = 1
		}
	}
	_, err := conn.ExecContext(ctx, "SET "+name+" = "+strconv.FormatInt(ms, 10))
	return err
}
