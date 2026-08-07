package protocol

import (
	"bytes"
	"encoding/json"
)

// NodeID identifies a node uniquely within one plan. The planner assigns it;
// the state store keys node state on it.
type NodeID string

func (id NodeID) String() string { return string(id) }

// Node is a single typed unit of work: the executor's dispatch unit.
//
// The executor dispatches on Kind alone and contains no knowledge of indexes,
// constraints, backfills or partitions beyond the vocabulary (FR-EXEC-1).
type Node struct {
	// ID is unique within the plan.
	ID NodeID `json:"id"`

	// Kind is what the executor dispatches on. It must match Params.Kind().
	Kind NodeKind `json:"kind"`

	// Params are the AUTHORITATIVE structured execution input (FR-PLANFILE-6).
	// The concrete type is determined by Kind; see [NewParams].
	Params NodeParams `json:"params"`

	// DependsOn lists the nodes that must reach DONE or SKIPPED before this
	// one may be dispatched (FR-ORD-1).
	DependsOn []NodeID `json:"depends_on,omitempty"`

	// RenderedSQL is a NON-AUTHORITATIVE human preview for the reviewer of the
	// plan file. The executor MUST ignore it and re-render from Params
	// (FR-PLANFILE-7). It is never sent to the server, which is what keeps it
	// off the injection surface (T2).
	RenderedSQL string `json:"rendered_sql,omitempty"`

	// EstimatedSeconds is the planner's duration estimate, computed from
	// pg_class.relpages (FR-PLAN-9). It drives the ETA in `status`
	// (FR-ORD-5) and nothing else.
	EstimatedSeconds int `json:"estimated_seconds,omitempty"`

	// Authorization is required on a destructive Kind and forbidden otherwise
	// (FR-AUTH-1). It is a proposal: the executor re-evaluates it against live
	// state at dispatch (FR-AUTH-5, INV-2).
	Authorization *Authorization `json:"authorization,omitempty"`
}

// nodeWire is Node's on-disk shape. Params is held as raw JSON so that
// unmarshalling can dispatch on kind.
type nodeWire struct {
	ID               NodeID          `json:"id"`
	Kind             NodeKind        `json:"kind"`
	Params           json.RawMessage `json:"params"`
	DependsOn        []NodeID        `json:"depends_on,omitempty"`
	RenderedSQL      string          `json:"rendered_sql,omitempty"`
	EstimatedSeconds int             `json:"estimated_seconds,omitempty"`
	Authorization    *Authorization  `json:"authorization,omitempty"`
}

// MarshalJSON writes the node, rejecting a params/kind mismatch rather than
// writing a plan that cannot be read back.
func (n Node) MarshalJSON() ([]byte, error) {
	w := nodeWire{
		ID:               n.ID,
		Kind:             n.Kind,
		Params:           json.RawMessage("null"),
		DependsOn:        n.DependsOn,
		RenderedSQL:      n.RenderedSQL,
		EstimatedSeconds: n.EstimatedSeconds,
		Authorization:    n.Authorization,
	}
	if n.Params != nil {
		if n.Params.Kind() != n.Kind {
			return nil, ErrInvalidPlan.Detailf(
				"node %q: kind is %q but params are for %q", n.ID, n.Kind, n.Params.Kind())
		}
		b, err := marshalNoEscape(n.Params)
		if err != nil {
			return nil, ErrInvalidPlan.Detailf("node %q: %v", n.ID, err)
		}
		w.Params = b
	}
	return marshalNoEscape(w)
}

// marshalNoEscape is json.Marshal without HTML escaping.
//
// The params carry the two operator-authored SQL fragments that reach the
// server verbatim, [IndexColumn.Expression] and [IndexDefinition.Where], and
// the plan file is where a human reviews them (G2, T2). Escaping them here
// would survive into the artifact whatever the outer encoder does, because the
// result is embedded as a json.RawMessage and passed through untouched.
//
// The digest is unaffected either way: canonical.go emits `<`, `>` and `&`
// literally regardless of the encoder.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encoder.Encode appends a newline; a MarshalJSON result must not have one.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// UnmarshalJSON reads the node, decoding params into the concrete type for the
// declared kind. An unknown kind is an error matching [ErrUnknownNodeKind],
// which is what makes NFR-COMPAT-3's version gate meaningful: a plan written by
// a newer binary fails loudly instead of losing a node's parameters.
func (n *Node) UnmarshalJSON(data []byte) error {
	var w nodeWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	n.ID = w.ID
	n.Kind = w.Kind
	n.DependsOn = w.DependsOn
	n.RenderedSQL = w.RenderedSQL
	n.EstimatedSeconds = w.EstimatedSeconds
	n.Authorization = w.Authorization
	n.Params = nil

	if len(w.Params) == 0 || string(w.Params) == "null" {
		return nil
	}
	p, err := NewParams(w.Kind)
	if err != nil {
		return ErrUnknownNodeKind.Detailf("node %q: %q", w.ID, w.Kind)
	}
	if err := json.Unmarshal(w.Params, p); err != nil {
		return ErrInvalidPlan.Detailf("node %q: params: %v", w.ID, err)
	}
	n.Params = p
	return nil
}

// Validate checks the node in isolation: kind, params, and the
// destructive-kind/authorization correspondence. Cross-node checks such as
// dependency resolution and acyclicity live in [Plan.Validate].
func (n *Node) Validate() error {
	if n.ID == "" {
		return ErrInvalidPlan.Detailf("node has an empty id")
	}
	if !n.Kind.Valid() {
		return ErrUnknownNodeKind.Detailf("node %q: %q is not one of %v", n.ID, n.Kind, allNodeKinds)
	}
	if n.Params == nil {
		return ErrInvalidPlan.Detailf("node %q: params are required (FR-PLANFILE-6)", n.ID)
	}
	if n.Params.Kind() != n.Kind {
		return ErrInvalidPlan.Detailf(
			"node %q: kind is %q but params are for %q", n.ID, n.Kind, n.Params.Kind())
	}
	if err := n.Params.Validate(); err != nil {
		return ErrInvalidPlan.Detailf("node %q: %v", n.ID, err)
	}
	if n.EstimatedSeconds < 0 {
		return ErrInvalidPlan.Detailf("node %q: estimated_seconds is negative", n.ID)
	}
	switch {
	case n.Kind.IsDestructive() && n.Authorization == nil:
		return ErrAuthorizationUnsatisfied.Detailf(
			"node %q of destructive kind %q carries no authorization (FR-AUTH-1)", n.ID, n.Kind)
	case !n.Kind.IsDestructive() && n.Authorization != nil:
		return ErrInvalidPlan.Detailf(
			"node %q of non-destructive kind %q carries an authorization", n.ID, n.Kind)
	case n.Authorization != nil:
		if err := n.Authorization.Validate(); err != nil {
			return ErrInvalidPlan.Detailf("node %q: %v", n.ID, err)
		}
	}
	return nil
}
