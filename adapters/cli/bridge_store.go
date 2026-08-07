package cli

import (
	"context"
	"errors"
	"time"

	"github.com/atulsinha/partitionctl/engine/executor"
	"github.com/atulsinha/partitionctl/engine/protocol"
	"github.com/atulsinha/partitionctl/engine/state"
)

// executorStore adapts the full [state.StateStore] to the narrow
// [executor.StateStore] the dispatch loop declares.
//
// The two are deliberately different. engine/state owns runs, leases, locks,
// orphan recovery and the audit trail; engine/executor asks only for the six
// operations a dispatch loop performs, so that a fake is a few lines. Joining
// them is this type's whole job, and there are four places where the join is
// not mechanical:
//
//  1. Provenance and authorization are recorded through
//     [state.ProvenanceStore.WriteProvenance] and
//     [state.AuthorizationStore.RecordAuthorization], both of which take the
//     guarded statement as a callback so that INV-1 and INV-2 are structural.
//     The executor's port splits the two: it records, then dispatches. Passing
//     a nil callback is therefore correct and is the only shape that fits — the
//     ordering guarantee still holds, because the executor's record call
//     returns only once the record is committed, and the statement is issued
//     after it returns. The callback form remains the stronger contract, and it
//     is what the resume-time cleanup in cleanup.go uses, where this package
//     does own both halves.
//
//  2. [state.AuthorizationRecord] requires typed evidence per mode: a
//     provenance id for AuthProvenance, a reindex run id for AuthLeftover.
//     [executor.Authorize] produces evidence as a string map and does not carry
//     the provenance record's id at all. So this type resolves the id from the
//     store itself before writing the record. That is not a workaround: it means
//     the id written to the audit trail is one this process read, rather than
//     one it was handed.
//
//  3. [state.NodeStore.TransitionNode] is a compare-and-set that also owns the
//     attempt counter, while [executor.Transition] reports the cumulative count.
//     The counter is incremented here exactly on READY -> RUNNING, which is the
//     one edge that represents a dispatch.
//
//  4. Provenance is written once per object rather than once per attempt. The
//     executor's port permits either; writing once keeps a node that retries
//     four hundred times from leaving four hundred identical records, and the
//     invariant INV-1 protects is "a committed record existed before the
//     statement ran", which an earlier attempt already satisfies.
type executorStore struct {
	store state.StateStore
	// database narrows provenance and authorization records to one target,
	// because a file store can hold state for more than one.
	database string
}

// newExecutorStore wires the bridge.
func newExecutorStore(s state.StateStore, database string) *executorStore {
	return &executorStore{store: s, database: database}
}

var _ executor.StateStore = (*executorStore)(nil)

// LookupProvenance implements [executor.AuthorityReader] (FR-AUTH-2).
func (b *executorStore) LookupProvenance(ctx context.Context, object protocol.ObjectName) (executor.Provenance, bool, error) {
	if b.database == "" {
		// Fail closed, for the same reason provenanceLookup does: an empty
		// Database makes ProvenanceQuery match *any* database, and a file store
		// deliberately holds state for several targets, so an unscoped answer
		// could authorize dropping a production index using a staging record.
		// This is the dispatch-time re-check that FR-AUTH-5 makes the last line
		// of defence, so it must not be weaker than the plan-time check it
		// backstops.
		return executor.Provenance{}, false, nil
	}
	recs, err := b.store.FindProvenance(ctx, state.ProvenanceQuery{
		Object:   object,
		Database: b.database,
	})
	if err != nil {
		return executor.Provenance{}, false, err
	}
	if len(recs) == 0 {
		return executor.Provenance{}, false, nil
	}
	r := recs[0]
	return executor.Provenance{
		RunID:      executor.RunID(r.RunID),
		NodeID:     r.NodeID,
		Object:     r.Object,
		ObjectKind: string(r.ObjectKind),
		Relation:   r.Relation,
		CreatedAt:  r.RecordedAt.Time,
	}, true, nil
}

