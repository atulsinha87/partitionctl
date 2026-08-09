package executor

import (
	"errors"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// The executor's error classes. They extend [protocol.ErrorKind] rather than
// redefining it, so [protocol.ExitCodeFor] and [protocol.KindOf] keep working
// on every error this package returns (FR-CLI-13, AC-26).
const (
	// KindUnsupportedNodeKind means the kind is in the vocabulary but this
	// executor build does not implement it.
	KindUnsupportedNodeKind protocol.ErrorKind = "unsupported_node_kind"
	// KindMissingPort means a port the plan needs was not configured.
	KindMissingPort protocol.ErrorKind = "missing_port"
	// KindCheckpointFailed means the state store could not record a transition,
	// so the executor cannot prove where it is.
	KindCheckpointFailed protocol.ErrorKind = "checkpoint_failed"
	// KindNodePreviouslyFailed means a node is FAILED from an earlier run.
	KindNodePreviouslyFailed protocol.ErrorKind = "node_previously_failed"
	// KindInvalidRun means the run itself, not the plan, is malformed.
	KindInvalidRun protocol.ErrorKind = "invalid_run"
	// KindDependencyNotComplete means a node was reached while a predecessor
	// was not DONE or SKIPPED.
	KindDependencyNotComplete protocol.ErrorKind = "dependency_not_complete"
)

// The executor's sentinel errors. Match them with errors.Is; derive richer
// values with Detailf or Wrap, exactly as in [protocol].
var (
	// ErrUnsupportedNodeKind means the plan carries a node kind this build
	// cannot dispatch. It is raised as a pre-flight check, before any statement
	// runs, so a plan containing index.reindex_concurrently or
	// index.drop_partitioned fails without issuing DDL. Exit 1.
	ErrUnsupportedNodeKind = &protocol.Error{
		Kind: KindUnsupportedNodeKind,
		Code: protocol.ExitFailure,
		Msg:  "node kind is not implemented by this executor build",
	}

	// ErrMissingPort means the plan requires a port that was not configured,
	// for example a catalog.assert node with no [CatalogEvaluator]. Exit 1.
	ErrMissingPort = &protocol.Error{
		Kind: KindMissingPort,
		Code: protocol.ExitFailure,
		Msg:  "required executor port is not configured",
	}

	// ErrCheckpointFailed means a [StateStore] write failed. The executor halts
	// rather than proceeding, because an unrecorded transition is an
	// unrecoverable intermediate (FR-EXEC-2, NFR-REL-2). Exit 1.
	ErrCheckpointFailed = &protocol.Error{
		Kind: KindCheckpointFailed,
		Code: protocol.ExitFailure,
		Msg:  "state store checkpoint failed",
	}

	// ErrNodePreviouslyFailed means the run cannot continue past a node that is
	// FAILED, which is a terminal state with no outgoing edge (D7). Re-plan
	// against the live catalog instead. Exit 1.
	ErrNodePreviouslyFailed = &protocol.Error{
		Kind: KindNodePreviouslyFailed,
		Code: protocol.ExitFailure,
		Msg:  "node failed in an earlier run",
	}

	// ErrInvalidRun means the run identity or its stored state is unusable.
	// Exit 1.
	ErrInvalidRun = &protocol.Error{
		Kind: KindInvalidRun,
		Code: protocol.ExitFailure,
		Msg:  "invalid run",
	}

	// ErrDependencyNotComplete means a node was reached before every
	// predecessor was DONE or SKIPPED (FR-ORD-1). Exit 1.
	ErrDependencyNotComplete = &protocol.Error{
		Kind: KindDependencyNotComplete,
		Code: protocol.ExitFailure,
		Msg:  "node reached before its dependencies completed",
	}
)

// ErrFenced means this process no longer holds the run's lease or its advisory
// lock: another process has taken the target.
//
// It is the fencing signal a [Heartbeater] returns, and it is the one heartbeat
// failure that stops dispatch. Everything else a heartbeat can report is
// transient and must not abandon a multi-hour build, so the distinction is the
// implementation's to make: an adapter returns this only when the store told it
// the lease is definitively held by someone else (FR-LOCK-1, FR-LOCK-3, AC-10).
var ErrFenced = &protocol.Error{
	Kind: protocol.KindLockHeld,
	Code: protocol.ExitLockHeld,
	Msg:  "fenced out: this run's lease or advisory lock is held by another process",
}

// errStopped is the internal signal that the run stopped at a node boundary
// rather than failing. It never escapes [Executor.Run], which turns it into
// [Result.Cancelled].
var errStopped = errors.New("executor: stopped at a node boundary")

// errUnsafeRetry is the internal signal that a node failed retryably but its
// kind cannot be re-issued in process ([protocol.NodeKind.RetrySafe]). Like
// errStopped it never escapes [Executor.Run]; the node is left in RETRY_WAIT so
// `resume` can clean up and roll forward.
var errUnsafeRetry = errors.New("executor: retryable failure of a statement that cannot be re-issued")

// errorForExitCode maps a plan-declared exit code back to the protocol sentinel
// that carries it, so a failing assertion produces the exit code the planner
// asked for (AC-26). An unset code means verification failed, per the
// documented meaning of [protocol.Assertion.FailureCode].
func errorForExitCode(c protocol.ExitCode) *protocol.Error {
	switch c {
	case protocol.ExitDigestMismatch:
		return protocol.ErrDigestMismatch
	case protocol.ExitTopologyDrift:
		return protocol.ErrTopologyDrift
	case protocol.ExitLockHeld:
		return protocol.ErrLockHeld
	case protocol.ExitAuthorizationUnsatisfied:
		return protocol.ErrAuthorizationUnsatisfied
	case protocol.ExitUnsupportedTopology:
		return protocol.ErrUnsupportedTopology
	case protocol.ExitInsufficientPrivilege:
		return protocol.ErrInsufficientPrivilege
	case protocol.ExitFailure:
		return protocol.ErrFailure
	default:
		return protocol.ErrVerificationFailed
	}
}
