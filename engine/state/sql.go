package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// SQLStore is the database/sql [StateStore] (FR-STATE-2), and the default.
//
// It lives in a dedicated schema, [DefaultSchema] unless configured otherwise,
// created on first use (FR-STATE-3). Putting it in the target database mirrors
// where Liquibase keeps DATABASECHANGELOG and needs no second connection. The
// hazard is documented rather than hidden: a PITR restore or a failover rewinds
// execution state alongside the data (TRD §7.2.5). Because the planner
// reconstructs remaining work from the live catalog, a rewind degrades to loss
// of provenance and audit history, not to incorrect execution, and losing
// provenance makes FR-PLAN-7 halt on a pre-existing INVALID index, which is the
// safe direction.
//
// # No driver
//
// This package imports no PostgreSQL driver, which is what keeps the engine
// offline-testable: the caller registers one and hands over a *sql.DB. The SQL
// is plain PostgreSQL with $n placeholders, and every statement is built once
// against a validated, quoted schema identifier.
//
// # Connection budget
//
// A held [Lock] pins one connection for its whole life, because a session-level
// advisory lock belongs to a session and returning the connection to the pool
// would hand the lock to whoever borrowed it next. So the *sql.DB must allow at
// least two open connections: one for the lock and one for everything else.
// A pool of one deadlocks the moment a run takes its lock.
type SQLStore struct {
	db   *sql.DB
	text sqlText

	clock  Clock
	holder string

	bootstrapOnce sync.Mutex
	bootstrapped  bool
}

// SQLOptions configures a [SQLStore]. The zero value is valid.
type SQLOptions struct {
	// Schema is the dedicated schema name. Defaults to [DefaultSchema]. It is
	// validated as a PostgreSQL identifier and quoted before use.
	Schema string

	// Clock is injected so lease expiry is testable. Defaults to
	// [SystemClock]. Note that the lease's authoritative comparison for
	// takeover happens server-side inside the upsert; this clock supplies the
	// timestamps.
	Clock Clock

	// Holder identifies this process in lease records. Defaults to host/pid.
	Holder string

	// SkipBootstrap suppresses automatic schema creation, for a deployment
	// where the role cannot CREATE and a DBA ran [SchemaDDL] beforehand.
	SkipBootstrap bool
}

// NewSQLStore builds a store over an already-open *sql.DB.
//
// It issues no SQL. The schema is created on the first call that needs it
// (FR-STATE-3), so constructing a store never blocks on the network and
// `status` against an unreachable target fails at the read it actually needed
// rather than at construction (FR-CLI-12, AC-25).
func NewSQLStore(db *sql.DB, opts SQLOptions) (*SQLStore, error) {
	if db == nil {
		return nil, protocol.ErrFailure.Detailf("sql state store requires a non-nil *sql.DB")
	}
	text, err := newSQLText(opts.Schema)
	if err != nil {
		return nil, err
	}
	s := &SQLStore{
		db:           db,
		text:         text,
		clock:        opts.Clock,
		holder:       opts.Holder,
		bootstrapped: opts.SkipBootstrap,
	}
	if s.clock == nil {
		s.clock = SystemClock
	}
	if s.holder == "" {
		s.holder = DefaultHolder()
	}
	return s, nil
}

// Schema returns the schema the store uses.
func (s *SQLStore) Schema() string { return s.text.schema }

// Statements returns every statement the store can issue, keyed by a stable
// name. It exists so the SQL surface can be reviewed and asserted on without a
// database.
func (s *SQLStore) Statements() map[string]string {
	return map[string]string{
		"insert_run":            s.text.insertRun,
		"select_run":            s.text.selectRun,
		"update_run_status":     s.text.updateRunStatus,
		"update_run_cancel":     s.text.updateRunCancel,
		"select_running_for":    s.text.selectRunningFor,
		"insert_node":           s.text.insertNode,
		"select_node":           s.text.selectNode,
		"select_nodes":          s.text.selectNodes,
		"transition_node":       s.text.transitionNode,
		"insert_provenance":     s.text.insertProvenance,
		"insert_authorization":  s.text.insertAuthorization,
		"select_authorizations": s.text.selectAuthorizations,
		"upsert_lease":          s.text.upsertLease,
		"heartbeat":             s.text.heartbeat,
		"select_lease":          s.text.selectLease,
		"delete_lease":          s.text.deleteLease,
		"insert_audit":          s.text.insertAudit,
		"select_audit":          s.text.selectAudit,
		"try_advisory_lock":     s.text.tryAdvisoryLock,
		"advisory_unlock":       s.text.advisoryUnlock,
	}
}

// Close is a no-op: the caller owns the *sql.DB it handed over, and closing
// someone else's pool from a store handle is a surprise nobody wants.
func (s *SQLStore) Close() error { return nil }

func (s *SQLStore) now(at time.Time) time.Time {
	if !at.IsZero() {
		return at.UTC()
	}
	return s.clock().UTC()
}

