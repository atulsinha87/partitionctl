package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/atulsinha87/partitionctl/engine/executor"
	"github.com/atulsinha87/partitionctl/engine/planner"
	"github.com/atulsinha87/partitionctl/engine/protocol"
	"github.com/atulsinha87/partitionctl/engine/state"
	"github.com/atulsinha87/partitionctl/engine/verifier"
)

// Target is the CLI's whole view of a live database.
//
// It is a struct of interfaces rather than a connection, which is what lets
// every command be exercised against in-memory fakes with no PostgreSQL. Each
// field is the narrowest surface its consumer declared: the planner's catalog
// reader, the verifier's catalog, and the executor's one-method SQL port.
type Target struct {
	// Catalog reads the catalog for assertion evaluation and for drift
	// detection. Each query sees its own snapshot, which is correct for a check
	// that must describe the catalog *now*.
	Catalog planner.CatalogReader

	// Snapshot opens a reader pinned to one consistent catalog snapshot, and a
	// release function the caller must call. Planning uses it, because a
	// fingerprint computed across a torn view describes a topology that never
	// existed at any instant.
	Snapshot func(ctx context.Context) (planner.CatalogReader, func() error, error)

	// Verify is the verifier's read surface (FR-VER-5).
	Verify verifier.Catalog

	// SQL issues DDL. It is nil for a command that must not.
	SQL executor.SQLExecutor

	// Close releases whatever the target holds.
	Close func() error
}

func (t *Target) close() {
	if t != nil && t.Close != nil {
		_ = t.Close()
	}
}

// App is the command-line application.
//
// Every external dependency is a field so that the whole surface is testable:
// the writers, the clock, the environment, the database handle, the target's
// three views, the state store and the signal handler. The zero value plus
// [App.Stdout] and [App.Stderr] is the production configuration.
type App struct {
	Stdout io.Writer
	Stderr io.Writer

	// Env reads the environment. Nil uses the process environment.
	Env func(string) (string, bool)

	// Now is the clock. Nil uses time.Now.
	Now func() time.Time

	// OpenDB opens the target database handle. Nil uses database/sql with the
	// configured driver, which main is responsible for registering.
	OpenDB func(ctx context.Context, cfg Config) (*sql.DB, error)

	// NewTarget builds the target's views over a handle. Nil uses the real
	// catalog reader, verifier catalog and DDL executor.
	NewTarget func(ctx context.Context, cfg Config, db *sql.DB) (*Target, error)

	// NewStore builds the state store (FR-STATE-1, FR-STATE-2). Nil selects the
	// file or SQL implementation from the configuration.
	//
	// The intent says whether the caller may write: `plan` asks for
	// [StoreReadOnly] so that opening the store cannot bootstrap a schema and
	// turn a read-only planning pass into eighteen DDL statements (AC-1).
	NewStore func(ctx context.Context, cfg Config, db *sql.DB, intent StoreIntent) (state.StateStore, error)

	// Signals installs the SIGINT/SIGTERM handler (FR-EXEC-8). Nil uses
	// [executor.StopOnSignals]. A test replaces it so no handler is installed.
	Signals func(context.Context) (context.Context, context.CancelFunc)

	log *Logger
}

// Main is the whole binary: it runs the command and returns the process exit
// status (FR-CLI-13).
func Main(ctx context.Context, args []string) int {
	app := &App{Stdout: os.Stdout, Stderr: os.Stderr}
	return app.Run(ctx, args)
}

// Run dispatches one command and returns its exit status.
//
// Every failure path funnels through [protocol.ExitCodeFor], so the exit code
// contract of TRD §7.2.12 is a property of the error rather than of the command
// that produced it. That is what keeps a new command from inventing a new
// meaning for exit 11.
func (a *App) Run(ctx context.Context, args []string) int {
	a.normalize()

	if len(args) == 0 {
		a.usage()
		return int(protocol.ExitFailure)
	}
	switch args[0] {
	case "-h", "--help", "help":
		a.usage()
		return int(protocol.ExitSuccess)
	case "version", "--version":
		fmt.Fprintf(a.Stdout, "partitionctl plan-format v%d\n", protocol.PlanFormatVersion)
		return int(protocol.ExitSuccess)
	}

	command := args[0]
	rest := args[1:]

	var err error
	switch command {
	case "plan":
		err = a.cmdPlan(ctx, rest)
	case "execute":
		err = a.cmdExecute(ctx, rest)
	case "resume":
		err = a.cmdResume(ctx, rest)
	case "status":
		err = a.cmdStatus(ctx, rest)
	case "verify":
		err = a.cmdVerify(ctx, rest)
	case "render":
		err = a.cmdRender(ctx, rest)
	case "cancel":
		err = a.cmdCancel(ctx, rest)
	default:
		a.usage()
		return int(protocol.ExitFailure)
	}

	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return int(protocol.ExitFailure)
		}
		code := protocol.ExitCodeFor(err)
		fmt.Fprintf(a.Stderr, "partitionctl %s: %v\n", command, err)
		a.log.Failure(command, err)
		return int(code)
	}
	return int(protocol.ExitSuccess)
}

