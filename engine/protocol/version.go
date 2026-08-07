package protocol

// PlanFormatVersion is the plan-format schema version this binary writes
// (FR-PLANFILE-8).
//
// The version is bumped whenever the plan format changes in a way an older
// binary could misread: a new [NodeKind], a new required field, or a change to
// canonicalization. TRD §7.2.2 makes adding a node kind a versioned engine
// change for exactly this reason.
//
// Version 2 adds [Plan.Topology], the tree the fingerprint was computed over,
// so that a drift refusal can name what changed rather than print two hashes
// (AC-3). An older binary must refuse a v2 plan rather than read it: the plan
// body is digest-covered, so a binary that silently dropped the new field would
// compute a different digest and report tampering. This binary still executes
// v1 plans, whose drift refusals fall back to reporting the live tree.
const PlanFormatVersion = 2

// supportedFormatVersions lists every version this binary can execute.
var supportedFormatVersions = []int{1, 2}

// SupportedFormatVersions returns the plan-format versions this binary can
// execute, ascending. The returned slice is a copy.
func SupportedFormatVersions() []int {
	out := make([]int, len(supportedFormatVersions))
	copy(out, supportedFormatVersions)
	return out
}

// IsSupportedFormatVersion reports whether v is a version this binary can
// execute.
func IsSupportedFormatVersion(v int) bool {
	for _, s := range supportedFormatVersions {
		if s == v {
			return true
		}
	}
	return false
}

// CheckFormatVersion refuses a plan whose format version this binary does not
// understand (NFR-COMPAT-3). It returns an error matching
// [ErrUnsupportedFormatVersion].
func CheckFormatVersion(v int) error {
	if IsSupportedFormatVersion(v) {
		return nil
	}
	return ErrUnsupportedFormatVersion.Detailf(
		"plan declares format version %d; this binary supports %v", v, supportedFormatVersions)
}