// EnsureSchema creates the dedicated schema and its tables if they are absent
// (FR-STATE-3). It is idempotent and is called automatically before the first
// statement that needs it.
func (s *SQLStore) EnsureSchema(ctx context.Context) error {
	s.bootstrapOnce.Lock()
	defer s.bootstrapOnce.Unlock()
	if s.bootstrapped {
		return nil
	}
	for i, stmt := range s.text.ddl {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return ioErr(fmt.Sprintf("bootstrap statement %d of %d", i+1, len(s.text.ddl)), err)
		}
	}
	s.bootstrapped = true
	return nil
}

func (s *SQLStore) ready(ctx context.Context) error { return s.EnsureSchema(ctx) }

// ---------------------------------------------------------------- runs

// CreateRun implements [RunStore].
func (s *SQLStore) CreateRun(ctx context.Context, req NewRun) (Run, error) {
	if err := req.validate(); err != nil {
		return Run{}, err
	}
	if err := s.ready(ctx); err != nil {
		return Run{}, err
	}
	now := s.now(req.StartedAt)
	id := req.RunID
	if id == "" {
		id = NewRunID(now)
	}
	run := Run{
		RunID:      id,
		PlanID:     req.Plan.PlanID,
		Operation:  req.Plan.Operation,
		Target:     req.Plan.Target,
		PlanDigest: req.Plan.Digest, // INV-6
		Actor:      req.Actor,
		Status:     RunRunning,
		NodeCount:  len(req.Plan.Nodes),
		StartedAt:  protocol.NewTimestamp(now),
		UpdatedAt:  protocol.NewTimestamp(now),
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, ioErr("begin transaction", err)
	}
	defer func() { _ = tx.Rollback() }()

	idxSchema, idxName, hasIdx := "", "", false
	if run.Target.Index != nil {
		idxSchema, idxName, hasIdx = run.Target.Index.Schema, run.Target.Index.Name, true
	}
	if _, err := tx.ExecContext(ctx, s.text.insertRun,
		string(run.RunID), string(run.PlanID), run.PlanDigest, string(run.Operation),
		run.Target.Database, run.Target.Table.Schema, run.Target.Table.Name,
		idxSchema, idxName, hasIdx,
		string(run.Status), run.Actor, run.NodeCount,
		false, nil, "", "", "",
		now, now, nil,
	); err != nil {
		return Run{}, ioErr("insert run", err)
	}

	for i := range req.Plan.Nodes {
		n := &req.Plan.Nodes[i]
		if _, err := tx.ExecContext(ctx, s.text.insertNode,
			string(run.RunID), string(n.ID), string(n.Kind), string(protocol.InitialNodeState()),
			0, "", "", nil, now,
		); err != nil {
			return Run{}, ioErr("insert node state", err)
		}
	}

	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		RunID: run.RunID,
		Type:  EventRunOpened,
		At:    now,
		Detail: auditDetail(
			"plan_id", string(run.PlanID),
			"plan_digest", run.PlanDigest,
			"operation", string(run.Operation),
			"target", run.Target.Table.String(),
			"actor", run.Actor,
			"nodes", strconv.Itoa(run.NodeCount),
		),
	}); err != nil {
		return Run{}, err
	}

	if err := tx.Commit(); err != nil {
		return Run{}, ioErr("commit run creation", err)
	}
	return run, nil
}

// GetRun implements [RunStore].
func (s *SQLStore) GetRun(ctx context.Context, id RunID) (Run, error) {
	if err := s.ready(ctx); err != nil {
		return Run{}, err
	}
	rows, err := s.db.QueryContext(ctx, s.text.selectRun, string(id))
	if err != nil {
		return Run{}, ioErr("select run", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Run{}, ioErr("select run", err)
		}
		return Run{}, ErrNotFound.Detailf("run %s", id)
	}
	return scanRun(rows)
}

// FindRuns implements [RunReader].
func (s *SQLStore) FindRuns(ctx context.Context, q RunQuery) ([]Run, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	stmt, args := s.text.runQuery(q)
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, ioErr("select runs", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, ioErr("scan runs", err)
	}
	return out, nil
}

// SetRunStatus implements [RunStore].
func (s *SQLStore) SetRunStatus(ctx context.Context, u RunStatusUpdate) (Run, error) {
	if err := CheckRunTransition(u.From, u.To); err != nil {
		return Run{}, err
	}
	if err := s.ready(ctx); err != nil {
		return Run{}, err
	}
	now := s.now(u.At)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, ioErr("begin transaction", err)
	}
	defer func() { _ = tx.Rollback() }()

	var finished any
	if u.To.IsTerminal() || u.To.IsResumable() {
		finished = now
	}
	rows, err := tx.QueryContext(ctx, s.text.updateRunStatus,
		string(u.RunID), string(u.To), now, finished, u.Error, string(u.From), u.ClearCancel)
	if err != nil {
		return Run{}, ioErr("update run status", err)
	}
	if !rows.Next() {
		cerr := rows.Err()
		_ = rows.Close()
		// Release the connection before the diagnostic read. A *sql.Tx pins
		// one connection for its whole life, so querying through s.db while it
		// is open deadlocks against a pool of one and doubles the connection
		// count against a larger one.
		_ = tx.Rollback()
		if cerr != nil {
			return Run{}, ioErr("update run status", cerr)
		}
		return Run{}, s.conflictOrNotFound(ctx, u.RunID, u.From)
	}
	run, err := scanRun(rows)
	_ = rows.Close()
	if err != nil {
		return Run{}, err
	}

	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		RunID:  u.RunID,
		Type:   EventRunStatusChanged,
		At:     now,
		Detail: auditDetail("from", string(u.From), "to", string(u.To), "error", u.Error),
	}); err != nil {
		return Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, ioErr("commit run status", err)
	}
	return run, nil
}

