package protocol

import (
	"bytes"
	"container/heap"
	"encoding/json"
	"errors"
	"strings"
)

// PlanID identifies one plan. It is stable for the life of the artifact and is
// not the digest: the digest changes when the content changes, the PlanID does
// not.
type PlanID string

func (id PlanID) String() string { return string(id) }

// Operation names the planner that produced a plan.
type Operation string

// The three v0.1 operations. The strings match the CLI's --op values.
const (
	OpCreateIndex  Operation = "create-index"
	OpReindexIndex Operation = "reindex-index"
	OpDropIndex    Operation = "drop-index"
)

var allOperations = []Operation{OpCreateIndex, OpReindexIndex, OpDropIndex}

// AllOperations returns the three v0.1 operations. The returned slice is a copy.
func AllOperations() []Operation {
	out := make([]Operation, len(allOperations))
	copy(out, allOperations)
	return out
}

// Valid reports whether o is a known operation.
func (o Operation) Valid() bool {
	switch o {
	case OpCreateIndex, OpReindexIndex, OpDropIndex:
		return true
	}
	return false
}

func (o Operation) String() string { return string(o) }

// Target names what a plan acts on. It is recorded explicitly so that a plan
// cannot be executed against an unintended database by accident (T8). It never
// carries credentials (NFR-SEC-3).
type Target struct {
	// Database is the database name, for the reviewer's benefit. It is not a
	// connection string.
	Database string `json:"database,omitempty"`
	// Table is the partitioned parent table.
	Table ObjectName `json:"table"`
	// Index is the partitioned index the operation acts on, where the
	// operation has one.
	Index *ObjectName `json:"index,omitempty"`
}

// Validate checks the target's identifiers.
func (t Target) Validate() error {
	if err := validateObject("table", t.Table); err != nil {
		return err
	}
	return validateOptionalObject("index", t.Index)
}

// Confirmation records that an operator supplied a required acknowledgement at
// plan time. DropPartitionedIndex requires [ConfirmExclusiveLock] and the plan
// must record that it was given (FR-DROP-3, AC-13); it is also the evidence an
// [AuthExplicit] node's RequiredConfirmation points at (FR-AUTH-4).
type Confirmation struct {
	// Flag is the CLI flag, for example [ConfirmExclusiveLock].
	Flag string `json:"flag"`
	// Actor is who supplied it, for the audit trail.
	Actor string `json:"actor,omitempty"`
	// At is when it was supplied.
	At Timestamp `json:"at"`
	// Note is free-form operator context.
	Note string `json:"note,omitempty"`
}

// Plan is the immutable, git-committable execution artifact (FR-PLANFILE-1).
//
// It is verified twice before it runs: the digest proves the file was not
// edited after approval (FR-PLANFILE-3), and the topology fingerprint proves
// the database has not drifted (FR-PLANFILE-5).
type Plan struct {
	// FormatVersion is the plan-format schema version (FR-PLANFILE-8). The
	// executor refuses a version it does not support (NFR-COMPAT-3).
	FormatVersion int `json:"format_version"`

	// PlanID identifies this plan.
	PlanID PlanID `json:"plan_id"`

	// Operation names the planner that produced it.
	Operation Operation `json:"operation"`

	// Target names what it acts on.
	Target Target `json:"target"`

	// CreatedAt is when it was planned.
	CreatedAt Timestamp `json:"created_at"`

	// Confirmations records every acknowledgement the operator supplied at
	// plan time (FR-DROP-3).
	Confirmations []Confirmation `json:"confirmations,omitempty"`

	// Nodes is the graph. Edges are [Node.DependsOn].
	Nodes []Node `json:"nodes"`

	// TopologyFingerprint is a hash over the discovered partition set and its
	// relevant catalog state (FR-PLANFILE-4). See [TopologyInput.Fingerprint].
	TopologyFingerprint string `json:"topology_fingerprint,omitempty"`

	// Topology is the tree the fingerprint was computed over.
	//
	// The fingerprint alone answers "did anything change?", which is not what
	// AC-3 asks for: the refusal must *name* the drift. Without the tree there
	// is nothing to diff against, so the best a refusal could do was print two
	// hashes and dump the live partition list, leaving an operator with a
	// 400-partition table to find the new one by hand against a plan file that
	// did not contain the corresponding list. Recording it makes
	// [DiffTopology] reachable and the refusal actionable.
	//
	// It is diagnosis, not verification: the fingerprint remains what decides
	// whether drift occurred. Optional so that a v1 plan still executes, in
	// which case the refusal falls back to reporting the live tree.
	Topology *TopologyInput `json:"topology,omitempty"`

	// Digest is "sha256:" + hex of SHA-256 over the canonicalized plan body,
	// excluding this field (FR-PLANFILE-2). Set it with [Plan.Seal].
	Digest string `json:"digest,omitempty"`
}