// HasReindexRun implements [executor.AuthorityReader], the second and
// non-forgeable half of AuthLeftover (FR-AUTH-3, INV-7, AC-19).
//
// The query names the relation and, when it is a partition, would also name its
// parent; the executor's port carries only the relation, so the store is asked
// about that one. A reindex run recorded against the partitioned parent is
// found through the same query when the caller passes the parent, which is what
// the reindex planner will do in M3.
func (b *executorStore) HasReindexRun(ctx context.Context, relation protocol.ObjectName) (bool, error) {
	_, found, err := state.ReindexRunFor(ctx, b.store, state.ReindexHistoryQuery{
		Database:  b.database,
		Relations: []protocol.ObjectName{relation},
	})
	return found, err
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

// RecordProvenance implements [executor.StateStore] and INV-1.
//
// The guarded-action callback is nil because the executor's port records and
// dispatches separately; see the type doc. A record that already exists is left
// alone: it was committed before an earlier attempt's statement, which is the
// same guarantee, and re-writing it would grow the trail without adding
// information.
func (b *executorStore) RecordProvenance(ctx context.Context, p executor.Provenance) error {
	has, _, err := state.HasProvenance(ctx, b.store, state.ProvenanceQuery{
		Object:   p.Object,
		Database: b.database,
	})
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	rec := state.Provenance{
		RunID:      state.RunID(p.RunID),
		NodeID:     p.NodeID,
		Database:   b.database,
		Object:     p.Object,
		ObjectKind: state.ObjectKind(p.ObjectKind),
		Relation:   p.Relation,
	}
	if rec.ObjectKind == "" {
		rec.ObjectKind = state.ObjectIndex
	}
	if !p.CreatedAt.IsZero() {
		rec.RecordedAt = protocol.NewTimestamp(p.CreatedAt)
	}
	_, err = b.store.WriteProvenance(ctx, rec, nil)
	return err
}

// RecordAuthorization implements [executor.StateStore] and INV-2 (FR-AUTH-6,
// AC-20).
//
// The typed evidence the state store demands is resolved here rather than taken
// on trust from the decision's evidence map: see the type doc, point 2.
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

	switch a.Mode {
	case protocol.AuthProvenance:
		id, err := b.provenanceID(ctx, a.Object)
		if err != nil {
			return err
		}
		rec.ProvenanceID = id
	case protocol.AuthLeftover:
		if rec.Relation == nil {
			return protocol.ErrAuthorizationUnsatisfied.Detailf(
				"leftover authorization for %s carries no relation, so reindex-run history cannot be cited "+
					"(FR-AUTH-3, INV-7)", a.Object)
		}
		run, found, err := state.ReindexRunFor(ctx, b.store, state.ReindexHistoryQuery{
			Database:  b.database,
			Relations: []protocol.ObjectName{*rec.Relation},
		})
		if err != nil {
			return err
		}
		if !found {
			return protocol.ErrAuthorizationUnsatisfied.Detailf(
				"no PartitionCTL reindex run is recorded for %s, so %s is not ours to drop "+
					"(FR-AUTH-3, INV-7, AC-19)", rec.Relation, a.Object)
		}
		rec.ReindexRunID = run.RunID
	case protocol.AuthExplicit:
		rec.Confirmation = a.Evidence["confirmation"]
	}

	_, err := b.store.RecordAuthorization(ctx, rec, nil)
	return err
}

// provenanceID reads back the id of the committed record that satisfies
// AuthProvenance. Its absence here is a halt, not a warning: an authorization
// that cites nothing is not an authorization (INV-2).
func (b *executorStore) provenanceID(ctx context.Context, object protocol.ObjectName) (string, error) {
	if b.database == "" {
		return "", protocol.ErrAuthorizationUnsatisfied.Detailf(
			"no target database is scoped for the provenance lookup on %s, so ownership cannot be "+
				"proven for this database rather than some other one (FR-AUTH-2)", object)
	}
	has, id, err := state.HasProvenance(ctx, b.store, state.ProvenanceQuery{
		Object:   object,
		Database: b.database,
	})
	if err != nil {
		return "", err
	}
	if !has {
		return "", protocol.ErrAuthorizationUnsatisfied.Detailf(
			"no committed provenance record proves PartitionCTL created %s (FR-AUTH-2, AC-6)", object)
	}
	return id, nil
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
// Provenance, for the planner
// ---------------------------------------------------------------------------

// provenanceLookup adapts the state store to the one-method view the planner
// and the create-index operation depend on (FR-PLAN-6, FR-PLAN-7).
//
// Both declare the same method under different interface names, so one type
// satisfies both. That is the point of a one-method port: it needs a two-line
// adapter, not a shared dependency.
type provenanceLookup struct {
	store    state.ProvenanceReader
	database string
}

// HasProvenance reports whether a committed record proves PartitionCTL created
// object (FR-AUTH-2).
//
// Both ways of having no answer fail closed, because this is the decision that
// authorizes a DROP (FR-PLAN-6, FR-PLAN-7, AC-5, AC-6, NFR-REL-3).
func (p provenanceLookup) HasProvenance(ctx context.Context, object protocol.ObjectName) (bool, error) {
	if p.store == nil {
		// No provenance source is not "no provenance record is required": with
		// nothing to prove ownership, the planner must halt on an INVALID index
		// rather than plan its destruction (FR-PLAN-7, NFR-REL-3).
		return false, nil
	}
	if p.database == "" {
		// An unscoped query is not a broad question, it is the wrong one.
		// [state.ProvenanceQuery] treats an empty Database as "match any", and a
		// file state store deliberately holds state for several targets, so an
		// unscoped lookup can return a record proving PartitionCTL built an index
		// of this name in a *different* database. Answering "yes" from that
		// record would authorize dropping an index in this one that this tool
		// never created. Ownership that cannot be scoped has not been proven.
		return false, nil
	}
	has, _, err := state.HasProvenance(ctx, p.store, state.ProvenanceQuery{
		Object:   object,
		Database: p.database,
	})
	return has, err
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