func (a *App) normalize() {
	if a.Stdout == nil {
		a.Stdout = io.Discard
	}
	if a.Stderr == nil {
		a.Stderr = io.Discard
	}
	if a.Env == nil {
		a.Env = osEnv
	}
	if a.Now == nil {
		a.Now = time.Now
	}
	if a.log == nil {
		// Structured logs go to stderr so that a command's own output stays
		// pipeable: `render` writes SQL and `status --json` writes JSON, and
		// neither should be interleaved with log records (NFR-OBS-2).
		a.log = NewLogger(a.Stderr, a.Now)
	}
}

func (a *App) usage() {
	fmt.Fprint(a.Stderr, usageText)
}

const usageText = `partitionctl - partition-aware online schema evolution for PostgreSQL

usage: partitionctl <command> [flags] [<plan>]

Flags must come before the positional argument: execute [flags] <plan>

commands:
  plan    --spec <file> -o <plan>   read the catalog and emit a plan artifact
  execute <plan>                    verify digest, then fingerprint, then take
                                    the advisory lock, then run the graph
  resume  <plan>                    adopt an incomplete or orphaned run and roll
                                    forward; the only command that performs
                                    provenance-authorized cleanup
  status  [<run-id>]                report run state from the state store alone
  verify  <plan>                    evaluate the verifier's assertions, no DDL
  render  <plan>                    emit an offline SQL runbook
  cancel  <run-id>                  ask a run to stop at its next node boundary

exit codes:
   0 success, or a converged no-op      12 advisory lock held by another run
   1 generic failure                    13 destructive action halted
  10 plan digest mismatch               14 verification failed
  11 topology drift                     15 unsupported topology
                                        16 insufficient privilege

configuration resolves in the order flags, environment, file. No flag accepts a
password or a data source name: use PARTITIONCTL_DSN, the configuration file, or
PostgreSQL's own PGPASSWORD / ~/.pgpass mechanisms.

run 'partitionctl <command> -h' for a command's flags.
`

// ---------------------------------------------------------------------------
// Shared flags and configuration resolution
// ---------------------------------------------------------------------------

// globalFlags registers the configuration flags every command shares and
// returns a function that reports which of them the operator actually set.
//
// Only the keys the operator typed are returned, because FR-CLI-7's precedence
// is meaningless otherwise: a flag left at its default must not shadow an
// environment variable.
func globalFlags(fs *flag.FlagSet) func() map[string]string {
	var (
		configPath       string
		driver           string
		host             string
		port             string
		dbname           string
		user             string
		sslmode          string
		stateKind        string
		stateDir         string
		stateSchema      string
		lockTimeout      string
		buildLockTimeout string
		stmtTimeout      string
		maxAttempts      string
		actor            string
	)
	fs.StringVar(&configPath, "config", "", "configuration file (default: ./partitionctl.yaml, then ~/.config/partitionctl/config.yaml)")
	fs.StringVar(&driver, "driver", "", "database/sql driver name registered by main")
	fs.StringVar(&host, "host", "", "database host")
	fs.StringVar(&port, "port", "", "database port")
	fs.StringVar(&dbname, "dbname", "", "database name")
	fs.StringVar(&user, "user", "", "database role to connect as")
	fs.StringVar(&sslmode, "sslmode", "", "libpq sslmode")
	fs.StringVar(&stateKind, "state", "", "state store: sql or file (FR-STATE-2)")
	fs.StringVar(&stateDir, "state-dir", "", "root directory for the file state store")
	fs.StringVar(&stateSchema, "state-schema", "", "schema for the SQL state store (FR-STATE-3)")
	fs.StringVar(&lockTimeout, "lock-timeout", "", "lock_timeout for every DDL statement, e.g. 5s (FR-EXEC-5)")
	fs.StringVar(&buildLockTimeout, "build-lock-timeout", "",
		"lock_timeout for the CONCURRENTLY statements, which wait for application transactions "+
			"as part of their work; size it above your longest transaction, e.g. 15m (FR-EXEC-5)")
	fs.StringVar(&stmtTimeout, "statement-timeout", "", "statement_timeout where the node kind permits one (FR-EXEC-5)")
	fs.StringVar(&maxAttempts, "max-attempts", "", "retry budget per node per run (FR-EXEC-4)")
	fs.StringVar(&actor, "actor", "", "who is running this, recorded in the audit trail")

	return func() map[string]string {
		set := make(map[string]string)
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "config":
				set["__config"] = f.Value.String()
			case "driver", "host", "port", "dbname", "user", "sslmode",
				"state", "state-dir", "state-schema", "actor":
				set[strings.ReplaceAll(f.Name, "-", "_")] = f.Value.String()
			case "lock-timeout":
				set["lock_timeout"] = f.Value.String()
			case "build-lock-timeout":
				set["build_lock_timeout"] = f.Value.String()
			case "statement-timeout":
				set["statement_timeout"] = f.Value.String()
			case "max-attempts":
				set["max_attempts"] = f.Value.String()
			}
		})
		return set
	}
}