// Confirmed reports whether the plan records the named acknowledgement.
func (p *Plan) Confirmed(flag string) bool {
	for _, c := range p.Confirmations {
		if c.Flag == flag {
			return true
		}
	}
	return false
}

// NodeByID returns the node with the given id.
func (p *Plan) NodeByID(id NodeID) (*Node, bool) {
	for i := range p.Nodes {
		if p.Nodes[i].ID == id {
			return &p.Nodes[i], true
		}
	}
	return nil, false
}

// TotalEstimatedSeconds sums the per-node estimates, for the ETA in `plan`
// output and `status` (FR-PLAN-9, FR-ORD-5).
func (p *Plan) TotalEstimatedSeconds() int {
	total := 0
	for i := range p.Nodes {
		total += p.Nodes[i].EstimatedSeconds
	}
	return total
}

// Validate checks the plan's structure: format version, identity, every node,
// dependency resolution, acyclicity, and the two plan-level invariants that
// govern destructive plans (INV-8, FR-DROP-3).
//
// It does not check the digest or the fingerprint: those compare the plan
// against something outside it. See [Plan.VerifyDigest] and
// [Plan.VerifyTopology].
func (p *Plan) Validate() error {
	if err := CheckFormatVersion(p.FormatVersion); err != nil {
		return err
	}
	if p.PlanID == "" {
		return ErrInvalidPlan.Detailf("plan_id is empty")
	}
	if !p.Operation.Valid() {
		return ErrInvalidPlan.Detailf("unknown operation %q, want one of %v", p.Operation, allOperations)
	}
	if err := p.Target.Validate(); err != nil {
		return ErrInvalidPlan.Detailf("target: %v", err)
	}
	// FR-PLANFILE-4: the plan SHALL contain a topology fingerprint. Enforcing
	// it here makes the requirement structural rather than a convention the
	// CLI's plan path happens to follow. Without it a plan validates, seals and
	// encodes, and under --allow-drift the missing fingerprint degrades to a
	// warning and the run issues DDL against a catalog whose shape was never
	// bound to the artifact, which is what T8 and T9 exist to prevent.
	if p.TopologyFingerprint == "" {
		return ErrInvalidPlan.Detailf(
			"topology_fingerprint is empty; a plan is bound to the tree it was computed over (FR-PLANFILE-4)")
	}
	if !strings.HasPrefix(p.TopologyFingerprint, FingerprintPrefix) {
		return ErrInvalidPlan.Detailf(
			"topology_fingerprint %q does not name its algorithm; want a %q prefix (FR-PLANFILE-4)",
			p.TopologyFingerprint, FingerprintPrefix)
	}
	if len(p.Nodes) == 0 {
		return ErrInvalidPlan.Detailf("plan contains no nodes")
	}
	for i, c := range p.Confirmations {
		if c.Flag == "" {
			return ErrInvalidPlan.Detailf("confirmation %d has an empty flag", i)
		}
	}

	byID := make(map[NodeID]struct{}, len(p.Nodes))
	dropPartitioned := 0
	for i := range p.Nodes {
		n := &p.Nodes[i]
		if err := n.Validate(); err != nil {
			return err
		}
		if _, dup := byID[n.ID]; dup {
			return ErrInvalidPlan.Detailf("duplicate node id %q", n.ID)
		}
		byID[n.ID] = struct{}{}
		if n.Kind == KindIndexDropPartitioned {
			dropPartitioned++
		}
	}

	// INV-8 (amended). At most one index.drop_partitioned per plan. The only
	// other destructive kind permitted alongside it is index.drop_concurrently,
	// for the unattached orphan leaf indexes an abandoned create leaves behind.
	//
	// The original text forbade *any* other destructive node, which forbade the
	// drop choreography TRD §7.2.13 specifies: an unattached orphan survives the
	// parent's cascade, so a correct drop plan removes the orphans first and
	// only then drops the parent. Under the original rule every such plan failed
	// Validate, which is a refusal to plan the only correct sequence.
	//
	// What the invariant is actually protecting is unchanged: one plan never
	// destroys two partitioned index families, and the AccessExclusiveLock
	// statement is never one of several the operator did not separately weigh.
	if dropPartitioned > 1 {
		return ErrInvalidPlan.Detailf(
			"INV-8: plan contains %d %s nodes; at most one is allowed", dropPartitioned, KindIndexDropPartitioned)
	}
	if dropPartitioned == 1 {
		for i := range p.Nodes {
			n := &p.Nodes[i]
			if !n.Kind.IsDestructive() || n.Kind == KindIndexDropPartitioned {
				continue
			}
			if n.Kind != KindIndexDropConcurrently {
				return ErrInvalidPlan.Detailf(
					"INV-8: a plan containing %s may contain no destructive kind other than %s; node %q is %s",
					KindIndexDropPartitioned, KindIndexDropConcurrently, n.ID, n.Kind)
			}
		}
	}
	// FR-DROP-3 / AC-13: the acknowledgement must be recorded in the artifact.
	if dropPartitioned == 1 && !p.Confirmed(ConfirmExclusiveLock) {
		return ErrInvalidPlan.Detailf(
			"FR-DROP-3: a plan containing %s must record the %s acknowledgement",
			KindIndexDropPartitioned, ConfirmExclusiveLock)
	}

	for i := range p.Nodes {
		n := &p.Nodes[i]
		seen := make(map[NodeID]struct{}, len(n.DependsOn))
		for _, d := range n.DependsOn {
			if d == n.ID {
				return ErrInvalidPlan.Detailf("node %q depends on itself", n.ID)
			}
			if _, dup := seen[d]; dup {
				return ErrInvalidPlan.Detailf("node %q lists dependency %q twice", n.ID, d)
			}
			seen[d] = struct{}{}
			if _, ok := byID[d]; !ok {
				return ErrInvalidPlan.Detailf("node %q depends on unknown node %q", n.ID, d)
			}
		}
	}

	_, err := p.TopologicalOrder()
	return err
}

