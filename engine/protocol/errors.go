package protocol

import (
	"errors"
	"fmt"
)

// ExitCode is a PartitionCTL process exit status. The set is fixed by TRD
// §7.2.12 and is part of the CLI contract, because CI branches on it
// (FR-CLI-13, AC-26).
type ExitCode int

// The exit codes from TRD §7.2.12.
const (
	// ExitSuccess is success, or a converged no-op (AC-7).
	ExitSuccess ExitCode = 0
	// ExitFailure is any failure without a more specific code.
	ExitFailure ExitCode = 1
	// ExitDigestMismatch means the plan artifact was edited after approval.
	ExitDigestMismatch ExitCode = 10
	// ExitTopologyDrift means the catalog changed since planning.
	ExitTopologyDrift ExitCode = 11
	// ExitLockHeld means the advisory lock is held by another run.
	ExitLockHeld ExitCode = 12
	// ExitAuthorizationUnsatisfied means a destructive action was halted
	// because no authorization mode was satisfied (INV-2).
	ExitAuthorizationUnsatisfied ExitCode = 13
	// ExitVerificationFailed means a catalog assertion failed.
	ExitVerificationFailed ExitCode = 14
	// ExitUnsupportedTopology means HASH, multi-level, or a DEFAULT partition.
	ExitUnsupportedTopology ExitCode = 15
	// ExitInsufficientPrivilege means the role is not a member of the owning
	// role.
	ExitInsufficientPrivilege ExitCode = 16
)

// AllExitCodes returns every exit code in the contract, ascending.
func AllExitCodes() []ExitCode {
	return []ExitCode{
		ExitSuccess,
		ExitFailure,
		ExitDigestMismatch,
		ExitTopologyDrift,
		ExitLockHeld,
		ExitAuthorizationUnsatisfied,
		ExitVerificationFailed,
		ExitUnsupportedTopology,
		ExitInsufficientPrivilege,
	}
}

func (c ExitCode) String() string { return fmt.Sprintf("%d", int(c)) }

// ErrorKind is the stable machine-readable class of an [Error]. It is what
// errors.Is matches on, and it is safe to emit in structured logs (NFR-OBS-2).
type ErrorKind string

// The error classes. Each maps to exactly one [ExitCode].
const (
	KindGeneric                  ErrorKind = "generic"
	KindDigestMismatch           ErrorKind = "digest_mismatch"
	KindTopologyDrift            ErrorKind = "topology_drift"
	KindLockHeld                 ErrorKind = "lock_held"
	KindAuthorizationUnsatisfied ErrorKind = "authorization_unsatisfied"
	KindVerificationFailed       ErrorKind = "verification_failed"
	KindUnsupportedTopology      ErrorKind = "unsupported_topology"
	KindInsufficientPrivilege    ErrorKind = "insufficient_privilege"
	KindUnsupportedFormatVersion ErrorKind = "unsupported_format_version"
	KindInvalidPlan              ErrorKind = "invalid_plan"
	KindUnknownNodeKind          ErrorKind = "unknown_node_kind"
	KindInvalidTransition        ErrorKind = "invalid_transition"
	KindInvalidIdentifier        ErrorKind = "invalid_identifier"
	KindNameCollision            ErrorKind = "name_collision"
)

// Error is PartitionCTL's typed error. Every error the engine surfaces to the
// CLI should be, or wrap, one of these so that [ExitCodeFor] can map it to the
// contract exit code.
//
// Compare with errors.Is against the sentinels below; Is matches on Kind, so a
// value derived with [Error.Detailf] or [Error.Wrap] still matches its sentinel.
type Error struct {
	// Kind is the stable error class.
	Kind ErrorKind
	// Code is the process exit status this class maps to.
	Code ExitCode
	// Msg is the human-readable message.
	Msg string
	// Err is the wrapped cause, if any.
	Err error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	return e.Msg
}

// Unwrap returns the wrapped cause.
func (e *Error) Unwrap() error { return e.Err }

// ExitCode implements [ExitCoder].
func (e *Error) ExitCode() ExitCode { return e.Code }

// Is reports whether target is an *Error of the same Kind.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return t.Kind == e.Kind
}

// Detailf returns a copy of e whose message carries an extra detail. The Kind
// and Code are preserved, so errors.Is against the original sentinel still
// matches.
func (e *Error) Detailf(format string, args ...any) *Error {
	return &Error{
		Kind: e.Kind,
		Code: e.Code,
		Msg:  e.Msg + ": " + fmt.Sprintf(format, args...),
		Err:  e.Err,
	}
}

// Wrap returns a copy of e wrapping cause. The Kind and Code are preserved.
func (e *Error) Wrap(cause error) *Error {
	return &Error{Kind: e.Kind, Code: e.Code, Msg: e.Msg, Err: cause}
}

