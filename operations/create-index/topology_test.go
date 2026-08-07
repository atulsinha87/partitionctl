package createindex

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// AC-11: a HASH-partitioned, multi-level, or DEFAULT-containing target fails at
// plan time with a named, actionable error and exit code 15.
func TestPlanRejectsUnsupportedTopologies(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*protocol.TopologyInput)
		wantMsg string
	}{
		{
			name:    "hash strategy",
			mutate:  func(tp *protocol.TopologyInput) { tp.Strategy = protocol.StrategyHash },
			wantMsg: "HASH",
		},
		{
			name: "default partition",
			mutate: func(tp *protocol.TopologyInput) {
				tp.Partitions[1].IsDefault = true
			},
			wantMsg: "DEFAULT partition",
		},
		{
			name: "sub-partitioned child",
			mutate: func(tp *protocol.TopologyInput) {
				tp.Partitions[1].RelKind = RelKindPartitionedTable
			},
			wantMsg: "depth > 1",
		},
		{
			name: "grandchild partition",
			mutate: func(tp *protocol.TopologyInput) {
				tp.Partitions[1].ParentOID = 999
			},
			wantMsg: "depth > 1",
		},
		{
			name: "root is not partitioned",
			mutate: func(tp *protocol.TopologyInput) {
				tp.Root.RelKind = RelKindTable
			},
			wantMsg: "partitioned table",
		},
		{
			name: "foreign table partition",
			mutate: func(tp *protocol.TopologyInput) {
				tp.Partitions[1].RelKind = "f"
			},
			wantMsg: "relkind",
		},
		{
			name: "no leaf partitions",
			mutate: func(tp *protocol.TopologyInput) {
				tp.Partitions = nil
			},
			wantMsg: "no leaf partitions",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cat := newCatalog("p1", "p2")
			tc.mutate(&cat.topology)

			p, err := testPlanner().Plan(context.Background(), newSpec(), cat)
			if p != nil {
				t.Fatalf("Plan() returned a plan for an unsupported topology")
			}
			if !errors.Is(err, protocol.ErrUnsupportedTopology) {
				t.Fatalf("Plan() error = %v, want ErrUnsupportedTopology", err)
			}
			if got := protocol.ExitCodeFor(err); got != protocol.ExitUnsupportedTopology {
				t.Errorf("exit code = %d, want %d (AC-11, AC-26)", got, protocol.ExitUnsupportedTopology)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not name the problem (%q)", err, tc.wantMsg)
			}
		})
	}
}

func TestPlanAcceptsRangeAndList(t *testing.T) {
	for _, s := range []protocol.PartitionStrategy{protocol.StrategyRange, protocol.StrategyList} {
		t.Run(string(s), func(t *testing.T) {
			cat := newCatalog("p1")
			cat.topology.Strategy = s
			mustPlan(t, testPlanner(), newSpec(), cat)
		})
	}
}

// AC-12 / FR-PLAN-10: the run must work as a member of the owning role, and
// fail at plan time with exit 16 when it is not one.
func TestPlanRejectsInsufficientRoleMembership(t *testing.T) {
	tests := []struct {
		name string
		not  protocol.ObjectName
	}{
		{"parent not owned", obj(testTable)},
		{"one leaf not owned", obj("p2")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cat := newCatalog("p1", "p2", "p3")
			cat.notMember = map[protocol.ObjectName]bool{tc.not: true}

			p, err := testPlanner().Plan(context.Background(), newSpec(), cat)
			if p != nil {
				t.Fatal("Plan() returned a plan despite insufficient privilege")
			}
			if !errors.Is(err, protocol.ErrInsufficientPrivilege) {
				t.Fatalf("Plan() error = %v, want ErrInsufficientPrivilege", err)
			}
			if got := protocol.ExitCodeFor(err); got != protocol.ExitInsufficientPrivilege {
				t.Errorf("exit code = %d, want %d", got, protocol.ExitInsufficientPrivilege)
			}
			if !strings.Contains(err.Error(), tc.not.String()) {
				t.Errorf("error %q does not name %s", err, tc.not)
			}
		})
	}
}

func TestPlanFailsClosedOnAMissingMembershipAnswer(t *testing.T) {
	cat := newCatalog("p1")
	// An empty answer means "no entry", which must read as "not a member".
	cat.members = map[protocol.ObjectName]bool{}

	_, err := testPlanner().Plan(context.Background(), newSpec(), cat)
	if !errors.Is(err, protocol.ErrInsufficientPrivilege) {
		t.Fatalf("Plan() error = %v, want ErrInsufficientPrivilege", err)
	}
}

