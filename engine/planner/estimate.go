package planner

// Estimator turns pg_class.relpages into the per-node duration estimates the
// plan records (FR-PLAN-9).
//
// Two properties matter more than accuracy. First, the arithmetic is integer
// only, so the same catalog produces the same estimate in every process and the
// plan digest is reproducible. Second, the estimate drives the ETA in `status`
// and nothing else (FR-ORD-5): no scheduling, no timeout, no retry decision
// reads it, so an estimate that is wrong by 3x wastes an operator's patience
// and never corrupts a run.
//
// relpages is a statistic maintained by VACUUM and ANALYZE, not a measurement.
// On a table that has never been analyzed it is 0, which is why the estimate
// has a floor rather than a proportional model alone.
type Estimator struct {
	// PageBytes is the server's block size. 8192 unless the cluster was
	// compiled otherwise.
	PageBytes int64
	// BuildBytesPerSecond is the assumed effective throughput of an index
	// build, covering the scan, the sort and the write.
	BuildBytesPerSecond int64
	// ScansPerBuild is how many times CREATE INDEX CONCURRENTLY reads the
	// table. Two: it builds, then it validates against a second snapshot.
	ScansPerBuild int64
	// MinBuildSeconds is the floor for any build. It keeps an unanalyzed
	// table, where relpages is 0, from being reported as instant.
	MinBuildSeconds int
	// CatalogSeconds is the estimate for a catalog-only node: the ON ONLY
	// parent index, an attach, an assertion, a verification.
	CatalogSeconds int
}

// Every field follows the same rule: zero or negative means "use the default".
// A partially filled Estimator is therefore a valid override of one setting
// rather than a silent zeroing of the rest, and a zero Estimator behaves
// exactly like [DefaultEstimator] instead of estimating everything at 0.

// The estimator defaults. They are deliberately conservative round numbers
// rather than tuned constants, because the estimate is advisory.
const (
	// DefaultPageBytes is PostgreSQL's default block size.
	DefaultPageBytes int64 = 8192
	// DefaultBuildBytesPerSecond assumes 50 MiB/s of effective index-build
	// throughput, which is a plausible floor for managed storage under
	// concurrent load.
	DefaultBuildBytesPerSecond int64 = 50 << 20
	// DefaultScansPerBuild is the two table scans CREATE INDEX CONCURRENTLY
	// performs.
	DefaultScansPerBuild int64 = 2
	// DefaultMinBuildSeconds is the floor for a build node.
	DefaultMinBuildSeconds = 5
	// DefaultCatalogSeconds is the estimate for a catalog-only node.
	DefaultCatalogSeconds = 1
)

// DefaultEstimator returns the estimator the host uses when none is configured.
func DefaultEstimator() Estimator {
	return Estimator{
		PageBytes:           DefaultPageBytes,
		BuildBytesPerSecond: DefaultBuildBytesPerSecond,
		ScansPerBuild:       DefaultScansPerBuild,
		MinBuildSeconds:     DefaultMinBuildSeconds,
		CatalogSeconds:      DefaultCatalogSeconds,
	}
}

// withDefaults fills in any zero field, so a caller can override one setting
// without restating the rest and a zero Estimator still works.
func (e Estimator) withDefaults() Estimator {
	d := DefaultEstimator()
	if e.PageBytes <= 0 {
		e.PageBytes = d.PageBytes
	}
	if e.BuildBytesPerSecond <= 0 {
		e.BuildBytesPerSecond = d.BuildBytesPerSecond
	}
	if e.ScansPerBuild <= 0 {
		e.ScansPerBuild = d.ScansPerBuild
	}
	if e.MinBuildSeconds <= 0 {
		e.MinBuildSeconds = d.MinBuildSeconds
	}
	if e.CatalogSeconds <= 0 {
		e.CatalogSeconds = d.CatalogSeconds
	}
	return e
}

// Bytes converts a page count to bytes, clamping a missing or nonsensical
// statistic to zero.
func (e Estimator) Bytes(relPages int64) int64 {
	e = e.withDefaults()
	if relPages <= 0 {
		return 0
	}
	return relPages * e.PageBytes
}

// BuildSeconds estimates one CREATE INDEX CONCURRENTLY against a leaf of
// relPages pages (FR-PLAN-9).
func (e Estimator) BuildSeconds(relPages int64) int {
	e = e.withDefaults()
	secs := (e.Bytes(relPages) * e.ScansPerBuild) / e.BuildBytesPerSecond
	if secs < int64(e.MinBuildSeconds) {
		return e.MinBuildSeconds
	}
	return int(secs)
}

// ReindexSeconds estimates one REINDEX INDEX CONCURRENTLY. A reindex reads the
// table it indexes, so it is sized from the table's page count, not the index's.
func (e Estimator) ReindexSeconds(tableRelPages int64) int {
	return e.BuildSeconds(tableRelPages)
}

// ReindexPeakBytes estimates the peak *additional* storage a reindex needs: a
// REINDEX CONCURRENTLY transiently holds both the old index and the new one, so
// the new copy is the additional demand (FR-REIDX-7).
func (e Estimator) ReindexPeakBytes(indexRelPages int64) int64 {
	return e.Bytes(indexRelPages)
}

// CatalogNodeSeconds is the estimate for a node that only touches the catalog:
// the ON ONLY parent index, an attach, an assertion, a verification.
func (e Estimator) CatalogNodeSeconds() int {
	return e.withDefaults().CatalogSeconds
}

// WaitSeconds is the estimate for a wait node: exactly the pause it encodes.
func (e Estimator) WaitSeconds(seconds int) int {
	if seconds < 0 {
		return 0
	}
	return seconds
}
