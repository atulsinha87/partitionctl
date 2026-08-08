package protocol

// NodeKind is the type tag the executor dispatches on. The vocabulary is fixed
// at nine kinds by TRD §7.2.2 and is a versioned engine contract: adding a kind
// requires a [PlanFormatVersion] bump.
//
// There is deliberately no barrier kind. A barrier is a node with N incoming
// edges, which the graph's edge set already expresses.
type NodeKind string

// The nine node kinds (TRD §7.2.2).
const (
	// KindCatalogAssert evaluates catalog predicates and fails the run if any
	// is false. No lock. Introduced by CreatePartitionedIndex.
	KindCatalogAssert NodeKind = "catalog.assert"

	// KindIndexCreateParentInvalid issues CREATE INDEX ON ONLY <parent>,
	// creating the deliberately invalid parent index. ShareUpdateExclusive.
	KindIndexCreateParentInvalid NodeKind = "index.create_parent_invalid"

	// KindIndexCreateConcurrently issues CREATE INDEX CONCURRENTLY on one leaf.
	// ShareUpdateExclusive, non-transactional, no finite statement_timeout
	// (FR-EXEC-5, FR-EXEC-6).
	KindIndexCreateConcurrently NodeKind = "index.create_concurrently"

	// KindIndexAttach issues ALTER INDEX <parent> ATTACH PARTITION <child>.
	// ShareUpdateExclusive.
	KindIndexAttach NodeKind = "index.attach"

	// KindIndexVerify asserts indisvalid / indisready / indislive /
	// attachment. No lock, terminal on false.
	KindIndexVerify NodeKind = "index.verify"

	// KindWait is a fixed pause emitted by the planner for pacing (FR-ORD-3).
	// The executor introduces no delays of its own.
	KindWait NodeKind = "wait"

	// KindIndexDropConcurrently issues DROP INDEX CONCURRENTLY on an
	// *unattached* leaf index. Destructive: authorization-gated.
	KindIndexDropConcurrently NodeKind = "index.drop_concurrently"

	// KindIndexReindexConcurrently issues REINDEX INDEX CONCURRENTLY on one
	// leaf index. ShareUpdateExclusive, non-transactional.
	//
	// The v0.0 spike measured that the statement works on an attached leaf index
	// and that the attachment survives the internal swap, on 14.23 and 17.10
	// (docs/spikes/v0.0-results.md, question 1). It also works on the
	// partitioned parent, which the TRD denied; the planner declines to use the
	// parent form for reasons of resumability, not legality. See
	// [ReindexConcurrentlyParams].
	KindIndexReindexConcurrently NodeKind = "index.reindex_concurrently"

	// KindIndexDropPartitioned issues DROP INDEX on a partitioned parent and
	// cascades to every attached child. Destructive: authorization-gated. The
	// only kind that takes AccessExclusiveLock, on the parent and every leaf
	// simultaneously (TRD §7.2.10).
	KindIndexDropPartitioned NodeKind = "index.drop_partitioned"
)

// LockLevel is the heaviest lock a node kind takes on a user relation, as
// MEASURED in docs/spikes/v0.0-results.md. It exists so `render` and `plan`
// output can warn the operator (FR-DROP-5); the executor does not act on it.
//
// The spike overrides TRD §7.2.2, which this type used to cite. The one value
// the TRD gets wrong is the one that matters most to an operator: it lists
// ShareUpdateExclusive for CREATE INDEX ON ONLY <parent>, and PostgreSQL takes
// ShareLock. The difference is the whole question the operator is asking.
// ShareUpdateExclusiveLock does not conflict with RowExclusiveLock, so writes
// continue; ShareLock does, so every INSERT/UPDATE/DELETE routed through the
// partitioned parent blocks for the duration. Re-measured on PG 17.10: with
// `BEGIN; CREATE INDEX ot_d_idx ON ONLY ot (d);` open, pg_locks shows ShareLock
// on ot, and an INSERT through the parent fails on lock_timeout while an INSERT
// straight into a leaf succeeds.
type LockLevel string

// The lock levels in the vocabulary, weakest first.
const (
	LockNone LockLevel = ""

	// LockShareUpdateExclusive does not conflict with RowExclusiveLock:
	// reads and writes both continue.
	LockShareUpdateExclusive LockLevel = "ShareUpdateExclusive"

	// LockShare conflicts with RowExclusiveLock. Reads continue; every write
	// to the relation blocks for the duration of the statement, including its
	// lock_timeout wait.
	LockShare LockLevel = "Share"

	// LockAccessExclusive conflicts with everything.
	LockAccessExclusive LockLevel = "AccessExclusive"
)

