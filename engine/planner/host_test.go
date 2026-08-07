package planner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// stubPlanner is a minimal OperationPlanner. It emits the smallest legal graph
// for the operation it is asked about, so host_test can exercise the host
// without depending on operations/, which this package does not own.
type stubPlanner struct {
	op      protocol.Operation
	err     error
	calls   int
	seen    Request
	emit    func(req Request) []protocol.Node
	notes   []string
	noNodes bool
}

func (s *stubPlanner) Operation() protocol.Operation { return s.op }

func (s *stubPlanner) Plan(ctx context.Context, req Request) (Result, error) {
	s.calls++
	s.seen = req
	if s.err != nil {
		return Result{}, s.err
	}
	if s.noNodes {
		return Result{}, nil
	}
	if s.emit != nil {
		return Result{Nodes: s.emit(req), Notes: s.notes}, nil
	}
	return Result{Nodes: defaultNodes(req), Notes: s.notes}, nil
}

// defaultNodes emits the create-index shape in miniature: an assertion, the ON
// ONLY parent index, then one build/verify/attach chain per leaf.
func defaultNodes(req Request) []protocol.Node {
	parent := req.Spec.Index
	nodes := []protocol.Node{
		{
			ID:   "assert",
			Kind: protocol.KindCatalogAssert,
			Params: &protocol.CatalogAssertParams{Assertions: []protocol.Assertion{{
				Assertion:   protocol.AssertPartitionDepth,
				Relation:    &req.Spec.Table,
				Expected:    []string{"1"},
				FailureCode: protocol.ExitUnsupportedTopology,
			}}},
			EstimatedSeconds: req.Estimator.CatalogNodeSeconds(),
		},
		{
			ID:        "parent",
			Kind:      protocol.KindIndexCreateParentInvalid,
			DependsOn: []protocol.NodeID{"assert"},
			Params: &protocol.CreateParentInvalidParams{
				Parent:     req.Topology.Root.Name,
				Index:      parent,
				Definition: req.Spec.Definition,
			},
			EstimatedSeconds: req.Estimator.CatalogNodeSeconds(),
		},
	}
	for i, leaf := range req.Topology.Leaves {
		child := protocol.NewObjectName(leaf.Name.Schema,
			protocol.ChildIndexName(parent.Name, leaf.Name.Name))
		id := protocol.NodeID("build-" + leaf.Name.Name)
		nodes = append(nodes, protocol.Node{
			ID:        id,
			Kind:      protocol.KindIndexCreateConcurrently,
			DependsOn: []protocol.NodeID{"parent"},
			Params: &protocol.CreateConcurrentlyParams{
				Partition:   leaf.Name,
				Index:       child,
				Definition:  req.Spec.Definition,
				ParentIndex: &parent,
			},
			EstimatedSeconds: req.Estimator.BuildSeconds(req.Topology.Leaves[i].RelPages),
		})
	}
	return nodes
}

func createSpec() Specification {
	return Specification{
		Operation: protocol.OpCreateIndex,
		Table:     name("public", "orders"),
		Index:     name("public", parentIndexName),
		Definition: protocol.IndexDefinition{
			Method:  "btree",
			Columns: []protocol.IndexColumn{{Name: "created_at"}},
		},
		Actor: "platform",
	}
}

func testHost(f *FakeCatalog) *Host {
	return &Host{
		Catalog:   f,
		Now:       func() time.Time { return time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC) },
		NewPlanID: func() protocol.PlanID { return "plan-fixed" },
	}
}