// The sentinel errors. Match them with errors.Is; derive richer values with
// [Error.Detailf] or [Error.Wrap].
var (
	// ErrFailure is the generic failure class (exit 1).
	ErrFailure = &Error{Kind: KindGeneric, Code: ExitFailure, Msg: "failure"}
	// ErrDigestMismatch means the plan file no longer matches its digest
	// (FR-PLANFILE-3, AC-2, T1). Exit 10.
	ErrDigestMismatch = &Error{Kind: KindDigestMismatch, Code: ExitDigestMismatch, Msg: "plan digest mismatch"}
	// ErrTopologyDrift means the live catalog no longer matches the plan's
	// topology fingerprint (FR-PLANFILE-5, AC-3). Exit 11.
	ErrTopologyDrift = &Error{Kind: KindTopologyDrift, Code: ExitTopologyDrift, Msg: "topology drift"}
	// ErrLockHeld means another run holds the advisory lock (FR-LOCK-2,
	// AC-10). Exit 12.
	ErrLockHeld = &Error{Kind: KindLockHeld, Code: ExitLockHeld, Msg: "advisory lock held by another run"}
	// ErrAuthorizationUnsatisfied means a destructive node had no satisfied
	// authorization mode (FR-AUTH-5, INV-2, AC-6, AC-19, AC-20). Exit 13.
	ErrAuthorizationUnsatisfied = &Error{Kind: KindAuthorizationUnsatisfied, Code: ExitAuthorizationUnsatisfied, Msg: "destructive action halted: authorization unsatisfied"}
	// ErrVerificationFailed means a catalog assertion failed (FR-VER-*).
	// Exit 14.
	ErrVerificationFailed = &Error{Kind: KindVerificationFailed, Code: ExitVerificationFailed, Msg: "verification failed"}
	// ErrUnsupportedTopology means HASH, multi-level, or a DEFAULT partition
	// (FR-PLAN-2, FR-PLAN-3, AC-11). Exit 15.
	ErrUnsupportedTopology = &Error{Kind: KindUnsupportedTopology, Code: ExitUnsupportedTopology, Msg: "unsupported topology"}
	// ErrInsufficientPrivilege means the connected role is not a member of the
	// owning role (FR-PLAN-10, AC-12). Exit 16.
	ErrInsufficientPrivilege = &Error{Kind: KindInsufficientPrivilege, Code: ExitInsufficientPrivilege, Msg: "insufficient privilege"}

	// ErrUnsupportedFormatVersion means the plan file's format version is not
	// one this binary understands (NFR-COMPAT-3). Exit 1: an unreadable plan
	// is not a tampered plan.
	ErrUnsupportedFormatVersion = &Error{Kind: KindUnsupportedFormatVersion, Code: ExitFailure, Msg: "unsupported plan format version"}
	// ErrInvalidPlan means the plan is structurally invalid. Exit 1.
	ErrInvalidPlan = &Error{Kind: KindInvalidPlan, Code: ExitFailure, Msg: "invalid plan"}
	// ErrUnknownNodeKind means a node carries a kind outside the vocabulary.
	// Exit 1.
	ErrUnknownNodeKind = &Error{Kind: KindUnknownNodeKind, Code: ExitFailure, Msg: "unknown node kind"}
	// ErrInvalidTransition means a NodeState transition is not permitted by
	// TRD diagram D7 (INV-5). Exit 1.
	ErrInvalidTransition = &Error{Kind: KindInvalidTransition, Code: ExitFailure, Msg: "invalid node state transition"}
	// ErrInvalidIdentifier means a string cannot be a PostgreSQL identifier
	// (NFR-SEC-4). Exit 1.
	ErrInvalidIdentifier = &Error{Kind: KindInvalidIdentifier, Code: ExitFailure, Msg: "invalid identifier"}
	// ErrNameCollision means two partitions map to the same generated child
	// index name (FR-PLAN-13). Exit 1.
	ErrNameCollision = &Error{Kind: KindNameCollision, Code: ExitFailure, Msg: "generated index name collision"}
)

// ExitCoder is implemented by errors that carry a contract exit code.
type ExitCoder interface {
	ExitCode() ExitCode
}

// ExitCodeFor maps an error to its process exit status (FR-CLI-13, AC-26).
// A nil error is [ExitSuccess]; an error with no code in its chain is
// [ExitFailure].
func ExitCodeFor(err error) ExitCode {
	if err == nil {
		return ExitSuccess
	}
	var coder ExitCoder
	if errors.As(err, &coder) {
		return coder.ExitCode()
	}
	return ExitFailure
}

// KindOf reports the [ErrorKind] of an error, for structured logging
// (NFR-OBS-2). It returns [KindGeneric] for errors that carry no kind.
func KindOf(err error) ErrorKind {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindGeneric
}
