package cli

import (
	"context"
	"errors"
	"time"

	"github.com/atulsinha87/partitionctl/engine/executor"
	"github.com/atulsinha87/partitionctl/engine/protocol"
	"github.com/atulsinha87/partitionctl/engine/state"
)

// executorStore adapts the full [state.StateStore] to the narrow
// [executor.StateStore] the dispatch loop declares.
//
// The two are deliberately different. engine/state owns runs, leases, locks,
// orphan recovery and the audit trail; engine/executor asks only for the five
// operations a dispatch loop performs, so that a fake is a few lines. Joining
// them is this type's whole job, and there are two places where the join is not
// mechanical:
//
//  1. [state.AuthorizationStore.RecordAuthorization] takes the destructive
//     statement as a callback, so that INV-2 is structural rather than a rule
//     the caller follows. The executor's port splits the two: it records, then
//     dispatches. Passing a nil callback is therefore correct and is the only
//     shape that fits — the ordering guarantee still holds, because the
//     executor's record call returns only once the record is committed, and the
//     statement is issued after it returns.
//
//  2. [state.NodeStore.TransitionNode] is a compare-and-set that also owns the
//     attempt counter, while [executor.Transition] reports the cumulative count.
//     The counter is incremented here exactly on READY -> RUNNING, which is the
//     one edge that represents a dispatch.
type executorStore struct {
	store state.StateStore
	// database narrows claim and authorization records to one target, because a
	// file store can hold state for more than one.
	database string
}

// newExecutorStore wires the bridge.
func newExecutorStore(s state.StateStore, database string) *executorStore {
	return &executorStore{store: s, database: database}
}

var _ executor.StateStore = (*executorStore)(nil)

// ClaimsObject implements [executor.AuthorityReader] (FR-AUTH-2 as amended).
//
// It is scoped to the target database. A [state.FileStore] deliberately holds
// state for several targets, so an unscoped answer could adopt an index in one
// database on the strength of a run against another. An unscoped store fails
// closed, which is the direction that halts rather than drops.
func (b *executorStore) ClaimsObject(ctx context.Context, object protocol.ObjectName) (executor.RunID, bool, error) {
	if b.database == "" {
		return "", false, nil
	}
	run, found, err := state.ClaimsObjectIn(ctx, b.store, b.database, object)
	return executor.RunID(run), found, err
}

// NodeStates implements [executor.StateStore].
func (b *executorStore) NodeStates(ctx context.Context, run executor.RunID) (map[protocol.NodeID]executor.NodeRecord, error) {
	recs, err := b.store.ListNodes(ctx, state.RunID(run))
	if err != nil {
		return nil, err
	}
	out := make(map[protocol.NodeID]executor.NodeRecord, len(recs))
	for _, r := range recs {
		out[r.NodeID] = executor.NodeRecord{
			NodeID:    r.NodeID,
			State:     r.State,
			Attempts:  r.Attempts,
			LastError: r.LastError,
			UpdatedAt: r.UpdatedAt.Time,
		}
	}
	return out, nil
}

// RecordTransition implements [executor.StateStore] (FR-EXEC-2).
func (b *executorStore) RecordTransition(ctx context.Context, t executor.Transition) error {
	reason := t.Reason
	if reason == "" {
		reason = protocol.ReasonNormal
	}
	var cause error
	if t.Error != "" {
		cause = &transitionCause{msg: t.Error, kind: t.ErrorKind}
	}
	_, err := b.store.TransitionNode(ctx, state.NodeTransition{
		RunID:  state.RunID(t.RunID),
		NodeID: t.NodeID,
		From:   t.From,
		To:     t.To,
		Reason: reason,
		// READY -> RUNNING is the edge that represents one dispatch, and the
		// attempt counter counts dispatches (FR-EXEC-4).
		IncrementAttempt: t.From == protocol.NodeReady && t.To == protocol.NodeRunning,
		Err:              cause,
		At:               t.At,
	})
	return err
}

// transitionCause carries an executor-reported failure across the port without
// losing its machine-readable class, which is what the state store records and
// what `status --json` reports (NFR-OBS-2).
type transitionCause struct {
	msg  string
	kind protocol.ErrorKind
}

func (e *transitionCause) Error() string { return e.msg }

// As lets [protocol.KindOf] recover the class the executor assigned, so a
// failure classified as, say, a topology drift is still recorded as one after
// crossing the port.
func (e *transitionCause) As(target any) bool {
	p, ok := target.(**protocol.Error)
	if !ok || e.kind == "" {
		return false
	}
	*p = &protocol.Error{Kind: e.kind, Code: protocol.ExitFailure, Msg: e.msg}
	return true
}

// CancelRequested implements [executor.StateStore] (FR-CLI-10).
func (b *executorStore) CancelRequested(ctx context.Context, run executor.RunID) (bool, error) {
	return b.store.CancellationRequested(ctx, state.RunID(run))
}

// RecordAuthorization implements [executor.StateStore] and INV-2 (FR-AUTH-6,
// AC-20).
func (b *executorStore) RecordAuthorization(ctx context.Context, a executor.AuthorizationRecord) error {
	rec := state.AuthorizationRecord{
		RunID:    state.RunID(a.RunID),
		NodeID:   a.NodeID,
		Mode:     a.Mode,
		Object:   a.Object,
		Database: b.database,
		Evidence: a.Evidence,
	}
	if !a.GrantedAt.IsZero() {
		rec.GrantedAt = protocol.NewTimestamp(a.GrantedAt)
	}
	if rel, ok := a.Evidence["relation"]; ok {
		if name, err := protocol.ParseObjectName(rel); err == nil {
			rec.Relation = &name
		}
	}

	if a.Mode == protocol.AuthExplicit {
		rec.Confirmation = a.Evidence["confirmation"]
	}

	_, err := b.store.RecordAuthorization(ctx, rec, nil)
	return err
}