// config resolves the effective configuration from the flags the operator set,
// the environment, and the configuration file (FR-CLI-7).
func (a *App) config(flags map[string]string) (Config, error) {
	path := flags["__config"]
	delete(flags, "__config")

	explicit := path != ""
	if path == "" {
		path = findConfigFile()
	}

	var (
		file map[string]string
		err  error
	)
	if path != "" {
		file, err = LoadConfigFile(path)
		if err != nil {
			if explicit || !os.IsNotExist(err) {
				return Config{}, protocol.ErrFailure.Detailf("configuration file %s: %v", path, err)
			}
			path, file = "", nil
		}
	}

	cfg, err := resolveConfig(Defaults(), flags, a.Env, file, path)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// ---------------------------------------------------------------------------
// Ports
// ---------------------------------------------------------------------------

// openDB opens the target database handle.
func (a *App) openDB(ctx context.Context, cfg Config) (*sql.DB, error) {
	if a.OpenDB != nil {
		return a.OpenDB(ctx, cfg)
	}
	return openDatabase(ctx, cfg)
}

// openTarget opens the live target: the catalog reader, the verifier's catalog
// and the DDL executor.
func (a *App) openTarget(ctx context.Context, cfg Config, db *sql.DB) (*Target, error) {
	if a.NewTarget != nil {
		return a.NewTarget(ctx, cfg, db)
	}
	return newSQLTarget(db)
}

// openStore opens the state store for a command that will write to it
// (FR-STATE-1).
func (a *App) openStore(ctx context.Context, cfg Config, db *sql.DB) (state.StateStore, error) {
	return a.openStoreWithIntent(ctx, cfg, db, StoreReadWrite)
}

// openReadOnlyStore opens the state store for `plan`, which reads provenance
// and must issue nothing (AC-1, FR-PLAN-8).
func (a *App) openReadOnlyStore(ctx context.Context, cfg Config, db *sql.DB) (state.StateStore, error) {
	return a.openStoreWithIntent(ctx, cfg, db, StoreReadOnly)
}

func (a *App) openStoreWithIntent(ctx context.Context, cfg Config, db *sql.DB, intent StoreIntent) (state.StateStore, error) {
	if a.NewStore != nil {
		return a.NewStore(ctx, cfg, db, intent)
	}
	return newStateStore(cfg, db, intent)
}

// signals installs the SIGINT/SIGTERM handler (FR-EXEC-8).
func (a *App) signals(ctx context.Context) (context.Context, context.CancelFunc) {
	if a.Signals != nil {
		return a.Signals(ctx)
	}
	return executor.StopOnSignals(ctx)
}

// ---------------------------------------------------------------------------
// Shared command helpers
// ---------------------------------------------------------------------------

// loadPlan reads a plan artifact and verifies its digest (FR-PLANFILE-3, AC-2).
//
// The digest is checked before anything else touches the plan, in every command
// that reads one, because a plan whose digest does not describe it is not a
// plan: it is an edited file, and threat T1 is exactly that.
func loadPlan(path string) (*protocol.Plan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, protocol.ErrFailure.Detailf("reading plan %s: %v", path, err)
	}
	plan, err := protocol.DecodePlan(data)
	if err != nil {
		return nil, err
	}
	if err := plan.VerifyDigest(); err != nil {
		return nil, err
	}
	return plan, nil
}

