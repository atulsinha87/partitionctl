package state

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

func TestRunStatusPredicates(t *testing.T) {
	tests := []struct {
		status     RunStatus
		terminal   bool
		resumable  bool
		incomplete bool
	}{
		{status: RunRunning, terminal: false, resumable: false, incomplete: true},
		{status: RunCompleted, terminal: true, resumable: false, incomplete: false},
		{status: RunFailed, terminal: false, resumable: true, incomplete: true},
		{status: RunOrphaned, terminal: false, resumable: true, incomplete: true},
		{status: RunInterrupted, terminal: false, resumable: true, incomplete: true},
		{status: RunCancelled, terminal: true, resumable: false, incomplete: false},
	}
	for _, tc := range tests {
		t.Run(string(tc.status), func(t *testing.T) {
			if !tc.status.Valid() {
				t.Fatalf("%s is not Valid", tc.status)
			}
			if got := tc.status.IsTerminal(); got != tc.terminal {
				t.Errorf("IsTerminal = %v, want %v", got, tc.terminal)
			}
			if got := tc.status.IsResumable(); got != tc.resumable {
				t.Errorf("IsResumable = %v, want %v", got, tc.resumable)
			}
			if got := tc.status.IsIncomplete(); got != tc.incomplete {
				t.Errorf("IsIncomplete = %v, want %v", got, tc.incomplete)
			}
			if tc.status.IsTerminal() && tc.status.IsResumable() {
				t.Error("a status cannot be both terminal and resumable")
			}
		})
	}
	if RunStatus("MADE_UP").Valid() {
		t.Error("an unknown status is Valid")
	}
	if len(AllRunStatuses()) != len(tests) {
		t.Errorf("AllRunStatuses has %d entries but the table covers %d", len(AllRunStatuses()), len(tests))
	}
	// The returned slice is a copy.
	all := AllRunStatuses()
	all[0] = "mutated"
	if AllRunStatuses()[0] == "mutated" {
		t.Error("AllRunStatuses returns the backing array")
	}
}