// conflictOrNotFound turns "the compare-and-set matched no row" into the error
// that says which of the two reasons applied.
func (s *SQLStore) conflictOrNotFound(ctx context.Context, id RunID, expected RunStatus) error {
	cur, err := s.GetRun(ctx, id)
	if err != nil {
		return err
	}
	return ErrConflict.Detailf("run %s is %s, not the expected %s", id, cur.Status, expected)
}

// ---------------------------------------------------------------- nodes

// GetNode implements [NodeStore].
func (s *SQLStore) GetNode(ctx context.Context, runID RunID, nodeID protocol.NodeID) (NodeRecord, error) {
	if err := s.ready(ctx); err != nil {
		return NodeRecord{}, err
	}
	rows, err := s.db.QueryContext(ctx, s.text.selectNode, string(runID), string(nodeID))
	if err != nil {
		return NodeRecord{}, ioErr("select node", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return NodeRecord{}, ioErr("select node", err)
		}
		return NodeRecord{}, ErrNotFound.Detailf("run %s node %s", runID, nodeID)
	}
	return scanNode(rows)
}

// ListNodes implements [NodeStore].
func (s *SQLStore) ListNodes(ctx context.Context, runID RunID) ([]NodeRecord, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, s.text.selectNodes, string(runID))
	if err != nil {
		return nil, ioErr("select nodes", err)
	}
	defer func() { _ = rows.Close() }()
	var out []NodeRecord
	for rows.Next() {
		rec, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, ioErr("scan nodes", err)
	}
	return out, nil
}

// TransitionNode implements [NodeStore].
func (s *SQLStore) TransitionNode(ctx context.Context, t NodeTransition) (NodeRecord, error) {
	if err := t.validate(); err != nil {
		return NodeRecord{}, err
	}
	if err := s.ready(ctx); err != nil {
		return NodeRecord{}, err
	}
	now := s.now(t.At)

	attemptDelta := 0
	if t.IncrementAttempt {
		attemptDelta = 1
	}
	lastError, errorKind := "", ""
	if t.Err != nil {
		lastError = t.Err.Error()
		errorKind = string(protocol.KindOf(t.Err))
	}
	var startedAt any
	if t.To == protocol.NodeRunning {
		startedAt = now
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NodeRecord{}, ioErr("begin transaction", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, s.text.transitionNode,
		string(t.RunID), string(t.NodeID), string(t.To), attemptDelta,
		lastError, errorKind, startedAt, now, string(t.From))
	if err != nil {
		return NodeRecord{}, ioErr("transition node", err)
	}
	if !rows.Next() {
		cerr := rows.Err()
		_ = rows.Close()
		// Release the connection before the diagnostic read; see SetRunStatus.
		_ = tx.Rollback()
		if cerr != nil {
			return NodeRecord{}, ioErr("transition node", cerr)
		}
		cur, gerr := s.GetNode(ctx, t.RunID, t.NodeID)
		if gerr != nil {
			return NodeRecord{}, gerr
		}
		return NodeRecord{}, ErrConflict.Detailf(
			"run %s node %s is %s, not the expected %s", t.RunID, t.NodeID, cur.State, t.From)
	}
	rec, err := scanNode(rows)
	_ = rows.Close()
	if err != nil {
		return NodeRecord{}, err
	}

	if _, err := s.appendAuditTx(ctx, tx, nodeTransitionEvent(t, rec, now)); err != nil {
		return NodeRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return NodeRecord{}, ioErr("commit node transition", err)
	}
	return rec, nil
}

// ---------------------------------------------------------------- provenance

// WriteProvenance implements [ProvenanceStore] and enforces INV-1.
func (s *SQLStore) WriteProvenance(ctx context.Context, rec Provenance, create GuardedAction) (Provenance, error) {
	if err := rec.Validate(); err != nil {
		return Provenance{}, ErrProvenanceNotRecorded.Wrap(err)
	}
	if err := s.ready(ctx); err != nil {
		return Provenance{}, ErrProvenanceNotRecorded.Wrap(err)
	}
	run, err := s.GetRun(ctx, rec.RunID)
	if err != nil {
		return Provenance{}, ErrProvenanceNotRecorded.Wrap(err)
	}
	now := s.now(time.Time{})
	if rec.RecordedAt.Time.IsZero() {
		rec.RecordedAt = protocol.NewTimestamp(now)
	}
	if rec.PlanDigest == "" {
		rec.PlanDigest = run.PlanDigest
	}
	if rec.Database == "" {
		rec.Database = run.Target.Database
	}
	if rec.Actor == "" {
		rec.Actor = run.Actor
	}
	if rec.ProvenanceID == "" {
		rec.ProvenanceID = newRecordID(rec.RunID, "prov", now)
	}

	relSchema, relName, hasRel := "", "", false
	if rec.Relation != nil {
		relSchema, relName, hasRel = rec.Relation.Schema, rec.Relation.Name, true
	}

	// The commit. create is an argument, so it cannot have run yet (INV-1).
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Provenance{}, ErrProvenanceNotRecorded.Wrap(ioErr("begin transaction", err))
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx, s.text.insertProvenance,
		rec.ProvenanceID, string(rec.RunID), string(rec.NodeID), rec.PlanDigest, rec.Database,
		rec.Object.Schema, rec.Object.Name, string(rec.ObjectKind),
		relSchema, relName, hasRel, rec.Actor, rec.RecordedAt.Time.UTC(),
	); err != nil {
		return Provenance{}, ErrProvenanceNotRecorded.Wrap(ioErr("insert provenance", err))
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		RunID:  rec.RunID,
		NodeID: rec.NodeID,
		Type:   EventProvenanceRecorded,
		At:     now,
		Detail: auditDetail(
			"provenance_id", rec.ProvenanceID,
			"object", rec.Object.String(),
			"object_kind", string(rec.ObjectKind),
			"database", rec.Database,
		),
	}); err != nil {
		return Provenance{}, ErrProvenanceNotRecorded.Wrap(err)
	}
	if err := tx.Commit(); err != nil {
		return Provenance{}, ErrProvenanceNotRecorded.Wrap(ioErr("commit provenance", err))
	}
	rollback = false

	return rec, s.runGuarded(ctx, rec.RunID, rec.NodeID, rec.Object, create,
		EventObjectCreated, EventObjectCreateFailed)
}

