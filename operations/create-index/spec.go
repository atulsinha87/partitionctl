package createindex

import (
	"github.com/atulsinha/partitionctl/engine/protocol"
)

// Estimation constants behind the per-node duration estimate (FR-PLAN-9).
const (
	// PageBytes is PostgreSQL's default block size, the unit of
	// pg_class.relpages.
	PageBytes = 8192

	// DefaultBuildBytesPerSecond is the assumed effective throughput of an
	// index build, deliberately conservative. The estimate exists to drive the
	// ETA in `status` (FR-ORD-5) and nothing else: no dispatch, retry or
	// timeout decision reads it.
	DefaultBuildBytesPerSecond int64 = 20 << 20

	// catalogOnlySeconds is the estimate for a statement that touches only the
	// catalog: CREATE INDEX ON ONLY, ALTER INDEX ... ATTACH PARTITION, and
	// DROP INDEX CONCURRENTLY once its wait for concurrent transactions is
	// over. None of the three scans data.
	catalogOnlySeconds = 1

	// maxEstimateSeconds keeps an estimate inside a 32-bit int on every
	// platform, so a pathological relpages cannot overflow the plan's total.
	maxEstimateSeconds = 1<<31 - 1

	// maxEstimatePages bounds the arithmetic below well inside int64 even
	// before the clamp above applies. It is far larger than NFR-SCALE-2's
	// 10 TB single partition.
	maxEstimatePages int64 = 1 << 40
)

// Specification is the declarative statement of the desired end state that this
// planner compiles (TRD §17.1). It is the operation's whole input; everything
// else the planner uses it reads from the catalog.
type Specification struct {
	// Database is the target database name, recorded in the plan so a plan
	// cannot be executed against an unintended database by accident. It is
	// never a connection string and never carries credentials (NFR-SEC-3).
	Database string

	// Table is the partitioned parent table. An empty Schema is resolved by
	// the catalog's own resolution of the name.
	Table protocol.ObjectName

	// Index is the partitioned parent index to create. An empty Schema
	// resolves to the table's schema, which is where PostgreSQL puts an index
	// regardless of what the statement asks for.
	Index protocol.ObjectName

	// Definition is the index shape. It is applied identically to the parent
	// index and to every leaf index, which is what makes the leaves attachable.
	Definition protocol.IndexDefinition

	// Role is the connected role. The planner checks it is a member of the
	// owning role of the parent and of every leaf, and records the same check
	// as a precondition assertion for the executor to re-evaluate (FR-PLAN-10,
	// AC-12). It is required: the check is not optional.
	Role string

	// PaceSeconds is the pause the planner emits after each leaf attaches
	// (FR-ORD-3). Zero still emits the wait node, with a zero duration, so the
	// graph's shape never depends on pacing and every pause remains visible in
	// the reviewed artifact.
	PaceSeconds int

	// BuildBytesPerSecond overrides the throughput assumption behind the
	// per-leaf duration estimate. Zero uses [DefaultBuildBytesPerSecond].
	BuildBytesPerSecond int64

	// PlanID names the plan. Empty derives one deterministically from the
	// plan's own identity, so two planning runs over an unchanged catalog
	// produce byte-identical artifacts.
	PlanID protocol.PlanID
}

// Validate checks the specification in isolation, before any catalog access.
func (s Specification) Validate() error {
	if s.Table.IsZero() {
		return protocol.ErrInvalidPlan.Detailf("specification: table is required")
	}
	if err := s.Table.Validate(); err != nil {
		return protocol.ErrInvalidPlan.Detailf("specification: table: %v", err)
	}
	if s.Index.IsZero() {
		return protocol.ErrInvalidPlan.Detailf("specification: index is required")
	}
	if err := s.Index.Validate(); err != nil {
		return protocol.ErrInvalidPlan.Detailf("specification: index: %v", err)
	}
	if err := s.Definition.Validate(); err != nil {
		return protocol.ErrInvalidPlan.Detailf("specification: definition: %v", err)
	}
	if s.Role == "" {
		return protocol.ErrInsufficientPrivilege.Detailf(
			"specification: role is required; the planner must check membership of every owning role (FR-PLAN-10)")
	}
	if err := protocol.ValidateIdentifier(s.Role); err != nil {
		return protocol.ErrInvalidPlan.Detailf("specification: role: %v", err)
	}
	if s.PaceSeconds < 0 {
		return protocol.ErrInvalidPlan.Detailf("specification: pace_seconds is negative: %d", s.PaceSeconds)
	}
	if s.BuildBytesPerSecond < 0 {
		return protocol.ErrInvalidPlan.Detailf(
			"specification: build_bytes_per_second is negative: %d", s.BuildBytesPerSecond)
	}
	return nil
}

// buildRate returns the throughput assumption to estimate with.
func (s Specification) buildRate() int64 {
	if s.BuildBytesPerSecond > 0 {
		return s.BuildBytesPerSecond
	}
	return DefaultBuildBytesPerSecond
}

// estimateBuildSeconds converts pg_class.relpages into a duration estimate for
// one CREATE INDEX CONCURRENTLY (FR-PLAN-9).
//
// CREATE INDEX CONCURRENTLY makes two passes over the table (TRD §7.2.13), so
// the estimate charges twice the relation's size. The result is at least one
// second: a node that is expected to take no time at all would make the ETA
// read as though the run were free.
func estimateBuildSeconds(pages, rate int64) int {
	if rate <= 0 {
		rate = DefaultBuildBytesPerSecond
	}
	if pages < 0 {
		pages = 0
	}
	if pages > maxEstimatePages {
		pages = maxEstimatePages
	}
	work := pages * PageBytes * 2
	seconds := work / rate
	if work%rate != 0 {
		seconds++
	}
	if seconds < catalogOnlySeconds {
		seconds = catalogOnlySeconds
	}
	if seconds > maxEstimateSeconds {
		seconds = maxEstimateSeconds
	}
	return int(seconds)
}
