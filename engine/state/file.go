package state

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// FileStore is the local-filesystem [StateStore] (FR-STATE-2).
//
// It exists for operators who want execution state outside the target
// database's blast radius: a PITR restore or a failover rewinds a [SQLStore]
// alongside the data, and the reindex gate is the one consumer that cannot
// tolerate that (TRD §7.2.5, §7.2.8). It is also the store the unit tests use,
// because it needs no server.
//
// Every record is written by rename over a fully written, fsynced temporary
// file, so a crash at any instant leaves either the previous record or the new
// one. The audit trail is the one exception and is stricter: it is opened
// O_APPEND and only ever grown (INV-3).
//
// # Layout
//
//	<root>/.tmp/                       staging for atomic renames, swept on open
//	<root>/locks/<key>.lock            advisory locks (FR-LOCK-1)
//	<root>/runs/<run>/run.json         the run record
//	<root>/runs/<run>/lease.json       the lease (FR-STATE-7)
//	<root>/runs/<run>/nodes/<node>.json per-node state
//	<root>/runs/<run>/prov/<n>.json    provenance (FR-STATE-6, INV-1)
//	<root>/runs/<run>/auth/<n>.json    authorization records (INV-2)
//	<root>/runs/<run>/audit.log        append-only JSON Lines trail (INV-3)
//
// A run directory with no run.json is invisible to every read, which is what
// makes "write the node records, then the run record" a crash-safe creation
// order.
type FileStore struct {
	root  string
	w     atomicWriter
	clock Clock

	lockTTL time.Duration
	holder  string

	hooks fileHooks

	mu       sync.Mutex
	auditSeq map[RunID]auditCursor
}

// auditCursor caches where a run's trail ended after this store last appended
// to it.
//
// The size is the validity check. Re-deriving the next sequence number by
// parsing the whole trail on every append is O(trail) per event, which at a
// thousand partitions is quadratic. Trusting a cached counter unconditionally
// is worse: a second process appending to the same directory would make this
// store reuse a sequence number and produce two events with the same id. A
// stat is cheap, and a size that no longer matches means someone else wrote, so
// the counter is rebuilt.
type auditCursor struct {
	seq  int64
	size int64
}

// FileOptions configures a [FileStore]. The zero value is valid.
type FileOptions struct {
	// Clock is injected so lease expiry and orphan detection can be tested
	// without sleeping. Defaults to [SystemClock].
	Clock Clock

	// LockTTL bounds how long a lock file stays authoritative without a
	// refresh. A process that dies holding one would otherwise block the
	// target forever, since a file has no session to end. Defaults to
	// [DefaultLeaseTTL].
	LockTTL time.Duration

	// Holder identifies this process in lock files and lease records.
	// Defaults to host/pid.
	Holder string
}

// DefaultHolder returns the conventional holder identity for this process:
// hostname/pid.
func DefaultHolder() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return host + "/" + strconv.Itoa(os.Getpid())
}

// OpenFileStore opens or creates a file-backed store rooted at dir.
//
// Opening sweeps abandoned temporary files. That sweep is also the proof that
// a crash between write and rename is inert: the previous record is still in
// place and the partial one is not in a directory any read scans.
func OpenFileStore(dir string, opts FileOptions) (*FileStore, error) {
	if dir == "" {
		return nil, protocol.ErrFailure.Detailf("file store requires a root directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, ioErr("resolve state directory", err)
	}
	s := &FileStore{
		root:     abs,
		clock:    opts.Clock,
		lockTTL:  opts.LockTTL,
		holder:   opts.Holder,
		auditSeq: make(map[RunID]auditCursor),
	}
	if s.clock == nil {
		s.clock = SystemClock
	}
	if s.lockTTL <= 0 {
		s.lockTTL = DefaultLeaseTTL
	}
	if s.holder == "" {
		s.holder = DefaultHolder()
	}
	s.w = atomicWriter{tmpDir: filepath.Join(abs, ".tmp"), hooks: &s.hooks}

	for _, d := range []string{abs, s.w.tmpDir, filepath.Join(abs, "runs"), filepath.Join(abs, "locks")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, ioErr("create state directory", err)
		}
	}
	s.w.sweepTmp()
	return s, nil
}

// Root returns the store's root directory.
func (s *FileStore) Root() string { return s.root }

// Close releases the store's resources. The file store holds none, so it is a
// no-op that exists to satisfy [StateStore].
func (s *FileStore) Close() error { return nil }

func (s *FileStore) now(at time.Time) time.Time {
	if !at.IsZero() {
		return at.UTC()
	}
	return s.clock().UTC()
}

// ---------------------------------------------------------------- paths

