// Package executor walks a sealed [protocol.Plan] in topological order and
// dispatches its nodes, one at a time, against a live PostgreSQL database.
//
// It is deliberately dumb (TRD §6.1, §7.2.4). It dispatches on [protocol.NodeKind]
// alone (FR-EXEC-1); it holds no knowledge of *why* a node was emitted, which
// operation produced it, or what the resulting catalog will look like. That
// knowledge lives entirely in the planners, which is what makes AC-21 ("all
// three operations execute on the same executor binary with no
// operation-specific execution, retry, ordering, or state code") checkable by
// reading this package.
//
// # What it guarantees
//
//   - Topological order, one node at a time, never dispatching a node before
//     every predecessor is DONE or SKIPPED (FR-ORD-1, FR-ORD-2). A cyclic graph
//     is refused before any statement runs.
//   - Every node transition is checkpointed to the [StateStore] *before* the
//     executor proceeds (FR-EXEC-2). A checkpoint that fails halts the run.
//   - Destructive nodes have their authorization re-evaluated against live
//     state immediately before dispatch, and the satisfied mode plus its
//     evidence is recorded before the statement runs (FR-AUTH-5, FR-AUTH-6,
//     INV-2). A plan is a proposal, never a permission.
//   - Provenance for an object the executor is about to create is committed
//     before the DDL that creates it (INV-1).
//   - Errors are classified by PostgreSQL SQLSTATE, not by string matching, and
//     only retryable classes are retried, with bounded exponential backoff plus
//     jitter (FR-EXEC-3, FR-EXEC-4).
//   - Every DDL statement carries a finite lock_timeout, and
//     index.create_concurrently never carries a finite statement_timeout
//     (FR-EXEC-5). CONCURRENTLY forms are never issued inside an explicit
//     transaction block (FR-EXEC-6).
//   - Cancellation is observed at node boundaries only and never interrupts an
//     in-flight statement (FR-CLI-10, FR-ORD-4). SIGINT and SIGTERM are handled
//     identically (FR-EXEC-8).
//   - The executor introduces no delays of its own. Pacing arrives only as
//     `wait` nodes emitted by the planner (FR-ORD-3).
//
// # Scope of this build
//
// Seven of the nine node kinds are implemented: the set
// CreatePartitionedIndex emits. index.reindex_concurrently and
// index.drop_partitioned return [ErrUnsupportedNodeKind], raised as a
// pre-flight check before any statement runs. See [SupportedKinds].
//
// # Ports
//
// Everything the executor touches outside itself is an interface, so the whole
// package is unit-testable with in-memory fakes and no live PostgreSQL:
// [SQLExecutor], [StateStore], [CatalogEvaluator], [Clock], [Logger] and the
// optional [Heartbeater]. [DBExecutor] is the database/sql implementation of
// [SQLExecutor]; it imports no driver, so the caller registers one.
//
// # Why in-flight statements are uninterruptible
//
// [Executor.Run] consults its caller's context at node boundaries only. Every
// statement and every checkpoint runs under a context derived with
// [context.WithoutCancel], so a SIGINT arriving during a six-hour CREATE INDEX
// CONCURRENTLY stops the *next* node rather than killing the build in flight. A
// half-killed CIC leaves exactly the INVALID index this tool exists to avoid.
package executor
