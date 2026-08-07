package executor

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

func TestClassifyBySQLState(t *testing.T) {
	tests := []struct {
		name     string
		state    string
		want     ErrorClass
		wantExit protocol.ExitCode
	}{
		// FR-EXEC-3 names four retryable conditions explicitly.
		{"connection reset", "08006", ClassRetryable, 0},
		{"connection does not exist", "08003", ClassRetryable, 0},
		{"protocol violation", "08P01", ClassRetryable, 0},
		{"deadlock", "40P01", ClassRetryable, 0},
		{"lock timeout", "55P03", ClassRetryable, 0},
		{"serialization failure", "40001", ClassRetryable, 0},

		// Transient conditions worth surviving on managed PostgreSQL.
		{"admin shutdown", "57P01", ClassRetryable, 0},
		{"cannot connect now", "57P03", ClassRetryable, 0},
		{"query canceled by the server", "57014", ClassRetryable, 0},
		{"too many connections", "53300", ClassRetryable, 0},
		{"object in use", "55006", ClassRetryable, 0},
		{"io error", "58030", ClassRetryable, 0},

		// FR-EXEC-3 names three terminal conditions explicitly.
		{"syntax error", "42601", ClassTerminal, 0},
		{"insufficient privilege", "42501", ClassTerminal, protocol.ExitInsufficientPrivilege},
		{"unique violation", "23505", ClassTerminal, 0},

		// Retrying these makes the situation worse, not better.
		{"disk full", "53100", ClassTerminal, 0},
		{"duplicate relation", "42P07", ClassTerminal, 0},
		{"undefined table", "42P01", ClassTerminal, 0},
		{"feature not supported", "0A000", ClassTerminal, 0},
		{"internal error", "XX000", ClassTerminal, 0},
		{"invalid password", "28P01", ClassTerminal, protocol.ExitInsufficientPrivilege},
		{"object not in prerequisite state", "55000", ClassTerminal, 0},

		// Anything unrecognized fails safe.
		{"unknown class", "ZZ999", ClassTerminal, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(&pgErr{code: tc.state, msg: tc.name})
			if got.Class != tc.want {
				t.Fatalf("Classify(%s).Class = %q, want %q", tc.state, got.Class, tc.want)
			}
			if got.SQLState != tc.state {
				t.Fatalf("SQLState = %q, want %q", got.SQLState, tc.state)
			}
			if tc.wantExit != 0 && got.ExitCode != tc.wantExit {
				t.Fatalf("ExitCode = %d, want %d", got.ExitCode, tc.wantExit)
			}
			if got.Retryable() != (tc.want == ClassRetryable) {
				t.Fatalf("Retryable() disagrees with Class %q", got.Class)
			}
		})
	}
}

func TestClassifyTransportFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ErrorClass
	}{
		{"nil", nil, ClassNone},
		{"bad connection", driver.ErrBadConn, ClassRetryable},
		{"connection done", sql.ErrConnDone, ClassRetryable},
		{"eof", io.EOF, ClassRetryable},
		{"unexpected eof", io.ErrUnexpectedEOF, ClassRetryable},
		{"connection reset", fmt.Errorf("write tcp: %w", syscall.ECONNRESET), ClassRetryable},
		{"broken pipe", fmt.Errorf("write tcp: %w", syscall.EPIPE), ClassRetryable},
		{"connection refused", fmt.Errorf("dial tcp: %w", syscall.ECONNREFUSED), ClassRetryable},
		{"net closed", net.ErrClosed, ClassRetryable},
		{"op error", &net.OpError{Op: "read", Err: errors.New("i/o problem")}, ClassRetryable},
		{"timeout", &timeoutError{}, ClassRetryable},

		// Our own cancellation is not a database failure.
		{"context canceled", context.Canceled, ClassCancelled},
		{"deadline exceeded", context.DeadlineExceeded, ClassCancelled},

		// A plain error carries no evidence that a retry would differ.
		{"opaque", errors.New("something went wrong"), ClassTerminal},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.err).Class; got != tc.want {
				t.Fatalf("Classify(%v).Class = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

type timeoutError struct{}

func (*timeoutError) Error() string   { return "i/o timeout" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

func TestClassifyPrefersSQLStateOverWrapping(t *testing.T) {
	// A SQLSTATE recovered through several layers of fmt.Errorf still decides
	// the class, which is why the executor never matches on message text.
	err := fmt.Errorf("exec create index: %w", fmt.Errorf("pq: %w", &pgErr{code: "40P01", msg: "deadlock detected"}))
	got := Classify(err)
	if got.Class != ClassRetryable || got.SQLState != "40P01" {
		t.Fatalf("Classify = %+v, want retryable 40P01", got)
	}
}

func TestClassifierFallsBackToTheCallerSuppliedExtractor(t *testing.T) {
	// lib/pq's error type carries the code in a field rather than a method, so
	// the caller, who knows which driver is registered, supplies the extractor.
	c := Classifier{
		SQLState: func(err error) (string, bool) {
			var e *pqLikeError
			if errors.As(err, &e) {
				return e.code, true
			}
			return "", false
		},
	}
	got := c.Classify(&pqLikeError{code: "40001"})
	if got.Class != ClassRetryable {
		t.Fatalf("Classify = %+v, want retryable via the caller-supplied extractor", got)
	}
	// Without the extractor the same error is terminal, which is the safe
	// default rather than a guess.
	if got := DefaultClassifier.Classify(&pqLikeError{code: "40001"}); got.Class != ClassTerminal {
		t.Fatalf("Classify = %+v, want terminal without an extractor", got)
	}
}

type pqLikeError struct{ code string }

func (e *pqLikeError) Error() string { return "pq: " + e.code }

func TestCancelledContextIsNeverRetried(t *testing.T) {
	// 57014 is query_canceled. When *we* cancelled, it must not be retried; the
	// ordering inside Classify is what guarantees that.
	err := fmt.Errorf("%w: %w", context.Canceled, &pgErr{code: "57014", msg: "canceling statement"})
	if got := Classify(err).Class; got != ClassCancelled {
		t.Fatalf("Classify = %q, want cancelled", got)
	}
}
