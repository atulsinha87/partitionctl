package executor

import (
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// timeLayout is the one time format this package emits, matching
// [protocol.Timestamp]'s canonical form.
const timeLayout = time.RFC3339Nano

// LogEvent is one structured log record. The field set is the stable schema in
// TRD §14.1 (NFR-OBS-2): run_id, node_id, kind, state, attempt, duration_ms,
// error_class. The rest are additive and optional.
type LogEvent struct {
	TS        string             `json:"ts"`
	Event     string             `json:"event"`
	RunID     RunID              `json:"run_id"`
	NodeID    protocol.NodeID    `json:"node_id,omitempty"`
	Kind      protocol.NodeKind  `json:"kind,omitempty"`
	State     protocol.NodeState `json:"state,omitempty"`
	PrevState protocol.NodeState `json:"prev_state,omitempty"`
	Attempt   int                `json:"attempt,omitempty"`

	DurationMS int64 `json:"duration_ms,omitempty"`

	// ErrorClass is the protocol error kind, which is what CI branches on
	// together with the exit code.
	ErrorClass protocol.ErrorKind `json:"error_class,omitempty"`
	// RetryClass is the executor's retry decision for the same error.
	RetryClass ErrorClass `json:"retry_class,omitempty"`
	// SQLState is the PostgreSQL SQLSTATE, when one was recovered.
	SQLState string `json:"sqlstate,omitempty"`
	Error    string `json:"error,omitempty"`
	Message  string `json:"message,omitempty"`
}

// Logger receives one record per node transition (FR-EXEC-7). Implementations
// must be safe for concurrent use: the heartbeat runs on its own goroutine.
type Logger interface {
	Log(ev LogEvent)
}

// NopLogger discards every record. It is the default.
type NopLogger struct{}

// Log discards ev.
func (NopLogger) Log(LogEvent) {}

// JSONLogger writes one JSON object per line (NFR-OBS-2). It never writes
// credentials, because [LogEvent] has nowhere to put them (NFR-SEC-3).
type JSONLogger struct {
	mu sync.Mutex
	w  io.Writer
}

// NewJSONLogger returns a logger writing newline-delimited JSON to w.
func NewJSONLogger(w io.Writer) *JSONLogger { return &JSONLogger{w: w} }

// Log writes ev as one line. Encoding failures are dropped: a log write must
// never be able to fail a run.
func (l *JSONLogger) Log(ev LogEvent) {
	if l == nil || l.w == nil {
		return
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.w.Write(append(b, '\n'))
}
