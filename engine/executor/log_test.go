package executor

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// captureLogger keeps every record so a test can assert the schema.
type captureLogger struct{ events []LogEvent }

func (l *captureLogger) Log(ev LogEvent) { l.events = append(l.events, ev) }

func TestEveryNodeTransitionIsLogged(t *testing.T) {
	h := newHarness()
	log := &captureLogger{}
	h.cfg.Logger = log

	if _, err := h.run(t, createChainPlan(t)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// FR-EXEC-7: one record per transition, and the store saw exactly the same
	// set of edges.
	if len(log.events) != len(h.store.transitions) {
		t.Fatalf("logged %d events for %d checkpoints", len(log.events), len(h.store.transitions))
	}
	for i, ev := range log.events {
		tr := h.store.transitions[i]
		if ev.NodeID != tr.NodeID || ev.State != tr.To || ev.PrevState != tr.From {
			t.Fatalf("event %d = %+v does not match checkpoint %+v", i, ev, tr)
		}
		// TRD §14.1 stable field schema.
		if ev.TS == "" || ev.RunID == "" || ev.Kind == "" {
			t.Fatalf("event %d is missing schema fields: %+v", i, ev)
		}
	}
}

func TestFailureLogRecordsTheErrorClass(t *testing.T) {
	h := newHarness()
	log := &captureLogger{}
	h.cfg.Logger = log
	h.sql.errs["n2"] = []error{&pgErr{code: "40P01", msg: "deadlock detected"}}

	if _, err := h.run(t, createChainPlan(t)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var found bool
	for _, ev := range log.events {
		if ev.NodeID == "n2" && ev.State == protocol.NodeRetryWait {
			found = true
			if ev.RetryClass != ClassRetryable || ev.SQLState != "40P01" {
				t.Fatalf("retry event = %+v, want the retry class and SQLSTATE recorded", ev)
			}
			if ev.Error == "" {
				t.Fatal("retry event carries no error text")
			}
		}
	}
	if !found {
		t.Fatal("no RETRY_WAIT event was logged")
	}
}

func TestJSONLoggerWritesOneObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	l := NewJSONLogger(&buf)
	l.Log(LogEvent{TS: "2026-08-07T12:00:00Z", Event: "node_transition", RunID: "run-1", NodeID: "n1", State: protocol.NodeDone})
	l.Log(LogEvent{TS: "2026-08-07T12:00:01Z", Event: "node_transition", RunID: "run-1", NodeID: "n2", State: protocol.NodeReady})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want 2", len(lines))
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 1 is not JSON: %v", err)
	}
	for _, field := range []string{"ts", "event", "run_id", "node_id", "state"} {
		if _, ok := first[field]; !ok {
			t.Fatalf("line 1 is missing %q: %v", field, first)
		}
	}
	// Zero-valued optional fields stay out of the record rather than becoming
	// noise a log pipeline has to filter.
	if _, ok := first["duration_ms"]; ok {
		t.Fatalf("a zero duration was emitted: %v", first)
	}
}

func TestNilJSONLoggerIsSafe(t *testing.T) {
	var l *JSONLogger
	l.Log(LogEvent{Event: "x"})
	NopLogger{}.Log(LogEvent{Event: "x"})
}