func TestValidRunTransition(t *testing.T) {
	tests := []struct {
		name string
		from RunStatus
		to   RunStatus
		want bool
	}{
		{name: "running completes", from: RunRunning, to: RunCompleted, want: true},
		{name: "running fails", from: RunRunning, to: RunFailed, want: true},
		{name: "running orphans", from: RunRunning, to: RunOrphaned, want: true},
		{name: "running interrupts", from: RunRunning, to: RunInterrupted, want: true},
		{name: "running cancels", from: RunRunning, to: RunCancelled, want: true},
		{name: "orphaned resumes", from: RunOrphaned, to: RunRunning, want: true},
		{name: "orphaned cancels", from: RunOrphaned, to: RunCancelled, want: true},
		{name: "failed resumes", from: RunFailed, to: RunRunning, want: true},
		{name: "interrupted resumes", from: RunInterrupted, to: RunRunning, want: true},
		{name: "completed is terminal", from: RunCompleted, to: RunFailed, want: false},
		{name: "cancelled is terminal", from: RunCancelled, to: RunRunning, want: false},
		{name: "orphaned cannot complete directly", from: RunOrphaned, to: RunCompleted, want: false},
		{name: "self transition", from: RunRunning, to: RunRunning, want: false},
		{name: "unknown from", from: "NOPE", to: RunRunning, want: false},
		{name: "unknown to", from: RunRunning, to: "NOPE", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidRunTransition(tc.from, tc.to); got != tc.want {
				t.Errorf("ValidRunTransition(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
			err := CheckRunTransition(tc.from, tc.to)
			if (err == nil) != tc.want {
				t.Errorf("CheckRunTransition = %v, want ok=%v", err, tc.want)
			}
		})
	}
}

func TestNewRunID(t *testing.T) {
	a := NewRunID(baseTime)
	b := NewRunID(baseTime)
	if a == b {
		t.Fatal("two ids generated at the same instant collided")
	}
	if !strings.HasPrefix(string(a), "run-20260807T120000Z-") {
		t.Errorf("id %q does not carry a sortable time prefix", a)
	}
	// The id must survive being used as a path segment and as a SQL value.
	if strings.ContainsAny(string(a), "/\\ \t\n\"'") {
		t.Errorf("id %q contains a character that needs escaping", a)
	}
}

func TestRunLockKey(t *testing.T) {
	r := Run{Target: protocol.Target{
		Database: "appdb",
		Table:    protocol.NewObjectName("public", "orders"),
	}}
	want := LockKey{Database: "appdb", Table: protocol.NewObjectName("public", "orders")}
	if got := r.LockKey(); got != want {
		t.Errorf("LockKey = %v, want %v", got, want)
	}
}

func TestEncodeSegment(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{name: "plain", id: "node-1"},
		{name: "slashes", id: "leaf/orders/2026"},
		{name: "parent traversal", id: "../../etc/passwd"},
		{name: "leading dot", id: ".hidden"},
		{name: "empty", id: ""},
		{name: "unicode", id: "ünïcøde"},
		{name: "very long", id: strings.Repeat("x", 500)},
		{name: "nul byte", id: "a\x00b"},
	}

	seen := map[string]string{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := encodeSegment(tc.id)
			if got == "" {
				t.Fatal("encoded to an empty segment")
			}
			if strings.HasPrefix(got, ".") {
				t.Errorf("%q encoded to the hidden file %q", tc.id, got)
			}
			if strings.ContainsAny(got, `/\`+"\x00") {
				t.Errorf("%q encoded to %q, which contains a path separator", tc.id, got)
			}
			if len(got) > segmentReadableMax+20 {
				t.Errorf("%q encoded to %d bytes", tc.id, len(got))
			}
			if again := encodeSegment(tc.id); again != got {
				t.Errorf("encodeSegment is not deterministic: %q then %q", got, again)
			}
			if prev, dup := seen[got]; dup {
				t.Errorf("%q and %q both encode to %q", prev, tc.id, got)
			}
			seen[got] = tc.id
		})
	}

	// Two ids that share a long prefix must not collide, which is the whole
	// reason the hash suffix exists.
	long := strings.Repeat("leaf-orders-partition-", 10)
	if encodeSegment(long+"a") == encodeSegment(long+"b") {
		t.Error("two long ids sharing a prefix collided")
	}
}

func TestNodeTransitionValidate(t *testing.T) {
	tests := []struct {
		name    string
		t       NodeTransition
		wantErr bool
	}{
		{
			name: "valid",
			t:    NodeTransition{RunID: "r", NodeID: "n", From: protocol.NodePending, To: protocol.NodeReady},
		},
		{
			name:    "no run id",
			t:       NodeTransition{NodeID: "n", From: protocol.NodePending, To: protocol.NodeReady},
			wantErr: true,
		},
		{
			name:    "no node id",
			t:       NodeTransition{RunID: "r", From: protocol.NodePending, To: protocol.NodeReady},
			wantErr: true,
		},
		{
			name:    "an edge D7 does not have",
			t:       NodeTransition{RunID: "r", NodeID: "n", From: protocol.NodeDone, To: protocol.NodeReady},
			wantErr: true,
		},
		{
			name: "an empty reason defaults to normal",
			t:    NodeTransition{RunID: "r", NodeID: "n", From: protocol.NodeReady, To: protocol.NodeRunning},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.t.validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("validate = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && tc.t.reason() != protocol.ReasonNormal && tc.t.Reason == "" {
				t.Error("an empty reason did not default to normal")
			}
		})
	}
}

func TestDefaultHolderIsStable(t *testing.T) {
	a, b := DefaultHolder(), DefaultHolder()
	if a != b {
		t.Errorf("DefaultHolder is not stable within a process: %q then %q", a, b)
	}
	if !strings.Contains(a, "/") {
		t.Errorf("holder %q does not carry host/pid", a)
	}
}

func TestOpenFileStoreRejectsAnEmptyRoot(t *testing.T) {
	if _, err := OpenFileStore("", FileOptions{}); err == nil {
		t.Fatal("want an error")
	}
}

func TestFileStoreCloseIsANoOp(t *testing.T) {
	s, _ := newFileStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A closed store is still usable: Close exists to satisfy the port, and
	// the file store owns nothing to release.
	if _, err := s.CreateRun(context.Background(), NewRun{Plan: testPlan(t), RunID: "after-close"}); err != nil {
		t.Fatalf("CreateRun after Close: %v", err)
	}
}

func TestSystemClockAdvances(t *testing.T) {
	a := SystemClock()
	if a.IsZero() {
		t.Fatal("SystemClock returned the zero time")
	}
	if time.Since(a) > time.Minute {
		t.Errorf("SystemClock returned %s, which is not now", a)
	}
}