// requirePositional pulls exactly one positional argument.
//
// The len > 1 case is almost always one specific operator mistake, so it is
// named rather than reported as a count. Go's flag package stops parsing at the
// first non-flag token, so `partitionctl execute build/migration.plan -driver
// postgres -state file` leaves all five flag tokens in Args() and the generic
// message reads "expected exactly one <plan>, got 6" while listing the flags as
// if they were plan files. That is the exact line `plan` tells the operator to
// run, with the connection flags they need appended to it.
func requirePositional(fs *flag.FlagSet, name string) (string, error) {
	args := fs.Args()
	if len(args) == 0 {
		return "", protocol.ErrFailure.Detailf("%s is required", name)
	}
	if len(args) > 1 {
		for _, a := range args[1:] {
			if strings.HasPrefix(a, "-") {
				return "", protocol.ErrFailure.Detailf(
					"flags must come before %s: got %s. Go's flag parser stops at the first "+
						"non-flag argument, so everything after %s was treated as another %s. "+
						"Re-run as: partitionctl <command> [flags] %s",
					name, strings.Join(args, " "), args[0], name, name)
			}
		}
		return "", protocol.ErrFailure.Detailf(
			"expected exactly one %s, got %d: %s", name, len(args), strings.Join(args, " "))
	}
	return args[0], nil
}

// lockKeyFor is the advisory-lock key for a plan's target (FR-LOCK-1).
//
// The key is the target table and not the plan digest, deliberately: two
// different plans against the same table must exclude each other, which is what
// AC-10 asserts.
func lockKeyFor(plan *protocol.Plan) state.LockKey {
	return state.LockKey{Database: plan.Target.Database, Table: plan.Target.Table}
}

// discoverLive re-discovers the plan's target from the live catalog, inside one
// read-only snapshot, and returns the topology to fingerprint against.
func discoverLive(ctx context.Context, tgt *Target, plan *protocol.Plan) (planner.Topology, error) {
	read, release, err := tgt.snapshot(ctx)
	if err != nil {
		return planner.Topology{}, err
	}
	defer func() { _ = release() }()
	return planner.Discover(ctx, read, plan.Target.Table)
}

// snapshot opens a consistent read-only catalog view, falling back to the
// per-query reader when the target does not offer one.
func (t *Target) snapshot(ctx context.Context) (planner.CatalogReader, func() error, error) {
	if t.Snapshot != nil {
		return t.Snapshot(ctx)
	}
	if t.Catalog == nil {
		return nil, nil, protocol.ErrFailure.Detailf("no catalog reader is configured")
	}
	return t.Catalog, func() error { return nil }, nil
}

// checkDatabaseIdentity refuses to run a plan against a database other than the
// one it was planned against (T8).
//
// [protocol.Target] records the database explicitly "so that a plan cannot be
// executed against an unintended database by accident", but nothing compared it
// to the live connection: current_database() was read at plan time and never
// again. Every downstream scope is then keyed on the plan's *claimed* name, and
// one of them authorizes destruction. `provenanceLookup` filters provenance by
// that name, so a plan that says "staging" run against production evaluates the
// FR-AUTH-2 gate against staging's records and can authorize dropping a
// same-named production index. The advisory lock key has the same shape, so two
// runs against one physical database via plans naming different databases would
// not exclude each other (FR-LOCK-1).
//
// The topology fingerprint does not cover this. It is built from OIDs, and
// `CREATE DATABASE staging TEMPLATE prod` is a physical copy that preserves
// every pg_class OID, so the fingerprints are byte-identical and the drift check
// passes silently.
//
// There is deliberately no --allow-drift escape. Drift is a statement about the
// shape of a tree that is still the right tree; this is a statement that the
// connection is pointed somewhere else entirely.
func (a *App) checkDatabaseIdentity(ctx context.Context, tgt *Target, plan *protocol.Plan) error {
	if tgt == nil || tgt.Catalog == nil {
		return protocol.ErrFailure.Detailf("no catalog reader is configured")
	}
	if plan.Target.Database == "" {
		return protocol.ErrTopologyDrift.Detailf(
			"the plan names no target database, so it cannot be bound to this connection (FR-PLANFILE-4, T8). " +
				"Re-plan: `plan` records current_database() in the artifact")
	}
	live, err := tgt.Catalog.CurrentDatabase(ctx)
	if err != nil {
		return err
	}
	if live != plan.Target.Database {
		return protocol.ErrTopologyDrift.Detailf(
			"this plan was built against database %q but the connection is to %q. "+
				"Refusing: provenance, the advisory lock and every run record are scoped by the "+
				"database name, so running it here would evaluate the destructive-action gate "+
				"against another database's records (T8). This is not overridable by --allow-drift, "+
				"which is about the shape of the tree rather than which database it is in",
			plan.Target.Database, live)
	}
	return nil
}

