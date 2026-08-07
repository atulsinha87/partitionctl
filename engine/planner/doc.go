// Package planner is the planner host and the catalog reader it plans against
// (TRD §7.2.1).
//
// It owns four things:
//
//  1. [CatalogReader]: the read-only catalog surface, plus [SQLCatalog], a
//     database/sql implementation that speaks only to pg_partition_tree,
//     pg_class, pg_index, pg_inherits, pg_constraint, pg_namespace, pg_roles
//     and pg_partitioned_table.
//  2. Discovery and validation: [Discover] walks the partition tree via
//     pg_partition_tree() and rejects every topology v0.1 does not support,
//     with a distinct [TopologyCode] per reason (FR-PLAN-1, FR-PLAN-2,
//     FR-PLAN-3, AC-11).
//  3. The plan-time safety checks that must fail before a multi-hour run
//     starts rather than during it: role membership against relation ownership
//     ([ValidateRoleMembership], FR-PLAN-10, AC-12) and existing-index state
//     per leaf ([InspectChildren], FR-PLAN-4).
//  4. The [OperationPlanner] interface an operation implements and the [Host]
//     that runs one, assembles the [protocol.Plan], computes the topology
//     fingerprint and seals the digest.
//
// # The planner issues no DDL, ever
//
// Every statement this package sends is a SELECT (FR-PLAN-8). [BeginReadOnly]
// is the supported way to build a [SQLCatalog]: it opens a READ ONLY,
// REPEATABLE READ transaction, so the whole discovery pass sees one catalog
// snapshot. A torn view would be worse than a stale one, because the topology
// fingerprint would describe a partition set that never existed at any instant.
// [Host.Run] asks the reader to prove it is read-only when the reader can
// ([ReadOnlyAsserter]).
//
// # Where the work is divided
//
// This package knows about partition trees, indexes, roles and page counts. It
// does not know what any particular operation wants to do with them. An
// operation planner in operations/ receives a [Request] with discovery,
// privilege validation and estimation already done, and returns nodes. The host
// owns plan identity, the fingerprint and the digest, so an operation cannot
// forget to seal a plan.
//
// # Unit-testable without PostgreSQL
//
// [FakeCatalog] is a complete in-memory [CatalogReader]. It derives the
// partition tree from ParentOID links the same way pg_partition_tree() does, so
// a test declares relations and gets a tree. Nothing in this package needs a
// live server to test.
package planner
