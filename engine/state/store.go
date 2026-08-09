package state

import (
	"context"
	"time"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// Clock returns the current time. Injecting it is what makes lease expiry and
// orphan detection testable without sleeping (FR-STATE-7, INV-4).
type Clock func() time.Time

// SystemClock is the default clock.
func SystemClock() time.Time { return time.Now() }

// StateStore is the port every piece of execution state goes through
// (FR-STATE-1). v0.1 ships two implementations, [FileStore] and [SQLStore]
// (FR-STATE-2), and adding a third requires no engine change (NFR-EXT-2).
//
// Read the package doc for how INV-1, INV-2, INV-3, INV-4 and INV-6 are
// enforced by the shape of this interface rather than by convention.
type StateStore interface {
	RunStore
	NodeStore
	AuthorizationStore
	LeaseStore
	AuditStore
	CancelStore
	Locker

	// Close releases the store's resources. It does not release advisory
	// locks: a [Lock] is unlocked through its own handle, because a lock
	// outliving a store handle is a bug the caller should see.
	Close() error
}

// RunStore owns run records (FR-STATE-4).
type RunStore interface {
	RunReader

	// CreateRun opens a run bound to exactly one plan digest (INV-6) and seeds
	// a [NodeRecord] in [protocol.InitialNodeState] for every node in the
	// plan, each carrying the object that node acts on ([protocol.Node.Object]).
	// It appends an [EventRunOpened] audit event.
	//
	// Seeding the object is what makes the node record a claim, and it is why
	// CreateRun takes the whole plan rather than a node count: the claim has to
	// exist before the first statement runs, and the only moment at which every
	// object is known is when the plan is read (INV-1 as amended).
	CreateRun(ctx context.Context, req NewRun) (Run, error)

	// GetRun returns one run, or an error matching [ErrNotFound].
	GetRun(ctx context.Context, id RunID) (Run, error)

	// SetRunStatus is a compare-and-set on run status. It enforces the run
	// state machine, refuses a transition out of a terminal status, and
	// appends an [EventRunStatusChanged] audit event. It never touches
	// PlanDigest (INV-6).
	SetRunStatus(ctx context.Context, u RunStatusUpdate) (Run, error)
}

// NodeStore owns per-node lifecycle state (FR-STATE-4, FR-EXEC-2).
type NodeStore interface {
	// GetNode returns one node's record, or an error matching [ErrNotFound].
	GetNode(ctx context.Context, runID RunID, nodeID protocol.NodeID) (NodeRecord, error)

	// ListNodes returns every node record for a run, ordered by node id.
	ListNodes(ctx context.Context, runID RunID) ([]NodeRecord, error)

	// TransitionNode applies a compare-and-set state change, enforcing
	// diagram D7 through [protocol.CheckTransition] and INV-5's
	// orphan-recovery restriction. It appends an [EventNodeTransition] audit
	// event.
	TransitionNode(ctx context.Context, t NodeTransition) (NodeRecord, error)
}

// AuthorizationStore owns destructive-action authorization records and enforces
// INV-2.
type AuthorizationStore interface {
	// RecordAuthorization commits rec and only then runs exec (INV-2,
	// FR-AUTH-6, AC-20).
	//
	// The record's per-mode evidence is validated before it is written, so an
	// authorization that cites nothing cannot be recorded and therefore cannot
	// gate a statement. If the record cannot be committed, exec is not called
	// and the returned error matches [ErrAuthorizationNotRecorded], which
	// carries exit 13.
	RecordAuthorization(ctx context.Context, rec AuthorizationRecord, exec GuardedAction) (AuthorizationRecord, error)

	// ListAuthorizations returns every authorization recorded for a run, in
	// the order granted.
	ListAuthorizations(ctx context.Context, runID RunID) ([]AuthorizationRecord, error)
}

// LeaseStore owns the liveness lease (FR-STATE-7, FR-LOCK-3).
type LeaseStore interface {
	// AcquireLease takes or renews the lease for a run. A lease held by a
	// different, unexpired holder is refused with an error matching
	// [ErrLeaseLost]. An expired lease may be taken over by anyone, which is
	// what makes an abandoned run adoptable (INV-4).
	AcquireLease(ctx context.Context, runID RunID, holder string, ttl time.Duration) (Lease, error)

	// Heartbeat extends the lease. It fails with an error matching
	// [ErrLeaseLost] if another holder has taken it, which is the fencing
	// signal that tells an executor to stop dispatching.
	Heartbeat(ctx context.Context, runID RunID, holder string) (Lease, error)

	// ReleaseLease drops the lease. Releasing a lease held by another holder
	// is refused.
	ReleaseLease(ctx context.Context, runID RunID, holder string) error

	// GetLease returns the run's lease, or an error matching [ErrNotFound] if
	// there is none.
	GetLease(ctx context.Context, runID RunID) (Lease, error)
}

// AuditStore owns the append-only trail (FR-STATE-5, INV-3).
//
// There is deliberately no update and no delete method. INV-3 is enforced by
// the absence of a path, not by a rule.
type AuditStore interface {
	// AppendAudit adds one event and returns it with its assigned EventID and
	// per-run Seq.
	AppendAudit(ctx context.Context, ev AuditEvent) (AuditEvent, error)

	// ListAudit returns a run's events with Seq greater than afterSeq, in
	// order. Pass 0 for the whole trail.
	ListAudit(ctx context.Context, runID RunID, afterSeq int64) ([]AuditEvent, error)
}

// CancelStore owns the cancellation flag a running executor polls
// (FR-CLI-10, FR-CLI-11).
type CancelStore interface {
	// RequestCancel asks a run to stop.
	//
	// Its effect depends on the run, which is what the single `cancel` command
	// contract in TRD §7.2.12 requires:
	//
	//   - A live run, meaning RUNNING with an unexpired lease, gets the flag
	//     set. The executor observes it at the next node boundary and never
	//     mid-statement (FR-CLI-10, AC-24), and the run stays resumable.
	//   - A run that is already abandoned, meaning RUNNING with an expired
	//     lease, or ORPHANED, FAILED or INTERRUPTED, is terminally cancelled
	//     so `resume` will not adopt it (FR-CLI-11).
	//   - A terminal run is left alone and returned unchanged.
	RequestCancel(ctx context.Context, runID RunID, actor, note string) (Run, error)

	// CancellationRequested reports whether the flag is set. This is the poll
	// the executor performs at node boundaries; it is deliberately cheap and
	// deliberately does not consider the lease.
	CancellationRequested(ctx context.Context, runID RunID) (bool, error)
}

// Compile-time proof that both v0.1 implementations satisfy the port
// (FR-STATE-2).
var (
	_ StateStore = (*FileStore)(nil)
	_ StateStore = (*SQLStore)(nil)
)
