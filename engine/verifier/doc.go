// Package verifier evaluates PostgreSQL catalog assertions and reports each one
// individually as pass or fail with a human-readable reason.
//
// There is one implementation and several consumers (TRD §7.2.7):
//
//   - the executor, dispatching an index.verify node ([protocol.KindIndexVerify]);
//   - the standalone `verify` CLI command (FR-VER-5, FR-CLI-14);
//   - `verify --json`, via [Report]'s stable field schema (FR-CLI-15, NFR-OBS-2);
//   - the three Liquibase gates (FR-LB-2, FR-LB-3, FR-LB-4), through the
//     catalog-derived entry points in gates.go.
//
// # No DDL, ever
//
// FR-VER-5 requires `verify` to be runnable standalone against a completed plan
// without executing any DDL. That is enforced structurally rather than by
// convention: [Catalog] exposes read methods only, and [SQLCatalog] holds a
// [Queryer] — an interface carrying QueryContext and nothing else — so it cannot
// reach ExecContext even by mistake. Every statement it issues is a SELECT.
//
// The package opens no connections and registers no driver. The caller supplies
// a *sql.DB, *sql.Conn or *sql.Tx. Passing a transaction opened with
// sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead} gives every
// assertion in one report a single consistent catalog snapshot, which is what a
// Liquibase gate wants; the package does not open one itself, because
// transaction scope belongs to the caller.
//
// # Failure is not error
//
// A [Result] carries three statuses, not two. [StatusFail] means the catalog was
// read successfully and the assertion is false: exit 14, [protocol.ErrVerificationFailed].
// [StatusError] means the assertion could not be evaluated at all — the
// connection dropped, the check was malformed — which is a different operational
// situation and a different exit code. Collapsing them would let an unreachable
// database look like a broken index.
//
// # What this package does not own
//
// catalog.assert nodes ([protocol.KindCatalogAssert]) carry [protocol.AssertionKind]
// predicates about topology, strategy, depth and role membership. Those are
// plan-time preconditions belonging to the planner, not end-state assertions, and
// they are deliberately not evaluated here.
//
// `<partitionctlReindexGate>` is a partial consumer only (TRD §7.2.7): this
// package answers its leftover half via [Verifier.VerifyNoLeftovers], but "a
// PartitionCTL run completed at or after `since`" is a StateStore query the
// verifier does not own.
package verifier