// AppendAudit implements [executor.StateStore] (INV-3).
//
// The executor's event vocabulary is passed through unchanged. engine/state
// documents its own set as open precisely so that a consumer can add to it
// without an engine change, and rewriting the executor's names here would make
// the trail describe events under names no package declares.
func (b *executorStore) AppendAudit(ctx context.Context, e executor.AuditEvent) error {
	_, err := b.store.AppendAudit(ctx, state.AuditEvent{
		RunID:  state.RunID(e.RunID),
		NodeID: e.NodeID,
		Type:   state.AuditEventType(e.Type),
		Detail: e.Detail,
		At:     e.At,
	})
	return err
}

// ---------------------------------------------------------------------------
// Heartbeat
// ---------------------------------------------------------------------------

// leaseHeartbeat renews both halves of INV-4 on one timer: the run lease, which
// answers "is the holder alive?", and the advisory lock, which answers "does
// anyone hold this target?".
//
// Refreshing the lock matters only for [state.FileStore], whose lock is a file
// with a TTL rather than a session; [state.SQLStore]'s Refresh is a documented
// no-op because the server releases a session-level advisory lock when the
// session ends. Calling both from one place is what keeps the two store
// implementations behaving the same from the executor's point of view.
type leaseHeartbeat struct {
	store  state.LeaseStore
	lock   state.Lock
	holder string
}

var _ executor.Heartbeater = (*leaseHeartbeat)(nil)

// Heartbeat implements [executor.Heartbeater] (FR-LOCK-3).
//
// A definitive loss of either half is translated to [executor.ErrFenced], which
// is the one heartbeat failure that stops dispatch (FR-LOCK-1, AC-10). Losing
// the lease means another process has adopted this run; losing the lock means
// another process holds the target. Continuing after either would put two
// executors on one partitioned table.
//
// Every other failure is left as it is, so a blip talking to the state store
// stays a warning rather than abandoning a multi-hour build.
func (h *leaseHeartbeat) Heartbeat(ctx context.Context, run executor.RunID) error {
	if h.lock != nil {
		if err := h.lock.Refresh(ctx); err != nil {
			return fenceIfLost(err)
		}
	}
	if _, err := h.store.Heartbeat(ctx, state.RunID(run), h.holder); err != nil {
		return fenceIfLost(err)
	}
	return nil
}

// fenceIfLost maps the two "someone else has it" errors onto the executor's
// fencing signal, and passes everything else through unchanged.
func fenceIfLost(err error) error {
	if errors.Is(err, state.ErrLeaseLost) || errors.Is(err, protocol.ErrLockHeld) {
		return executor.ErrFenced.Detailf("%v", err)
	}
	return err
}

// ---------------------------------------------------------------------------
// Claims, for the planner
// ---------------------------------------------------------------------------

// claimLookup adapts the state store to the one-method view the planner and the
// create-index operation depend on (FR-PLAN-6, FR-PLAN-7).
//
// Both declare the same method under different interface names, so one type
// satisfies both. That is the point of a one-method port: it needs a two-line
// adapter, not a shared dependency.
type claimLookup struct {
	store    state.ClaimReader
	database string
}

// ClaimsObject reports the run holding a live claim on object.
//
// Both ways of having no answer report "no claim", which is the safe direction:
// it leaves the ownership marker on the object as the only thing that can
// authorize a drop (FR-PLAN-6, FR-PLAN-7, AC-5, AC-6, NFR-REL-3).
//
// An unscoped store is one of those ways. [state.ClaimsObjectIn] treats an empty
// database as "match any", and a file state store deliberately holds state for
// several targets, so an unscoped lookup could report a claim held by a run
// against a *different* database.
func (c claimLookup) ClaimsObject(ctx context.Context, object protocol.ObjectName) (string, bool, error) {
	if c.store == nil || c.database == "" {
		return "", false, nil
	}
	run, found, err := state.ClaimsObjectIn(ctx, c.store, c.database, object)
	return string(run), found, err
}

// ---------------------------------------------------------------------------
// Small helpers over the store the commands share
// ---------------------------------------------------------------------------

// isNotFound reports whether err is the state store's "no such record".
func isNotFound(err error) bool { return errors.Is(err, state.ErrNotFound) }

// stateLocation names where the state store lives, for an error message that
// has to tell an operator which store was searched. It never reports the DSN,
// which can carry a password (NFR-SEC-3).
func stateLocation(c Config) string {
	switch c.State {
	case StateFile:
		if c.StateDir != "" {
			return " at " + c.StateDir
		}
	case StateSQL:
		if c.StateSchema != "" {
			return " in schema " + c.StateSchema
		}
	}
	return ""
}

// retryPolicyFrom builds the executor's retry policy from configuration
// (FR-EXEC-4).
func retryPolicyFrom(c Config) executor.RetryPolicy {
	p := executor.RetryPolicy{
		MaxAttempts: c.MaxAttempts,
		BaseDelay:   c.RetryBaseDelay,
		MaxDelay:    c.RetryMaxDelay,
		Jitter:      0.5,
	}
	if p.BaseDelay <= 0 {
		p.BaseDelay = time.Second
	}
	if p.MaxDelay < p.BaseDelay {
		p.MaxDelay = p.BaseDelay
	}
	return p
}
