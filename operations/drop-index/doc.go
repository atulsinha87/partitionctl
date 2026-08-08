// Package dropindex implements DropPartitionedIndex: the M2 operation that
// removes a partitioned index and the whole family of leaf indexes attached to
// it (TRD §7.2.13, FR-DROP-1…8).
//
// It is a planner and only a planner. There is no executor in this package and
// there is no operations/drop-index/executor/ directory, because execution is
// the engine's job and an operation that could execute would make the
// separation TRD §17.3 asserts unverifiable. AC-21 is the claim that adding an
// operation means adding one [planner.OperationPlanner] and nothing else; this
// package is that claim's second instance, and structure_test.go is the
// machine-checked form of it. The package therefore issues no DDL, opens no
// transaction, and imports nothing that could (FR-PLAN-8).
//
// # Why the operation exists at all
//
// PostgreSQL has no online way to remove a partitioned index. DROP INDEX
// CONCURRENTLY is rejected on a partitioned index, and there is no ALTER INDEX
// ... DETACH PARTITION, so an attached leaf index cannot be peeled off and
// dropped one partition at a time (TRD §7.2.10, FR-DROP-8). What is left is one
// atomic DROP INDEX that takes AccessExclusiveLock on the parent and on every
// leaf simultaneously. The tool's contribution is not to make that online. It
// is to make the blast radius impossible to miss before it is taken: the plan
// states the lock and the leaf count, the operator must have supplied
// --confirm-exclusive-lock at plan time, and the artifact records that they did
// (FR-DROP-3, FR-DROP-5, AC-13).
//
// # The graph
//
// One catalog.assert; then zero or more index.drop_concurrently nodes, one per
// unattached orphan leaf index found under the generated child names; then
// exactly one index.drop_partitioned; then one index.verify. No wait nodes:
// pacing a single atomic statement is meaningless.
//
// The orphans are the wreckage an abandoned CreatePartitionedIndex leaves
// behind. An unattached leaf index is not a dependency of the partitioned
// parent, so the parent's drop does not cascade to it and it survives as
// garbage under a name the tool generates. Removing it first is what makes the
// drop complete rather than merely successful (TRD §7.2.13 step 1).
//
// # The two rules that are easy to get backwards
//
// An orphan that PartitionCTL cannot prove it created is skipped with a note,
// never halted on. A foreign index sitting under a generated name is somebody
// else's object; refusing to plan because of it would let an unrelated index
// make the operator's own drop impossible.
//
// The drop itself is authorized under [protocol.AuthExplicit] and this package
// never reads the ownership marker to decide it. Explicit intent is the
// authorization: the operator named the index and acknowledged the lock. An
// index PartitionCTL did not create is still droppable, and an unmarked index
// is still droppable.
package dropindex
