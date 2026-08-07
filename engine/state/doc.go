// Package state implements the StateStore port (TRD §7.2.5, FR-STATE-1…7,
// FR-LOCK-1…4).
//
// The store owns everything the executor must remember across a process death:
// run identity, per-node lifecycle state, provenance, destructive-action
// authorization, the lease that makes an abandoned run detectable, an
// append-only audit trail, and the cancellation flag a live executor polls.
//
// # Two implementations
//
//   - [FileStore] keeps state on the local filesystem. Every record is written
//     to a temporary file, fsynced, and renamed into place, so a crash at any
//     instant leaves either the previous record or the new one and never a
//     half-written one. It is the store used by the unit tests, and the store
//     an operator picks when they want execution state outside the target
//     database's blast radius (TRD §7.2.5).
//
//   - [SQLStore] keeps state in a dedicated schema in the target database,
//     default [DefaultSchema], created on first use (FR-STATE-3). It talks to
//     database/sql and imports no driver: the caller registers one. This is
//     the default store, mirroring where Liquibase keeps DATABASECHANGELOG.
//
// # Invariants enforced here, in code
//
// INV-1: a provenance row is committed before the DDL that creates the object
// it describes. [StateStore.WriteProvenance] takes the DDL as a callback and
// runs it only after the record is durably committed, so the wrong order is
// not expressible. The record is never rolled back when the DDL fails: an
// INVALID index left behind by a failed CREATE INDEX CONCURRENTLY is exactly
// the object resume must be able to prove it owns (FR-PLAN-6, AC-5).
//
// INV-2: [StateStore.RecordAuthorization] has the same shape. The destructive
// statement is a callback that runs only after the justification is committed
// (FR-AUTH-6, AC-20). The record's per-mode required evidence is validated
// before it is written, so an authorization that cites nothing cannot be
// recorded.
//
// INV-3: the audit trail is append-only. There is no update or delete path on
// [AuditEvent] in this package's API, the file implementation opens the trail
// O_APPEND and never rewrites it, and the SQL schema installs a BEFORE UPDATE
// OR DELETE trigger that raises.
//
// INV-4: a run in RUNNING whose lease has expired and whose advisory lock is
// unheld is ORPHANED and resumable. [FindOrphans] and [AdoptOrphan] implement
// the detection, and both require a held [Lock] as an argument, which is how
// the advisory-lock half of the invariant is made structural rather than
// conventional.
//
// INV-6: a run is bound to exactly one plan digest for its lifetime. The
// digest is taken from the [protocol.Plan] passed to
// [StateStore.CreateRun] and there is no API that changes it. The SQL schema
// additionally installs a trigger that raises if an UPDATE would change it.
//
// # Concurrency
//
// A [StateStore] is safe for concurrent use by multiple goroutines in one
// process. Safety across processes is the advisory lock's job (FR-LOCK-1):
// `execute` and `resume` take it before any node runs, so only one process
// mutates a given target's state at a time.
package state