// checkTopology recomputes the topology fingerprint against the live catalog
// and reports drift (FR-PLANFILE-5, AC-3).
//
// --allow-drift downgrades the refusal to a named warning. The drift is always
// enumerated, because "something changed" is not actionable and "partition
// orders_2026_04 was added" is.
func (a *App) checkTopology(ctx context.Context, tgt *Target, plan *protocol.Plan, allowDrift bool) error {
	topo, err := discoverLive(ctx, tgt, plan)
	if err != nil {
		return err
	}
	live := topo.Input()
	if err := plan.VerifyTopology(live); err != nil {
		changes := describeDrift(plan, live)
		if !allowDrift {
			return protocol.ErrTopologyDrift.Detailf(
				"the catalog has changed since planning:\n%s\nre-plan, or pass --allow-drift to proceed anyway",
				changes)
		}
		fmt.Fprintf(a.Stderr, "warning: proceeding under --allow-drift; the catalog has changed since planning:\n%s\n", changes)
	}
	return nil
}

// describeDrift names the drift concretely enough to act on (AC-3).
//
// "Something changed" is not actionable and "partition public.orders_2026_04
// was added" is. A plan that records the tree it was fingerprinted over
// (format version 2 and later) is diffed against the live one field by field,
// so the refusal lists exactly what moved. At AC-1's stated scale of 400
// partitions that is the difference between a one-line answer and a list the
// operator has to diff by hand.
//
// A v1 plan carries no tree, so there is nothing to diff and the refusal falls
// back to reporting both fingerprints and the live partition set.
func describeDrift(plan *protocol.Plan, live protocol.TopologyInput) string {
	var b strings.Builder
	fingerprint, err := live.Fingerprint()
	if err != nil {
		fingerprint = "unavailable: " + err.Error()
	}
	fmt.Fprintf(&b, "  planned fingerprint: %s\n", plan.TopologyFingerprint)
	fmt.Fprintf(&b, "  live fingerprint:    %s\n", fingerprint)

	if plan.Topology != nil {
		changes := protocol.DiffTopology(*plan.Topology, live)
		if len(changes) == 0 {
			// The fingerprints differ but no field-level change is visible.
			// Say so plainly rather than printing an empty list.
			fmt.Fprintf(&b, "  the fingerprints differ but no structural change was found; "+
				"the plan may have been produced by a different build")
			return b.String()
		}
		fmt.Fprintf(&b, "  %d change(s) since planning:\n", len(changes))
		for _, c := range changes {
			fmt.Fprintf(&b, "    %-22s %s", c.Change, c.Relation)
			if c.OID != 0 {
				fmt.Fprintf(&b, " (oid %d)", c.OID)
			}
			if c.Detail != "" {
				fmt.Fprintf(&b, ": %s", c.Detail)
			}
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "  planned %d partition(s), live %d",
			len(plan.Topology.Partitions), len(live.Partitions))
		return b.String()
	}

	fmt.Fprintf(&b, "  live root:           %s (oid %d, strategy %s)\n",
		live.Root, live.Root.OID, live.Strategy)
	names := make([]string, 0, len(live.Partitions))
	for _, p := range live.Partitions {
		names = append(names, fmt.Sprintf("%s (oid %d)", p, p.OID))
	}
	sort.Strings(names)
	fmt.Fprintf(&b, "  live partitions (%d): %s\n", len(names), strings.Join(names, ", "))
	b.WriteString("  this plan predates format version 2 and records no topology, so the change " +
		"cannot be named; re-plan to get a named diff (AC-3)")
	return b.String()
}
