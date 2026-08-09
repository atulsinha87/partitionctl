package state

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// baseTime is a fixed instant. Every test that cares about ordering or expiry
// derives from it, so no test depends on the wall clock.
var baseTime = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock { return &fakeClock{now: baseTime} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) Clock() Clock { return c.Now }

// testPlan builds a small sealed plan. Node ids are deliberately awkward:
// slashes, spaces, a colon and a non-ASCII rune, because the file store turns a
// node id into a path segment and that is exactly where an encoding bug hides.
func testPlan(t *testing.T, nodeIDs ...string) *protocol.Plan {
	t.Helper()
	if len(nodeIDs) == 0 {
		nodeIDs = []string{"assert", "parent", "leaf/2026 03:cic", "wait"}
	}
	nodes := make([]protocol.Node, 0, len(nodeIDs))
	for i, id := range nodeIDs {
		params := &protocol.WaitParams{Seconds: 1, Reason: "pacing"}
		n := protocol.Node{
			ID:               protocol.NodeID(id),
			Kind:             protocol.KindWait,
			Params:           params,
			EstimatedSeconds: i + 1,
		}
		if i > 0 {
			n.DependsOn = []protocol.NodeID{protocol.NodeID(nodeIDs[0])}
		}
		nodes = append(nodes, n)
	}
	p := &protocol.Plan{
		FormatVersion: protocol.PlanFormatVersion,
		PlanID:        "plan-test-1",
		Operation:     protocol.OpCreateIndex,
		Target: protocol.Target{
			Database: "appdb",
			Table:    protocol.NewObjectName("public", "orders"),
		},
		CreatedAt: protocol.NewTimestamp(baseTime),
		Nodes:     nodes,
		// FR-PLANFILE-4: Validate requires a fingerprint, because a plan is
		// bound to the tree it was computed over.
		TopologyFingerprint: protocol.FingerprintPrefix +
			"0000000000000000000000000000000000000000000000000000000000000000",
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("test plan is invalid: %v", err)
	}
	if err := p.Seal(); err != nil {
		t.Fatalf("seal test plan: %v", err)
	}
	return p
}

// testPlanWithID returns a sealed plan with a distinct PlanID, so two plans in
// one test get two digests.
func testPlanWithID(t *testing.T, id protocol.PlanID, table string) *protocol.Plan {
	t.Helper()
	p := testPlan(t)
	p.PlanID = id
	p.Target.Table = protocol.NewObjectName("public", table)
	p.Digest = ""
	if err := p.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	return p
}

// testClaimPlan builds a sealed create-index plan whose nodes name real
// objects, so [NodeRecord.Object] is seeded and [ClaimsObject] has something to
// find. testPlan's nodes are all `wait`, which claim nothing by design.
func testClaimPlan(t *testing.T, leafIndexes ...string) *protocol.Plan {
	t.Helper()
	parent := protocol.NewObjectName("public", "orders")
	def := protocol.IndexDefinition{Columns: []protocol.IndexColumn{{Name: "created_at"}}}
	nodes := []protocol.Node{{
		ID:   "parent",
		Kind: protocol.KindIndexCreateParentInvalid,
		Params: &protocol.CreateParentInvalidParams{
			Parent: parent, Index: protocol.NewObjectName("public", "orders_idx"), Definition: def,
		},
	}}
	for _, name := range leafIndexes {
		nodes = append(nodes, protocol.Node{
			ID:        protocol.NodeID("cic:" + name),
			Kind:      protocol.KindIndexCreateConcurrently,
			DependsOn: []protocol.NodeID{"parent"},
			Params: &protocol.CreateConcurrentlyParams{
				Partition:  protocol.NewObjectName("public", "orders_p"),
				Index:      protocol.NewObjectName("public", name),
				Definition: def,
			},
		})
	}
	p := &protocol.Plan{
		FormatVersion: protocol.PlanFormatVersion,
		PlanID:        "plan-claim-1",
		Operation:     protocol.OpCreateIndex,
		Target: protocol.Target{
			Database: "appdb",
			Table:    parent,
		},
		CreatedAt: protocol.NewTimestamp(baseTime),
		Nodes:     nodes,
		TopologyFingerprint: protocol.FingerprintPrefix +
			"0000000000000000000000000000000000000000000000000000000000000000",
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("test plan is invalid: %v", err)
	}
	if err := p.Seal(); err != nil {
		t.Fatalf("seal test plan: %v", err)
	}
	return p
}

// newFileStore opens a store in a temp dir with a controlled clock.
func newFileStore(t *testing.T) (*FileStore, *fakeClock) {
	t.Helper()
	c := newClock()
	s, err := OpenFileStore(t.TempDir(), FileOptions{Clock: c.Clock(), Holder: "test/1"})
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, c
}

// mustCreateRun opens a run and fails the test if it cannot.
func mustCreateRun(t *testing.T, s StateStore, p *protocol.Plan, id RunID) Run {
	t.Helper()
	run, err := s.CreateRun(context.Background(), NewRun{Plan: p, RunID: id, Actor: "tester"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run
}

func mustLease(t *testing.T, s StateStore, id RunID, holder string, ttl time.Duration) Lease {
	t.Helper()
	l, err := s.AcquireLease(context.Background(), id, holder, ttl)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	return l
}

func mustLock(t *testing.T, s StateStore, key LockKey) Lock {
	t.Helper()
	l, err := s.TryLock(context.Background(), key)
	if err != nil {
		t.Fatalf("TryLock(%s): %v", key, err)
	}
	t.Cleanup(func() { _ = l.Unlock(context.Background()) })
	return l
}

func testKey() LockKey {
	return LockKey{Database: "appdb", Table: protocol.NewObjectName("public", "orders")}
}

// tsAt is a shorthand for a protocol timestamp in tests.
func tsAt(t time.Time) protocol.Timestamp { return protocol.NewTimestamp(t) }
