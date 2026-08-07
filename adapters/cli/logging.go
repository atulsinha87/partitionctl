package cli

import (
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/atulsinha/partitionctl/engine/executor"
	"github.com/atulsinha/partitionctl/engine/protocol"
)

// timeLayout is the timestamp format in every log record: RFC 3339 with
// millisecond precision, always UTC, so records from two hosts sort correctly.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// Logger writes the structured JSON records of NFR-OBS-2.
//
// The field schema is [executor.LogEvent]'s and is stable: run_id, node_id,
// kind, state, attempt, duration_ms, error_class (TRD §14.1). Everything else
// is additive and optional. The CLI's own records reuse the same struct rather
// than declaring a second schema, so a consumer parses one shape.
//
// Nothing here can carry a credential: [executor.LogEvent] has no field for one
// and [Config.Redacted] reports the DSN only as present or absent (NFR-SEC-3).
type Logger struct {
	mu  sync.Mutex
	enc *json.Encoder
	now func() time.Time
}

// NewLogger returns a logger writing newline-delimited JSON to w.
func NewLogger(w io.Writer, now func() time.Time) *Logger {
	if now == nil {
		now = time.Now
	}
	return &Logger{enc: json.NewEncoder(w), now: now}
}

var _ executor.Logger = (*Logger)(nil)

// Log writes one record. It is safe for concurrent use, because the executor's
// heartbeat logs from its own goroutine.
//
// An encoding failure is dropped. A log write must never be able to fail a run:
// losing a line is an observability problem, and aborting a six-hour index
// build over one is a correctness problem.
func (l *Logger) Log(ev executor.LogEvent) {
	if l == nil || l.enc == nil {
		return
	}
	if ev.TS == "" {
		ev.TS = l.now().UTC().Format(timeLayout)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.enc.Encode(ev)
}

// Event writes a CLI-level record: one that belongs to a command rather than to
// a node transition.
func (l *Logger) Event(name string, detail map[string]string) {
	ev := executor.LogEvent{Event: name}
	if msg, ok := detail["message"]; ok {
		ev.Message = msg
	}
	if run, ok := detail["run_id"]; ok {
		ev.RunID = executor.RunID(run)
	}
	l.Log(ev)
}

// Failure writes the record that closes a failed command, carrying the error's
// stable class and the exit code CI branches on (FR-CLI-13, NFR-OBS-2).
func (l *Logger) Failure(command string, err error) {
	if err == nil {
		return
	}
	l.Log(executor.LogEvent{
		Event:      command + "_failed",
		ErrorClass: protocol.KindOf(err),
		Error:      err.Error(),
		Message:    "exit " + protocol.ExitCodeFor(err).String(),
	})
}