// allNodeKinds is the vocabulary in TRD §7.2.2 table order.
var allNodeKinds = []NodeKind{
	KindCatalogAssert,
	KindIndexCreateParentInvalid,
	KindIndexCreateConcurrently,
	KindIndexAttach,
	KindIndexVerify,
	KindWait,
	KindIndexDropConcurrently,
	KindIndexReindexConcurrently,
	KindIndexDropPartitioned,
}

// AllNodeKinds returns the complete node vocabulary in TRD §7.2.2 table order.
// The returned slice is a copy.
func AllNodeKinds() []NodeKind {
	out := make([]NodeKind, len(allNodeKinds))
	copy(out, allNodeKinds)
	return out
}

// Valid reports whether k is one of the nine kinds.
func (k NodeKind) Valid() bool {
	switch k {
	case KindCatalogAssert,
		KindIndexCreateParentInvalid,
		KindIndexCreateConcurrently,
		KindIndexAttach,
		KindIndexVerify,
		KindWait,
		KindIndexDropConcurrently,
		KindIndexReindexConcurrently,
		KindIndexDropPartitioned:
		return true
	}
	return false
}

// IsDestructive reports whether k destroys a catalog object. Exactly two kinds
// are destructive, and every destructive node carries exactly one
// [AuthorizationMode] that the executor re-evaluates against live state
// immediately before dispatch (FR-AUTH-1, FR-AUTH-5, INV-2).
func (k NodeKind) IsDestructive() bool {
	switch k {
	case KindIndexDropConcurrently, KindIndexDropPartitioned:
		return true
	}
	return false
}

// IssuesDDL reports whether k sends a DDL statement. False for
// [KindCatalogAssert], [KindIndexVerify] and [KindWait], which touch no
// catalog object.
func (k NodeKind) IssuesDDL() bool {
	switch k {
	case KindIndexCreateParentInvalid,
		KindIndexCreateConcurrently,
		KindIndexAttach,
		KindIndexDropConcurrently,
		KindIndexReindexConcurrently,
		KindIndexDropPartitioned:
		return true
	}
	return false
}

// MustRunOutsideTransaction reports whether k's statement must be issued
// outside any explicit transaction block (FR-EXEC-6). True for every
// CONCURRENTLY form, which PostgreSQL rejects inside a transaction.
func (k NodeKind) MustRunOutsideTransaction() bool {
	switch k {
	case KindIndexCreateConcurrently,
		KindIndexDropConcurrently,
		KindIndexReindexConcurrently:
		return true
	}
	return false
}

// AllowsStatementTimeout reports whether a finite statement_timeout may be set
// for k.
//
// FR-EXEC-5 forbids it on [KindIndexCreateConcurrently], which legitimately
// runs for hours. [KindIndexReindexConcurrently] is included here because it
// has the same unbounded duration profile: it is a full index rebuild on a leaf
// that may be 10 TB.
//
// [KindIndexDropConcurrently] is included for a different reason. It is fast in
// itself, but a concurrent drop waits for every transaction that can still see
// the index before it finishes, and that wait is bounded by the target's
// workload rather than by the statement. Killing it at a finite
// statement_timeout leaves the index with indislive = false, which is precisely
// the wreckage the planner then has to recognize and recover from. The resume
// cleanup path already issues its own drop with no finite statement_timeout and
// documents this reasoning; the two paths must not disagree about the same
// statement.
//
// lock_timeout is always set, on every DDL kind.
func (k NodeKind) AllowsStatementTimeout() bool {
	switch k {
	case KindIndexCreateConcurrently, KindIndexReindexConcurrently, KindIndexDropConcurrently:
		return false
	}
	return true
}

// WaitsForConcurrentTransactions reports whether k's statement blocks on the
// application's own transactions as part of doing its work, rather than only
// while acquiring its initial lock.
//
// This is the CONCURRENTLY family. Each of these statements has one or more
// wait-for-lockers phases in which it takes a ShareLock on every concurrent
// transaction's virtual XID through the regular lock manager, which means
// lock_timeout bounds those waits too. That wait is not a lock queue that can
// be retried later; it is the visibility barrier the statement must clear
// before it can finish, and its length is a property of the target's workload.
//
// The executor uses this to give these kinds a much larger lock_timeout than
// the short bound that protects ordinary DDL from queueing in front of
// application traffic (FR-EXEC-5).
func (k NodeKind) WaitsForConcurrentTransactions() bool {
	switch k {
	case KindIndexCreateConcurrently, KindIndexDropConcurrently, KindIndexReindexConcurrently:
		return true
	}
	return false
}