func (s *FileStore) runsDir() string { return filepath.Join(s.root, "runs") }
func (s *FileStore) runDir(id RunID) string {
	return filepath.Join(s.runsDir(), encodeSegment(string(id)))
}
func (s *FileStore) runPath(id RunID) string { return filepath.Join(s.runDir(id), "run.json") }
func (s *FileStore) leasePath(id RunID) string {
	return filepath.Join(s.runDir(id), "lease.json")
}
func (s *FileStore) nodesDir(id RunID) string { return filepath.Join(s.runDir(id), "nodes") }
func (s *FileStore) nodePath(id RunID, node protocol.NodeID) string {
	return filepath.Join(s.nodesDir(id), encodeSegment(string(node))+".json")
}
func (s *FileStore) provDir(id RunID) string { return filepath.Join(s.runDir(id), "prov") }
func (s *FileStore) authDir(id RunID) string { return filepath.Join(s.runDir(id), "auth") }
func (s *FileStore) auditPath(id RunID) string {
	return filepath.Join(s.runDir(id), "audit.log")
}
func (s *FileStore) lockPath(k LockKey) string {
	return filepath.Join(s.root, "locks", encodeSegment(k.String())+".lock")
}

// ---------------------------------------------------------------- runs

// CreateRun implements [RunStore].
func (s *FileStore) CreateRun(ctx context.Context, req NewRun) (Run, error) {
	if err := req.validate(); err != nil {
		return Run{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now(req.StartedAt)
	id := req.RunID
	if id == "" {
		id = NewRunID(now)
	}
	if _, err := os.Stat(s.runPath(id)); err == nil {
		return Run{}, ErrConflict.Detailf("run %s already exists", id)
	}

	run := Run{
		RunID:      id,
		PlanID:     req.Plan.PlanID,
		Operation:  req.Plan.Operation,
		Target:     req.Plan.Target,
		PlanDigest: req.Plan.Digest, // INV-6: bound here, never rewritten
		Actor:      req.Actor,
		Status:     RunRunning,
		NodeCount:  len(req.Plan.Nodes),
		StartedAt:  protocol.NewTimestamp(now),
		UpdatedAt:  protocol.NewTimestamp(now),
	}

	// Node records first, run record last. The run record is what every read
	// keys on, so a crash midway leaves a directory nothing can find rather
	// than a run whose nodes are missing.
	for i := range req.Plan.Nodes {
		n := &req.Plan.Nodes[i]
		rec := NodeRecord{
			RunID:     id,
			NodeID:    n.ID,
			Kind:      n.Kind,
			State:     protocol.InitialNodeState(),
			UpdatedAt: protocol.NewTimestamp(now),
		}
		if err := s.w.writeJSON(s.nodePath(id, n.ID), rec); err != nil {
			return Run{}, err
		}
	}
	if err := s.w.writeJSON(s.runPath(id), run); err != nil {
		return Run{}, err
	}
	if _, err := s.appendAuditLocked(AuditEvent{
		RunID: id,
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
	return run, nil
}

// GetRun implements [RunStore].
func (s *FileStore) GetRun(ctx context.Context, id RunID) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getRunLocked(id)
}

func (s *FileStore) getRunLocked(id RunID) (Run, error) {
	if id == "" {
		return Run{}, ErrNotFound.Detailf("empty run id")
	}
	var r Run
	if err := readJSON(s.runPath(id), &r); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Run{}, ErrNotFound.Detailf("run %s", id)
		}
		return Run{}, err
	}
	if r.RunID != id {
		return Run{}, ioErr("run record mismatch",
			fmt.Errorf("file for %q holds run %q", id, r.RunID))
	}
	return r, nil
}

// FindRuns implements [RunReader].
func (s *FileStore) FindRuns(ctx context.Context, q RunQuery) ([]Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if q.RunID != "" {
		r, err := s.getRunLocked(q.RunID)
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if !q.matches(r) {
			return nil, nil
		}
		return []Run{r}, nil
	}

	entries, err := os.ReadDir(s.runsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, ioErr("list runs", err)
	}
	var out []Run
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var r Run
		if err := readJSON(filepath.Join(s.runsDir(), e.Name(), "run.json"), &r); err != nil {
			if errors.Is(err, ErrNotFound) {
				// A run directory with no run record: a crash during
				// CreateRun. Invisible by design.
				continue
			}
			return nil, err
		}
		if q.matches(r) {
			out = append(out, r)
		}
	}
	sortRuns(out)
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func sortRuns(runs []Run) {
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].StartedAt.Time.Equal(runs[j].StartedAt.Time) {
			return runs[i].RunID < runs[j].RunID
		}
		return runs[i].StartedAt.Time.Before(runs[j].StartedAt.Time)
	})
}

// SetRunStatus implements [RunStore].
func (s *FileStore) SetRunStatus(ctx context.Context, u RunStatusUpdate) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setRunStatusLocked(u)
}

func (s *FileStore) setRunStatusLocked(u RunStatusUpdate) (Run, error) {
	run, err := s.getRunLocked(u.RunID)
	if err != nil {
		return Run{}, err
	}
	if run.Status != u.From {
		return Run{}, ErrConflict.Detailf(
			"run %s is %s, not the expected %s", u.RunID, run.Status, u.From)
	}
	if err := CheckRunTransition(u.From, u.To); err != nil {
		return Run{}, err
	}
	now := s.now(u.At)
	prev := run.Status
	run.Status = u.To
	run.UpdatedAt = protocol.NewTimestamp(now)
	if u.Error != "" {
		run.LastError = u.Error
	}
	if u.ClearCancel {
		run.CancelRequested = false
		run.CancelRequestedAt = nil
		run.CancelActor = ""
		run.CancelNote = ""
	}
	if u.To.IsTerminal() || u.To.IsResumable() {
		ts := protocol.NewTimestamp(now)
		run.FinishedAt = &ts
	} else {
		run.FinishedAt = nil
	}
	// INV-6 holds structurally: PlanDigest is never assigned here.
	if err := s.w.writeJSON(s.runPath(run.RunID), run); err != nil {
		return Run{}, err
	}
	if _, err := s.appendAuditLocked(AuditEvent{
		RunID: run.RunID,
		Type:  EventRunStatusChanged,
		At:    now,
		Detail: auditDetail(
			"from", string(prev),
			"to", string(u.To),
			"error", u.Error,
		),
	}); err != nil {
		return Run{}, err
	}
	return run, nil
}

