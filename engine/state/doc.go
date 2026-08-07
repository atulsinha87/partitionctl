// Package state implements the StateStore port (TRD §7.2.5, FR-STATE-1…7,
// FR-LOCK-1…4).
//
// The store owns everything the executor must remember across a process death:
// run identity, per-node lifecycle state including the object each node claims,
// destructive-action authorization, the lease that makes an abandoned run
// detectable, an append-only audit trail, and the cancellation flag a live
// executor polls.
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
// INV-1 (amended): a durable record naming the object and the run is committed
// before the DDL that creates the object. That record is the node checkpoint.
// [StateStore.CreateRun] seeds [NodeRecord.Object] from the plan before any
// node runs, and the executor checkpoints READY -> RUNNING before dispatch, so
// a crash can leave an object with a live claim and no ownership marker but
// never the reverse. [ClaimsObject] is what reads that claim back.
//
// The permanent ownership record is a [protocol.Marker] written onto the object
// itself as a COMMENT, which is why this package no longer has a provenance
// table. A record keyed on a name authorizes whatever later occupies that name;
// a record read off the object authorizes only that object (AC-6).
//
// INV-2: [StateStore.RecordAuthorization] has the same shape. The destructive
// statement is a callback that runs only after the justification is committed
// (FR-AUTH-6, AC-20). The record's per-mode required evidence is validated
// before it is written, so an authorization that cites nothing cannot be
// recorded.
//
// INV-3: the audit trail is append-only. There is no update and no delete path
// on [AuditEvent] anywhere in this package's API, and the file implementation
// opens the trail O_APPEND and never rewrites it. Enforcement is the absence of
// a path, not a rule the callers follow.
//
// INV-4: a run in RUNNING whose lease has expired and whose advisory lock is
// unheld is ORPHANED and resumable. [FindOrphans] and [AdoptOrphan] implement
// the detection, and both require a held [Lock] as an argument, which is how
// the advisory-lock half of the invariant is made structural rather than
// conventional.
//
// INV-6: a run is bound to exactly one plan digest for its lifetime. The
// digest is taken from the [protocol.Plan] passed to [StateStore.CreateRun],
// and [RunStore.SetRunStatus], the only other writer of a run record, never
// touches it. There is no other write path.
//
// # Concurrency
//
// A [StateStore] is safe for concurrent use by multiple goroutines in one
// process. Safety across processes is the advisory lock's job (FR-LOCK-1):
// `execute` and `resume` take it before any node runs, so only one process
// mutates a given target's state at a time.
package state
