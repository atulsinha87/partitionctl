package executor

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"syscall"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// ErrorClass is the retry decision for one failure (FR-EXEC-3).
type ErrorClass string

// The retry classes.
const (
	// ClassNone is the absence of an error.
	ClassNone ErrorClass = ""
	// ClassRetryable means the same statement may succeed on a later attempt:
	// connection loss, deadlock, lock timeout, serialization failure.
	ClassRetryable ErrorClass = "retryable"
	// ClassTerminal means retrying cannot help: syntax, permission, constraint
	// violation, unsupported feature. It is the safe default for anything
	// unrecognized.
	ClassTerminal ErrorClass = "terminal"
	// ClassCancelled means the executor's own context was cancelled. It is not
	// a database failure and is never retried.
	ClassCancelled ErrorClass = "cancelled"
)

// Classification is the verdict on one error.
type Classification struct {
	// Class is the retry decision.
	Class ErrorClass
	// SQLState is the five-character PostgreSQL SQLSTATE, when one was
	// recovered. Empty for transport-level and non-database failures.
	SQLState string
	// Condition is the PostgreSQL condition name for SQLState, for logs.
	Condition string
	// ExitCode is the process exit status this failure maps to, where the
	// failure identifies one. Zero means "no opinion": use [protocol.ExitFailure].
	ExitCode protocol.ExitCode
}

// Retryable reports whether the executor may attempt the statement again.
func (c Classification) Retryable() bool { return c.Class == ClassRetryable }

// SQLStateError is implemented by driver errors that carry a PostgreSQL
// SQLSTATE. pgx's *pgconn.PgError satisfies it directly.
//
// This package imports no driver (the caller registers one), so the interface
// is declared here rather than imported. A driver whose error type does not
// implement it is handled by [Classifier.SQLState].
type SQLStateError interface {
	SQLState() string
}

// Classifier turns a driver error into a retry decision using SQLSTATE codes
// rather than message matching, because messages are localized, version
// dependent, and not a contract (FR-EXEC-3).
type Classifier struct {
	// SQLState extracts a SQLSTATE from a driver error whose type does not
	// implement [SQLStateError]. lib/pq is the case in point: *pq.Error carries
	// the code in a field. It is consulted only when the interface assertion
	// fails, and only the caller knows which driver is registered.
	SQLState func(error) (string, bool)
}

// DefaultClassifier classifies using [SQLStateError] alone.
var DefaultClassifier = Classifier{}

// sqlState recovers a SQLSTATE from err, preferring the standard interface.
func (c Classifier) sqlState(err error) (string, bool) {
	var se SQLStateError
	if errors.As(err, &se) {
		if s := se.SQLState(); len(s) == 5 {
			return s, true
		}
	}
	if c.SQLState != nil {
		if s, ok := c.SQLState(err); ok && len(s) == 5 {
			return s, true
		}
	}
	return "", false
}

// Classify decides whether err may be retried.
//
// The order is deliberate: our own cancellation first (it is not a database
// failure), then SQLSTATE, then typed transport failures. Anything
// unrecognized is terminal, which is the direction that fails safe: a run that
// stops and reports is recoverable, a run that retries a permission error 400
// times is noise.
func (c Classifier) Classify(err error) Classification {
	if err == nil {
		return Classification{Class: ClassNone}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Classification{Class: ClassCancelled, Condition: "context_cancelled"}
	}
	if state, ok := c.sqlState(err); ok {
		return classifySQLState(state)
	}
	return classifyTransport(err)
}

// Classify uses [DefaultClassifier].
func Classify(err error) Classification { return DefaultClassifier.Classify(err) }

// retryableStates are the SQLSTATEs a later attempt may survive. Sources:
// PostgreSQL "Appendix A. Error Codes".
var retryableStates = map[string]string{
	"40000": "transaction_rollback",
	"40001": "serialization_failure",
	"40003": "statement_completion_unknown",
	"40P01": "deadlock_detected",
	"55006": "object_in_use",
	"55P03": "lock_not_available", // what lock_timeout raises (FR-EXEC-5)
	"53200": "out_of_memory",
	"53300": "too_many_connections",
	"53400": "configuration_limit_exceeded",
	"58000": "system_error",
	"58030": "io_error",
	"72000": "snapshot_too_old",
}