// ClaimsOwnership reports whether a node of this kind, by acting, makes the
// object it names PartitionCTL's.
//
// It is exactly the set of kinds that write an ownership marker
// ([MarkerTargetFor]), and the correspondence is the point: a claim is the
// durable stand-in for a marker that could not be written yet, so a kind that
// would never write one can never claim.
//
// The distinction is load-bearing and its absence is circular. A node record
// names the object its node acts on, including for the two destructive kinds,
// because the audit trail is unreadable without it. If that counted as a claim,
// a plan node saying "drop X" would itself be the proof that X is ours to drop,
// and AC-6 would be satisfied by asking the question.
func (k NodeKind) ClaimsOwnership() bool {
	switch k {
	case KindIndexCreateParentInvalid,
		KindIndexCreateConcurrently,
		KindIndexAttach,
		KindIndexReindexConcurrently:
		return true
	}
	return false
}

// Retryable reports whether a failure of k may be retried at all. Errors are
// still classified as retryable or terminal by the executor (FR-EXEC-3); this
// only says the kind is not inherently single-shot.
func (k NodeKind) Retryable() bool { return k.IssuesDDL() }

// RetrySafe reports whether k's statement may be re-issued verbatim, in the
// same process, after a failure the classifier called retryable.
//
// # Why this is not the same question as Retryable
//
// A retryable *error class* says the condition may pass. It says nothing about
// whether the statement is still the right one to send, and for most of the DDL
// here it is not, because the statement commits catalog state before it can
// fail:
//
//   - CREATE INDEX CONCURRENTLY commits its phase-1 catalog entry and then
//     waits for lockers, twice. A failure in either wait (55P03) leaves an
//     INVALID index behind, so re-issuing gives 42P07 duplicate_table, which is
//     terminal. The retry cannot succeed, and its terminal error replaces the
//     lock timeout that actually killed the build.
//   - CREATE INDEX ... ON ONLY and DROP INDEX CONCURRENTLY have the same shape
//     whenever the statement committed but its response was lost to a reset
//     connection: the retry meets 42P07 or 42704 undefined_object.
//
// Two kinds are exceptions:
//
//   - ALTER INDEX ... ATTACH PARTITION: PostgreSQL's ATExecAttachPartitionIdx
//     silently no-ops when the child is already attached to that parent, so
//     re-issuing it is genuinely idempotent.
//
//   - DROP INDEX on a partitioned parent: it is a single atomic statement that
//     commits nothing before it can fail. The failure FR-DROP-6 and AC-15 are
//     actually about is 55P03 lock_not_available, where the statement never got
//     its AccessExclusiveLock and the index is untouched; re-issuing after a
//     backoff is exactly right, and exhausting the budget abandons cleanly with
//     the index intact. The one degraded case is a committed statement whose
//     response was lost to a reset connection, where the retry meets 42704 on
//     an index that is genuinely gone. That is a misleading error, not a
//     destructive act.
//
// The recovery for an unsafe kind is `resume`, which drops the wreckage under
// the ownership marker and rebuilds (FR-PLAN-6, AC-5). The executor therefore
// leaves the node resumable and stops the run rather than re-issuing. Deciding
// this per kind, rather than by re-reading the catalog, is what keeps the
// executor dispatching on node kind alone and knowing nothing about indexes.
func (k NodeKind) RetrySafe() bool {
	switch k {
	case KindIndexAttach, KindIndexDropPartitioned:
		return true
	}
	return false
}

// LockLevel returns the heaviest lock k takes on a user relation, as measured
// in docs/spikes/v0.0-results.md.
//
// KindIndexAttach stays at ShareUpdateExclusive deliberately, and this is a
// recorded decision rather than an omission: the spike measured
// AccessExclusiveLock for an attach, but on the child INDEX, not on a user
// relation, and a user relation is what this method documents itself as
// reporting. An operator reading "AccessExclusive" here would schedule around
// application blocking that does not happen.
func (k NodeKind) LockLevel() LockLevel {
	switch k {
	case KindIndexCreateParentInvalid:
		// CREATE INDEX ON ONLY <parent>. Measured ShareLock, not the
		// ShareUpdateExclusive the TRD claims: writes through the parent block.
		return LockShare
	case KindIndexCreateConcurrently,
		KindIndexAttach,
		KindIndexDropConcurrently,
		KindIndexReindexConcurrently:
		return LockShareUpdateExclusive
	case KindIndexDropPartitioned:
		return LockAccessExclusive
	}
	return LockNone
}

func (k NodeKind) String() string { return string(k) }

// CheckNodeKind returns an error matching [ErrUnknownNodeKind] if k is outside
// the vocabulary.
func CheckNodeKind(k NodeKind) error {
	if k.Valid() {
		return nil
	}
	return ErrUnknownNodeKind.Detailf("%q is not one of %v", string(k), allNodeKinds)
}
