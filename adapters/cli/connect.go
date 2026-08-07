package cli

import (
	"context"
	"database/sql"
	"sort"
	"strings"

	"github.com/atulsinha/partitionctl/engine/executor"
	"github.com/atulsinha/partitionctl/engine/planner"
	"github.com/atulsinha/partitionctl/engine/protocol"
	"github.com/atulsinha/partitionctl/engine/state"
	"github.com/atulsinha/partitionctl/engine/verifier"
)

// minOpenConns is the floor on the connection pool.
//
// A held advisory lock pins one connection for the life of a run, because a
// session-level lock belongs to a session and returning that connection to the
// pool would hand the lock to whoever borrowed it next. A pool of one therefore
// deadlocks the moment a run takes its lock (engine/state, "Connection
// budget"). Three leaves room for the lock, the state store and the DDL.
const minOpenConns = 3

// openDatabase opens the target handle with the driver main registered.
//
// No package under adapters/ imports a driver. That is what keeps the whole CLI
// testable with in-memory fakes, and it is why a missing driver is reported here
// as an actionable message rather than as a panic from database/sql.
func openDatabase(ctx context.Context, cfg Config) (*sql.DB, error) {
	if !driverRegistered(cfg.Driver) {
		return nil, protocol.ErrFailure.Detailf(
			"no database/sql driver named %q is registered.\n"+
				"partitionctl imports no driver, so the binary's main package must register one, for example:\n"+
				"    import _ \"github.com/lib/pq\"          // registers \"postgres\"\n"+
				"    import _ \"github.com/jackc/pgx/v5/stdlib\" // registers \"pgx\"\n"+
				"registered drivers: %s",
			cfg.Driver, driverList())
	}
	db, err := sql.Open(cfg.Driver, cfg.DataSourceName())
	if err != nil {
		return nil, protocol.ErrFailure.Detailf("opening %s: %v", cfg.Driver, err)
	}
	if db.Stats().MaxOpenConnections > 0 && db.Stats().MaxOpenConnections < minOpenConns {
		db.SetMaxOpenConns(minOpenConns)
	}
	// Ping so an unreachable target fails here, with the connection parameters
	// still in scope, rather than three checks later inside a catalog read.
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, protocol.ErrFailure.Detailf("connecting to the target database: %v", err)
	}
	return db, nil
}

func driverRegistered(name string) bool {
	for _, d := range sql.Drivers() {
		if d == name {
			return true
		}
	}
	return false
}

func driverList() string {
	d := sql.Drivers()
	if len(d) == 0 {
		return "(none)"
	}
	sort.Strings(d)
	return strings.Join(d, ", ")
}

// newSQLTarget wires the three live views over one handle.
//
// The catalog reader and the verifier's catalog are separate because they are
// separate interfaces owned by separate packages, and both are read-only by
// construction: [planner.Querier] carries no Exec and [verifier.Queryer] carries
// only QueryContext, so neither can issue DDL even by mistake (FR-PLAN-8,
// FR-VER-5).
func newSQLTarget(db *sql.DB) (*Target, error) {
	if db == nil {
		return nil, protocol.ErrFailure.Detailf("no database handle: this command needs a connection to the target")
	}
	return &Target{
		Catalog: planner.NewSQLCatalog(db),
		Snapshot: func(ctx context.Context) (planner.CatalogReader, func() error, error) {
			cat, release, err := planner.BeginReadOnly(ctx, db)
			if err != nil {
				return nil, nil, err
			}
			return cat, release, nil
		},
		Verify: verifier.NewSQLCatalog(db),
		SQL:    executor.NewDBExecutor(db),
		Close:  db.Close,
	}, nil
}

// newStateStore selects the state store implementation (FR-STATE-2).
//
// The SQL store is the default and lives in a dedicated schema in the target
// database (FR-STATE-3). The file store exists for operators who want execution
// state outside the target's blast radius, and it is the one that makes `status`
// answerable while the target is unreachable (TRD §7.2.5, FR-CLI-12, AC-25).
func newStateStore(cfg Config, db *sql.DB, intent StoreIntent) (state.StateStore, error) {
	switch cfg.State {
	case StateFile:
		return state.OpenFileStore(cfg.StateDir, state.FileOptions{
			LockTTL: cfg.LeaseTTL,
			Holder:  state.DefaultHolder(),
		})
	case StateSQL:
		if db == nil {
			return nil, protocol.ErrFailure.Detailf(
				"state backend %q needs a connection to the target database; "+
					"use --state file with --state-dir to keep execution state outside it", StateSQL)
		}
		return state.NewSQLStore(db, state.SQLOptions{
			Schema: cfg.StateSchema,
			Holder: state.DefaultHolder(),
			// The SQL store bootstraps its schema lazily, on the first read
			// that needs it, which for `plan` is the provenance lookup. That
			// lookup fires whenever an index is not healthy, i.e. on exactly
			// the post-crash re-plan, and the bootstrap is eighteen DDL
			// statements: CREATE SCHEMA, CREATE TABLE, CREATE FUNCTION, CREATE
			// TRIGGER, REVOKE. Issuing those from `plan` breaks AC-1's "never
			// opens a write transaction" and TRD §7.2.12's "plan connects
			// read-only", and it fails outright for an operator planning as a
			// read-only role or against a hot standby.
			//
			// Suppressing it makes the absent-schema case answer "no
			// provenance", which is the fail-closed direction the planner
			// already wants: with no record to prove ownership it halts on an
			// INVALID index rather than planning its destruction (FR-PLAN-7).
			SkipBootstrap: intent == StoreReadOnly,
		})
	}
	return nil, protocol.ErrFailure.Detailf("unknown state backend %q", cfg.State)
}

// StoreIntent says whether the caller may write execution state.
//
// It exists because `plan` and `execute` need the same store for different
// reasons: `plan` reads provenance and must issue nothing, `execute` owns the
// run and must be able to create its schema.
type StoreIntent int

const (
	// StoreReadWrite is the executing commands: they create runs, checkpoint
	// nodes and append audit events.
	StoreReadWrite StoreIntent = iota
	// StoreReadOnly is `plan`, which reads provenance and writes nothing
	// (AC-1, FR-PLAN-8).
	StoreReadOnly
)
