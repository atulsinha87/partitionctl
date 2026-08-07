package state

import (
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// AuditEventType is the machine-readable class of an audit event. The set is
// open: a new operation may emit a new type without an engine change, which is
// why this is a string and not an enum with a closed switch.
type AuditEventType string

// The event types the engine emits. Values are dotted and stable; the audit
// trail is the artifact a change-management reviewer reads after the fact
// (TRD §11.3).
const (
	EventRunOpened        AuditEventType = "run.opened"
	EventRunStatusChanged AuditEventType = "run.status_changed"
	EventRunAdopted       AuditEventType = "run.adopted"
	EventRunOrphaned      AuditEventType = "run.orphaned"
	EventCancelRequested  AuditEventType = "run.cancel_requested"

	EventLeaseAcquired AuditEventType = "lease.acquired"
	EventLeaseReleased AuditEventType = "lease.released"

	EventNodeTransition AuditEventType = "node.transition"

	EventObjectCreated      AuditEventType = "object.created"
	EventObjectCreateFailed AuditEventType = "object.create_failed"

	EventAuthorizationRecorded AuditEventType = "authorization.recorded"
	EventDestructiveExecuted   AuditEventType = "destructive.executed"
	EventDestructiveFailed     AuditEventType = "destructive.failed"
)

// AuditEvent is one entry in the append-only trail (FR-STATE-5, INV-3).
//
// There is no update and no delete path for an audit event anywhere in this
// package. That is the enforcement: not a convention the callers follow, but an
// API that has no such method to call. The file implementation opens the trail
// O_APPEND and never rewrites it; the SQL implementation only ever INSERTs.
type AuditEvent struct {
	// EventID is assigned by the store and is stable and unique.
	EventID string `json:"event_id"`

	// Seq is monotonically increasing within a run, starting at 1. It is the
	// cursor [StateStore.ListAudit] pages on.
	Seq int64 `json:"seq"`

	RunID  RunID           `json:"run_id"`
	NodeID protocol.NodeID `json:"node_id,omitempty"`

	Type AuditEventType `json:"type"`

	// Detail is structured supporting context. Keys are stable per event type
	// (NFR-OBS-2).
	Detail map[string]string `json:"detail,omitempty"`

	OccurredAt protocol.Timestamp `json:"occurred_at"`

	// At is the caller's preferred timestamp. It is not serialized: the store
	// normalizes it into OccurredAt.
	At time.Time `json:"-"`
}

func (e *AuditEvent) validate() error {
	if e == nil {
		return protocol.ErrFailure.Detailf("audit event is nil")
	}
	if e.RunID == "" {
		return protocol.ErrFailure.Detailf("audit event has an empty run id")
	}
	if e.Type == "" {
		return protocol.ErrFailure.Detailf("audit event has an empty type")
	}
	return nil
}

// auditDetail builds a detail map, skipping empty values so the trail stays
// readable.
func auditDetail(kv ...string) map[string]string {
	if len(kv) == 0 {
		return nil
	}
	m := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i] == "" || kv[i+1] == "" {
			continue
		}
		m[kv[i]] = kv[i+1]
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
