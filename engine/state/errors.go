package state

import (
	"github.com/atulsinha/partitionctl/engine/protocol"
)

// The state store's error kinds. They extend [protocol.ErrorKind] rather than
// reusing KindGeneric, so a caller can tell "the run does not exist" from "the
// run exists and someone else changed it underneath you" with errors.Is.
const (
	// KindNotFound means the addressed record does not exist.
	KindNotFound protocol.ErrorKind = "state_not_found"
	// KindConflict means a compare-and-set failed: the record's current value
	// is not the one the caller asserted.
	KindConflict protocol.ErrorKind = "state_conflict"
	// KindImmutable means the caller tried to change a field that is bound for
	// the record's lifetime (INV-3, INV-6).
	KindImmutable protocol.ErrorKind = "state_immutable"
	// KindLeaseLost means the lease is held by a different holder, or has
	// expired and been taken.
	KindLeaseLost protocol.ErrorKind = "lease_lost"
	// KindStoreIO means the underlying medium failed.
	KindStoreIO protocol.ErrorKind = "state_io"
	// KindGuardNotRecorded means a guarded action was refused because its
	// record could not be committed first (INV-1, INV-2).
	KindGuardNotRecorded protocol.ErrorKind = "guard_not_recorded"
)

// The state store's sentinel errors. Match with errors.Is; derive richer values
// with Detailf or Wrap, both of which preserve Kind.
var (
	// ErrNotFound is returned for a run, node, lease or record that does not
	// exist. Exit 1.
	ErrNotFound = &protocol.Error{Kind: KindNotFound, Code: protocol.ExitFailure, Msg: "not found in state store"}

	// ErrConflict is returned when a compare-and-set fails. Exit 1.
	ErrConflict = &protocol.Error{Kind: KindConflict, Code: protocol.ExitFailure, Msg: "state changed underneath this caller"}

	// ErrImmutable is returned on an attempt to change a field bound for the
	// record's lifetime: a run's plan digest (INV-6) or any audit event
	// (INV-3). Exit 1.
	ErrImmutable = &protocol.Error{Kind: KindImmutable, Code: protocol.ExitFailure, Msg: "record is immutable"}

	// ErrLeaseLost is returned when a heartbeat or release is attempted by a
	// holder that no longer owns the lease. It is the fencing signal: an
	// executor that sees it must stop dispatching, because another process has
	// adopted its run. Exit 1.
	ErrLeaseLost = &protocol.Error{Kind: KindLeaseLost, Code: protocol.ExitFailure, Msg: "lease lost"}

	// ErrStoreIO wraps a failure of the underlying medium. Exit 1.
	ErrStoreIO = &protocol.Error{Kind: KindStoreIO, Code: protocol.ExitFailure, Msg: "state store i/o"}

	// ErrAuthorizationNotRecorded means the authorization record could not be
	// committed, so the guarded destructive statement was NOT executed
	// (INV-2). It carries exit 13, because a destructive action that could not
	// be justified is exactly the halt FR-AUTH-5 describes.
	ErrAuthorizationNotRecorded = &protocol.Error{
		Kind: KindGuardNotRecorded, Code: protocol.ExitAuthorizationUnsatisfied,
		Msg: "authorization not recorded, guarded destructive statement was not executed (INV-2)"}
)

// ioErr wraps an underlying medium failure so that errors.Is(err, ErrStoreIO)
// holds while the cause stays inspectable.
func ioErr(what string, cause error) error {
	if cause == nil {
		return nil
	}
	return ErrStoreIO.Detailf("%s", what).Wrap(cause)
}
