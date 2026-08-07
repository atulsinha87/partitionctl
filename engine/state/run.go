package state

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// RunID identifies one execution of one plan (TRD §17.1: Run replaces Job).
type RunID string

func (id RunID) String() string { return string(id) }

// RunStatus is the lifecycle state of a run.
//
// The TRD names RUNNING (D3a), ORPHANED (INV-4, FR-LOCK-4) and terminal
// cancellation (FR-CLI-11) directly. The remaining three fall out of the
// commands: a run either completes, fails, or is stopped at a node boundary by
// a cancellation request while remaining resumable (AC-24).
type RunStatus string

// The run statuses.
const (
	// RunRunning means a process holds the lease and is dispatching nodes.
	RunRunning RunStatus = "RUNNING"

	// RunCompleted is terminal success: every node reached DONE or SKIPPED.
	RunCompleted RunStatus = "COMPLETED"

	// RunFailed means a node reached a terminal failure. It is resumable:
	// `resume` re-plans against the live catalog and rolls forward.
	RunFailed RunStatus = "FAILED"

	// RunOrphaned means the run was RUNNING, its lease expired, and its
	// advisory lock was unheld (INV-4, FR-LOCK-4). It is resumable.
	RunOrphaned RunStatus = "ORPHANED"

	// RunInterrupted means the executor observed a cancellation request and
	// stopped at a node boundary without interrupting a statement
	// (FR-CLI-10). It is resumable, which is what AC-24 requires.
	RunInterrupted RunStatus = "INTERRUPTED"

	// RunCancelled is terminal: the operator cancelled the run outright and
	// `resume` will not adopt it (FR-CLI-11).
	RunCancelled RunStatus = "CANCELLED"
)

var allRunStatuses = []RunStatus{
	RunRunning, RunCompleted, RunFailed, RunOrphaned, RunInterrupted, RunCancelled,
}

// AllRunStatuses returns every status. The returned slice is a copy.
func AllRunStatuses() []RunStatus {
	out := make([]RunStatus, len(allRunStatuses))
	copy(out, allRunStatuses)
	return out
}

// Valid reports whether s is a known status.
func (s RunStatus) Valid() bool {
	for _, k := range allRunStatuses {
		if k == s {
			return true
		}
	}
	return false
}

func (s RunStatus) String() string { return string(s) }

// IsTerminal reports whether the run can never change status again.
func (s RunStatus) IsTerminal() bool {
	return s == RunCompleted || s == RunCancelled
}

// IsResumable reports whether `resume` may adopt this run. A terminally
// cancelled run is deliberately excluded (FR-CLI-11).
func (s RunStatus) IsResumable() bool {
	switch s {
	case RunFailed, RunOrphaned, RunInterrupted:
		return true
	}
	return false
}

// IsIncomplete reports whether the run left work undone. It is the predicate
// `execute` uses to refuse a plan and name `resume` (FR-CLI-9, AC-23).
func (s RunStatus) IsIncomplete() bool {
	switch s {
	case RunRunning, RunFailed, RunOrphaned, RunInterrupted:
		return true
	}
	return false
}

// runTransitions is the run-status state machine.
var runTransitions = map[RunStatus]map[RunStatus]bool{
	RunRunning: {
		RunCompleted:   true, // every node DONE or SKIPPED
		RunFailed:      true, // a node reached terminal failure
		RunOrphaned:    true, // INV-4: lease expired, lock unheld
		RunInterrupted: true, // stopped at a node boundary (FR-CLI-10)
		RunCancelled:   true, // terminally cancelled while abandoned
	},
	RunOrphaned:    {RunRunning: true, RunCancelled: true},
	RunFailed:      {RunRunning: true, RunCancelled: true},
	RunInterrupted: {RunRunning: true, RunCancelled: true},
	RunCompleted:   {},
	RunCancelled:   {},
}

// ValidRunTransition reports whether from -> to is permitted. Self-transitions
// are rejected: rewriting the same status is not a status change.
func ValidRunTransition(from, to RunStatus) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	return runTransitions[from][to]
}

