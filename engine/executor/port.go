package executor

import (
	"context"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// RunID identifies one execution of one plan (TRD §17.1). A run is bound to
// exactly one plan digest for its lifetime (INV-6); binding it is the caller's
// job, because the executor never opens or closes a run.
type RunID string

func (id RunID) String() string { return string(id) }

// ---------------------------------------------------------------------------
// SQL port
// ---------------------------------------------------------------------------

// SessionSettings are the session GUCs a statement must run under. They are
// part of the port rather than baked into the SQL so that a fake can assert
// FR-EXEC-5 directly, which is the requirement most easily broken by accident.
type SessionSettings struct {
	// LockTimeout bounds how long the statement queues for its lock. It is
	// always positive for a DDL statement (FR-EXEC-5), so nothing ever waits
	// behind a long transaction indefinitely.
	LockTimeout time.Duration

	// StatementTimeout bounds total statement duration. Zero means *no finite
	// timeout*, which is mandatory for index.create_concurrently: it
	// legitimately runs for hours (FR-EXEC-5).
	StatementTimeout time.Duration
}

// Statement is one rendered DDL statement plus the contract it must run under.
//
// SQL is re-rendered from the node's structured parameters; [protocol.Node.RenderedSQL]
// is never sent to the server (FR-PLANFILE-7, T2).
type Statement struct {
	// RunID and NodeID are correlation only. An implementation may log them; it
	// must not branch on them.
	RunID  RunID
	NodeID protocol.NodeID

	// Kind is the node kind that produced the statement, for logging.
	Kind protocol.NodeKind

	// SQL is the statement to execute, exactly as given.
	SQL string

	// Settings are the session GUCs to apply before SQL runs.
	Settings SessionSettings

	// MustRunOutsideTransaction forbids wrapping SQL in an explicit
	// transaction block (FR-EXEC-6). PostgreSQL rejects every CONCURRENTLY form
	// inside one.
	MustRunOutsideTransaction bool
}

// SQLExecutor is the executor's whole view of the target database: one method,
// no transactions, no result sets.
//
// The narrowness is deliberate. Everything the executor needs to say about a
// statement is in [Statement], so a fake implementation is a few lines and the
// requirements that govern statement execution are assertable without a server.
//
// An implementation MUST honour [Statement.Settings] and
// [Statement.MustRunOutsideTransaction], and MUST NOT retry: retry
// classification and backoff belong to the executor (FR-EXEC-3, FR-EXEC-4).
type SQLExecutor interface {
	Exec(ctx context.Context, stmt Statement) error
}

// ---------------------------------------------------------------------------
// Catalog port
// ---------------------------------------------------------------------------

// CheckResult is the outcome of one catalog predicate. engine/verifier owns
// evaluation; the executor only reads Passed and reports the rest.
type CheckResult struct {
	// Name identifies the predicate that was evaluated, for the message.
	Name string
	// Passed is the only field that decides anything.
	Passed bool
	// Detail is the operator-facing explanation on failure.
	Detail string
	// FailureCode overrides the plan's exit code for this failure. Zero defers
	// to the plan.
	FailureCode protocol.ExitCode
}

// CatalogEvaluator answers the two read-only node kinds, catalog.assert and
// index.verify, and reads the ownership marker off an object. It issues no DDL.
//
// Assert and Verify MUST return exactly one [CheckResult] per input, in order:
// the executor pairs them positionally to recover each predicate's failure code.
type CatalogEvaluator interface {
	Assert(ctx context.Context, assertions []protocol.Assertion) ([]CheckResult, error)
	Verify(ctx context.Context, checks []protocol.VerifyCheck) ([]CheckResult, error)

	// Marker reads the ownership marker on object (FR-AUTH-2 as amended). An
	// object that does not exist, or that carries no comment, is
	// [protocol.MarkerAbsent] and a nil error: absence is an answer, not a
	// failure. An error means the catalog could not be read, which is "cannot
	// decide" and halts rather than denying.
	Marker(ctx context.Context, object protocol.ObjectName) (protocol.Marker, protocol.MarkerStatus, error)
}

// ---------------------------------------------------------------------------
// State port
// ---------------------------------------------------------------------------

// NodeRecord is the stored state of one node within one run (TRD §10, NODE_STATE).
type NodeRecord struct {
	NodeID    protocol.NodeID
	State     protocol.NodeState
	Attempts  int
	LastError string
	UpdatedAt time.Time
}

// Transition is one checkpoint: a node moving from one state to another
// (FR-EXEC-2, FR-STATE-4). It is also the record `status` reads to report
// progress (FR-ORD-5).
type Transition struct {
	RunID  RunID
	NodeID protocol.NodeID
	Kind   protocol.NodeKind

	// From and To are the edge, already checked against diagram D7.
	From protocol.NodeState
	To   protocol.NodeState

	// Reason is [protocol.ReasonOrphanRecovery] only for the RUNNING -> PENDING
	// edge, which is the single non-monotonic transition (INV-5).
	Reason protocol.TransitionReason

	// Attempts is the cumulative attempt count for this node.
	Attempts int

	// Duration is how long the statement took, for the RUNNING -> VERIFYING
	// edge. Zero elsewhere.
	Duration time.Duration

	// Error and ErrorKind describe the failure that caused the edge, if any.
	Error     string
	ErrorKind protocol.ErrorKind

	At time.Time
}

// AuthorizationRecord is the justification for one destructive statement,
// written before the statement runs (FR-AUTH-6, INV-2, AC-20).
type AuthorizationRecord struct {
	RunID     RunID
	NodeID    protocol.NodeID
	Object    protocol.ObjectName
	Mode      protocol.AuthorizationMode
	Evidence  map[string]string
	GrantedAt time.Time
}

// AuditEventType classifies an [AuditEvent].
type AuditEventType string

// The audit event types the executor emits.
const (
	AuditRunStarted           AuditEventType = "run_started"
	AuditRunFinished          AuditEventType = "run_finished"
	AuditRunCancelled         AuditEventType = "run_cancelled"
	AuditRunFailed            AuditEventType = "run_failed"
	AuditNodeFailed           AuditEventType = "node_failed"
	AuditAuthorizationGranted AuditEventType = "authorization_granted"
	AuditAuthorizationDenied  AuditEventType = "authorization_denied"
	AuditOrphanRecovered      AuditEventType = "orphan_recovered"
)

// AuditEvent is one row of the append-only audit trail (INV-3, FR-STATE-5).
type AuditEvent struct {
	RunID  RunID
	NodeID protocol.NodeID
	Type   AuditEventType
	Detail map[string]string
	At     time.Time
}

// AuthorityReader is the slice of the state store that answers "does any run
// still hold a live claim on this object?" (FR-AUTH-2 as amended). It is
// separated so [Authorize] can be tested without a whole store.
//
// It replaced a provenance lookup, and the replacement is strictly stronger. A
// provenance record keyed on a name outlived the run that wrote it, so a
// completed build authorized destroying whatever later occupied that name. A
// claim expires by state transition: when a run completes, every node is DONE
// and no claim survives (AC-6, NFR-REL-3).
type AuthorityReader interface {
	// ClaimsObject reports the run holding a live claim on object, if any. It
	// is deliberately not run-scoped: the claim that covers a half-built index
	// is normally held by the run that died (TRD §7.3.2).
	ClaimsObject(ctx context.Context, object protocol.ObjectName) (RunID, bool, error)
}

// StateStore is the executor's whole view of persisted execution state
// (FR-STATE-1). engine/state owns the implementations; this is the
// consumer-side port, kept to what the dispatch loop actually needs.
//
// Every method must be durable on return. The executor treats a successful
// return as the checkpoint and proceeds on that basis (FR-EXEC-2), so an
// implementation that buffers writes breaks resume correctness.
type StateStore interface {
	AuthorityReader

	// NodeStates returns the recorded state of every node in the run. A node
	// absent from the map is [protocol.InitialNodeState].
	NodeStates(ctx context.Context, run RunID) (map[protocol.NodeID]NodeRecord, error)

	// RecordTransition durably records one checkpoint (FR-EXEC-2).
	RecordTransition(ctx context.Context, t Transition) error

	// CancelRequested reports whether `cancel` set the flag for this run. The
	// executor polls it at node boundaries only (FR-CLI-10).
	CancelRequested(ctx context.Context, run RunID) (bool, error)

	// RecordAuthorization commits the satisfied mode and its evidence. The
	// executor calls it before the destructive statement (FR-AUTH-6).
	//
	// One record per attempt, not per node: every statement that could destroy
	// something gets its own justification, evaluated against the state that
	// was live at that moment.
	RecordAuthorization(ctx context.Context, a AuthorizationRecord) error

	// AppendAudit appends one event. The trail is never updated in place
	// (INV-3).
	AppendAudit(ctx context.Context, e AuditEvent) error
}

// Heartbeater renews the run's lease so an orphaned run is detectable
// (FR-LOCK-3, FR-LOCK-4, INV-4). It is optional: leave [Config.Heartbeat] nil
// and the executor starts no goroutine.
type Heartbeater interface {
	Heartbeat(ctx context.Context, run RunID) error
}

// ---------------------------------------------------------------------------
// Clock port
// ---------------------------------------------------------------------------

// Clock is the executor's only source of time, so `wait` nodes and retry
// backoff are testable without sleeping.
type Clock interface {
	Now() time.Time
	// Sleep blocks for d, returning early with ctx.Err() if ctx is done. A
	// non-positive d returns nil immediately.
	Sleep(ctx context.Context, d time.Duration) error
}

// SystemClock is the real clock.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }

// Sleep blocks for d or until ctx is done.
func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