// ---------------------------------------------------------------- nodes

// GetNode implements [NodeStore].
func (s *FileStore) GetNode(ctx context.Context, runID RunID, nodeID protocol.NodeID) (NodeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getNodeLocked(runID, nodeID)
}

func (s *FileStore) getNodeLocked(runID RunID, nodeID protocol.NodeID) (NodeRecord, error) {
	var rec NodeRecord
	if err := readJSON(s.nodePath(runID, nodeID), &rec); err != nil {
		if errors.Is(err, ErrNotFound) {
			return NodeRecord{}, ErrNotFound.Detailf("run %s node %s", runID, nodeID)
		}
		return NodeRecord{}, err
	}
	if rec.NodeID != nodeID || rec.RunID != runID {
		return NodeRecord{}, ioErr("node record mismatch",
			fmt.Errorf("file for %s/%s holds %s/%s", runID, nodeID, rec.RunID, rec.NodeID))
	}
	return rec, nil
}

// ListNodes implements [NodeStore].
func (s *FileStore) ListNodes(ctx context.Context, runID RunID) ([]NodeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.nodesDir(runID))
	if err != nil {
		if os.IsNotExist(err) {
			if _, gerr := s.getRunLocked(runID); gerr != nil {
				return nil, gerr
			}
			return nil, nil
		}
		return nil, ioErr("list nodes", err)
	}
	out := make([]NodeRecord, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var rec NodeRecord
		if err := readJSON(filepath.Join(s.nodesDir(runID), e.Name()), &rec); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

// TransitionNode implements [NodeStore].
func (s *FileStore) TransitionNode(ctx context.Context, t NodeTransition) (NodeRecord, error) {
	if err := t.validate(); err != nil {
		return NodeRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.getNodeLocked(t.RunID, t.NodeID)
	if err != nil {
		return NodeRecord{}, err
	}
	if rec.State != t.From {
		return NodeRecord{}, ErrConflict.Detailf(
			"run %s node %s is %s, not the expected %s", t.RunID, t.NodeID, rec.State, t.From)
	}
	now := s.now(t.At)
	applyTransition(&rec, t, now)
	if err := s.w.writeJSON(s.nodePath(t.RunID, t.NodeID), rec); err != nil {
		return NodeRecord{}, err
	}
	if _, err := s.appendAuditLocked(nodeTransitionEvent(t, rec, now)); err != nil {
		return NodeRecord{}, err
	}
	return rec, nil
}

// applyTransition mutates rec in place. It is shared by both store
// implementations so the two cannot drift on what a transition means.
func applyTransition(rec *NodeRecord, t NodeTransition, now time.Time) {
	rec.State = t.To
	rec.UpdatedAt = protocol.NewTimestamp(now)
	if t.IncrementAttempt {
		rec.Attempts++
	}
	if t.To == protocol.NodeRunning && rec.StartedAt == nil {
		ts := protocol.NewTimestamp(now)
		rec.StartedAt = &ts
	}
	if t.Err != nil {
		rec.LastError = t.Err.Error()
		rec.ErrorKind = protocol.KindOf(t.Err)
	} else if t.To == protocol.NodeDone || t.To == protocol.NodeSkipped {
		rec.LastError = ""
		rec.ErrorKind = ""
	}
	if t.Reason == protocol.ReasonOrphanRecovery {
		// The node was in flight when the process died. Its attempt already
		// counted, and the recorded error, if any, describes nothing that
		// happened.
		rec.LastError = ""
		rec.ErrorKind = ""
	}
}

func nodeTransitionEvent(t NodeTransition, rec NodeRecord, now time.Time) AuditEvent {
	detail := auditDetail(
		"from", string(t.From),
		"to", string(t.To),
		"kind", string(rec.Kind),
		"reason", string(t.reason()),
		"attempts", strconv.Itoa(rec.Attempts),
	)
	if t.Err != nil {
		if detail == nil {
			detail = map[string]string{}
		}
		detail["error"] = t.Err.Error()
		detail["error_kind"] = string(protocol.KindOf(t.Err))
	}
	return AuditEvent{
		RunID:  t.RunID,
		NodeID: t.NodeID,
		Type:   EventNodeTransition,
		At:     now,
		Detail: detail,
	}
}

// ---------------------------------------------------------------- provenance

// WriteProvenance implements [ProvenanceStore] and enforces INV-1.
func (s *FileStore) WriteProvenance(ctx context.Context, rec Provenance, create GuardedAction) (Provenance, error) {
	if err := rec.Validate(); err != nil {
		return Provenance{}, ErrProvenanceNotRecorded.Wrap(err)
	}

	s.mu.Lock()
	run, err := s.getRunLocked(rec.RunID)
	if err != nil {
		s.mu.Unlock()
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

	seq, err := s.nextSeqLocked(s.provDir(rec.RunID))
	if err != nil {
		s.mu.Unlock()
		return Provenance{}, ErrProvenanceNotRecorded.Wrap(err)
	}
	rec.ProvenanceID = fmt.Sprintf("%s:prov:%08d", rec.RunID, seq)

	// The commit. Until this returns, create has not been called and cannot
	// be: it is an argument, not the next statement (INV-1).
	if err := s.w.writeJSON(filepath.Join(s.provDir(rec.RunID), fmt.Sprintf("%08d.json", seq)), rec); err != nil {
		s.mu.Unlock()
		return Provenance{}, ErrProvenanceNotRecorded.Wrap(err)
	}
	if _, err := s.appendAuditLocked(AuditEvent{
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
		s.mu.Unlock()
		return Provenance{}, ErrProvenanceNotRecorded.Wrap(err)
	}
	s.mu.Unlock()

	return rec, s.runGuarded(ctx, rec.RunID, rec.NodeID, rec.Object, create,
		EventObjectCreated, EventObjectCreateFailed)
}

// runGuarded executes the guarded action outside the store mutex, then records
// its outcome. The action can take hours: a CREATE INDEX CONCURRENTLY on a
// 10 TB partition is the design centre. Holding the store lock across it would
// block `status` and `cancel`, which are the two commands an operator reaches
// for precisely while it runs.
func (s *FileStore) runGuarded(ctx context.Context, runID RunID, nodeID protocol.NodeID,
	object protocol.ObjectName, action GuardedAction, okEvent, failEvent AuditEventType) error {
	if action == nil {
		return nil
	}
	actionErr := action(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
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
	if _, err := s.appendAuditLocked(ev); err != nil && actionErr == nil {
		return err
	}
	return actionErr
}

// FindProvenance implements [ProvenanceReader].
func (s *FileStore) FindProvenance(ctx context.Context, q ProvenanceQuery) ([]Provenance, error) {
	if err := q.validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var runIDs []RunID
	if q.RunID != "" {
		runIDs = []RunID{q.RunID}
	} else {
		ids, err := s.allRunIDsLocked()
		if err != nil {
			return nil, err
		}
		runIDs = ids
	}

	var out []Provenance
	for _, id := range runIDs {
		recs, err := s.readProvenanceLocked(id)
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			if q.matches(r) {
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RecordedAt.Time.Equal(out[j].RecordedAt.Time) {
			return out[i].ProvenanceID < out[j].ProvenanceID
		}
		return out[i].RecordedAt.Time.Before(out[j].RecordedAt.Time)
	})
	return out, nil
}

func (s *FileStore) readProvenanceLocked(id RunID) ([]Provenance, error) {
	entries, err := os.ReadDir(s.provDir(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, ioErr("list provenance", err)
	}
	out := make([]Provenance, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var p Provenance
		if err := readJSON(filepath.Join(s.provDir(id), e.Name()), &p); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *FileStore) allRunIDsLocked() ([]RunID, error) {
	entries, err := os.ReadDir(s.runsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, ioErr("list runs", err)
	}
	var out []RunID
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var r Run
		if err := readJSON(filepath.Join(s.runsDir(), e.Name(), "run.json"), &r); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, r.RunID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// ---------------------------------------------------------------- authorization

// RecordAuthorization implements [AuthorizationStore] and enforces INV-2.
func (s *FileStore) RecordAuthorization(ctx context.Context, rec AuthorizationRecord, exec GuardedAction) (AuthorizationRecord, error) {
	if err := rec.Validate(); err != nil {
		return AuthorizationRecord{}, ErrAuthorizationNotRecorded.Wrap(err)
	}

	s.mu.Lock()
	run, err := s.getRunLocked(rec.RunID)
	if err != nil {
		s.mu.Unlock()
		return AuthorizationRecord{}, ErrAuthorizationNotRecorded.Wrap(err)
	}
	now := s.now(time.Time{})
	if rec.GrantedAt.Time.IsZero() {
		rec.GrantedAt = protocol.NewTimestamp(now)
	}
	if rec.Database == "" {
		rec.Database = run.Target.Database
	}
	seq, err := s.nextSeqLocked(s.authDir(rec.RunID))
	if err != nil {
		s.mu.Unlock()
		return AuthorizationRecord{}, ErrAuthorizationNotRecorded.Wrap(err)
	}
	rec.AuthorizationID = fmt.Sprintf("%s:auth:%08d", rec.RunID, seq)

	// The commit. FR-AUTH-6: the satisfied mode and its evidence are written
	// before the destructive statement executes.
	if err := s.w.writeJSON(filepath.Join(s.authDir(rec.RunID), fmt.Sprintf("%08d.json", seq)), rec); err != nil {
		s.mu.Unlock()
		return AuthorizationRecord{}, ErrAuthorizationNotRecorded.Wrap(err)
	}
	if _, err := s.appendAuditLocked(authorizationEvent(rec, now)); err != nil {
		s.mu.Unlock()
		return AuthorizationRecord{}, ErrAuthorizationNotRecorded.Wrap(err)
	}
	s.mu.Unlock()

	return rec, s.runGuarded(ctx, rec.RunID, rec.NodeID, rec.Object, exec,
		EventDestructiveExecuted, EventDestructiveFailed)
}

func authorizationEvent(rec AuthorizationRecord, now time.Time) AuditEvent {
	detail := auditDetail(
		"authorization_id", rec.AuthorizationID,
		"mode", string(rec.Mode),
		"object", rec.Object.String(),
		"provenance_id", rec.ProvenanceID,
		"reindex_run_id", string(rec.ReindexRunID),
		"confirmation", rec.Confirmation,
	)
	if rec.Relation != nil {
		if detail == nil {
			detail = map[string]string{}
		}
		detail["relation"] = rec.Relation.String()
	}
	for k, v := range rec.Evidence {
		if detail == nil {
			detail = map[string]string{}
		}
		detail["evidence."+k] = v
	}
	return AuditEvent{
		RunID:  rec.RunID,
		NodeID: rec.NodeID,
		Type:   EventAuthorizationRecorded,
		At:     now,
		Detail: detail,
	}
}

// ListAuthorizations implements [AuthorizationStore].
func (s *FileStore) ListAuthorizations(ctx context.Context, runID RunID) ([]AuthorizationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.authDir(runID))
	if err != nil {
		if os.IsNotExist(err) {
			if _, gerr := s.getRunLocked(runID); gerr != nil {
				return nil, gerr
			}
			return nil, nil
		}
		return nil, ioErr("list authorizations", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	out := make([]AuthorizationRecord, 0, len(names))
	for _, n := range names {
		var rec AuthorizationRecord
		if err := readJSON(filepath.Join(s.authDir(runID), n), &rec); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

// nextSeqLocked returns one past the highest %08d.json in dir.
func (s *FileStore) nextSeqLocked(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 1, nil
		}
		return 0, ioErr("list records", err)
	}
	var max int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".json")
		n, err := strconv.ParseInt(base, 10, 64)
		if err != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	return max + 1, nil
}

// ---------------------------------------------------------------- lease

// AcquireLease implements [LeaseStore].
func (s *FileStore) AcquireLease(ctx context.Context, runID RunID, holder string, ttl time.Duration) (Lease, error) {
	if holder == "" {
		holder = s.holder
	}
	secs, err := normalizeTTL(ttl)
	if err != nil {
		return Lease{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.getRunLocked(runID); err != nil {
		return Lease{}, err
	}
	now := s.now(time.Time{})

	existing, err := s.getLeaseLocked(runID)
	switch {
	case errors.Is(err, ErrNotFound):
		// No lease yet.
	case err != nil:
		return Lease{}, err
	default:
		if existing.Holder != holder && !existing.Expired(now) {
			return Lease{}, ErrLeaseLost.Detailf(
				"run %s is leased by %q until %s", runID, existing.Holder,
				existing.ExpiresAt().UTC().Format(time.RFC3339))
		}
	}

	lease := Lease{
		RunID:       runID,
		Holder:      holder,
		AcquiredAt:  protocol.NewTimestamp(now),
		HeartbeatAt: protocol.NewTimestamp(now),
		TTLSeconds:  secs,
	}
	if existing.Holder == holder && !existing.AcquiredAt.Time.IsZero() {
		lease.AcquiredAt = existing.AcquiredAt
	}
	if err := s.w.writeJSON(s.leasePath(runID), lease); err != nil {
		return Lease{}, err
	}
	if _, err := s.appendAuditLocked(AuditEvent{
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
func (s *FileStore) Heartbeat(ctx context.Context, runID RunID, holder string) (Lease, error) {
	if holder == "" {
		holder = s.holder
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	lease, err := s.getLeaseLocked(runID)
	if err != nil {
		return Lease{}, err
	}
	if lease.Holder != holder {
		return Lease{}, ErrLeaseLost.Detailf(
			"run %s is leased by %q, not %q", runID, lease.Holder, holder)
	}
	lease.HeartbeatAt = protocol.NewTimestamp(s.now(time.Time{}))
	if err := s.w.writeJSON(s.leasePath(runID), lease); err != nil {
		return Lease{}, err
	}
	return lease, nil
}

// ReleaseLease implements [LeaseStore].
func (s *FileStore) ReleaseLease(ctx context.Context, runID RunID, holder string) error {
	if holder == "" {
		holder = s.holder
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	lease, err := s.getLeaseLocked(runID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if lease.Holder != holder {
		return ErrLeaseLost.Detailf("run %s is leased by %q, not %q", runID, lease.Holder, holder)
	}
	if err := os.Remove(s.leasePath(runID)); err != nil && !os.IsNotExist(err) {
		return ioErr("remove lease", err)
	}
	_, err = s.appendAuditLocked(AuditEvent{
		RunID:  runID,
		Type:   EventLeaseReleased,
		At:     s.now(time.Time{}),
		Detail: auditDetail("holder", holder),
	})
	return err
}

// GetLease implements [LeaseStore].
func (s *FileStore) GetLease(ctx context.Context, runID RunID) (Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLeaseLocked(runID)
}

func (s *FileStore) getLeaseLocked(runID RunID) (Lease, error) {
	var l Lease
	if err := readJSON(s.leasePath(runID), &l); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Lease{}, ErrNotFound.Detailf("no lease for run %s", runID)
		}
		return Lease{}, err
	}
	return l, nil
}

// ---------------------------------------------------------------- cancellation

// RequestCancel implements [CancelStore].
func (s *FileStore) RequestCancel(ctx context.Context, runID RunID, actor, note string) (Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	run, err := s.getRunLocked(runID)
	if err != nil {
		return Run{}, err
	}
	now := s.now(time.Time{})

	if run.Status.IsTerminal() {
		return run, nil
	}

	live := false
	if run.Status == RunRunning {
		lease, lerr := s.getLeaseLocked(runID)
		switch {
		case errors.Is(lerr, ErrNotFound):
			live = false
		case lerr != nil:
			return Run{}, lerr
		default:
			live = !lease.Expired(now)
		}
	}

	run.CancelRequested = true
	ts := protocol.NewTimestamp(now)
	run.CancelRequestedAt = &ts
	run.CancelActor = actor
	run.CancelNote = note
	run.UpdatedAt = ts
	if err := s.w.writeJSON(s.runPath(runID), run); err != nil {
		return Run{}, err
	}
	if _, err := s.appendAuditLocked(AuditEvent{
		RunID: runID,
		Type:  EventCancelRequested,
		At:    now,
		Detail: auditDetail(
			"actor", actor,
			"note", note,
			"live", strconv.FormatBool(live),
		),
	}); err != nil {
		return Run{}, err
	}

	if live {
		// FR-CLI-10: the executor observes the flag at a node boundary and
		// never mid-statement. The run stays resumable (AC-24).
		return run, nil
	}
	// FR-CLI-11: nothing is alive, so cancel terminally. `resume` will refuse
	// to adopt a CANCELLED run.
	return s.setRunStatusLocked(RunStatusUpdate{
		RunID: runID,
		From:  run.Status,
		To:    RunCancelled,
		At:    now,
	})
}

// CancellationRequested implements [CancelStore].
func (s *FileStore) CancellationRequested(ctx context.Context, runID RunID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.getRunLocked(runID)
	if err != nil {
		return false, err
	}
	return run.CancelRequested || run.Status == RunCancelled, nil
}

// ---------------------------------------------------------------- audit

// AppendAudit implements [AuditStore]. INV-3: this is the only writer, and it
// only ever appends.
func (s *FileStore) AppendAudit(ctx context.Context, ev AuditEvent) (AuditEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendAuditLocked(ev)
}

func (s *FileStore) appendAuditLocked(ev AuditEvent) (AuditEvent, error) {
	if err := ev.validate(); err != nil {
		return AuditEvent{}, err
	}
	size, err := s.auditSizeLocked(ev.RunID)
	if err != nil {
		return AuditEvent{}, err
	}
	cur, ok := s.auditSeq[ev.RunID]
	if !ok || cur.size != size {
		// Either this store has never appended to this trail, or something
		// else has since it last did. Rebuild rather than reuse a number.
		n, cerr := s.countAuditLocked(ev.RunID)
		if cerr != nil {
			return AuditEvent{}, cerr
		}
		cur = auditCursor{seq: n, size: size}
	}
	seq := cur.seq + 1
	ev.Seq = seq
	ev.EventID = fmt.Sprintf("%s:evt:%08d", ev.RunID, seq)
	ev.OccurredAt = protocol.NewTimestamp(s.now(ev.At))
	ev.At = time.Time{}

	line, merr := json.Marshal(ev)
	if merr != nil {
		return AuditEvent{}, ioErr("encode audit event", merr)
	}
	if err := os.MkdirAll(s.runDir(ev.RunID), 0o750); err != nil {
		return AuditEvent{}, ioErr("create run directory", err)
	}
	// O_APPEND with no O_TRUNC and no seek: the trail can only grow (INV-3).
	f, oerr := os.OpenFile(s.auditPath(ev.RunID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if oerr != nil {
		return AuditEvent{}, ioErr("open audit trail", oerr)
	}
	// Isolate a torn record left by a crash. An interrupted append leaves bytes
	// with no trailing newline; writing straight after them would merge the
	// wreckage and this record into one malformed line, and a malformed line
	// that is not last poisons every subsequent read of the trail. Since every
	// append must first count the existing events, that would also block every
	// later checkpoint, so one crash mid-write would make the run permanently
	// unreadable and unwritable (NFR-REL-2).
	//
	// The leading newline keeps the torn bytes on a line of their own, where
	// readAuditLocked can drop them and keep the records either side.
	if needsNewline, serr := s.auditNeedsSeparatorLocked(ev.RunID); serr != nil {
		_ = f.Close()
		return AuditEvent{}, serr
	} else if needsNewline {
		line = append([]byte{'\n'}, line...)
	}
	if _, werr := f.Write(append(line, '\n')); werr != nil {
		_ = f.Close()
		return AuditEvent{}, ioErr("append audit event", werr)
	}
	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		return AuditEvent{}, ioErr("sync audit trail", serr)
	}
	if cerr := f.Close(); cerr != nil {
		return AuditEvent{}, ioErr("close audit trail", cerr)
	}
	if after, serr := s.auditSizeLocked(ev.RunID); serr == nil {
		s.auditSeq[ev.RunID] = auditCursor{seq: seq, size: after}
	} else {
		// The size is unknown, so the cache would be unverifiable. Drop it and
		// pay the rebuild next time rather than risk a duplicate id.
		delete(s.auditSeq, ev.RunID)
	}
	return ev, nil
}

// isTornRecord reports whether line is a JSON object that stops part way
// through, which is what an append interrupted by a crash leaves behind.
//
// It is deliberately not "anything that fails to parse": a line of unrelated
// bytes in the middle of the trail is corruption with a different cause, and
// INV-3's whole value is that the trail is not quietly editable.
func isTornRecord(line []byte) bool {
	if len(line) == 0 || line[0] != '{' {
		return false
	}
	var raw json.RawMessage
	err := json.NewDecoder(bytes.NewReader(line)).Decode(&raw)
	return errors.Is(err, io.ErrUnexpectedEOF)
}

// auditNeedsSeparatorLocked reports whether the trail ends mid-line, which is
// the signature of an append interrupted by a crash.
func (s *FileStore) auditNeedsSeparatorLocked(runID RunID) (bool, error) {
	f, err := os.Open(s.auditPath(runID))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, ioErr("open audit trail", err)
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return false, ioErr("stat audit trail", err)
	}
	if fi.Size() == 0 {
		return false, nil
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], fi.Size()-1); err != nil {
		return false, ioErr("read audit trail", err)
	}
	return last[0] != '\n', nil
}

// auditSizeLocked returns the trail's current size in bytes; a missing trail is
// zero, not an error.
func (s *FileStore) auditSizeLocked(runID RunID) (int64, error) {
	fi, err := os.Stat(s.auditPath(runID))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, ioErr("stat audit trail", err)
	}
	return fi.Size(), nil
}

func (s *FileStore) countAuditLocked(runID RunID) (int64, error) {
	evs, err := s.readAuditLocked(runID)
	if err != nil {
		return 0, err
	}
	if len(evs) == 0 {
		return 0, nil
	}
	return evs[len(evs)-1].Seq, nil
}

// ListAudit implements [AuditStore].
func (s *FileStore) ListAudit(ctx context.Context, runID RunID, afterSeq int64) ([]AuditEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	evs, err := s.readAuditLocked(runID)
	if err != nil {
		return nil, err
	}
	out := make([]AuditEvent, 0, len(evs))
	for _, e := range evs {
		if e.Seq > afterSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

// readAuditLocked parses the JSON Lines trail.
//
// A torn final line is tolerated and dropped: an append that was interrupted
// mid-write is not an event that happened, and refusing to read the whole trail
// because of it would make a crash unrecoverable. A malformed line anywhere
// else is corruption and is reported.
func (s *FileStore) readAuditLocked(runID RunID) ([]AuditEvent, error) {
	data, err := os.ReadFile(s.auditPath(runID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, ioErr("read audit trail", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var lines [][]byte
	for sc.Scan() {
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		lines = append(lines, append([]byte(nil), b...))
	}
	if err := sc.Err(); err != nil {
		return nil, ioErr("scan audit trail", err)
	}
	trailingNewline := len(data) > 0 && data[len(data)-1] == '\n'

	out := make([]AuditEvent, 0, len(lines))
	for i, l := range lines {
		var ev AuditEvent
		if err := json.Unmarshal(l, &ev); err != nil {
			if i == len(lines)-1 && !trailingNewline {
				break // torn append, see the doc comment
			}
			// A torn record that a later append pushed out of last position.
			// It is not an event that happened: the write never completed, so
			// dropping it loses nothing, while refusing the whole trail would
			// take every complete record with it and block every future
			// checkpoint for the run (NFR-REL-2).
			//
			// The tolerance is deliberately narrow. Only a truncated JSON
			// object qualifies, which is the one shape an interrupted append
			// can leave. Anything else is corruption with another cause and is
			// still reported.
			if isTornRecord(l) {
				continue
			}
			return nil, ioErr("decode audit event", err)
		}
		out = append(out, ev)
	}
	return out, nil
}

// ---------------------------------------------------------------- advisory lock

// lockFile is the on-disk representation of a held advisory lock.
type lockFile struct {
	Key       LockKey            `json:"key"`
	Holder    string             `json:"holder"`
	Token     string             `json:"token"`
	PID       int                `json:"pid"`
	Acquired  protocol.Timestamp `json:"acquired_at"`
	Refreshed protocol.Timestamp `json:"refreshed_at"`
	TTLSecs   int                `json:"ttl_seconds"`
}

func (l lockFile) expired(now time.Time) bool {
	if l.TTLSecs <= 0 {
		return true
	}
	return !now.Before(l.Refreshed.Time.Add(time.Duration(l.TTLSecs) * time.Second))
}

// fileLock is a [Lock] backed by a lock file.
type fileLock struct {
	store *FileStore
	key   LockKey
	token string

	mu       sync.Mutex
	released bool
}

func (l *fileLock) Key() LockKey { return l.key }

func (l *fileLock) Held(ctx context.Context) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return false, nil
	}
	cur, err := l.store.readLockFile(l.key)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return cur.Token == l.token, nil
}

func (l *fileLock) Refresh(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return protocol.ErrLockHeld.Detailf("advisory lock for %s was released", l.key)
	}
	cur, err := l.store.readLockFile(l.key)
	if err != nil {
		return err
	}
	if cur.Token != l.token {
		return protocol.ErrLockHeld.Detailf(
			"advisory lock for %s was taken over by %q", l.key, cur.Holder)
	}
	cur.Refreshed = protocol.NewTimestamp(l.store.now(time.Time{}))
	return l.store.w.writeJSON(l.store.lockPath(l.key), cur)
}

func (l *fileLock) Unlock(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.released = true
	cur, err := l.store.readLockFile(l.key)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if cur.Token != l.token {
		// Someone else's lock now. Removing it would be worse than leaving it.
		return nil
	}
	if err := os.Remove(l.store.lockPath(l.key)); err != nil && !os.IsNotExist(err) {
		return ioErr("remove lock file", err)
	}
	return nil
}

func (s *FileStore) readLockFile(k LockKey) (lockFile, error) {
	var lf lockFile
	if err := readJSON(s.lockPath(k), &lf); err != nil {
		return lockFile{}, err
	}
	return lf, nil
}

// TryLock implements [Locker].
//
// A lock file has no session to end, so a process that dies holding one would
// block its target forever. The file therefore carries a TTL that the holder
// refreshes through [Lock.Refresh] alongside its lease heartbeat (FR-LOCK-3),
// and an expired lock file may be taken over. That is the one behavioural
// difference from the SQL store's pg_try_advisory_lock, and it is documented
// rather than hidden.
func (s *FileStore) TryLock(ctx context.Context, key LockKey) (Lock, error) {
	if err := key.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now(time.Time{})
	path := s.lockPath(key)

	var tok [16]byte
	if _, err := rand.Read(tok[:]); err != nil {
		return nil, ioErr("generate lock token", err)
	}
	token := hex.EncodeToString(tok[:])
	lf := lockFile{
		Key:       key,
		Holder:    s.holder,
		Token:     token,
		PID:       os.Getpid(),
		Acquired:  protocol.NewTimestamp(now),
		Refreshed: protocol.NewTimestamp(now),
		TTLSecs:   int(s.lockTTL / time.Second),
	}
	if lf.TTLSecs <= 0 {
		lf.TTLSecs = int(DefaultLeaseTTL / time.Second)
	}
	body, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return nil, ioErr("encode lock file", err)
	}
	body = append(body, '\n')

	// O_EXCL is the mutual exclusion. It is a single atomic filesystem
	// operation, which is what makes AC-10 hold between two processes.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err == nil {
		if _, werr := f.Write(body); werr != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return nil, ioErr("write lock file", werr)
		}
		if serr := f.Sync(); serr != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return nil, ioErr("sync lock file", serr)
		}
		if cerr := f.Close(); cerr != nil {
			_ = os.Remove(path)
			return nil, ioErr("close lock file", cerr)
		}
		syncDir(filepath.Dir(path))
		return &fileLock{store: s, key: key, token: token}, nil
	}
	if !os.IsExist(err) {
		return nil, ioErr("create lock file", err)
	}

	cur, rerr := s.readLockFile(key)
	if errors.Is(rerr, ErrNotFound) {
		// Released between the failed create and the read. Report it as held;
		// the caller retries the command, which is far better than racing.
		return nil, protocol.ErrLockHeld.Detailf("advisory lock for %s is contended", key)
	}
	if rerr != nil {
		return nil, rerr
	}
	if !cur.expired(now) {
		return nil, s.lockHeldErrorLocked(key, cur)
	}

	// Stale takeover. The write is atomic, so the loser of a two-process race
	// is detected by the read-back below rather than silently believing it
	// won.
	if err := s.w.writeFile(path, body); err != nil {
		return nil, err
	}
	after, err := s.readLockFile(key)
	if err != nil {
		return nil, err
	}
	if after.Token != token {
		return nil, protocol.ErrLockHeld.Detailf(
			"advisory lock for %s was taken by %q while reclaiming it from an expired holder", key, after.Holder)
	}
	return &fileLock{store: s, key: key, token: token}, nil
}

// lockHeldErrorLocked builds the FR-LOCK-2 message, naming the holding run
// where the store can determine it (AC-10).
func (s *FileStore) lockHeldErrorLocked(key LockKey, cur lockFile) error {
	ids, err := s.allRunIDsLocked()
	if err == nil {
		for _, id := range ids {
			r, gerr := s.getRunLocked(id)
			if gerr != nil {
				continue
			}
			if r.Status == RunRunning && r.LockKey() == key {
				return protocol.ErrLockHeld.Detailf(
					"%s is locked by run %s (holder %q, pid %d, since %s)",
					key, r.RunID, cur.Holder, cur.PID, cur.Acquired.Canonical())
			}
		}
	}
	return protocol.ErrLockHeld.Detailf(
		"%s is locked by holder %q (pid %d, since %s); no RUNNING run is recorded for it",
		key, cur.Holder, cur.PID, cur.Acquired.Canonical())
}