// TestHostRunProducesASealedPlan is the happy path: the artifact comes back
// validated, fingerprinted and sealed (FR-PLANFILE-1..4, FR-PLANFILE-8).
func TestHostRunProducesASealedPlan(t *testing.T) {
	f := standardTree()
	h := testHost(f)
	op := &stubPlanner{op: protocol.OpCreateIndex, notes: []string{"3 leaves remain"}}

	out, err := h.Run(ctx(), op, createSpec())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	p := out.Plan
	if p.FormatVersion != protocol.PlanFormatVersion {
		t.Errorf("FormatVersion = %d, want %d", p.FormatVersion, protocol.PlanFormatVersion)
	}
	if p.PlanID != "plan-fixed" {
		t.Errorf("PlanID = %q", p.PlanID)
	}
	if p.Operation != protocol.OpCreateIndex {
		t.Errorf("Operation = %q", p.Operation)
	}
	if p.Target.Database != f.Database {
		t.Errorf("Target.Database = %q, want %q", p.Target.Database, f.Database)
	}
	if p.Target.Table.String() != "public.orders" {
		t.Errorf("Target.Table = %v", p.Target.Table)
	}
	if p.Target.Index == nil || p.Target.Index.Name != parentIndexName {
		t.Errorf("Target.Index = %v", p.Target.Index)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("plan does not validate: %v", err)
	}
	if err := p.VerifyDigest(); err != nil {
		t.Errorf("plan is not sealed: %v", err)
	}
	if err := p.VerifyTopology(out.Topology.Input()); err != nil {
		t.Errorf("fingerprint does not match the tree it was planned against: %v", err)
	}
	if out.Role != f.Role {
		t.Errorf("Outcome.Role = %q, want %q", out.Role, f.Role)
	}
	if len(out.Notes) != 1 {
		t.Errorf("Notes = %v", out.Notes)
	}
	if p.TotalEstimatedSeconds() <= 0 {
		t.Error("no duration estimate reached the plan (FR-PLAN-9)")
	}

	// The artifact must round-trip through the file format.
	data, err := protocol.EncodePlan(p)
	if err != nil {
		t.Fatalf("EncodePlan: %v", err)
	}
	back, err := protocol.DecodePlan(data)
	if err != nil {
		t.Fatalf("DecodePlan: %v", err)
	}
	if err := back.VerifyDigest(); err != nil {
		t.Errorf("digest broke over the round trip: %v", err)
	}
}

// TestHostRunIsReproducible: same catalog, same clock, same plan ID, same
// bytes. Without this the plan file cannot be committed and diffed.
func TestHostRunIsReproducible(t *testing.T) {
	encode := func() []byte {
		h := testHost(standardTree())
		out, err := h.Run(ctx(), &stubPlanner{op: protocol.OpCreateIndex}, createSpec())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		data, err := protocol.EncodePlan(out.Plan)
		if err != nil {
			t.Fatalf("EncodePlan: %v", err)
		}
		return data
	}
	a, b := encode(), encode()
	if string(a) != string(b) {
		t.Error("two identical planning passes produced different plan files")
	}
}

// TestHostRunOrdersItsChecks is the requirement behind FR-PLAN-10: a plan-time
// safety check that runs after the operation has been asked for nodes is a
// check the operator can still trip over mid-run.
func TestHostRunOrdersItsChecks(t *testing.T) {
	tests := []struct {
		name    string
		build   func() *FakeCatalog
		spec    func() Specification
		wantErr error
		wantMsg string
	}{
		{
			name: "read-only refusal comes before anything else",
			build: func() *FakeCatalog {
				f := standardTree()
				f.ReadOnly = false
				return f
			},
			spec:    createSpec,
			wantErr: ErrNotReadOnly,
		},
		{
			name: "server version gate",
			build: func() *FakeCatalog {
				f := standardTree()
				f.ServerVersion = 130010
				return f
			},
			spec:    createSpec,
			wantErr: ErrUnsupportedServerVersion,
			wantMsg: "140000",
		},
		{
			name:  "topology rejection",
			build: func() *FakeCatalog { return tree(protocol.StrategyHash, "orders_h0") },
			spec:  createSpec,
			// A HASH tree never reaches the operation planner.
			wantErr: protocol.ErrUnsupportedTopology,
		},
		{
			name: "privilege rejection",
			build: func() *FakeCatalog {
				f := standardTree()
				f.Members[ownerOID] = false
				return f
			},
			spec:    createSpec,
			wantErr: protocol.ErrInsufficientPrivilege,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			op := &stubPlanner{op: protocol.OpCreateIndex}
			h := testHost(tc.build())

			_, err := h.Run(ctx(), op, tc.spec())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("message %q does not contain %q", err.Error(), tc.wantMsg)
			}
			if op.calls != 0 {
				t.Errorf("the operation planner was called %d times; every plan-time check "+
					"must run before any node is emitted", op.calls)
			}
		})
	}
}