// TopologicalOrder returns the node IDs in a deterministic topological order:
// among the nodes whose dependencies are all satisfied, the one earliest in
// [Plan.Nodes] comes first. Two callers therefore agree on the order without
// coordinating.
//
// It returns an error matching [ErrInvalidPlan] if the graph has a cycle or an
// unresolved dependency.
func (p *Plan) TopologicalOrder() ([]NodeID, error) {
	n := len(p.Nodes)
	index := make(map[NodeID]int, n)
	for i := range p.Nodes {
		index[p.Nodes[i].ID] = i
	}

	indegree := make([]int, n)
	dependents := make([][]int, n)
	for i := range p.Nodes {
		for _, d := range p.Nodes[i].DependsOn {
			j, ok := index[d]
			if !ok {
				return nil, ErrInvalidPlan.Detailf("node %q depends on unknown node %q", p.Nodes[i].ID, d)
			}
			indegree[i]++
			dependents[j] = append(dependents[j], i)
		}
	}

	ready := &intHeap{}
	for i := 0; i < n; i++ {
		if indegree[i] == 0 {
			*ready = append(*ready, i)
		}
	}
	heap.Init(ready)

	order := make([]NodeID, 0, n)
	for ready.Len() > 0 {
		i := heap.Pop(ready).(int)
		order = append(order, p.Nodes[i].ID)
		for _, j := range dependents[i] {
			indegree[j]--
			if indegree[j] == 0 {
				heap.Push(ready, j)
			}
		}
	}

	if len(order) != n {
		var stuck []NodeID
		for i := 0; i < n && len(stuck) < 8; i++ {
			if indegree[i] > 0 {
				stuck = append(stuck, p.Nodes[i].ID)
			}
		}
		return nil, ErrInvalidPlan.Detailf("graph has a cycle; nodes still blocked: %v", stuck)
	}
	return order, nil
}

