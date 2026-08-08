// Package createindex emits the CreatePartitionedIndex graph (TRD §7.2.13).
//
// # It is a planner and nothing else: there is no executor here
//
// No file in this package opens a connection, issues DDL, or executes anything.
// Its only database access is [planner.Request.Catalog], which runs in a
// read-only transaction the host has already proved (FR-PLAN-8). The absence of
// an executor here is the structural expression of TRD §6.2 that AC-21 asserts,
// and it is the first thing a reviewer should check: `operations/` holds
// planners exclusively (TRD §17.3).
//
// # It declares no seams of its own
//
// [Planner] implements [planner.OperationPlanner] directly. It defines no
// catalog interface, no specification type, no claim interface and no SQL
// renderer, because engine/planner already owns all four and engine/protocol
// owns the renderer. That is what NFR-EXT-1 means in practice: an operation is
// one file's worth of "which nodes do I emit", and everything that must not be
// forgotten — the read-only proof (FR-PLAN-8), the version gate
// (NFR-COMPAT-1), discovery and topology rejection (FR-PLAN-1..3), role
// membership (FR-PLAN-10), plan identity, the fingerprint and the digest —
// lives in the host, where an operation cannot skip it.
//
// # The graph
//
//  1. one catalog.assert node carrying every precondition,
//  2. one index.create_parent_invalid node (CREATE INDEX ON ONLY <parent>),
//     emitted only when the parent index is not already there,
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
// checked no-op that exits zero (AC-7).
//
// # Destructive cleanup is marker-gated
//
// Where a leaf index exists but is unusable, it is the wreckage of an
// interrupted CREATE INDEX CONCURRENTLY and must be dropped before the rebuild.
// The decision is [planner.DecideCleanup] — the same function `resume` uses —
// and it drops only what PartitionCTL's ownership marker on the object itself
// proves is ours (FR-PLAN-6, AC-5). Otherwise the planner halts and emits no
// nodes at all rather than plan the destruction of an object it cannot prove it
// created (FR-PLAN-7, AC-6, NFR-REL-3).
//
// The authorization the planner attaches is a proposal, never a permission. The
// executor re-evaluates it against live state immediately before dispatch
// (FR-AUTH-5, INV-2).
//
// # Determinism
//
// Given the same request, the planner produces byte-identical node sequences.
// Leaves arrive from [planner.Topology] already sorted by schema then name,
// child index names come from [protocol.ChildIndexNamesQualified] (FR-PLAN-11),
// and node IDs are derived from relation identities. The plan's identity,
// timestamp and digest belong to the host.
package createindex
