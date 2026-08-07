// Package cli is PartitionCTL's command-line surface: the seven commands of
// TRD §7.2.12 and the wiring that joins the five engine packages together.
//
// # What lives here and why
//
// The engine packages are deliberately unaware of each other. engine/planner
// reads a catalog, engine/executor walks a graph, engine/state persists,
// engine/verifier asserts, and operations/create-index emits nodes. Each one
// declares its own narrow consumer-side port for whatever it needs from the
// others, so that each can be unit-tested with in-memory fakes and no live
// PostgreSQL (HANDOFF §3).
//
// The cost of that independence is that the ports do not line up by
// themselves, and joining them is this package's main job. Every adapter is in
// bridge_store.go, bridge_catalog.go, assert.go and operation.go, and each one
// says which mismatch it reconciles. Nothing else in the repository knows about
// more than one engine package at a time, which is what keeps NFR-EXT-1 true:
// adding an operation adds a planner and one [planner.OperationPlanner]
// adapter, and touches no dispatch, retry, ordering or state code.
//
// # Command contracts
//
//	plan --spec <f> -o <plan>   read-only; emits the plan artifact
//	execute <plan>              digest, then fingerprint, then advisory lock, then DDL
//	resume <plan>               adopts an incomplete or orphaned run; the only
//	                            command that performs provenance-authorized cleanup
//	status [<run-id>]           state store only; works with the target unreachable
//	verify <plan>               verifier assertions; issues no DDL
//	render <plan>               offline SQL runbook; --rollback emits the unwind
//	cancel <run-id>             sets the state store's cancellation flag
//
// `render` and `execute --dry-run` are not redundant (TRD §7.2.12). `render`
// is offline and touches nothing. `--dry-run` is a live pre-flight: it verifies
// the digest, recomputes the topology fingerprint against the catalog and tests
// the advisory lock, then prints the dispatch sequence without issuing DDL.
//
// # No driver
//
// Nothing under this package imports a PostgreSQL driver. [App.Connect] and
// [App.OpenStore] take a *sql.DB that main opens, and main is where a driver is
// registered. That is what lets every command be tested against in-memory fakes
// with no server, which is how the exit-code contract (AC-26) is asserted.
//
// # Credentials
//
// No flag accepts a password or a connection string (FR-CLI-8, NFR-SEC-2).
// Connection parameters that are not secret are flags; the DSN and the password
// come only from the environment, the config file, or PostgreSQL's own
// mechanisms (PGPASSWORD, ~/.pgpass, a service file). See [Config].
package cli