func TestHostRunSkipReadOnlyCheck(t *testing.T) {
	f := standardTree()
	f.ReadOnly = false
	h := testHost(f)
	h.SkipReadOnlyCheck = true

	if _, err := h.Run(ctx(), &stubPlanner{op: protocol.OpCreateIndex}, createSpec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestHostRunRejectsBadInputs(t *testing.T) {
	tests := []struct {
		name    string
		host    func(*FakeCatalog) *Host
		op      OperationPlanner
		spec    Specification
		wantErr error
		wantMsg string
	}{
		{
			name:    "no catalog",
			host:    func(*FakeCatalog) *Host { return &Host{} },
			op:      &stubPlanner{op: protocol.OpCreateIndex},
			spec:    createSpec(),
			wantErr: ErrInvalidSpecification,
			wantMsg: "catalog reader",
		},
		{
			name:    "no operation planner",
			host:    testHost,
			op:      nil,
			spec:    createSpec(),
			wantErr: ErrInvalidSpecification,
			wantMsg: "operation planner",
		},
		{
			name: "operation mismatch",
			host: testHost,
			op:   &stubPlanner{op: protocol.OpDropIndex},
			spec: createSpec(),
			// A create spec handed to the drop planner would silently plan the
			// wrong thing.
			wantErr: ErrInvalidSpecification,
			wantMsg: "implements",
		},
		{
			name:    "unknown operation",
			host:    testHost,
			op:      &stubPlanner{op: protocol.OpCreateIndex},
			spec:    Specification{Operation: "vacuum-everything", Table: name("public", "orders"), Index: name("public", "i")},
			wantErr: ErrInvalidSpecification,
		},
		{
			name: "operation emits nothing",
			host: testHost,
			op:   &stubPlanner{op: protocol.OpCreateIndex, noNodes: true},
			spec: createSpec(),
			// A converged target still needs a plan that verifies it (AC-7).
			wantErr: protocol.ErrInvalidPlan,
			wantMsg: "no nodes",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.host(standardTree())
			_, err := h.Run(ctx(), tc.op, tc.spec)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("message %q does not contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

func TestHostRunPropagatesOperationErrors(t *testing.T) {
	sentinel := errors.New("the operation could not decide")
	h := testHost(standardTree())
	_, err := h.Run(ctx(), &stubPlanner{op: protocol.OpCreateIndex, err: sentinel}, createSpec())
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the operation's error", err)
	}
}

// TestHostRunRejectsAnInvalidGraph proves the host validates what the operation
// hands back, rather than trusting it.
func TestHostRunRejectsAnInvalidGraph(t *testing.T) {
	tests := []struct {
		name string
		emit func(req Request) []protocol.Node
	}{
		{
			name: "dangling dependency",
			emit: func(req Request) []protocol.Node {
				return []protocol.Node{{
					ID: "a", Kind: protocol.KindWait, DependsOn: []protocol.NodeID{"ghost"},
					Params: &protocol.WaitParams{Seconds: 1},
				}}
			},
		},
		{
			name: "cycle",
			emit: func(req Request) []protocol.Node {
				return []protocol.Node{
					{ID: "a", Kind: protocol.KindWait, DependsOn: []protocol.NodeID{"b"}, Params: &protocol.WaitParams{Seconds: 1}},
					{ID: "b", Kind: protocol.KindWait, DependsOn: []protocol.NodeID{"a"}, Params: &protocol.WaitParams{Seconds: 1}},
				}
			},
		},
		{
			name: "destructive node with no authorization (FR-AUTH-1)",
			emit: func(req Request) []protocol.Node {
				return []protocol.Node{{
					ID: "drop", Kind: protocol.KindIndexDropConcurrently,
					Params: &protocol.DropConcurrentlyParams{Index: name("public", "leftover")},
				}}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := testHost(standardTree())
			op := &stubPlanner{op: protocol.OpCreateIndex, emit: tc.emit}
			if _, err := h.Run(ctx(), op, createSpec()); err == nil {
				t.Fatal("Run accepted an invalid graph")
			}
		})
	}
}

// TestHostRunPassesAPreparedRequest: the operation must never have to repeat
// discovery or the privilege check, and must never have to reach for a clock.
func TestHostRunPassesAPreparedRequest(t *testing.T) {
	f := standardTree()
	h := testHost(f)
	h.Provenance = NewFakeProvenance()
	op := &stubPlanner{op: protocol.OpCreateIndex}

	out, err := h.Run(ctx(), op, createSpec())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	req := op.seen
	if req.Catalog == nil {
		t.Error("Request.Catalog is nil")
	}
	if req.Topology.LeafCount() != 3 {
		t.Errorf("Request.Topology has %d leaves", req.Topology.LeafCount())
	}
	if req.Role != f.Role {
		t.Errorf("Request.Role = %q", req.Role)
	}
	if req.Database != f.Database {
		t.Errorf("Request.Database = %q", req.Database)
	}
	if req.ServerVersionNum != f.ServerVersion {
		t.Errorf("Request.ServerVersionNum = %d", req.ServerVersionNum)
	}
	if req.PlanID != out.Plan.PlanID {
		t.Errorf("Request.PlanID = %q, plan has %q", req.PlanID, out.Plan.PlanID)
	}
	if !req.Now.Time.Equal(out.Plan.CreatedAt.Time) {
		t.Errorf("Request.Now = %v, plan CreatedAt = %v", req.Now, out.Plan.CreatedAt)
	}
	if req.Provenance == nil {
		t.Error("Request.Provenance was not passed through")
	}
	// A zero Estimator would silently make every estimate 0.
	if req.Estimator.BuildBytesPerSecond != DefaultBuildBytesPerSecond {
		t.Errorf("Request.Estimator was not defaulted: %+v", req.Estimator)
	}
}

func TestHostDefaultsClockAndPlanID(t *testing.T) {
	h := &Host{Catalog: standardTree()}
	out, err := h.Run(ctx(), &stubPlanner{op: protocol.OpCreateIndex}, createSpec())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Plan.PlanID == "" {
		t.Error("no plan id was minted")
	}
	if out.Plan.CreatedAt.Time.IsZero() {
		t.Error("CreatedAt was not set")
	}

	// The identity must not be the digest (INV-6): two plans with identical
	// content still get different identities.
	seen := map[protocol.PlanID]bool{}
	for i := 0; i < 100; i++ {
		id := NewPlanID()
		if seen[id] {
			t.Fatalf("NewPlanID repeated %q", id)
		}
		seen[id] = true
		if !strings.HasPrefix(string(id), "plan-") {
			t.Errorf("NewPlanID = %q, want a plan- prefix", id)
		}
	}
}

func TestSpecificationValidate(t *testing.T) {
	valid := createSpec()

	tests := []struct {
		name    string
		mutate  func(*Specification)
		wantErr bool
		wantMsg string
	}{
		{name: "valid create", mutate: func(*Specification) {}},
		{
			name:    "unknown operation",
			mutate:  func(s *Specification) { s.Operation = "rewrite-table" },
			wantErr: true, wantMsg: "unknown operation",
		},
		{
			name:    "no table",
			mutate:  func(s *Specification) { s.Table = protocol.ObjectName{} },
			wantErr: true, wantMsg: "table is required",
		},
		{
			name:    "illegal table identifier",
			mutate:  func(s *Specification) { s.Table = protocol.ObjectName{Name: strings.Repeat("x", 64)} },
			wantErr: true, wantMsg: "table",
		},
		{
			name:    "no index",
			mutate:  func(s *Specification) { s.Index = protocol.ObjectName{} },
			wantErr: true, wantMsg: "index is required",
		},
		{
			name:    "illegal index identifier",
			mutate:  func(s *Specification) { s.Index = protocol.ObjectName{Name: "bad\x00name"} },
			wantErr: true, wantMsg: "index",
		},
		{
			name:    "create with no definition",
			mutate:  func(s *Specification) { s.Definition = protocol.IndexDefinition{} },
			wantErr: true, wantMsg: "definition",
		},
		{
			name: "drop needs no definition",
			mutate: func(s *Specification) {
				s.Operation = protocol.OpDropIndex
				s.Definition = protocol.IndexDefinition{}
			},
		},
		{
			name:    "negative pace",
			mutate:  func(s *Specification) { s.PaceSeconds = -1 },
			wantErr: true, wantMsg: "pace_seconds",
		},
		{
			name: "pace is allowed",
			mutate: func(s *Specification) {
				s.PaceSeconds = 30
				s.PaceReason = "let replicas catch up"
			},
		},
		{
			name: "confirmation with no flag",
			mutate: func(s *Specification) {
				s.Confirmations = []protocol.Confirmation{{Actor: "platform"}}
			},
			wantErr: true, wantMsg: "confirmation",
		},
		{
			name: "confirmation is recorded",
			mutate: func(s *Specification) {
				s.Confirmations = []protocol.Confirmation{{
					Flag: protocol.ConfirmExclusiveLock, Actor: "platform", At: protocol.Now(),
				}}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := valid
			tc.mutate(&s)
			err := s.Validate()
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidSpecification) {
					t.Fatalf("err = %v, want ErrInvalidSpecification", err)
				}
				if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
					t.Errorf("message %q does not contain %q", err.Error(), tc.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestSpecificationConfirmed(t *testing.T) {
	s := createSpec()
	if s.Confirmed(protocol.ConfirmExclusiveLock) {
		t.Error("an unconfirmed spec reported a confirmation")
	}
	s.Confirmations = append(s.Confirmations, protocol.Confirmation{Flag: protocol.ConfirmExclusiveLock})
	if !s.Confirmed(protocol.ConfirmExclusiveLock) {
		t.Error("a recorded confirmation was not found")
	}
	if s.Confirmed("--some-other-flag") {
		t.Error("the wrong flag matched")
	}
}

// TestHostRunRecordsConfirmations is FR-DROP-3 / AC-13 at the host level: the
// acknowledgement reaches the artifact.
func TestHostRunRecordsConfirmations(t *testing.T) {
	spec := createSpec()
	spec.Confirmations = []protocol.Confirmation{{
		Flag:  protocol.ConfirmExclusiveLock,
		Actor: "platform",
		At:    protocol.NewTimestamp(time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)),
		Note:  "maintenance window agreed",
	}}

	h := testHost(standardTree())
	out, err := h.Run(ctx(), &stubPlanner{op: protocol.OpCreateIndex}, spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Plan.Confirmed(protocol.ConfirmExclusiveLock) {
		t.Error("the acknowledgement was not recorded in the plan artifact")
	}
}

// TestHostRunCatalogFailures: every catalog method the host calls can fail, and
// none of them may produce a plan.
func TestHostRunCatalogFailures(t *testing.T) {
	f := standardTree()
	f.Err = ErrCatalogUnavailable.Detailf("connection reset")
	h := testHost(f)
	op := &stubPlanner{op: protocol.OpCreateIndex}

	out, err := h.Run(ctx(), op, createSpec())
	if err == nil {
		t.Fatal("Run succeeded against an unreachable catalog")
	}
	if out != nil {
		t.Error("an outcome was returned alongside an error")
	}
	if op.calls != 0 {
		t.Error("the operation planner ran against an unreachable catalog")
	}
}

// failOn makes exactly one catalog method fail, so each of the host's
// plan-time checks can be shown to actually run and to actually stop the pass.
type failOn struct {
	*FakeCatalog
	method string
	err    error
}

func (f failOn) trip(method string) error {
	if f.method == method {
		return f.err
	}
	return nil
}

func (f failOn) CurrentRole(c context.Context) (string, error) {
	if err := f.trip("CurrentRole"); err != nil {
		return "", err
	}
	return f.FakeCatalog.CurrentRole(c)
}

func (f failOn) CurrentDatabase(c context.Context) (string, error) {
	if err := f.trip("CurrentDatabase"); err != nil {
		return "", err
	}
	return f.FakeCatalog.CurrentDatabase(c)
}

func (f failOn) ServerVersionNum(c context.Context) (int, error) {
	if err := f.trip("ServerVersionNum"); err != nil {
		return 0, err
	}
	return f.FakeCatalog.ServerVersionNum(c)
}

func (f failOn) LookupRelation(c context.Context, n protocol.ObjectName) (Relation, error) {
	if err := f.trip("LookupRelation"); err != nil {
		return Relation{}, err
	}
	return f.FakeCatalog.LookupRelation(c, n)
}

func (f failOn) RoleMemberships(c context.Context, role string, oids []uint32) (map[uint32]RoleMembership, error) {
	if err := f.trip("RoleMemberships"); err != nil {
		return nil, err
	}
	return f.FakeCatalog.RoleMemberships(c, role, oids)
}

func TestHostRunStopsOnEveryCatalogStep(t *testing.T) {
	boom := ErrCatalogUnavailable.Detailf("connection reset")
	methods := []string{
		"ServerVersionNum", "CurrentRole", "CurrentDatabase", "LookupRelation", "RoleMemberships",
	}
	for _, m := range methods {
		t.Run(m, func(t *testing.T) {
			op := &stubPlanner{op: protocol.OpCreateIndex}
			h := testHost(standardTree())
			h.Catalog = failOn{FakeCatalog: h.Catalog.(*FakeCatalog), method: m, err: boom}

			out, err := h.Run(ctx(), op, createSpec())
			if !errors.Is(err, ErrCatalogUnavailable) {
				t.Fatalf("err = %v, want ErrCatalogUnavailable", err)
			}
			if out != nil {
				t.Error("an outcome was returned alongside an error")
			}
			if op.calls != 0 {
				t.Error("the operation planner ran after a failed plan-time check")
			}
		})
	}
}

func TestHostRunPassesDiscoverOptions(t *testing.T) {
	f := tree(protocol.StrategyRange)
	h := testHost(f)
	op := &stubPlanner{
		op: protocol.OpCreateIndex,
		emit: func(req Request) []protocol.Node {
			return []protocol.Node{{
				ID: "wait", Kind: protocol.KindWait, Params: &protocol.WaitParams{Seconds: 0},
			}}
		},
	}

	if _, err := h.Run(ctx(), op, createSpec()); err == nil {
		t.Fatal("Run accepted a childless tree without the option")
	}

	h.DiscoverOptions = []DiscoverOption{AllowNoPartitions()}
	if _, err := h.Run(ctx(), op, createSpec()); err != nil {
		t.Fatalf("Run with AllowNoPartitions: %v", err)
	}
}