// terminalStates are SQLSTATEs worth naming, either because the condition name
// is useful in a log or because the failure carries a specific exit code.
var terminalStates = map[string]struct {
	condition string
	exit      protocol.ExitCode
}{
	"42501": {"insufficient_privilege", protocol.ExitInsufficientPrivilege},
	"28000": {"invalid_authorization_specification", protocol.ExitInsufficientPrivilege},
	"28P01": {"invalid_password", protocol.ExitInsufficientPrivilege},
	"42601": {"syntax_error", 0},
	"42P07": {"duplicate_table", 0},
	"42P01": {"undefined_table", 0},
	"42703": {"undefined_column", 0},
	"42704": {"undefined_object", 0},
	"42710": {"duplicate_object", 0},
	"23505": {"unique_violation", 0},
	"23514": {"check_violation", 0},
	"23503": {"foreign_key_violation", 0},
	"0A000": {"feature_not_supported", 0},
	// Disk full is transient in principle and futile in practice: every retry
	// of an index build re-fills the disk and costs hours. Stop and let the
	// operator resume once there is room.
	"53100": {"disk_full", 0},
	"40002": {"transaction_integrity_constraint_violation", 0},
	"55000": {"object_not_in_prerequisite_state", 0},
	"58P01": {"undefined_file", 0},
	"58P02": {"duplicate_file", 0},
	"XX000": {"internal_error", 0},
}

// classifySQLState applies the code table, then the two error classes that are
// retryable as a whole: 08 (connection exception) and 57 (operator
// intervention, which is what a failover looks like from the client).
func classifySQLState(state string) Classification {
	if cond, ok := retryableStates[state]; ok {
		return Classification{Class: ClassRetryable, SQLState: state, Condition: cond}
	}
	if t, ok := terminalStates[state]; ok {
		return Classification{Class: ClassTerminal, SQLState: state, Condition: t.condition, ExitCode: t.exit}
	}
	switch state[:2] {
	case "08":
		return Classification{Class: ClassRetryable, SQLState: state, Condition: "connection_exception"}
	case "57":
		// 57014 query_canceled, 57P01 admin_shutdown, 57P02 crash_shutdown,
		// 57P03 cannot_connect_now, 57P05 idle_session_timeout. Our own
		// cancellation was already excluded above, so a 57014 here means the
		// server ended the statement, not us.
		return Classification{Class: ClassRetryable, SQLState: state, Condition: "operator_intervention"}
	case "42":
		return Classification{Class: ClassTerminal, SQLState: state, Condition: "syntax_error_or_access_rule_violation"}
	case "23":
		return Classification{Class: ClassTerminal, SQLState: state, Condition: "integrity_constraint_violation"}
	}
	return Classification{Class: ClassTerminal, SQLState: state, Condition: "unclassified"}
}

// classifyTransport handles failures that never reached the server, or that
// killed the connection before a SQLSTATE could be read. Every check is on a
// typed error, never on message text.
func classifyTransport(err error) Classification {
	switch {
	case errors.Is(err, driver.ErrBadConn),
		errors.Is(err, sql.ErrConnDone),
		errors.Is(err, io.EOF),
		errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, net.ErrClosed),
		errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, syscall.ECONNABORTED),
		errors.Is(err, syscall.ECONNREFUSED),
		errors.Is(err, syscall.EPIPE),
		errors.Is(err, syscall.ETIMEDOUT),
		errors.Is(err, syscall.EHOSTUNREACH),
		errors.Is(err, syscall.ENETUNREACH),
		errors.Is(err, syscall.ENETRESET):
		return Classification{Class: ClassRetryable, Condition: "connection_lost"}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return Classification{Class: ClassRetryable, Condition: "network_timeout"}
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return Classification{Class: ClassRetryable, Condition: "network_error"}
	}
	return Classification{Class: ClassTerminal, Condition: "unclassified", ExitCode: protocol.ExitCodeFor(err)}
}
