// Package reindexindex plans ReindexPartitionedIndex: rebuilding every leaf
// index under a partitioned index, online, one leaf at a time (M3).
//
// # Why per leaf, when the parent form works
//
// This is the counter-intuitive part of the operation and it is written down
// here so that a later reader does not "simplify" it away.
//
// REINDEX INDEX CONCURRENTLY on the *partitioned parent* works. The v0.0 spike
// measured it succeeding on both PostgreSQL 14.23 and 17.10: PostgreSQL loops
// the partitions itself, one per transaction, at ShareUpdateExclusiveLock.
// FR-REIDX-2's claim that PostgreSQL rejects the parent form is simply wrong,
// and the measurement overrides the TRD.
//
// The planner declines to use it anyway, for one reason: the parent form has no
// "already fresh" concept. A re-run after an interruption rebuilds all 400
// partitions from the beginning. That destroys resume, destroys the ETA,
// destroys pacing, and makes FR-PLAN-5 unsatisfiable. Per-leaf buys exactly
// those four things — and those four things are the entire value this tool adds
// over typing the statement by hand. A one-statement plan would be a wrapper
// around psql.
//
// There is therefore no runtime branching and no server_version_num fork: both
// paths work on every supported version, so the choice is made once, here, on
// operational grounds rather than on capability grounds.
//
// # Leftovers
//
// A failed REINDEX CONCURRENTLY leaves a transient index behind, and which one
// it leaves says what happened (TRD §7.2.11):
//
//   - _ccnew: the rebuild failed and the original is intact. Drop the leftover
//     and still reindex the leaf (FR-REIDX-3).
//   - _ccold: the rebuild succeeded and the old copy could not be dropped. Drop
//     the leftover and treat the leaf as already complete (FR-REIDX-4).
//
// Both are resolved at plan time, from the catalog, per leaf. Both are
// unattached ordinary indexes, so DROP INDEX CONCURRENTLY clears them fully
// online: reindex recovery never needs an AccessExclusiveLock.
//
// PostgreSQL appends a disambiguating integer when the plain suffix is taken
// (_ccnew1, _ccold2), so detection goes through [protocol.ClassifyLeftover] and
// never through a literal string compare. Matching the name is not by itself
// authorization: [protocol.DecideLeftoverDrop] also requires the *base* index to
// carry a PartitionCTL ownership marker, and a leftover that fails that halts
// the whole plan (FR-REIDX-5, AC-19).
//
// # The completion marker
//
// After each successful REINDEX INDEX CONCURRENTLY the executor rewrites the
// leaf index's comment with `reindexed` and `reindex_run` set. That makes "was
// this leaf reindexed since T?" a catalog question, which is what lets
// [Planner.ReindexSince] skip already-fresh leaves and what makes a
// 400-partition reindex resumable across days. With no watermark supplied every
// leaf is reindexed, because an operator who asked for a reindex asked for a
// reindex.
package reindexindex