// CheckRunTransition returns nil if from -> to is permitted and an error
// matching [protocol.ErrInvalidTransition] otherwise.
func CheckRunTransition(from, to RunStatus) error {
	if ValidRunTransition(from, to) {
		return nil
	}
	return protocol.ErrInvalidTransition.Detailf("run status %s -> %s is not permitted", from, to)
}

// Run is one execution of one plan.
//
// PlanDigest is bound at creation and never changes (INV-6). There is no API in
// this package that mutates it.
type Run struct {
	RunID     RunID              `json:"run_id"`
	PlanID    protocol.PlanID    `json:"plan_id"`
	Operation protocol.Operation `json:"operation"`
	Target    protocol.Target    `json:"target"`

	// PlanDigest is the digest of the plan this run executes. INV-6: it is
	// bound for the run's lifetime.
	PlanDigest string `json:"plan_digest"`

	// Actor is who started the run, for the audit trail.
	Actor string `json:"actor,omitempty"`

	Status RunStatus `json:"status"`

	// NodeCount is how many nodes the plan contained when the run opened. It
	// lets `status` report progress without reading the plan file
	// (FR-CLI-12).
	NodeCount int `json:"node_count"`

	StartedAt  protocol.Timestamp  `json:"started_at"`
	UpdatedAt  protocol.Timestamp  `json:"updated_at"`
	FinishedAt *protocol.Timestamp `json:"finished_at,omitempty"`

	// CancelRequested is the flag `cancel` sets and a running executor polls at
	// node boundaries (FR-CLI-10). It never interrupts a statement.
	CancelRequested   bool                `json:"cancel_requested,omitempty"`
	CancelRequestedAt *protocol.Timestamp `json:"cancel_requested_at,omitempty"`
	CancelActor       string              `json:"cancel_actor,omitempty"`
	CancelNote        string              `json:"cancel_note,omitempty"`

	// LastError is the message that moved the run to FAILED, if any.
	LastError string `json:"last_error,omitempty"`
}

// LockKey is the advisory-lock key for this run's target (FR-LOCK-1).
func (r Run) LockKey() LockKey {
	return LockKey{Database: r.Target.Database, Table: r.Target.Table}
}

// NewRun is the request that opens a run.
//
// It takes the whole [protocol.Plan] rather than a digest, because that is what
// binds the run to exactly one digest without the caller being able to get it
// wrong (INV-6), and because the node set is what the store seeds node state
// from.
type NewRun struct {
	// Plan is required. Its digest, id, operation, target and node set are
	// copied into the run.
	Plan *protocol.Plan

	// RunID is optional. When empty the store generates one.
	RunID RunID

	// Actor is who started the run.
	Actor string

	// StartedAt is optional. When zero the store's clock is used.
	StartedAt time.Time
}

func (n NewRun) validate() error {
	if n.Plan == nil {
		return protocol.ErrInvalidPlan.Detailf("NewRun.Plan is required: a run is bound to exactly one plan digest (INV-6)")
	}
	if n.Plan.Digest == "" {
		return protocol.ErrDigestMismatch.Detailf("plan is unsealed: call Plan.Seal before opening a run (INV-6)")
	}
	if !strings.HasPrefix(n.Plan.Digest, protocol.DigestPrefix) {
		return protocol.ErrDigestMismatch.Detailf("plan digest %q is missing the %q prefix", n.Plan.Digest, protocol.DigestPrefix)
	}
	if n.Plan.PlanID == "" {
		return protocol.ErrInvalidPlan.Detailf("plan_id is empty")
	}
	if !n.Plan.Operation.Valid() {
		return protocol.ErrInvalidPlan.Detailf("unknown operation %q", n.Plan.Operation)
	}
	if err := n.Plan.Target.Validate(); err != nil {
		return protocol.ErrInvalidPlan.Detailf("target: %v", err)
	}
	if len(n.Plan.Nodes) == 0 {
		return protocol.ErrInvalidPlan.Detailf("plan contains no nodes")
	}
	seen := make(map[protocol.NodeID]struct{}, len(n.Plan.Nodes))
	for i := range n.Plan.Nodes {
		nd := &n.Plan.Nodes[i]
		if nd.ID == "" {
			return protocol.ErrInvalidPlan.Detailf("node %d has an empty id", i)
		}
		if !nd.Kind.Valid() {
			return protocol.ErrUnknownNodeKind.Detailf("node %q: %q", nd.ID, nd.Kind)
		}
		if _, dup := seen[nd.ID]; dup {
			return protocol.ErrInvalidPlan.Detailf("duplicate node id %q", nd.ID)
		}
		seen[nd.ID] = struct{}{}
	}
	return nil
}

