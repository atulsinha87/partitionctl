// Package createindex compiles a CreatePartitionedIndex specification into a
// PartitionCTL plan (TRD §7.2.13).
//
// # It is a planner and nothing else: there is no executor here
//
// No file in this package opens a connection, issues DDL, renders a statement
// it then sends anywhere, or executes anything. It reads the catalog through
// [CatalogReader], whose implementations run in read-only transactions
// (FR-PLAN-8), and returns a sealed [protocol.Plan]. The absence of an executor
// here is the structural expression of TRD §6.2 that AC-21 asserts, and it is
// the first thing a reviewer should check: `operations/` holds planners
// exclusively (TRD §17.3).
//
// # The graph
//
// The emitted graph is exactly the one in TRD §7.2.13:
//
//  1. one catalog.assert node carrying every precondition,
//  2. one index.create_parent_invalid node (CREATE INDEX ON ONLY <parent>),
//  3. per leaf partition, a chain
//     index.create_concurrently → index.verify → index.attach → wait,
//  4. one final index.verify depending on every leaf chain, asserting the
//     parent index is valid and the leaf index count equals the partition
//     count.
//
// There is no barrier node kind. The final verify is simply a node with N
// incoming edges (TRD §7.2.2).
//
// # Only remaining work is emitted
//
// The planner emits no node for work already complete, which is what makes a
// plan naturally idempotent (FR-PLAN-5) and what makes `resume` converge. A
// leaf whose index is already built, valid and attached contributes no nodes at
// all. A leaf whose index is built and valid but not yet attached contributes
// only verify → attach → wait. When every leaf is complete the plan still
// carries the precondition assert and the final verify, so re-executing it is a
// checked no-op that exits zero (AC-7); [HasWork] reports whether a plan
// contains any DDL at all.
//
// # Destructive cleanup is provenance-gated
//
// Where a leaf index exists but is INVALID, it is the wreckage of an
// interrupted CREATE INDEX CONCURRENTLY and must be dropped before the rebuild.
// The planner emits an index.drop_concurrently node with authorization mode
// [protocol.AuthProvenance] only when a committed provenance record proves
// PartitionCTL created that index (FR-PLAN-6, AC-5). Where no such record
// exists, the planner halts and emits no plan at all rather than plan the
// destruction of an object it cannot prove it created (FR-PLAN-7, AC-6,
// NFR-REL-3). The zero-value [Planner] has no provenance source, so its default
// behaviour is to halt; that is deliberate.
//
// The authorization the planner attaches is a proposal, never a permission. The
// executor re-evaluates it against live state immediately before dispatch
// (FR-AUTH-5, INV-2).
//
// # Determinism
//
// Given the same specification and the same catalog, the planner produces
// byte-identical plans. Partitions are ordered by schema then name, child index
// names come from [protocol.ChildIndexName] (FR-PLAN-11), node IDs are derived
// from relation identities, and an unset [Specification.PlanID] is derived by
// hashing the plan's own identity rather than by drawing randomness.
package createindex