// intHeap is a min-heap of plan indices, which is what makes
// [Plan.TopologicalOrder] deterministic.
type intHeap []int

func (h intHeap) Len() int           { return len(h) }
func (h intHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h intHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *intHeap) Push(x any)        { *h = append(*h, x.(int)) }
func (h *intHeap) Pop() (x any)      { old := *h; n := len(old); x = old[n-1]; *h = old[:n-1]; return x }

// DecodePlan parses a plan file, checks its format version (NFR-COMPAT-3), and
// proves the parse was lossless.
//
// The lossless check matters more than it looks. The digest is computed over
// the *parsed* plan, so any content the parser silently drops — an unknown
// field, a duplicate key — would vanish from both sides of
// [Plan.VerifyDigest] and pass. DecodePlan therefore canonicalizes the file
// bytes and the parsed plan and requires them to be identical, so that what a
// reviewer reads in the artifact is exactly what the executor holds.
//
// It deliberately does not verify the digest or validate the graph: `execute`
// verifies the digest before doing anything else (FR-PLANFILE-3), while
// `render` and `status` legitimately read plans they will not run.
func DecodePlan(data []byte) (*Plan, error) {
	// The version is read before anything else looks at the contents
	// (NFR-COMPAT-3). A plan written by a newer binary will pair a newer format
	// version with a node kind this build has never heard of, and unmarshalling
	// first would report the unknown kind: the operator would be told their
	// plan is malformed when the real answer is that their binary is too old.
	// TRD §7.2.2 makes adding a node kind a versioned change, so that pairing
	// is the expected shape of a future plan rather than an edge case.
	var envelope struct {
		FormatVersion int `json:"format_version"`
	}
	if err := json.Unmarshal(data, &envelope); err == nil {
		if verr := CheckFormatVersion(envelope.FormatVersion); verr != nil {
			return nil, verr
		}
	}

	var p Plan
	if err := json.Unmarshal(data, &p); err != nil {
		// Preserve a typed error raised from inside Node.UnmarshalJSON, so an
		// unknown node kind stays distinguishable from malformed JSON.
		var typed *Error
		if errors.As(err, &typed) {
			return nil, typed
		}
		return nil, ErrInvalidPlan.Detailf("%v", err)
	}
	if err := CheckFormatVersion(p.FormatVersion); err != nil {
		return nil, err
	}
	if key, dup := findDuplicateKey(data); dup {
		return nil, ErrInvalidPlan.Detailf(
			"plan file contains the key %q twice in one object; a reviewer and the executor "+
				"would not necessarily read the same value", key)
	}
	fileCanon, err := canonicalJSON(data)
	if err != nil {
		return nil, err
	}
	planCanon, err := canonicalOf(&p)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(fileCanon, planCanon) {
		return nil, ErrInvalidPlan.Detailf(
			"plan file did not round-trip: it carries fields or encodings this binary does not understand, " +
				"so the digest would not cover them")
	}
	return &p, nil
}

// EncodePlan renders a sealed plan as the bytes to write to disk: indented for
// review in a pull request, with a trailing newline.
//
// It refuses to encode a plan whose digest is missing or stale, so a plan file
// can never be written with a digest that does not describe it. Call
// [Plan.Seal] first.
// HTML escaping is turned off. encoding/json escapes <, > and & by default,
// which would obfuscate the two fields a reviewer most needs to read:
// [IndexColumn.Expression] and [IndexDefinition.Where] are the only parameters
// that are not identifier-quoted, so the renderer emits them into DDL byte for
// byte and their whole defence is plan-file review (G2, T2). A predicate like
// `status <> 'done'` must appear as itself in the pull request, not as
// `status <> 'done'`. canonical.go already avoids the encoder's
// escaping for the digest; the artifact and the canonical form should not
// disagree about presentation.
func EncodePlan(p *Plan) ([]byte, error) {
	if err := p.VerifyDigest(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(p); err != nil {
		return nil, ErrInvalidPlan.Detailf("%v", err)
	}
	// Encode already terminates with a newline.
	return buf.Bytes(), nil
}
