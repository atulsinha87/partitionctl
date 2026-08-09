package executor

import (
	"context"
	"errors"
	"math/rand"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// DefaultLockTimeout is the lock_timeout applied to every DDL statement when
// [Config.LockTimeout] is left zero (FR-EXEC-5). It is short on purpose: a
// statement that cannot get its lock quickly should back off and try again
// rather than queue in front of the application's own traffic.
const DefaultLockTimeout = 5 * time.Second

// DefaultBuildLockTimeout is the lock_timeout applied to the CONCURRENTLY
// statements when [Config.BuildLockTimeout] is left zero (FR-EXEC-5).
//
// It is deliberately far larger than [DefaultLockTimeout], because for those
// statements lock_timeout bounds something quite different. A CONCURRENTLY
// statement waits for the application's own in-flight transactions as part of
// doing its work, not merely while acquiring its initial lock
// ([protocol.NodeKind.WaitsForConcurrentTransactions]). At five seconds, one
// application transaction held open for six aborts an index build that has
// already paid for a full table scan, and PostgreSQL leaves the index INVALID.
//
// Fifteen minutes is a working default rather than a guarantee: an operator
// whose workload holds transactions open for longer should raise it to sit
// above their longest legitimate transaction.
const DefaultBuildLockTimeout = 15 * time.Minute

// DefaultHeartbeatInterval is used when [Config.Heartbeat] is set but
// [Config.HeartbeatInterval] is not (FR-LOCK-3).
const DefaultHeartbeatInterval = 10 * time.Second

// supportedKinds is what this build can dispatch: the whole vocabulary
// (TRD §7.2.2).
var supportedKinds = []protocol.NodeKind{
	protocol.KindCatalogAssert,
	protocol.KindIndexCreateParentInvalid,
	protocol.KindIndexCreateConcurrently,
	protocol.KindIndexAttach,
	protocol.KindIndexVerify,
	protocol.KindWait,
	protocol.KindIndexDropConcurrently,
	protocol.KindIndexReindexConcurrently,
	protocol.KindIndexDropPartitioned,
}

// SupportedKinds returns the node kinds this executor build implements, in
// vocabulary order. The returned slice is a copy.
//
// It is the whole vocabulary. The list stays as an explicit value rather than
// deferring to [protocol.AllNodeKinds] because the executor's claim is "this
// build dispatches these", which is not the same statement as "these kinds
// exist", and a future kind must fail loudly here rather than fall through
// dispatch's default.
func SupportedKinds() []protocol.NodeKind {
	out := make([]protocol.NodeKind, len(supportedKinds))
	copy(out, supportedKinds)
	return out
}

func supportedKind(k protocol.NodeKind) bool {
	for _, s := range supportedKinds {
		if s == k {
			return true
		}
	}
	return false
}

func unsupportedKind(n *protocol.Node) error {
	return ErrUnsupportedNodeKind.Detailf(
		"node %q has kind %q; this build implements %v", n.ID, n.Kind, supportedKinds)
}

// Config wires the executor to its ports. Everything except [Config.Store] and
// [Config.SQL] has a working default.
type Config struct {
	// Store is where every checkpoint, provenance record, authorization record
	// and audit event goes. Required unless DryRun.
	Store StateStore

	// SQL is the target database. Required unless DryRun.
	SQL SQLExecutor

	// Catalog evaluates catalog.assert and index.verify nodes. Required only
	// when the plan contains one of those kinds, which is checked before any
	// statement runs.
	Catalog CatalogEvaluator

	// Logger receives one record per node transition (FR-EXEC-7). Defaults to
	// [NopLogger].
	Logger Logger

	// Clock is the source of time for wait nodes and retry backoff. Defaults to
	// [SystemClock].
	Clock Clock

	// Classifier decides which failures may be retried (FR-EXEC-3). Defaults to
	// [DefaultClassifier].
	Classifier Classifier

	// Retry bounds retries (FR-EXEC-4). Defaults to [DefaultRetryPolicy].
	Retry RetryPolicy

	// LockTimeout is applied to every DDL statement (FR-EXEC-5). Must be
	// positive; defaults to [DefaultLockTimeout].
	LockTimeout time.Duration

	// BuildLockTimeout is the lock_timeout applied instead to the kinds that
	// wait for the application's own transactions as part of their work
	// ([protocol.NodeKind.WaitsForConcurrentTransactions]). Must not be
	// negative; defaults to [DefaultBuildLockTimeout].
	BuildLockTimeout time.Duration

	// StatementTimeout is applied to DDL statements whose kind permits one.
	// Zero, the default, means no finite statement timeout anywhere. It is
	// *always* zero for index.create_concurrently regardless of this value
	// (FR-EXEC-5).
	StatementTimeout time.Duration

	// Jitter draws the randomness for backoff, returning a value in [0, 1).
	// Defaults to math/rand. Injectable so tests are deterministic.
	Jitter func() float64

	// Heartbeat renews the run lease while the executor runs (FR-LOCK-3). Nil
	// starts no goroutine.
	Heartbeat Heartbeater

	// HeartbeatInterval defaults to [DefaultHeartbeatInterval].
	HeartbeatInterval time.Duration

	// DryRun prints the dispatch sequence and issues no DDL and no state
	// writes (FR-CLI-5).
	DryRun bool

	// AllowAdoption permits the one authorization path that is reserved to
	// `resume` (FR-CLI-9): dropping an object that carries no ownership marker
	// but is still named by a live claim ([AuthorizationDecision.Adopt]).
	//
	// It is a configuration flag rather than a plan property on purpose. The
	// old rule refused any plan containing a provenance-authorized destructive
	// node, which was both too broad — a marked object is a catalog fact and
	// `execute` may act on it — and defeated by any re-plan, because the guard
	// was keyed on the digest of a prior run. Deciding it here means the rule is
	// evaluated against the same live state the authorization is, at the moment
	// the statement would run.
	AllowAdoption bool
}

// Executor walks a plan. It holds no per-run state: everything durable lives in
// the [StateStore], which is what makes resume a re-read rather than a replay.
type Executor struct {
	cfg Config
}

// New validates the configuration and returns an executor.
func New(cfg Config) (*Executor, error) {
	if cfg.Logger == nil {
		cfg.Logger = NopLogger{}
	}
	if cfg.Clock == nil {
		cfg.Clock = SystemClock{}
	}
	if cfg.Jitter == nil {
		cfg.Jitter = rand.Float64
	}
	if cfg.Retry == (RetryPolicy{}) {
		cfg.Retry = DefaultRetryPolicy()
	}
	if err := cfg.Retry.Validate(); err != nil {
		return nil, ErrInvalidRun.Detailf("%v", err)
	}
	if cfg.LockTimeout == 0 {
		cfg.LockTimeout = DefaultLockTimeout
	}
	if cfg.LockTimeout < 0 {
		return nil, ErrInvalidRun.Detailf("lock_timeout is negative")
	}
	if cfg.BuildLockTimeout == 0 {
		cfg.BuildLockTimeout = DefaultBuildLockTimeout
	}
	if cfg.BuildLockTimeout < 0 {
		return nil, ErrInvalidRun.Detailf("build lock_timeout is negative")
	}
	if cfg.StatementTimeout < 0 {
		return nil, ErrInvalidRun.Detailf("statement_timeout is negative")
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if !cfg.DryRun {
		if cfg.Store == nil {
			return nil, ErrMissingPort.Detailf("a StateStore is required (FR-STATE-1)")
		}
		if cfg.SQL == nil {
			return nil, ErrMissingPort.Detailf("an SQLExecutor is required")
		}
	}
	return &Executor{cfg: cfg}, nil
}

// Result summarizes one invocation of [Executor.Run].
type Result struct {
	RunID RunID
	// Total is the node count in the plan.
	Total int
	// Done, Skipped and Failed count nodes in those terminal states after the
	// run. Remaining counts everything not yet complete, which is what makes
	// the run resumable.
	Done      int
	Skipped   int
	Failed    int
	Remaining int
	// Cancelled reports that the run stopped at a node boundary rather than
	// finishing or failing (FR-ORD-4, AC-24).
	Cancelled bool
	// CancelReason is [StopCancelFlag] or [StopSignal].
	CancelReason string
	// HaltedAt names the node that failed, if any.
	HaltedAt protocol.NodeID
	// Order is the topological order the executor walked, for `--dry-run`
	// output and for tests.
	Order []protocol.NodeID
}

// Complete reports whether every node reached DONE or SKIPPED.
func (r *Result) Complete() bool { return r != nil && r.Remaining == 0 && !r.Cancelled }

// The reasons a run stops at a node boundary.
const (
	// StopCancelFlag is `partitionctl cancel` (FR-CLI-10).
	StopCancelFlag = "cancel_flag"
	// StopSignal is SIGINT or SIGTERM, handled identically (FR-EXEC-8).
	StopSignal = "signal"
	// StopFenced is the loss of this run's lease or advisory lock to another
	// process (FR-LOCK-1, AC-10). Dispatch stops at the next node boundary so
	// that two processes never issue DDL against one target.
	StopFenced = "fenced"
	// StopUnsafeRetry is a retryable failure of a node kind whose statement
	// cannot be re-issued in process, because it commits catalog state before
	// it fails ([protocol.NodeKind.RetrySafe]). The node is left resumable and
	// `resume` performs the cleanup that makes the rebuild possible.
	StopUnsafeRetry = "unsafe_retry"
)

// nodeStatus mirrors the stored state of one node during a run. Its State field
// advances only after the corresponding checkpoint is durable, which is what
// makes "checkpoint before proceeding" true rather than aspirational
// (FR-EXEC-2).
type nodeStatus struct {
	State           protocol.NodeState
	Attempts        int
	attemptsThisRun int
}

// Run walks the plan to completion, to a halt, or to a cancellation.
//
// ctx is consulted at node boundaries only. Statements and checkpoints run
// under a context derived with [context.WithoutCancel], so a signal arriving
// mid-statement stops the next node instead of killing the one in flight
// (FR-CLI-10, FR-EXEC-8).
//
// The plan must be sealed: Run re-verifies its digest before doing anything
// else, so a plan mutated in memory after [protocol.Plan.Seal] is refused with
// exit 10 rather than executed.
//
// A returned error is the run's failure and carries the contract exit code via
// [protocol.ExitCodeFor]. A cancelled run returns a nil error with
// [Result.Cancelled] set: stopping on request is not a failure.
func (e *Executor) Run(ctx context.Context, run RunID, plan *protocol.Plan) (*Result, error) {
	if run == "" {
		return nil, ErrInvalidRun.Detailf("run id is empty")
	}
	if plan == nil {
		return nil, protocol.ErrInvalidPlan.Detailf("plan is nil")
	}
	// Defence in depth for FR-PLANFILE-3. The CLI verifies the digest against
	// the file it read, which is the check that catches tampering; this one
	// catches a plan mutated in memory after it was sealed, and costs one hash
	// of a structure already in memory.
	if err := plan.VerifyDigest(); err != nil {
		return nil, err
	}
	// Validate covers the format version, every node, dependency resolution and
	// acyclicity. A cyclic graph is refused here, before anything runs.
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	order, err := plan.TopologicalOrder()
	if err != nil {
		return nil, err
	}
	if err := e.preflight(plan); err != nil {
		return nil, err
	}

	res := &Result{RunID: run, Total: len(order), Order: order}
	if e.cfg.DryRun {
		return res, e.dryRun(run, plan, order)
	}

	// Everything durable happens under bg, which no signal cancels.
	bg := context.WithoutCancel(ctx)

	// fenced is set by the heartbeat when this process loses the run's lease or
	// advisory lock. It is read at every node boundary, alongside the
	// cancellation flag, so the run stops without interrupting a statement.
	var fenced atomic.Bool
	stopHeartbeat := e.startHeartbeat(bg, run, func() { fenced.Store(true) })
	defer stopHeartbeat()

	states, err := e.loadStates(bg, run, order)
	if err != nil {
		return res, err
	}
	if err := e.audit(bg, AuditEvent{
		RunID: run,
		Type:  AuditRunStarted,
		Detail: map[string]string{
			"plan_id": string(plan.PlanID),
			"digest":  plan.Digest,
			"nodes":   strconv.Itoa(len(order)),
		},
	}); err != nil {
		return res, err
	}
	if err := e.recoverOrphans(bg, run, plan, order, states); err != nil {
		e.finish(bg, res, states, err)
		return res, err
	}

	for _, id := range order {
		st := states[id]
		if st.State.IsComplete() {
			// Already done in an earlier run. Re-running a completed plan is a
			// no-op that exits zero (AC-7).
			continue
		}

		// Node boundary: the only place cancellation is observed (FR-CLI-10).
		stopped, reason, err := e.stopRequested(ctx, bg, run, &fenced)
		if err != nil {
			e.finish(bg, res, states, err)
			return res, err
		}
		if stopped {
			e.stop(bg, res, states, reason)
			return res, nil
		}

		n, ok := plan.NodeByID(id)
		if !ok {
			err := protocol.ErrInvalidPlan.Detailf("topological order names unknown node %q", id)
			e.finish(bg, res, states, err)
			return res, err
		}

		if err := e.runNode(ctx, bg, run, plan, n, st, states); err != nil {
			if errors.Is(err, errStopped) {
				e.stop(bg, res, states, StopSignal)
				return res, nil
			}
			if errors.Is(err, errUnsafeRetry) {
				e.stop(bg, res, states, StopUnsafeRetry)
				return res, nil
			}
			res.HaltedAt = id
			e.finish(bg, res, states, err)
			return res, err
		}
	}

	e.finish(bg, res, states, nil)
	return res, nil
}

// preflight refuses a plan this build cannot execute, before any statement
// runs: an unimplemented node kind, a node that cannot be rendered, or a
// missing port. Failing here rather than mid-run is what keeps a partially
// executed plan from being the price of a typo.
func (e *Executor) preflight(plan *protocol.Plan) error {
	needsCatalog := false
	for i := range plan.Nodes {
		n := &plan.Nodes[i]
		if !supportedKind(n.Kind) {
			return unsupportedKind(n)
		}
		switch {
		case n.Kind == protocol.KindCatalogAssert, n.Kind == protocol.KindIndexVerify:
			needsCatalog = true
		case n.Kind.IsDestructive():
			// Authorization reads the ownership marker off the object it is
			// about to destroy (FR-AUTH-2, FR-AUTH-3 as amended), so the
			// catalog port is as load-bearing here as it is for an assertion.
			needsCatalog = true
		case n.Kind.ClaimsOwnership():
			// Every marking kind reads whatever comment the object already
			// carries before it writes: a rewrite preserves the creation facts,
			// and all of them refuse to overwrite a comment somebody else wrote
			// ([protocol.RenderMarkerStatement]).
			needsCatalog = true
		}
		if n.Kind.IssuesDDL() {
			sql, err := Render(n)
			if err != nil {
				return err
			}
			if sql == "" {
				return protocol.ErrInvalidPlan.Detailf(
					"node %q of kind %q rendered an empty statement", n.ID, n.Kind)
			}
		}
	}
	// A dry run walks the order and touches nothing, so it needs no catalog
	// port, exactly as it needs no StateStore and no SQLExecutor (see New).
	// Requiring one here made --dry-run fail on every plan this tool can
	// produce, since create-index always emits catalog.assert and index.verify.
	if needsCatalog && !e.cfg.DryRun && e.cfg.Catalog == nil {
		return ErrMissingPort.Detailf(
			"the plan contains nodes that read the catalog (assertions, verification, " +
				"destructive authorization or a reindex marker rewrite) but no CatalogEvaluator is configured")
	}
	return nil
}

// dryRun prints what would be dispatched, in order, and touches nothing
// (FR-CLI-5).
func (e *Executor) dryRun(run RunID, plan *protocol.Plan, order []protocol.NodeID) error {
	for i, id := range order {
		n, ok := plan.NodeByID(id)
		if !ok {
			return protocol.ErrInvalidPlan.Detailf("topological order names unknown node %q", id)
		}
		sql, err := Render(n)
		if err != nil {
			return err
		}
		e.cfg.Logger.Log(LogEvent{
			TS:      e.cfg.Clock.Now().UTC().Format(timeLayout),
			Event:   "dry_run_dispatch",
			RunID:   run,
			NodeID:  n.ID,
			Kind:    n.Kind,
			Attempt: i + 1,
			Message: sql,
		})
	}
	return nil
}

// loadStates reads the recorded state of every node, defaulting to PENDING.
func (e *Executor) loadStates(ctx context.Context, run RunID, order []protocol.NodeID) (map[protocol.NodeID]*nodeStatus, error) {
	recs, err := e.cfg.Store.NodeStates(ctx, run)
	if err != nil {
		return nil, ErrCheckpointFailed.Detailf("reading node states for run %q", run).Wrap(err)
	}
	out := make(map[protocol.NodeID]*nodeStatus, len(order))
	for _, id := range order {
		st := &nodeStatus{State: protocol.InitialNodeState()}
		if r, ok := recs[id]; ok {
			if !r.State.Valid() {
				return nil, ErrInvalidRun.Detailf(
					"stored state %q for node %q is not a known NodeState", r.State, id)
			}
			st.State = r.State
			st.Attempts = r.Attempts
		}
		out[id] = st
	}
	return out, nil
}

// recoverOrphans adopts a run whose process died mid-node. RUNNING -> PENDING
// is the single non-monotonic edge in D7 and is legal only under
// [protocol.ReasonOrphanRecovery] (INV-5).
//
// The statement may or may not have committed; the planner reconstructs
// remaining work from the live catalog, and a half-built index is cleaned up
// through the provenance-gated path (TRD §7.3.2).
func (e *Executor) recoverOrphans(ctx context.Context, run RunID, plan *protocol.Plan, order []protocol.NodeID, states map[protocol.NodeID]*nodeStatus) error {
	for _, id := range order {
		st := states[id]
		if st.State != protocol.NodeRunning {
			continue
		}
		n, ok := plan.NodeByID(id)
		if !ok {
			return protocol.ErrInvalidPlan.Detailf("topological order names unknown node %q", id)
		}
		if err := e.transition(ctx, run, n, st, protocol.NodePending, protocol.ReasonOrphanRecovery, transitionInfo{
			message: "adopting a node left RUNNING by a dead process",
		}); err != nil {
			return err
		}
		if err := e.audit(ctx, AuditEvent{
			RunID:  run,
			NodeID: id,
			Type:   AuditOrphanRecovered,
			Detail: map[string]string{"kind": string(n.Kind)},
		}); err != nil {
			return err
		}
	}
	return nil
}

// stopRequested checks both stop channels at a node boundary: the caller's
// context, which carries SIGINT and SIGTERM (FR-EXEC-8), and the state store's
// cancellation flag, which is how `cancel` reaches a process it cannot signal
// (FR-CLI-10).
func (e *Executor) stopRequested(ctx, bg context.Context, run RunID, fenced *atomic.Bool) (bool, string, error) {
	if ctx.Err() != nil {
		return true, StopSignal, nil
	}
	// Fencing is checked before the store: once another process holds this
	// run, this one must issue nothing further, and asking the store a question
	// it may answer on behalf of the new holder proves nothing (FR-LOCK-1).
	if fenced != nil && fenced.Load() {
		return true, StopFenced, nil
	}
	requested, err := e.cfg.Store.CancelRequested(bg, run)
	if err != nil {
		return false, "", ErrCheckpointFailed.Detailf("reading the cancellation flag for run %q", run).Wrap(err)
	}
	if requested {
		return true, StopCancelFlag, nil
	}
	return false, "", nil
}

// runNode takes one node from its recorded state to a terminal state, or
// returns the error that halts the run.
func (e *Executor) runNode(ctx, bg context.Context, run RunID, plan *protocol.Plan, n *protocol.Node, st *nodeStatus, states map[protocol.NodeID]*nodeStatus) error {
	if st.State == protocol.NodeFailed {
		return ErrNodePreviouslyFailed.Detailf(
			"node %q (%s) is FAILED; FAILED is terminal in D7, so re-plan against the live catalog rather than resuming past it",
			n.ID, n.Kind)
	}

	// FR-ORD-1. Topological order already guarantees this; checking anyway is
	// cheap and turns a corrupted store into a refusal rather than a
	// dependency-order violation against a live database.
	for _, dep := range n.DependsOn {
		d, ok := states[dep]
		if !ok || !d.State.IsComplete() {
			state := protocol.NodeState("absent")
			if ok {
				state = d.State
			}
			return ErrDependencyNotComplete.Detailf(
				"node %q depends on %q, which is %s", n.ID, dep, state)
		}
	}

	switch st.State {
	case protocol.NodePending:
		if err := e.transition(bg, run, n, st, protocol.NodeReady, protocol.ReasonNormal, transitionInfo{}); err != nil {
			return err
		}
	case protocol.NodeRetryWait:
		// Backoff elapsed while the process was away.
		if err := e.transition(bg, run, n, st, protocol.NodeReady, protocol.ReasonNormal, transitionInfo{}); err != nil {
			return err
		}
	case protocol.NodeReady:
	case protocol.NodeVerifying:
		// The statement returned before the process died. Re-verifying is safe
		// and cheaper than re-running the statement.
		return e.verifyPhase(bg, run, n, st)
	default:
		return ErrInvalidRun.Detailf("node %q is in state %s, which the dispatch loop cannot enter", n.ID, st.State)
	}

	for {
		st.Attempts++
		st.attemptsThisRun++

		// INV-2 / FR-AUTH-5: re-evaluate against live state immediately before
		// dispatch, and record the satisfied mode before the statement runs.
		var decision AuthorizationDecision
		if n.Kind.IsDestructive() {
			d, err := e.authorizeNode(bg, run, plan, n)
			if err != nil {
				return err
			}
			decision = d
		}

		// INV-1 as amended: the durable record naming the object and this run
		// was committed by CreateRun and became a live claim at READY, above.
		// Nothing further is written before the statement; the permanent
		// ownership record is the marker execDDL writes onto the object itself
		// once the statement returns.
		if err := e.transition(bg, run, n, st, protocol.NodeRunning, protocol.ReasonNormal, transitionInfo{}); err != nil {
			return err
		}

		start := e.cfg.Clock.Now()
		dispatchErr := e.dispatch(bg, run, plan, n, decision)
		elapsed := e.cfg.Clock.Now().Sub(start)

		if dispatchErr == nil {
			if err := e.transition(bg, run, n, st, protocol.NodeVerifying, protocol.ReasonNormal, transitionInfo{duration: elapsed}); err != nil {
				return err
			}
			return e.verifyPhase(bg, run, n, st)
		}

		class := e.cfg.Classifier.Classify(dispatchErr)
		info := transitionInfo{duration: elapsed, err: dispatchErr, class: class}

		// A kind that issues no DDL is never retried: a false assertion is
		// false again a second later.
		if !class.Retryable() || !n.Kind.Retryable() {
			if err := e.transition(bg, run, n, st, protocol.NodeFailed, protocol.ReasonNormal, info); err != nil {
				return err
			}
			e.auditNodeFailed(bg, run, n, class, dispatchErr)
			return nodeError(n, class, dispatchErr)
		}

		if err := e.transition(bg, run, n, st, protocol.NodeRetryWait, protocol.ReasonNormal, info); err != nil {
			return err
		}

		// FR-EXEC-4: back off and retry, but only where re-issuing the
		// statement can still succeed. A kind that commits catalog state before
		// it fails cannot be re-sent verbatim: the retry meets 42P07 or 42704
		// and dies terminally, replacing the operator's real diagnosis with a
		// misleading one and spending the budget on attempts that cannot work.
		//
		// The node stays in RETRY_WAIT, which resume re-enters as READY, and
		// the run stops. Resume drops the half-built object under provenance
		// first, so the rebuild starts from a clean leaf (FR-PLAN-6, AC-5).
		if !n.Kind.RetrySafe() {
			e.auditRetryDeferred(bg, run, n, class, dispatchErr)
			return errUnsafeRetry
		}

		if st.attemptsThisRun >= e.cfg.Retry.MaxAttempts {
			if err := e.transition(bg, run, n, st, protocol.NodeFailed, protocol.ReasonNormal, info); err != nil {
				return err
			}
			e.auditNodeFailed(bg, run, n, class, dispatchErr)
			return nodeError(n, class, dispatchErr).Detailf(
				"retry budget of %d attempts exhausted", e.cfg.Retry.MaxAttempts)
		}

		delay := e.cfg.Retry.JitteredDelay(st.attemptsThisRun+1, e.cfg.Jitter())
		// Backoff is the one wait the executor may interrupt: it issues no
		// statement, and a node left in RETRY_WAIT resumes cleanly.
		if err := e.cfg.Clock.Sleep(ctx, delay); err != nil {
			return errStopped
		}
		if err := e.transition(bg, run, n, st, protocol.NodeReady, protocol.ReasonNormal, transitionInfo{}); err != nil {
			return err
		}
	}
}

// verifyPhase runs the node's verification, if its kind has any, and closes the
// node out. Every node reaches DONE through VERIFYING: D7 has no
// RUNNING -> DONE edge.
func (e *Executor) verifyPhase(bg context.Context, run RunID, n *protocol.Node, st *nodeStatus) error {
	if err := e.verify(bg, n); err != nil {
		class := e.cfg.Classifier.Classify(err)
		if terr := e.transition(bg, run, n, st, protocol.NodeFailed, protocol.ReasonNormal, transitionInfo{err: err, class: class}); terr != nil {
			return terr
		}
		e.auditNodeFailed(bg, run, n, class, err)
		return nodeError(n, class, err)
	}
	return e.transition(bg, run, n, st, protocol.NodeDone, protocol.ReasonNormal, transitionInfo{})
}

// dispatch is the whole of FR-EXEC-1: one switch, on kind, and nothing else.
// No branch reads the plan's operation, the node's id, or anything about why
// the node exists. The plan and the decision are passed through to execDDL for
// the ownership marker, which is content, not control flow.
func (e *Executor) dispatch(ctx context.Context, run RunID, plan *protocol.Plan, n *protocol.Node, d AuthorizationDecision) error {
	switch n.Kind {
	case protocol.KindCatalogAssert:
		return e.runAssertions(ctx, n)

	case protocol.KindIndexCreateParentInvalid,
		protocol.KindIndexCreateConcurrently,
		protocol.KindIndexAttach,
		protocol.KindIndexDropConcurrently,
		protocol.KindIndexReindexConcurrently,
		protocol.KindIndexDropPartitioned:
		return e.execDDL(ctx, run, plan, n, d)

	case protocol.KindWait:
		p, err := paramsOf[*protocol.WaitParams](n)
		if err != nil {
			return err
		}
		// ctx here is the uncancellable one: a wait is a node, and nodes are
		// not interrupted. Cancellation lands at the next boundary.
		return e.cfg.Clock.Sleep(ctx, time.Duration(p.Seconds)*time.Second)

	case protocol.KindIndexVerify:
		// The work is the verification, which runs in verifyPhase.
		return nil
	}
	return protocol.ErrUnknownNodeKind.Detailf("node %q: %q", n.ID, n.Kind)
}

// verify evaluates the node's post-conditions.
func (e *Executor) verify(ctx context.Context, n *protocol.Node) error {
	if n.Kind != protocol.KindIndexVerify {
		return nil
	}
	p, err := paramsOf[*protocol.VerifyParams](n)
	if err != nil {
		return err
	}
	results, err := e.cfg.Catalog.Verify(ctx, p.Checks)
	if err != nil {
		return err
	}
	if len(results) != len(p.Checks) {
		return protocol.ErrVerificationFailed.Detailf(
			"node %q: verifier returned %d results for %d checks", n.ID, len(results), len(p.Checks))
	}
	for i, r := range results {
		if r.Passed {
			continue
		}
		msg := r.Detail
		if msg == "" {
			msg = p.Checks[i].Message
		}
		return errorForExitCode(r.FailureCode).Detailf("node %q: check %q failed: %s", n.ID, p.Checks[i].Check, msg)
	}
	return nil
}

// runAssertions evaluates a catalog.assert node. A false predicate is terminal
// and carries the exit code the planner attached to it, which is how a
// HASH-partitioned target exits 15 and a non-member role exits 16 (AC-11,
// AC-12, AC-26).
func (e *Executor) runAssertions(ctx context.Context, n *protocol.Node) error {
	p, err := paramsOf[*protocol.CatalogAssertParams](n)
	if err != nil {
		return err
	}
	results, err := e.cfg.Catalog.Assert(ctx, p.Assertions)
	if err != nil {
		return err
	}
	if len(results) != len(p.Assertions) {
		return protocol.ErrVerificationFailed.Detailf(
			"node %q: evaluator returned %d results for %d assertions", n.ID, len(results), len(p.Assertions))
	}
	for i, r := range results {
		if r.Passed {
			continue
		}
		a := p.Assertions[i]
		code := r.FailureCode
		if code == 0 {
			code = a.FailureCode
		}
		msg := r.Detail
		if msg == "" {
			msg = a.Message
		}
		return errorForExitCode(code).Detailf("node %q: assertion %q failed: %s", n.ID, a.Assertion, msg)
	}
	return nil
}

// execDDL renders the statement from structured params and sends it under the
// session contract its kind requires, then writes the ownership marker onto the
// object the node acted on.
//
// # Ordering
//
// The marker is written *after* the primary statement returns, and that is the
// only order INV-1 permits: an object cannot be marked before it exists. The
// window between the two is covered by the node checkpoint, which is already
// durable ([protocol.Node.Object], state.ClaimsObject). A crash there leaves an
// object with a live claim and no marker, which `resume` adopts; the reverse
// can never occur.
//
// # Cost
//
// One extra catalog-only statement per created leaf and one per attach, each
// taking ShareUpdateExclusiveLock for about a millisecond (spike question 2).
// At 400 partitions that is roughly 800 statements against a build measured in
// hours. The plan Notes say so.
//
// A failed COMMENT fails the node. That is correct and convergent: the object
// exists but reads as unowned, so the next plan halts on it rather than
// destroying it, and `resume` adopts it under the claim this run still holds.
func (e *Executor) execDDL(ctx context.Context, run RunID, plan *protocol.Plan, n *protocol.Node, d AuthorizationDecision) error {
	sql, err := Render(n)
	if err != nil {
		return err
	}
	if sql == "" {
		return protocol.ErrInvalidPlan.Detailf("node %q of kind %q rendered an empty statement", n.ID, n.Kind)
	}

	// The adopt-then-drop row of the decision table: the object is ours by a
	// live claim alone, so it is marked before it is destroyed. The marker is
	// what makes the drop auditable after the fact, when the claim is gone.
	if d.Adopt {
		if err := e.adoptObject(ctx, run, plan, n, d.Object); err != nil {
			return err
		}
	}

	if err := e.cfg.SQL.Exec(ctx, Statement{
		RunID:                     run,
		NodeID:                    n.ID,
		Kind:                      n.Kind,
		SQL:                       sql,
		Settings:                  e.settingsFor(n.Kind),
		MustRunOutsideTransaction: n.Kind.MustRunOutsideTransaction(),
	}); err != nil {
		return err
	}
	return e.markObject(ctx, run, plan, n)
}

// markObject writes the node's ownership marker, if its kind writes one.
func (e *Executor) markObject(ctx context.Context, run RunID, plan *protocol.Plan, n *protocol.Node) error {
	target, ok, err := protocol.MarkerTargetFor(n)
	if err != nil || !ok {
		return err
	}

	// Whatever the object already carries is read first, for every marking
	// kind and not only the rewrite ones.
	//
	// Two things depend on it. A rewrite preserves the creation facts already
	// there, so a reindex does not erase who built the index. And every kind
	// refuses to overwrite a comment somebody else wrote
	// ([protocol.RenderMarkerStatement]) — which needs the read, or the refusal
	// is unreachable. index.attach is the kind that made this necessary: it
	// marks unconditionally as the crash-window backstop, and with the read
	// skipped it silently replaced a DBA's comment on an index it had merely
	// attached, minting permanent provenance over an object this run never
	// created.
	//
	// For the two create kinds the object was brought into existence by the
	// statement that just returned, so the read is a formality that costs one
	// catalog query and buys uniformity: markObject switches on nothing.
	if e.cfg.Catalog == nil {
		return ErrMissingPort.Detailf(
			"node %q writes the ownership marker on %s but no CatalogEvaluator is configured",
			n.ID, target.Index)
	}
	prior, status, err := e.cfg.Catalog.Marker(ctx, target.Index)
	if err != nil {
		return err
	}

	stmt, ok, err := protocol.RenderMarkerStatement(n, e.markerBase(run, plan), prior, status)
	if err != nil || !ok {
		return err
	}
	return e.execMarker(ctx, run, n, stmt)
}

// adoptObject writes the ownership marker onto an object PartitionCTL is about
// to drop under a live claim, before the drop.
func (e *Executor) adoptObject(ctx context.Context, run RunID, plan *protocol.Plan, n *protocol.Node, object protocol.ObjectName) error {
	m := e.markerBase(run, plan)
	// index.drop_concurrently is only ever emitted for an unattached ordinary
	// index on a leaf (TRD §7.2.10), so the role is not in doubt.
	m.Role = protocol.MarkerRoleLeaf
	text, err := protocol.FormatMarker(m)
	if err != nil {
		return err
	}
	return e.execMarker(ctx, run, n, protocol.RenderComment(object, text))
}

// markerBase is the run-level half of every marker this run writes.
func (e *Executor) markerBase(run RunID, plan *protocol.Plan) protocol.Marker {
	m := protocol.Marker{Run: string(run), At: protocol.MarkerTime(e.cfg.Clock.Now())}
	if plan != nil {
		m.Plan = plan.Digest
		m.Op = string(plan.Operation)
	}
	return m
}

// execMarker sends a COMMENT ON INDEX.
//
// It is not a CONCURRENTLY form and does not wait on application transactions,
// so it takes the short lock_timeout rather than the build one, and it accepts
// the configured statement_timeout. It is transactional, so it carries no
// out-of-transaction requirement.
func (e *Executor) execMarker(ctx context.Context, run RunID, n *protocol.Node, sql string) error {
	return e.cfg.SQL.Exec(ctx, Statement{
		RunID:  run,
		NodeID: n.ID,
		Kind:   n.Kind,
		SQL:    sql,
		Settings: SessionSettings{
			LockTimeout:      e.cfg.LockTimeout,
			StatementTimeout: e.cfg.StatementTimeout,
		},
	})
}

// settingsFor builds the session contract for a kind (FR-EXEC-5).
//
// lock_timeout is always finite, but not always the same number: the
// CONCURRENTLY kinds wait for the application's own transactions as part of
// their work, so the short bound that keeps ordinary DDL out of the way of
// application traffic would abort them mid-build. statement_timeout is forced
// off for the kinds that legitimately run unbounded, whatever the configuration
// says.
func (e *Executor) settingsFor(k protocol.NodeKind) SessionSettings {
	lock := e.cfg.LockTimeout
	if k.WaitsForConcurrentTransactions() {
		lock = e.cfg.BuildLockTimeout
	}
	s := SessionSettings{LockTimeout: lock}
	if k.AllowsStatementTimeout() {
		s.StatementTimeout = e.cfg.StatementTimeout
	}
	return s
}

// authorizeNode re-evaluates a destructive node and records the verdict. Both
// the authorization record and its audit event are written before the statement
// runs (FR-AUTH-6, INV-2, AC-20).
func (e *Executor) authorizeNode(ctx context.Context, run RunID, plan *protocol.Plan, n *protocol.Node) (AuthorizationDecision, error) {
	d, err := Authorize(ctx, e.cfg.Store, e.cfg.Catalog, plan, n)
	if err != nil {
		return d, ErrCheckpointFailed.Detailf("evaluating authorization for node %q", n.ID).Wrap(err)
	}
	if !d.Satisfied {
		_ = e.audit(ctx, AuditEvent{
			RunID:  run,
			NodeID: n.ID,
			Type:   AuditAuthorizationDenied,
			Detail: map[string]string{
				"kind":   string(n.Kind),
				"mode":   string(d.Mode),
				"object": d.Object.String(),
				"reason": d.Reason,
			},
		})
		return d, protocol.ErrAuthorizationUnsatisfied.Detailf(
			"node %q (%s) would destroy %s under mode %q: %s",
			n.ID, n.Kind, d.Object, d.Mode, d.Reason)
	}
	// FR-CLI-9: adoption is `resume`'s alone. The object exists, carries no
	// ownership marker, and is ours only because a run that died still names
	// it. That is a recovery decision, not an ordinary one.
	if d.Adopt && !e.cfg.AllowAdoption {
		_ = e.audit(ctx, AuditEvent{
			RunID:  run,
			NodeID: n.ID,
			Type:   AuditAuthorizationDenied,
			Detail: map[string]string{
				"kind":   string(n.Kind),
				"mode":   string(d.Mode),
				"object": d.Object.String(),
				"reason": "adoption is reserved to resume (FR-CLI-9)",
			},
		})
		return d, protocol.ErrAuthorizationUnsatisfied.Detailf(
			"node %q would drop %s, which carries no PartitionCTL ownership marker and is claimed only "+
				"by the in-flight run %q. Cleaning up after an interrupted run is `resume`'s job, not "+
				"`execute`'s: run `partitionctl resume` against this target (FR-CLI-9)",
			n.ID, d.Object, d.Evidence["claim_run"])
	}
	if err := e.cfg.Store.RecordAuthorization(ctx, AuthorizationRecord{
		RunID:     run,
		NodeID:    n.ID,
		Object:    d.Object,
		Mode:      d.Mode,
		Evidence:  d.Evidence,
		GrantedAt: e.cfg.Clock.Now(),
	}); err != nil {
		return d, ErrCheckpointFailed.Detailf("recording authorization for node %q", n.ID).Wrap(err)
	}
	detail := map[string]string{"kind": string(n.Kind)}
	for k, v := range d.Evidence {
		detail[k] = v
	}
	return d, e.audit(ctx, AuditEvent{RunID: run, NodeID: n.ID, Type: AuditAuthorizationGranted, Detail: detail})
}

// transitionInfo carries the optional detail of one checkpoint.
type transitionInfo struct {
	duration time.Duration
	err      error
	class    Classification
	message  string
}

// transition checkpoints one node state change and only then advances the
// executor's own view of the node (FR-EXEC-2). A store failure halts the run:
// an unrecorded transition means the executor cannot prove where it is, and
// proceeding would create exactly the unrecoverable intermediate NFR-REL-2
// forbids.
func (e *Executor) transition(ctx context.Context, run RunID, n *protocol.Node, st *nodeStatus, to protocol.NodeState, reason protocol.TransitionReason, info transitionInfo) error {
	from := st.State
	if err := protocol.CheckTransition(from, to, reason); err != nil {
		return err
	}
	now := e.cfg.Clock.Now()
	t := Transition{
		RunID:    run,
		NodeID:   n.ID,
		Kind:     n.Kind,
		From:     from,
		To:       to,
		Reason:   reason,
		Attempts: st.Attempts,
		Duration: info.duration,
		At:       now,
	}
	if info.err != nil {
		t.Error = info.err.Error()
		t.ErrorKind = protocol.KindOf(info.err)
	}
	if err := e.cfg.Store.RecordTransition(ctx, t); err != nil {
		return ErrCheckpointFailed.Detailf(
			"node %q: %s -> %s was not recorded, so the run cannot safely continue", n.ID, from, to).Wrap(err)
	}
	st.State = to

	ev := LogEvent{
		TS:         now.UTC().Format(timeLayout),
		Event:      "node_transition",
		RunID:      run,
		NodeID:     n.ID,
		Kind:       n.Kind,
		State:      to,
		PrevState:  from,
		Attempt:    st.Attempts,
		DurationMS: info.duration.Milliseconds(),
		RetryClass: info.class.Class,
		SQLState:   info.class.SQLState,
		Message:    info.message,
	}
	if info.err != nil {
		ev.Error = info.err.Error()
		ev.ErrorClass = protocol.KindOf(info.err)
	}
	e.cfg.Logger.Log(ev)
	return nil
}

// audit appends one event to the append-only trail (INV-3). A failure halts the
// run: the trail is the artifact a compliance reviewer reads, and silently
// dropping an entry would make it a lie.
func (e *Executor) audit(ctx context.Context, ev AuditEvent) error {
	if ev.At.IsZero() {
		ev.At = e.cfg.Clock.Now()
	}
	if err := e.cfg.Store.AppendAudit(ctx, ev); err != nil {
		return ErrCheckpointFailed.Detailf("appending audit event %q", ev.Type).Wrap(err)
	}
	return nil
}

// auditNodeFailed is best effort: the run is already failing, and losing the
// event must not mask the failure that caused it.
func (e *Executor) auditNodeFailed(ctx context.Context, run RunID, n *protocol.Node, class Classification, cause error) {
	_ = e.audit(ctx, AuditEvent{
		RunID:  run,
		NodeID: n.ID,
		Type:   AuditNodeFailed,
		Detail: map[string]string{
			"kind":        string(n.Kind),
			"retry_class": string(class.Class),
			"sqlstate":    class.SQLState,
			"error":       cause.Error(),
		},
	})
}

// auditRetryDeferred records a retryable failure the executor deliberately did
// not retry, naming the SQLSTATE that caused it so the operator sees the real
// diagnosis rather than the 42P07 a blind retry would have produced.
func (e *Executor) auditRetryDeferred(ctx context.Context, run RunID, n *protocol.Node, class Classification, cause error) {
	_ = e.audit(ctx, AuditEvent{
		RunID:  run,
		NodeID: n.ID,
		Type:   AuditNodeFailed,
		Detail: map[string]string{
			"kind":        string(n.Kind),
			"retry_class": string(class.Class),
			"sqlstate":    class.SQLState,
			"error":       cause.Error(),
			"deferred_to": "resume",
			"reason": "the statement may have committed catalog state before it failed, " +
				"so re-issuing it in process cannot succeed; resume cleans up and rebuilds",
		},
	})
}

// stop finalizes a run that was cancelled at a node boundary. Everything not
// yet complete stays exactly where it is, which is what keeps the run resumable
// (AC-24).
func (e *Executor) stop(ctx context.Context, res *Result, states map[protocol.NodeID]*nodeStatus, reason string) {
	res.Cancelled = true
	res.CancelReason = reason
	tally(res, states)
	_ = e.audit(ctx, AuditEvent{
		RunID:  res.RunID,
		Type:   AuditRunCancelled,
		Detail: map[string]string{"reason": reason, "remaining": strconv.Itoa(res.Remaining)},
	})
}

// finish tallies the result and records the closing audit event.
func (e *Executor) finish(ctx context.Context, res *Result, states map[protocol.NodeID]*nodeStatus, cause error) {
	tally(res, states)
	ev := AuditEvent{
		RunID: res.RunID,
		Type:  AuditRunFinished,
		Detail: map[string]string{
			"done":      strconv.Itoa(res.Done),
			"skipped":   strconv.Itoa(res.Skipped),
			"failed":    strconv.Itoa(res.Failed),
			"remaining": strconv.Itoa(res.Remaining),
		},
	}
	if cause != nil {
		ev.Type = AuditRunFailed
		ev.NodeID = res.HaltedAt
		ev.Detail["error"] = cause.Error()
		ev.Detail["error_class"] = string(protocol.KindOf(cause))
		ev.Detail["exit_code"] = protocol.ExitCodeFor(cause).String()
	}
	_ = e.audit(ctx, ev)
}

func tally(res *Result, states map[protocol.NodeID]*nodeStatus) {
	res.Done, res.Skipped, res.Failed, res.Remaining = 0, 0, 0, 0
	for _, st := range states {
		switch st.State {
		case protocol.NodeDone:
			res.Done++
		case protocol.NodeSkipped:
			res.Skipped++
		case protocol.NodeFailed:
			res.Failed++
			res.Remaining++
		default:
			res.Remaining++
		}
	}
}

// nodeError wraps a dispatch failure so it carries the contract exit code. A
// cause that is already a typed protocol error keeps its own code, which is how
// a failed assertion's exit 15 survives back to the CLI.
func nodeError(n *protocol.Node, class Classification, cause error) *protocol.Error {
	var pe *protocol.Error
	if errors.As(cause, &pe) {
		return pe.Detailf("node %q (%s)", n.ID, n.Kind)
	}
	base := errorForExitCode(class.ExitCode)
	if class.ExitCode == 0 {
		base = protocol.ErrFailure
	}
	detail := "node %q (%s) failed"
	args := []any{n.ID, n.Kind}
	if class.SQLState != "" {
		detail += " with SQLSTATE %s (%s)"
		args = append(args, class.SQLState, class.Condition)
	}
	return base.Detailf(detail, args...).Wrap(cause)
}

// startHeartbeat renews the run lease on its own goroutine while the executor
// runs (FR-LOCK-3). A six-hour CREATE INDEX CONCURRENTLY is one node, so a
// heartbeat driven by the dispatch loop would let the lease expire mid-build
// and make a live run look orphaned (INV-4).
func (e *Executor) startHeartbeat(ctx context.Context, run RunID, fence func()) func() {
	if e.cfg.Heartbeat == nil || e.cfg.HeartbeatInterval <= 0 {
		return func() {}
	}
	hbCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(e.cfg.HeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				err := e.cfg.Heartbeat.Heartbeat(hbCtx, run)
				if err == nil {
					continue
				}
				// Losing the lease is not a warning. Another process has taken
				// this target, and it may already have adopted this run as
				// orphaned, so continuing would put two executors on one
				// partitioned table (FR-LOCK-1, AC-10). Fencing stops dispatch
				// at the next node boundary; it never interrupts a statement
				// that is already in flight.
				if errors.Is(err, ErrFenced) {
					e.cfg.Logger.Log(LogEvent{
						TS:      e.cfg.Clock.Now().UTC().Format(timeLayout),
						Event:   "fenced",
						RunID:   run,
						Error:   err.Error(),
						Message: "another process holds this run's lease or advisory lock; stopping at the next node boundary",
					})
					fence()
					return
				}
				e.cfg.Logger.Log(LogEvent{
					TS:      e.cfg.Clock.Now().UTC().Format(timeLayout),
					Event:   "heartbeat_failed",
					RunID:   run,
					Error:   err.Error(),
					Message: "the run lease may expire and the run may be adopted as orphaned",
				})
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