func (s *SQLStore) runGuarded(ctx context.Context, runID RunID, nodeID protocol.NodeID,
	object protocol.ObjectName, action GuardedAction, okEvent, failEvent AuditEventType) error {
	if action == nil {
		return nil
	}
	actionErr := action(ctx)

	ev := AuditEvent{
		RunID:  runID,
		NodeID: nodeID,
		Type:   okEvent,
		At:     s.now(time.Time{}),
		Detail: auditDetail("object", object.String()),
	}
	if actionErr != nil {
		ev.Type = failEvent
		if ev.Detail == nil {
			ev.Detail = map[string]string{}
		}
		ev.Detail["error"] = actionErr.Error()
		ev.Detail["error_kind"] = string(protocol.KindOf(actionErr))
	}
	if _, err := s.AppendAudit(ctx, ev); err != nil && actionErr == nil {
		return err
	}
	return actionErr
}

// FindProvenance implements [ProvenanceReader].
func (s *SQLStore) FindProvenance(ctx context.Context, q ProvenanceQuery) ([]Provenance, error) {
	if err := q.validate(); err != nil {
		return nil, err
	}
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	stmt, args := s.text.provenanceQuery(q)
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, ioErr("select provenance", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Provenance
	for rows.Next() {
		p, err := scanProvenance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, ioErr("scan provenance", err)
	}
	return out, nil
}

// ---------------------------------------------------------------- authorization

// RecordAuthorization implements [AuthorizationStore] and enforces INV-2.
func (s *SQLStore) RecordAuthorization(ctx context.Context, rec AuthorizationRecord, exec GuardedAction) (AuthorizationRecord, error) {
	if err := rec.Validate(); err != nil {
		return AuthorizationRecord{}, ErrAuthorizationNotRecorded.Wrap(err)
	}
	if err := s.ready(ctx); err != nil {
		return AuthorizationRecord{}, ErrAuthorizationNotRecorded.Wrap(err)
	}
	run, err := s.GetRun(ctx, rec.RunID)
	if err != nil {
		return AuthorizationRecord{}, ErrAuthorizationNotRecorded.Wrap(err)
	}
	now := s.now(time.Time{})
	if rec.GrantedAt.Time.IsZero() {
		rec.GrantedAt = protocol.NewTimestamp(now)
	}
	if rec.Database == "" {
		rec.Database = run.Target.Database
	}
	if rec.AuthorizationID == "" {
		rec.AuthorizationID = newRecordID(rec.RunID, "auth", now)
	}
	evidence, err := json.Marshal(nonNilMap(rec.Evidence))
	if err != nil {
		return AuthorizationRecord{}, ErrAuthorizationNotRecorded.Wrap(ioErr("encode evidence", err))
	}
	relSchema, relName, hasRel := "", "", false
	if rec.Relation != nil {
		relSchema, relName, hasRel = rec.Relation.Schema, rec.Relation.Name, true
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AuthorizationRecord{}, ErrAuthorizationNotRecorded.Wrap(ioErr("begin transaction", err))
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx, s.text.insertAuthorization,
		rec.AuthorizationID, string(rec.RunID), string(rec.NodeID), string(rec.Mode), rec.Database,
		rec.Object.Schema, rec.Object.Name, relSchema, relName, hasRel,
		rec.ProvenanceID, string(rec.ReindexRunID), rec.Confirmation,
		string(evidence), rec.GrantedAt.Time.UTC(),
	); err != nil {
		return AuthorizationRecord{}, ErrAuthorizationNotRecorded.Wrap(ioErr("insert authorization", err))
	}
	if _, err := s.appendAuditTx(ctx, tx, authorizationEvent(rec, now)); err != nil {
		return AuthorizationRecord{}, ErrAuthorizationNotRecorded.Wrap(err)
	}
	if err := tx.Commit(); err != nil {
		return AuthorizationRecord{}, ErrAuthorizationNotRecorded.Wrap(ioErr("commit authorization", err))
	}
	rollback = false

	return rec, s.runGuarded(ctx, rec.RunID, rec.NodeID, rec.Object, exec,
		EventDestructiveExecuted, EventDestructiveFailed)
}

// ListAuthorizations implements [AuthorizationStore].
func (s *SQLStore) ListAuthorizations(ctx context.Context, runID RunID) ([]AuthorizationRecord, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, s.text.selectAuthorizations, string(runID))
	if err != nil {
		return nil, ioErr("select authorizations", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AuthorizationRecord
	for rows.Next() {
		rec, err := scanAuthorization(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, ioErr("scan authorizations", err)
	}
	return out, nil
}

// ---------------------------------------------------------------- lease

// AcquireLease implements [LeaseStore].
func (s *SQLStore) AcquireLease(ctx context.Context, runID RunID, holder string, ttl time.Duration) (Lease, error) {
	if holder == "" {
		holder = s.holder
	}
	secs, err := normalizeTTL(ttl)
	if err != nil {
		return Lease{}, err
	}
	if err := s.ready(ctx); err != nil {
		return Lease{}, err
	}
	now := s.now(time.Time{})

	rows, err := s.db.QueryContext(ctx, s.text.upsertLease, string(runID), holder, now, secs)
	if err != nil {
		return Lease{}, ioErr("acquire lease", err)
	}
	if !rows.Next() {
		cerr := rows.Err()
		_ = rows.Close()
		if cerr != nil {
			return Lease{}, ioErr("acquire lease", cerr)
		}
		cur, gerr := s.GetLease(ctx, runID)
		if gerr != nil {
			return Lease{}, gerr
		}
		return Lease{}, ErrLeaseLost.Detailf(
			"run %s is leased by %q until %s", runID, cur.Holder,
			cur.ExpiresAt().UTC().Format(time.RFC3339))
	}
	lease, err := scanLease(rows)
	_ = rows.Close()
	if err != nil {
		return Lease{}, err
	}
	if _, err := s.AppendAudit(ctx, AuditEvent{
		RunID:  runID,
		Type:   EventLeaseAcquired,
		At:     now,
		Detail: auditDetail("holder", holder, "ttl_seconds", strconv.Itoa(secs)),
	}); err != nil {
		return Lease{}, err
	}
	return lease, nil
}

// Heartbeat implements [LeaseStore].
func (s *SQLStore) Heartbeat(ctx context.Context, runID RunID, holder string) (Lease, error) {
	if holder == "" {
		holder = s.holder
	}
	if err := s.ready(ctx); err != nil {
		return Lease{}, err
	}
	rows, err := s.db.QueryContext(ctx, s.text.heartbeat, string(runID), holder, s.now(time.Time{}))
	if err != nil {
		return Lease{}, ioErr("heartbeat", err)
	}
	if !rows.Next() {
		cerr := rows.Err()
		// Open *sql.Rows hold their connection, so close before the
		// diagnostic read or a one-connection pool deadlocks.
		_ = rows.Close()
		if cerr != nil {
			return Lease{}, ioErr("heartbeat", cerr)
		}
		cur, gerr := s.GetLease(ctx, runID)
		if errors.Is(gerr, ErrNotFound) {
			return Lease{}, ErrLeaseLost.Detailf("run %s has no lease to heartbeat", runID)
		}
		if gerr != nil {
			return Lease{}, gerr
		}
		return Lease{}, ErrLeaseLost.Detailf(
			"run %s is leased by %q, not %q", runID, cur.Holder, holder)
	}
	lease, err := scanLease(rows)
	_ = rows.Close()
	return lease, err
}

// ReleaseLease implements [LeaseStore].
func (s *SQLStore) ReleaseLease(ctx context.Context, runID RunID, holder string) error {
	if holder == "" {
		holder = s.holder
	}
	if err := s.ready(ctx); err != nil {
		return err
	}
	cur, err := s.GetLease(ctx, runID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if cur.Holder != holder {
		return ErrLeaseLost.Detailf("run %s is leased by %q, not %q", runID, cur.Holder, holder)
	}
	if _, err := s.db.ExecContext(ctx, s.text.deleteLease, string(runID), holder); err != nil {
		return ioErr("release lease", err)
	}
	_, err = s.AppendAudit(ctx, AuditEvent{
		RunID:  runID,
		Type:   EventLeaseReleased,
		At:     s.now(time.Time{}),
		Detail: auditDetail("holder", holder),
	})
	return err
}

// GetLease implements [LeaseStore].
func (s *SQLStore) GetLease(ctx context.Context, runID RunID) (Lease, error) {
	if err := s.ready(ctx); err != nil {
		return Lease{}, err
	}
	rows, err := s.db.QueryContext(ctx, s.text.selectLease, string(runID))
	if err != nil {
		return Lease{}, ioErr("select lease", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Lease{}, ioErr("select lease", err)
		}
		return Lease{}, ErrNotFound.Detailf("no lease for run %s", runID)
	}
	return scanLease(rows)
}

// ---------------------------------------------------------------- cancellation

// RequestCancel implements [CancelStore].
func (s *SQLStore) RequestCancel(ctx context.Context, runID RunID, actor, note string) (Run, error) {
	if err := s.ready(ctx); err != nil {
		return Run{}, err
	}
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return Run{}, err
	}
	if run.Status.IsTerminal() {
		return run, nil
	}
	now := s.now(time.Time{})

	live := false
	if run.Status == RunRunning {
		lease, lerr := s.GetLease(ctx, runID)
		switch {
		case errors.Is(lerr, ErrNotFound):
			live = false
		case lerr != nil:
			return Run{}, lerr
		default:
			live = !lease.Expired(now)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, ioErr("begin transaction", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()

	rows, err := tx.QueryContext(ctx, s.text.updateRunCancel, string(runID), now, actor, note)
	if err != nil {
		return Run{}, ioErr("set cancel flag", err)
	}
	if !rows.Next() {
		cerr := rows.Err()
		_ = rows.Close()
		if cerr != nil {
			return Run{}, ioErr("set cancel flag", cerr)
		}
		return Run{}, ErrNotFound.Detailf("run %s", runID)
	}
	updated, err := scanRun(rows)
	_ = rows.Close()
	if err != nil {
		return Run{}, err
	}
	if _, err := s.appendAuditTx(ctx, tx, AuditEvent{
		RunID: runID,
		Type:  EventCancelRequested,
		At:    now,
		Detail: auditDetail(
			"actor", actor, "note", note, "live", strconv.FormatBool(live)),
	}); err != nil {
		return Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, ioErr("commit cancel request", err)
	}
	rollback = false

	if live {
		// FR-CLI-10: the flag is all a live run gets. The executor observes it
		// at a node boundary and the run stays resumable (AC-24).
		return updated, nil
	}
	// FR-CLI-11: nothing is alive, so cancel terminally.
	return s.SetRunStatus(ctx, RunStatusUpdate{
		RunID: runID, From: updated.Status, To: RunCancelled, At: now,
	})
}

// CancellationRequested implements [CancelStore].
func (s *SQLStore) CancellationRequested(ctx context.Context, runID RunID) (bool, error) {
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		return false, err
	}
	return run.CancelRequested || run.Status == RunCancelled, nil
}

// ---------------------------------------------------------------- audit

// AppendAudit implements [AuditStore]. INV-3: insert only. There is no UPDATE
// and no DELETE against audit_event anywhere in this package, and the schema
// installs a trigger that refuses one from outside it.
func (s *SQLStore) AppendAudit(ctx context.Context, ev AuditEvent) (AuditEvent, error) {
	if err := ev.validate(); err != nil {
		return AuditEvent{}, err
	}
	if err := s.ready(ctx); err != nil {
		return AuditEvent{}, err
	}
	return s.appendAudit(ctx, s.db, ev)
}

// execQuerier is the shared surface of *sql.DB and *sql.Tx that the audit
// append needs, so one implementation serves both.
type execQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (s *SQLStore) appendAuditTx(ctx context.Context, tx *sql.Tx, ev AuditEvent) (AuditEvent, error) {
	if err := ev.validate(); err != nil {
		return AuditEvent{}, err
	}
	return s.appendAudit(ctx, tx, ev)
}

func (s *SQLStore) appendAudit(ctx context.Context, q execQuerier, ev AuditEvent) (AuditEvent, error) {
	detail, err := json.Marshal(nonNilMap(ev.Detail))
	if err != nil {
		return AuditEvent{}, ioErr("encode audit detail", err)
	}
	occurred := s.now(ev.At)
	rows, err := q.QueryContext(ctx, s.text.insertAudit,
		string(ev.RunID), string(ev.NodeID), string(ev.Type), string(detail), occurred)
	if err != nil {
		return AuditEvent{}, ioErr("append audit event", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return AuditEvent{}, ioErr("append audit event", err)
		}
		return AuditEvent{}, ioErr("append audit event", errors.New("insert returned no row"))
	}
	return scanAudit(rows)
}

// ListAudit implements [AuditStore].
func (s *SQLStore) ListAudit(ctx context.Context, runID RunID, afterSeq int64) ([]AuditEvent, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, s.text.selectAudit, string(runID), afterSeq)
	if err != nil {
		return nil, ioErr("select audit trail", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AuditEvent
	for rows.Next() {
		ev, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, ioErr("scan audit trail", err)
	}
	return out, nil
}

// ---------------------------------------------------------------- advisory lock

// sqlLock is a [Lock] backed by a session-level PostgreSQL advisory lock.
//
// The *sql.Conn is held for the lock's whole life on purpose: pg_advisory_lock
// is scoped to a session, so returning the connection to the pool would hand
// the lock to whoever borrowed it next, and losing the connection would release
// it silently.
type sqlLock struct {
	store *SQLStore
	conn  *sql.Conn
	key   LockKey

	mu       sync.Mutex
	released bool
}

func (l *sqlLock) Key() LockKey { return l.key }

// Held reports whether the lock is still held.
//
// A session-level advisory lock is released only by an explicit unlock or by
// the session ending, and this handle owns the only reference to the session.
// So "still held" reduces to "still ours and the session is still alive", which
// is what the ping tests. Reading pg_locks would be no more truthful and would
// need an oid cast of a signed key.
func (l *sqlLock) Held(ctx context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return false, nil
	}
	if err := l.conn.PingContext(ctx); err != nil {
		return false, nil
	}
	return true, nil
}

// Refresh is a no-op: the server releases the lock when the session ends, so
// there is nothing to keep alive (see [Lock.Refresh]).
func (l *sqlLock) Refresh(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return protocol.ErrLockHeld.Detailf("advisory lock for %s was released", l.key)
	}
	return nil
}

func (l *sqlLock) Unlock(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.released = true
	classID, objID := l.key.AdvisoryKeys()
	var ok bool
	err := l.conn.QueryRowContext(ctx, l.store.text.advisoryUnlock, classID, objID).Scan(&ok)
	closeErr := l.conn.Close()
	if err != nil {
		// Closing the session releases the lock regardless, so an unlock that
		// could not be issued is not a leak.
		return ioErr("release advisory lock", err)
	}
	if closeErr != nil {
		return ioErr("close advisory lock session", closeErr)
	}
	return nil
}

// TryLock implements [Locker] (FR-LOCK-1, FR-LOCK-2, AC-10).
func (s *SQLStore) TryLock(ctx context.Context, key LockKey) (Lock, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, ioErr("open advisory lock session", err)
	}
	classID, objID := key.AdvisoryKeys()
	var ok bool
	if err := conn.QueryRowContext(ctx, s.text.tryAdvisoryLock, classID, objID).Scan(&ok); err != nil {
		_ = conn.Close()
		return nil, ioErr("acquire advisory lock", err)
	}
	if !ok {
		_ = conn.Close()
		return nil, s.lockHeldError(ctx, key)
	}
	return &sqlLock{store: s, conn: conn, key: key}, nil
}

// lockHeldError builds the FR-LOCK-2 message, naming the holding run where one
// is recorded (AC-10).
func (s *SQLStore) lockHeldError(ctx context.Context, key LockKey) error {
	rows, err := s.db.QueryContext(ctx, s.text.selectRunningFor, key.Database, key.Table.Schema, key.Table.Name)
	if err == nil {
		defer func() { _ = rows.Close() }()
		if rows.Next() {
			if r, serr := scanRun(rows); serr == nil {
				return protocol.ErrLockHeld.Detailf(
					"%s is locked by run %s, started %s by %q",
					key, r.RunID, r.StartedAt.Canonical(), r.Actor)
			}
		}
	}
	return protocol.ErrLockHeld.Detailf(
		"%s is locked by another session; no RUNNING run is recorded for it", key)
}

// ---------------------------------------------------------------- scanning

func scanRun(rows *sql.Rows) (Run, error) {
	var (
		r          Run
		runID      string
		planID     string
		operation  string
		idxSchema  string
		idxName    string
		hasIdx     bool
		status     string
		cancelAt   sql.NullTime
		startedAt  time.Time
		updatedAt  time.Time
		finishedAt sql.NullTime
	)
	if err := rows.Scan(
		&runID, &planID, &r.PlanDigest, &operation,
		&r.Target.Database, &r.Target.Table.Schema, &r.Target.Table.Name,
		&idxSchema, &idxName, &hasIdx,
		&status, &r.Actor, &r.NodeCount,
		&r.CancelRequested, &cancelAt, &r.CancelActor, &r.CancelNote, &r.LastError,
		&startedAt, &updatedAt, &finishedAt,
	); err != nil {
		return Run{}, ioErr("scan run", err)
	}
	r.RunID = RunID(runID)
	r.PlanID = protocol.PlanID(planID)
	r.Operation = protocol.Operation(operation)
	r.Status = RunStatus(status)
	if hasIdx {
		idx := protocol.NewObjectName(idxSchema, idxName)
		r.Target.Index = &idx
	}
	r.StartedAt = protocol.NewTimestamp(startedAt)
	r.UpdatedAt = protocol.NewTimestamp(updatedAt)
	if cancelAt.Valid {
		ts := protocol.NewTimestamp(cancelAt.Time)
		r.CancelRequestedAt = &ts
	}
	if finishedAt.Valid {
		ts := protocol.NewTimestamp(finishedAt.Time)
		r.FinishedAt = &ts
	}
	return r, nil
}

func scanNode(rows *sql.Rows) (NodeRecord, error) {
	var (
		rec       NodeRecord
		runID     string
		nodeID    string
		kind      string
		state     string
		errorKind string
		startedAt sql.NullTime
		updatedAt time.Time
	)
	if err := rows.Scan(&runID, &nodeID, &kind, &state, &rec.Attempts,
		&rec.LastError, &errorKind, &startedAt, &updatedAt); err != nil {
		return NodeRecord{}, ioErr("scan node state", err)
	}
	rec.RunID = RunID(runID)
	rec.NodeID = protocol.NodeID(nodeID)
	rec.Kind = protocol.NodeKind(kind)
	rec.State = protocol.NodeState(state)
	rec.ErrorKind = protocol.ErrorKind(errorKind)
	if startedAt.Valid {
		ts := protocol.NewTimestamp(startedAt.Time)
		rec.StartedAt = &ts
	}
	rec.UpdatedAt = protocol.NewTimestamp(updatedAt)
	return rec, nil
}

func scanProvenance(rows *sql.Rows) (Provenance, error) {
	var (
		p          Provenance
		runID      string
		nodeID     string
		objectKind string
		relSchema  string
		relName    string
		hasRel     bool
		recordedAt time.Time
	)
	if err := rows.Scan(&p.ProvenanceID, &runID, &nodeID, &p.PlanDigest, &p.Database,
		&p.Object.Schema, &p.Object.Name, &objectKind,
		&relSchema, &relName, &hasRel, &p.Actor, &recordedAt); err != nil {
		return Provenance{}, ioErr("scan provenance", err)
	}
	p.RunID = RunID(runID)
	p.NodeID = protocol.NodeID(nodeID)
	p.ObjectKind = ObjectKind(objectKind)
	if hasRel {
		rel := protocol.NewObjectName(relSchema, relName)
		p.Relation = &rel
	}
	p.RecordedAt = protocol.NewTimestamp(recordedAt)
	return p, nil
}

func scanAuthorization(rows *sql.Rows) (AuthorizationRecord, error) {
	var (
		rec       AuthorizationRecord
		runID     string
		nodeID    string
		mode      string
		relSchema string
		relName   string
		hasRel    bool
		reindexID string
		evidence  []byte
		grantedAt time.Time
	)
	if err := rows.Scan(&rec.AuthorizationID, &runID, &nodeID, &mode, &rec.Database,
		&rec.Object.Schema, &rec.Object.Name, &relSchema, &relName, &hasRel,
		&rec.ProvenanceID, &reindexID, &rec.Confirmation, &evidence, &grantedAt); err != nil {
		return AuthorizationRecord{}, ioErr("scan authorization", err)
	}
	rec.RunID = RunID(runID)
	rec.NodeID = protocol.NodeID(nodeID)
	rec.Mode = protocol.AuthorizationMode(mode)
	rec.ReindexRunID = RunID(reindexID)
	if hasRel {
		rel := protocol.NewObjectName(relSchema, relName)
		rec.Relation = &rel
	}
	if len(evidence) > 0 {
		if err := json.Unmarshal(evidence, &rec.Evidence); err != nil {
			return AuthorizationRecord{}, ioErr("decode authorization evidence", err)
		}
	}
	if len(rec.Evidence) == 0 {
		rec.Evidence = nil
	}
	rec.GrantedAt = protocol.NewTimestamp(grantedAt)
	return rec, nil
}

func scanAudit(rows *sql.Rows) (AuditEvent, error) {
	var (
		ev         AuditEvent
		runID      string
		nodeID     string
		eventType  string
		detail     []byte
		occurredAt time.Time
	)
	if err := rows.Scan(&ev.EventID, &runID, &ev.Seq, &nodeID, &eventType, &detail, &occurredAt); err != nil {
		return AuditEvent{}, ioErr("scan audit event", err)
	}
	ev.RunID = RunID(runID)
	ev.NodeID = protocol.NodeID(nodeID)
	ev.Type = AuditEventType(eventType)
	if len(detail) > 0 {
		if err := json.Unmarshal(detail, &ev.Detail); err != nil {
			return AuditEvent{}, ioErr("decode audit detail", err)
		}
	}
	if len(ev.Detail) == 0 {
		ev.Detail = nil
	}
	ev.OccurredAt = protocol.NewTimestamp(occurredAt)
	return ev, nil
}

func scanLease(rows *sql.Rows) (Lease, error) {
	var (
		l           Lease
		runID       string
		acquiredAt  time.Time
		heartbeatAt time.Time
	)
	if err := rows.Scan(&runID, &l.Holder, &acquiredAt, &heartbeatAt, &l.TTLSeconds); err != nil {
		return Lease{}, ioErr("scan lease", err)
	}
	l.RunID = RunID(runID)
	l.AcquiredAt = protocol.NewTimestamp(acquiredAt)
	l.HeartbeatAt = protocol.NewTimestamp(heartbeatAt)
	return l, nil
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// newRecordID builds a stable, unique identifier for a provenance or
// authorization row. The run id and a nanosecond stamp are enough: both records
// are written under the advisory lock, so two rows for one run cannot share an
// instant.
func newRecordID(runID RunID, kind string, now time.Time) string {
	return fmt.Sprintf("%s:%s:%d", runID, kind, now.UTC().UnixNano())
}