// RunStatusUpdate is a compare-and-set on a run's status.
//
// From is required. Requiring the caller to state what it believes the current
// status is turns a lost update into an error matching [ErrConflict] instead of
// a silent overwrite, which matters because two processes can reach the same
// run through `cancel` and `resume` at once.
type RunStatusUpdate struct {
	RunID RunID
	From  RunStatus
	To    RunStatus

	// Error is recorded as the run's last error. Meaningful when To is
	// [RunFailed].
	Error string

	// ClearCancel revokes an outstanding cancellation request as part of this
	// transition.
	//
	// It exists for adoption (D3b): `cancel` sets a flag that the executor
	// polls at every node boundary, and nothing else ever unsets it. A run
	// cancelled at a boundary is recorded INTERRUPTED, which is resumable, so
	// without this the adopted run would stop again on its first boundary with
	// zero nodes dispatched, forever. Adopting a run *is* the decision to
	// continue it, so adoption revokes the stop request (AC-24).
	ClearCancel bool

	// At is optional. When zero the store's clock is used.
	At time.Time
}

// RunQuery selects runs. Zero-valued fields do not filter.
//
// It is the one read the leftover-authorization check (FR-AUTH-3), the
// Liquibase reindex gate (FR-LB-4, FR-LB-9), the incomplete-prior-run refusal
// (FR-CLI-9) and orphan detection (INV-4) all go through.
type RunQuery struct {
	// RunID selects exactly one run.
	RunID RunID

	// PlanDigest selects every run bound to one plan (INV-6).
	PlanDigest string

	// Database, Table and Index filter on the run's target.
	Database string
	Table    *protocol.ObjectName
	Index    *protocol.ObjectName

	// Operation filters on the planner that produced the plan.
	Operation protocol.Operation

	// Statuses filters on run status. Empty means any.
	Statuses []RunStatus

	// FinishedSince selects runs that finished at or after this instant. It is
	// what FR-LB-4's `since` bound maps to.
	FinishedSince time.Time

	// Limit caps the result. Zero means unlimited.
	Limit int
}

func (q RunQuery) matches(r Run) bool {
	if q.RunID != "" && r.RunID != q.RunID {
		return false
	}
	if q.PlanDigest != "" && r.PlanDigest != q.PlanDigest {
		return false
	}
	if q.Database != "" && r.Target.Database != q.Database {
		return false
	}
	if q.Table != nil && (r.Target.Table.Schema != q.Table.Schema || r.Target.Table.Name != q.Table.Name) {
		return false
	}
	if q.Index != nil {
		if r.Target.Index == nil {
			return false
		}
		if r.Target.Index.Schema != q.Index.Schema || r.Target.Index.Name != q.Index.Name {
			return false
		}
	}
	if q.Operation != "" && r.Operation != q.Operation {
		return false
	}
	if len(q.Statuses) > 0 {
		found := false
		for _, s := range q.Statuses {
			if r.Status == s {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if !q.FinishedSince.IsZero() {
		if r.FinishedAt == nil {
			return false
		}
		if r.FinishedAt.Time.Before(q.FinishedSince) {
			return false
		}
	}
	return true
}

// NewRunID generates a run identifier. The time prefix makes a directory
// listing sort chronologically; the random suffix makes collisions impossible
// in practice even when two runs open in the same second.
func NewRunID(now time.Time) RunID {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not recoverable and not something a state
		// store should paper over with a weaker source.
		panic(fmt.Sprintf("state: crypto/rand: %v", err))
	}
	return RunID("run-" + now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:]))
}
