package executor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// recorder is the single ordered event log every fake writes to. Requirements
// like "checkpoint before proceeding" and "record authorization before the
// statement" are statements about interleaving, so one shared log is the only
// honest way to assert them.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, fmt.Sprintf(format, args...))
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

// withPrefix returns the events whose name starts with prefix, in order.
func (r *recorder) withPrefix(prefix string) []string {
	var out []string
	for _, e := range r.all() {
		if strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

// indexOf returns the position of the first event equal to want, or -1.
func (r *recorder) indexOf(want string) int {
	for i, e := range r.all() {
		if e == want {
			return i
		}
	}
	return -1
}

// mustPrecede fails unless both events occurred and first came before second.
func (r *recorder) mustPrecede(t *testing.T, first, second string) {
	t.Helper()
	i, j := r.indexOf(first), r.indexOf(second)
	if i < 0 {
		t.Fatalf("event %q never happened; log: %v", first, r.all())
	}
	if j < 0 {
		t.Fatalf("event %q never happened; log: %v", second, r.all())
	}
	if i >= j {
		t.Fatalf("expected %q before %q, got positions %d and %d; log: %v", first, second, i, j, r.all())
	}
}

// ---------------------------------------------------------------------------
// StateStore fake
// ---------------------------------------------------------------------------

type fakeStore struct {
	rec *recorder

	mu          sync.Mutex
	states      map[protocol.NodeID]NodeRecord
	transitions []Transition
	claims      map[string]RunID
	authz       []AuthorizationRecord
	audits      []AuditEvent

	// cancelAfter makes CancelRequested return true from the nth call onward.
	// Zero never cancels.
	cancelAfter int
	cancelCalls int

	// Failure injection.
	failStates error
	failCancel error
	failClaims error
	failAuthz  error
	failAudit  error
	// failTransition returns an error for a specific edge, so a test can prove
	// the executor stops rather than proceeding on an unrecorded checkpoint.
	failTransition func(Transition) error
}

func newFakeStore(rec *recorder) *fakeStore {
	return &fakeStore{
		rec:    rec,
		states: map[protocol.NodeID]NodeRecord{},
		claims: map[string]RunID{},
	}
}

func (s *fakeStore) NodeStates(_ context.Context, run RunID) (map[protocol.NodeID]NodeRecord, error) {
	if s.failStates != nil {
		return nil, s.failStates
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[protocol.NodeID]NodeRecord, len(s.states))
	for k, v := range s.states {
		out[k] = v
	}
	return out, nil
}

func (s *fakeStore) RecordTransition(_ context.Context, t Transition) error {
	if s.failTransition != nil {
		if err := s.failTransition(t); err != nil {
			s.rec.add("transition_rejected:%s:%s->%s", t.NodeID, t.From, t.To)
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.transitions = append(s.transitions, t)
	s.states[t.NodeID] = NodeRecord{
		NodeID:    t.NodeID,
		State:     t.To,
		Attempts:  t.Attempts,
		LastError: t.Error,
		UpdatedAt: t.At,
	}
	s.rec.add("transition:%s:%s->%s", t.NodeID, t.From, t.To)
	return nil
}

func (s *fakeStore) CancelRequested(_ context.Context, run RunID) (bool, error) {
	if s.failCancel != nil {
		return false, s.failCancel
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelCalls++
	s.rec.add("cancel_check:%d", s.cancelCalls)
	return s.cancelAfter > 0 && s.cancelCalls >= s.cancelAfter, nil
}

func (s *fakeStore) ClaimsObject(_ context.Context, object protocol.ObjectName) (RunID, bool, error) {
	if s.failClaims != nil {
		return "", false, s.failClaims
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.add("claim_check:%s", object)
	run, ok := s.claims[object.String()]
	return run, ok, nil
}

// claim records that some run holds a live claim on object, standing in for a
// node checkpoint left behind by a process that died mid-statement.
func (s *fakeStore) claim(object protocol.ObjectName, run RunID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims[object.String()] = run
}

func (s *fakeStore) RecordAuthorization(_ context.Context, a AuthorizationRecord) error {
	if s.failAuthz != nil {
		return s.failAuthz
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authz = append(s.authz, a)
	s.rec.add("authorization:%s:%s:%s", a.NodeID, a.Mode, a.Object)
	return nil
}

func (s *fakeStore) AppendAudit(_ context.Context, e AuditEvent) error {
	if s.failAudit != nil {
		return s.failAudit
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audits = append(s.audits, e)
	s.rec.add("audit:%s:%s", e.Type, e.NodeID)
	return nil
}

// seed presets a node's stored state, standing in for a previous run.
func (s *fakeStore) seed(id protocol.NodeID, state protocol.NodeState, attempts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[id] = NodeRecord{NodeID: id, State: state, Attempts: attempts}
}

func (s *fakeStore) stateOf(id protocol.NodeID) protocol.NodeState {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.states[id]
	if !ok {
		return protocol.InitialNodeState()
	}
	return r.State
}

func (s *fakeStore) auditTypes() []AuditEventType {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AuditEventType, 0, len(s.audits))
	for _, a := range s.audits {
		out = append(out, a.Type)
	}
	return out
}

// ---------------------------------------------------------------------------
// SQLExecutor fake
// ---------------------------------------------------------------------------

type fakeSQL struct {
	rec *recorder

	mu    sync.Mutex
	stmts []Statement
	// errs is a per-node queue of results; a shorter queue than the number of
	// calls means the remaining calls succeed.
	errs map[protocol.NodeID][]error
	// hook runs before the queued result is consulted.
	hook func(ctx context.Context, stmt Statement) error
	// ctxErrAt records ctx.Err() as observed inside each Exec, which is how the
	// "never interrupt an in-flight statement" test proves its point.
	ctxErrAt map[protocol.NodeID]error
}

func newFakeSQL(rec *recorder) *fakeSQL {
	return &fakeSQL{rec: rec, errs: map[protocol.NodeID][]error{}, ctxErrAt: map[protocol.NodeID]error{}}
}

func (x *fakeSQL) Exec(ctx context.Context, stmt Statement) error {
	x.mu.Lock()
	x.stmts = append(x.stmts, stmt)
	x.mu.Unlock()
	// The ownership marker is a second statement inside the same node, so it
	// gets its own event name: every ordering assertion in this package is
	// about the primary statement, and conflating the two would make "exec"
	// mean two different things.
	if strings.HasPrefix(stmt.SQL, "COMMENT ON INDEX ") {
		x.rec.add("mark:%s", stmt.NodeID)
	} else {
		x.rec.add("exec:%s", stmt.NodeID)
	}

	if x.hook != nil {
		if err := x.hook(ctx, stmt); err != nil {
			return err
		}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.ctxErrAt[stmt.NodeID] = ctx.Err()
	q := x.errs[stmt.NodeID]
	if len(q) == 0 {
		return nil
	}
	err := q[0]
	x.errs[stmt.NodeID] = q[1:]
	return err
}

// execCount counts primary statements. Ownership markers are counted by
// markCount, because every "did any DDL run?" assertion in this package is
// about the statement that changes the catalog's shape.
func (x *fakeSQL) execCount() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.execCountLocked()
}

func (x *fakeSQL) markCount() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.stmts) - x.execCountLocked()
}

func (x *fakeSQL) execCountLocked() int {
	n := 0
	for _, s := range x.stmts {
		if !strings.HasPrefix(s.SQL, "COMMENT ON INDEX ") {
			n++
		}
	}
	return n
}

// statementsFor returns every statement a node issued, in order. A node that
// creates an object issues two: its DDL and then the ownership marker.
func (x *fakeSQL) statementsFor(id protocol.NodeID) []Statement {
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []Statement
	for _, s := range x.stmts {
		if s.NodeID == id {
			out = append(out, s)
		}
	}
	return out
}

func (x *fakeSQL) statementFor(id protocol.NodeID) (Statement, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, s := range x.stmts {
		if s.NodeID == id {
			return s, true
		}
	}
	return Statement{}, false
}

// ---------------------------------------------------------------------------
// CatalogEvaluator fake
// ---------------------------------------------------------------------------

type fakeCatalog struct {
	rec *recorder

	assertFn func([]protocol.Assertion) ([]CheckResult, error)
	verifyFn func([]protocol.VerifyCheck) ([]CheckResult, error)

	mu       sync.Mutex
	comments map[string]string
	markerFn func(protocol.ObjectName) (protocol.Marker, protocol.MarkerStatus, error)
}

func newFakeCatalog(rec *recorder) *fakeCatalog {
	return &fakeCatalog{rec: rec, comments: map[string]string{}}
}

// Marker implements [CatalogEvaluator]. The fake stores raw comment text and
// classifies it through the real parser, so a test cannot accidentally assert
// against a status the parser would never produce.
func (c *fakeCatalog) Marker(_ context.Context, object protocol.ObjectName) (protocol.Marker, protocol.MarkerStatus, error) {
	c.rec.add("marker_read:%s", object)
	if c.markerFn != nil {
		return c.markerFn(object)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	m, status := protocol.ParseMarker(c.comments[object.String()])
	return m, status, nil
}

// setComment gives an object a comment, ours or otherwise.
func (c *fakeCatalog) setComment(object protocol.ObjectName, comment string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.comments[object.String()] = comment
}

// mark gives an object a well-formed PartitionCTL marker.
func (c *fakeCatalog) mark(t *testing.T, object protocol.ObjectName, run string) {
	t.Helper()
	text, err := protocol.FormatMarker(protocol.Marker{
		Run: run, Plan: "sha256:test", Op: string(protocol.OpCreateIndex),
		Role: protocol.MarkerRoleLeaf, At: "2026-08-07T12:00:00Z",
	})
	if err != nil {
		t.Fatalf("FormatMarker: %v", err)
	}
	c.setComment(object, text)
}

func (c *fakeCatalog) Assert(_ context.Context, as []protocol.Assertion) ([]CheckResult, error) {
	c.rec.add("assert:%d", len(as))
	if c.assertFn != nil {
		return c.assertFn(as)
	}
	out := make([]CheckResult, len(as))
	for i, a := range as {
		out[i] = CheckResult{Name: string(a.Assertion), Passed: true}
	}
	return out, nil
}

func (c *fakeCatalog) Verify(_ context.Context, cs []protocol.VerifyCheck) ([]CheckResult, error) {
	c.rec.add("verify:%d", len(cs))
	if c.verifyFn != nil {
		return c.verifyFn(cs)
	}
	out := make([]CheckResult, len(cs))
	for i, ck := range cs {
		out[i] = CheckResult{Name: string(ck.Check), Passed: true}
	}
	return out, nil
}

// parseMarkerLiteral pulls the marker back out of a rendered COMMENT statement
// and classifies it with the real parser, so a test asserts what a server would
// have stored rather than what the renderer meant.
func parseMarkerLiteral(t *testing.T, stmt string) (protocol.Marker, protocol.MarkerStatus) {
	t.Helper()
	i := strings.Index(stmt, " IS '")
	if i < 0 || !strings.HasSuffix(stmt, "'") {
		t.Fatalf("not a COMMENT statement: %s", stmt)
	}
	literal := stmt[i+len(" IS '") : len(stmt)-1]
	return protocol.ParseMarker(strings.ReplaceAll(literal, "''", "'"))
}

// ---------------------------------------------------------------------------
// Clock fake
// ---------------------------------------------------------------------------

// fakeClock never actually sleeps, so the retry and pacing tests run in
// microseconds and assert on the durations that were requested.
type fakeClock struct {
	rec *recorder

	mu      sync.Mutex
	now     time.Time
	sleeps  []time.Duration
	onSleep func(d time.Duration) error
}

func newFakeClock(rec *recorder) *fakeClock {
	return &fakeClock{rec: rec, now: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}
}

// Now advances a millisecond per call, so a dispatch has a measurable duration
// without any real time passing.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Millisecond)
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	hook := c.onSleep
	c.mu.Unlock()
	c.rec.add("sleep:%s", d)
	if hook != nil {
		return hook(d)
	}
	return ctx.Err()
}

func (c *fakeClock) sleptFor() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, len(c.sleeps))
	copy(out, c.sleeps)
	return out
}

// ---------------------------------------------------------------------------
// Heartbeater fake
// ---------------------------------------------------------------------------

type fakeHeartbeat struct {
	beats chan struct{}

	mu sync.Mutex
	// err, once set, is returned by every subsequent beat. It is how a test
	// simulates another process taking the lease.
	err error
}

func (h *fakeHeartbeat) fail(err error) {
	h.mu.Lock()
	h.err = err
	h.mu.Unlock()
}

func (h *fakeHeartbeat) Heartbeat(context.Context, RunID) error {
	select {
	case h.beats <- struct{}{}:
	default:
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// ---------------------------------------------------------------------------
// Errors carrying a SQLSTATE
// ---------------------------------------------------------------------------

// pgErr stands in for a driver error implementing [SQLStateError], which is how
// pgx and any well-behaved driver surface a PostgreSQL condition.
type pgErr struct {
	code string
	msg  string
}

func (e *pgErr) Error() string    { return e.code + ": " + e.msg }
func (e *pgErr) SQLState() string { return e.code }

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	rec     *recorder
	store   *fakeStore
	sql     *fakeSQL
	catalog *fakeCatalog
	clock   *fakeClock
	cfg     Config
}

// newHarness wires every fake together with a deterministic jitter draw, so
// backoff durations in a test are exact rather than approximate.
func newHarness() *harness {
	rec := &recorder{}
	h := &harness{
		rec:     rec,
		store:   newFakeStore(rec),
		sql:     newFakeSQL(rec),
		catalog: newFakeCatalog(rec),
		clock:   newFakeClock(rec),
	}
	h.cfg = Config{
		Store:       h.store,
		SQL:         h.sql,
		Catalog:     h.catalog,
		Clock:       h.clock,
		LockTimeout: 3 * time.Second,
		Retry: RetryPolicy{
			MaxAttempts: 3,
			BaseDelay:   100 * time.Millisecond,
			MaxDelay:    time.Second,
			Jitter:      0, // deterministic unless a test opts in
		},
		Jitter: func() float64 { return 0 },
	}
	return h
}

func (h *harness) executor(t *testing.T) *Executor {
	t.Helper()
	e, err := New(h.cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func (h *harness) run(t *testing.T, plan *protocol.Plan) (*Result, error) {
	t.Helper()
	return h.executor(t).Run(context.Background(), "run-1", plan)
}

// ---------------------------------------------------------------------------
// Plan fixtures
// ---------------------------------------------------------------------------

func obj(t *testing.T, s string) protocol.ObjectName {
	t.Helper()
	o, err := protocol.ParseObjectName(s)
	if err != nil {
		t.Fatalf("ParseObjectName(%q): %v", s, err)
	}
	return o
}

func objPtr(t *testing.T, s string) *protocol.ObjectName {
	t.Helper()
	o := obj(t, s)
	return &o
}

func indexDef() protocol.IndexDefinition {
	return protocol.IndexDefinition{
		Method:  "btree",
		Columns: []protocol.IndexColumn{{Name: "created_at"}},
	}
}

func node(id string, kind protocol.NodeKind, params protocol.NodeParams, deps ...string) protocol.Node {
	n := protocol.Node{ID: protocol.NodeID(id), Kind: kind, Params: params}
	for _, d := range deps {
		n.DependsOn = append(n.DependsOn, protocol.NodeID(d))
	}
	return n
}

// newPlan builds and seals a valid plan around the given nodes.
func newPlan(t *testing.T, nodes ...protocol.Node) *protocol.Plan {
	t.Helper()
	p := &protocol.Plan{
		FormatVersion: protocol.PlanFormatVersion,
		PlanID:        "plan-1",
		Operation:     protocol.OpCreateIndex,
		Target: protocol.Target{
			Database: "appdb",
			Table:    obj(t, "public.orders"),
			Index:    objPtr(t, "public.orders_created_at_idx"),
		},
		CreatedAt: protocol.NewTimestamp(time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)),
		Nodes:     nodes,
		// FR-PLANFILE-4: Validate requires a fingerprint.
		TopologyFingerprint: protocol.FingerprintPrefix +
			"0000000000000000000000000000000000000000000000000000000000000000",
	}
	if err := p.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("fixture plan is invalid: %v", err)
	}
	return p
}

// createChainPlan is the CreatePartitionedIndex shape from TRD §7.2.13, one
// leaf partition, exercising all seven supported kinds.
func createChainPlan(t *testing.T) *protocol.Plan {
	t.Helper()
	return newPlan(t,
		node("n1", protocol.KindCatalogAssert, &protocol.CatalogAssertParams{
			Assertions: []protocol.Assertion{{
				Assertion:   protocol.AssertRelationIsPartitioned,
				Relation:    objPtr(t, "public.orders"),
				FailureCode: protocol.ExitUnsupportedTopology,
			}},
		}),
		node("n2", protocol.KindIndexCreateParentInvalid, &protocol.CreateParentInvalidParams{
			Parent:     obj(t, "public.orders"),
			Index:      obj(t, "public.orders_created_at_idx"),
			Definition: indexDef(),
		}, "n1"),
		node("n3", protocol.KindIndexCreateConcurrently, &protocol.CreateConcurrentlyParams{
			Partition:   obj(t, "public.orders_2026_03"),
			Index:       obj(t, "public.orders_created_at_idx_orders_2026_03"),
			Definition:  indexDef(),
			ParentIndex: objPtr(t, "public.orders_created_at_idx"),
		}, "n2"),
		node("n4", protocol.KindIndexVerify, &protocol.VerifyParams{
			Checks: []protocol.VerifyCheck{{
				Check: protocol.CheckIndexValid,
				Index: objPtr(t, "public.orders_created_at_idx_orders_2026_03"),
			}},
		}, "n3"),
		node("n5", protocol.KindIndexAttach, &protocol.AttachParams{
			ParentIndex: obj(t, "public.orders_created_at_idx"),
			ChildIndex:  obj(t, "public.orders_created_at_idx_orders_2026_03"),
		}, "n4"),
		node("n6", protocol.KindWait, &protocol.WaitParams{Seconds: 2, Reason: "pacing"}, "n5"),
		node("n7", protocol.KindIndexVerify, &protocol.VerifyParams{
			Checks: []protocol.VerifyCheck{{
				Check:       protocol.CheckParentIndexValid,
				ParentIndex: objPtr(t, "public.orders_created_at_idx"),
			}},
		}, "n6"),
	)
}

// dropPartitionedPlan is a valid DropPartitionedIndex plan: one node, the
// acknowledgement recorded in the artifact (INV-8, FR-DROP-3, AC-13). This
// build cannot execute it, which is what the test asserts.
func dropPartitionedPlan(t *testing.T) *protocol.Plan {
	t.Helper()
	n := node("n1", protocol.KindIndexDropPartitioned, &protocol.DropPartitionedParams{
		Parent:    obj(t, "public.orders"),
		Index:     obj(t, "public.orders_created_at_idx"),
		LeafCount: 400,
	})
	n.Authorization = &protocol.Authorization{
		Mode:                 protocol.AuthExplicit,
		Object:               obj(t, "public.orders_created_at_idx"),
		RequiredConfirmation: protocol.ConfirmExclusiveLock,
	}
	p := &protocol.Plan{
		FormatVersion: protocol.PlanFormatVersion,
		PlanID:        "plan-drop",
		Operation:     protocol.OpDropIndex,
		Target: protocol.Target{
			Database: "appdb",
			Table:    obj(t, "public.orders"),
			Index:    objPtr(t, "public.orders_created_at_idx"),
		},
		CreatedAt: protocol.NewTimestamp(time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)),
		Confirmations: []protocol.Confirmation{{
			Flag:  protocol.ConfirmExclusiveLock,
			Actor: "operator",
			At:    protocol.NewTimestamp(time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)),
		}},
		Nodes: []protocol.Node{n},
		// FR-PLANFILE-4: Validate requires a fingerprint.
		TopologyFingerprint: protocol.FingerprintPrefix +
			"0000000000000000000000000000000000000000000000000000000000000000",
	}
	if err := p.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("fixture plan is invalid: %v", err)
	}
	return p
}

// dropNode is a destructive cleanup node with the given authorization.
func dropNode(t *testing.T, id string, index string, auth *protocol.Authorization, deps ...string) protocol.Node {
	t.Helper()
	n := node(id, protocol.KindIndexDropConcurrently, &protocol.DropConcurrentlyParams{
		Index:    obj(t, index),
		Relation: objPtr(t, "public.orders_2026_03"),
		Reason:   protocol.DropInvalidBuild,
	}, deps...)
	n.Authorization = auth
	return n
}