func TestPlanRejectsAnInvalidTopologyFromTheCatalog(t *testing.T) {
	cat := newCatalog("p1", "p2")
	cat.topology.Partitions[1].OID = cat.topology.Partitions[0].OID // duplicate OID

	_, err := testPlanner().Plan(context.Background(), newSpec(), cat)
	if !errors.Is(err, protocol.ErrInvalidPlan) {
		t.Fatalf("Plan() error = %v, want ErrInvalidPlan from TopologyInput.Validate", err)
	}
}

// ---------------------------------------------------------------------------
// Specification validation
// ---------------------------------------------------------------------------

func TestSpecificationValidate(t *testing.T) {
	valid := newSpec()

	tests := []struct {
		name    string
		mutate  func(*Specification)
		wantErr error
		wantMsg string
	}{
		{"valid", func(*Specification) {}, nil, ""},
		{"no table", func(s *Specification) { s.Table = protocol.ObjectName{} }, protocol.ErrInvalidPlan, "table is required"},
		{"bad table", func(s *Specification) { s.Table = protocol.NewObjectName("", strings.Repeat("x", 64)) }, protocol.ErrInvalidPlan, "table"},
		{"no index", func(s *Specification) { s.Index = protocol.ObjectName{} }, protocol.ErrInvalidPlan, "index is required"},
		{"bad index", func(s *Specification) { s.Index = protocol.NewObjectName("", "\x00") }, protocol.ErrInvalidPlan, "index"},
		{"no columns", func(s *Specification) { s.Definition.Columns = nil }, protocol.ErrInvalidPlan, "definition"},
		{"no role", func(s *Specification) { s.Role = "" }, protocol.ErrInsufficientPrivilege, "role is required"},
		{"bad role", func(s *Specification) { s.Role = strings.Repeat("r", 64) }, protocol.ErrInvalidPlan, "role"},
		{"negative pacing", func(s *Specification) { s.PaceSeconds = -1 }, protocol.ErrInvalidPlan, "pace_seconds"},
		{"negative rate", func(s *Specification) { s.BuildBytesPerSecond = -1 }, protocol.ErrInvalidPlan, "build_bytes_per_second"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := valid
			tc.mutate(&s)
			err := s.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("Validate() = %q, want it to mention %q", err, tc.wantMsg)
			}
		})
	}
}

func TestPlanRejectsAnInvalidSpecificationBeforeTouchingTheCatalog(t *testing.T) {
	cat := newCatalog("p1")
	spec := newSpec()
	spec.Role = ""

	if _, err := testPlanner().Plan(context.Background(), spec, cat); err == nil {
		t.Fatal("Plan() accepted a specification with no role")
	}
	if cat.topologyCalls != 0 {
		t.Errorf("planner read the catalog %d times before validating the specification", cat.topologyCalls)
	}
}

func TestPlanResolvesAnUnqualifiedIndexIntoTheTablesSchema(t *testing.T) {
	cat := newCatalog("p1")
	spec := newSpec()
	spec.Index = protocol.ObjectName{Name: testIndex}

	p := mustPlan(t, testPlanner(), spec, cat)
	if p.Target.Index.Schema != testSchema {
		t.Errorf("target index schema = %q, want %q", p.Target.Index.Schema, testSchema)
	}
	params := node(t, p, nodeParentIndex).Params.(*protocol.CreateParentInvalidParams)
	if params.Index.Schema != testSchema {
		t.Errorf("parent index schema = %q, want %q", params.Index.Schema, testSchema)
	}
}

func TestSpecificationBuildRate(t *testing.T) {
	if got := (Specification{}).buildRate(); got != DefaultBuildBytesPerSecond {
		t.Errorf("buildRate() = %d, want the default %d", got, DefaultBuildBytesPerSecond)
	}
	if got := (Specification{BuildBytesPerSecond: 7}).buildRate(); got != 7 {
		t.Errorf("buildRate() = %d, want 7", got)
	}
}

func TestIndexStateHealthy(t *testing.T) {
	tests := []struct {
		name              string
		valid, ready, liv bool
		want              bool
	}{
		{"all three", true, true, true, true},
		{"not valid", false, true, true, false},
		{"not ready", true, false, true, false},
		{"not live", true, true, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := IndexState{Valid: tc.valid, Ready: tc.ready, Live: tc.liv}
			if got := s.Healthy(); got != tc.want {
				t.Errorf("Healthy() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNoClaimsHoldsNone(t *testing.T) {
	run, ok, err := NoClaims().ClaimsObject(context.Background(), obj("anything"))
	_ = run
	if ok || err != nil {
		t.Fatalf("ClaimsObject() = (%v, %v), want (false, nil)", ok, err)
	}
}
