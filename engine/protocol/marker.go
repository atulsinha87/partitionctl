package protocol

import (
	"encoding/json"
	"strings"
	"time"
)

// MarkerSentinel is the literal that opens every PartitionCTL ownership marker.
// A comment that does not start with it, byte for byte, is not a marker.
const MarkerSentinel = "partitionctl:v1:"

// markerPrefix is the tool prefix without the version. A comment that starts
// with it but not with [MarkerSentinel] was written by a PartitionCTL that
// speaks a version this binary does not, which is [MarkerUnreadable] rather
// than [MarkerForeign]: someone else's *tool* owns the object, and guessing at
// their format is exactly the mistake the version is there to prevent.
const markerPrefix = "partitionctl:"

// The two roles an index plays in a partitioned index family.
const (
	// MarkerRoleParent is the partitioned index created by CREATE INDEX ON ONLY.
	MarkerRoleParent = "parent"
	// MarkerRoleLeaf is a per-partition index, attached or not yet attached.
	MarkerRoleLeaf = "leaf"
)

// Marker is PartitionCTL's ownership record for one catalog object, written
// onto that object as a COMMENT (TRD §7.2.9 as amended; INV-1 amended).
//
// # Why the marker is on the object and not in a side table
//
// A record keyed on a *name* authorizes anything that later occupies that name.
// That is what made a stale record from a completed run able to authorize
// dropping a same-named INVALID index PartitionCTL never created, defeating
// AC-6 and NFR-REL-3. A record read off the object authorizes only that object,
// and reading it is one catalog query against the very thing about to be
// destroyed.
//
// The v0.0 spike measured the two properties this depends on: the comment
// survives REINDEX CONCURRENTLY's internal index swap, and setting one takes
// only ShareUpdateExclusiveLock (docs/spikes/v0.0-results.md, questions 2 and
// 3). It also survives PITR restore and failover, because catalogs restore with
// the data, which is what retires the "state store rewound underneath us" risk.
//
// Every field is tool-generated. Nothing an operator typed is ever interpolated
// into a marker.
type Marker struct {
	// Run is the RunID of the run that created the object.
	Run string `json:"run"`
	// Plan is the full plan digest, "sha256:...", of the reviewed artifact that
	// authorized the creation.
	Plan string `json:"plan"`
	// Op is the [Operation] that created it.
	Op string `json:"op"`
	// Role is [MarkerRoleParent] or [MarkerRoleLeaf].
	Role string `json:"role"`
	// Parent is the partitioned index a leaf belongs to, in
	// [ObjectName.String] form. Set for Role == MarkerRoleLeaf only.
	Parent string `json:"parent,omitempty"`
	// At is when the object was created, RFC 3339 in UTC.
	At string `json:"at"`
	// Reindexed is when the object was last rebuilt by PartitionCTL, RFC 3339
	// in UTC. It is what makes "was this leaf already reindexed?" a catalog
	// question, which is what FR-PLAN-5 needs for reindex resume and what
	// FR-LB-4's gate reads instead of run history.
	Reindexed string `json:"reindexed,omitempty"`
	// ReindexRun is the RunID of the reindex that set Reindexed.
	ReindexRun string `json:"reindex_run,omitempty"`
}

// MarkerStatus is the total classification of whatever comment an object
// carries. Every value other than [MarkerOurs] is a halt in every destructive
// decision (see [DecideProvenanceDrop]).
type MarkerStatus int

// The four marker statuses.
const (
	// MarkerAbsent means the object carries no comment at all.
	MarkerAbsent MarkerStatus = iota
	// MarkerOurs means the sentinel is present, the version is v1, and the
	// payload parses. This is the only status that proves ownership.
	MarkerOurs
	// MarkerForeign means a comment exists and is not a PartitionCTL marker.
	// Somebody wrote it deliberately; it is never overwritten and never
	// authorizes anything.
	MarkerForeign
	// MarkerUnreadable means the tool prefix is present but the payload is not
	// v1 JSON this binary can read: a newer PartitionCTL owns the object, or the
	// comment was corrupted. Either way this binary must not act on it.
	MarkerUnreadable
)

// String renders the status for messages and audit detail.
func (s MarkerStatus) String() string {
	switch s {
	case MarkerAbsent:
		return "absent"
	case MarkerOurs:
		return "ours"
	case MarkerForeign:
		return "foreign"
	case MarkerUnreadable:
		return "unreadable"
	}
	return "unknown"
}

// Validate checks that a marker about to be written names everything a later
// reader needs. It is not called on parse: [ParseMarker] is deliberately total.
func (m Marker) Validate() error {
	if m.Run == "" {
		return ErrInvalidPlan.Detailf("marker has no run id")
	}
	if m.Op == "" {
		return ErrInvalidPlan.Detailf("marker has no operation")
	}
	switch m.Role {
	case MarkerRoleParent, MarkerRoleLeaf:
	default:
		return ErrInvalidPlan.Detailf(
			"marker role %q is not %q or %q", m.Role, MarkerRoleParent, MarkerRoleLeaf)
	}
	if m.At == "" {
		return ErrInvalidPlan.Detailf("marker has no creation timestamp")
	}
	return nil
}

// MarkerTime formats t the way a marker records an instant: RFC 3339, UTC,
// second precision. Second precision is deliberate — the marker is read by
// humans in psql as often as by this binary, and sub-second digits buy nothing
// that `at` is used for.
func MarkerTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// FormatMarker renders a marker as the exact comment text to write: the
// sentinel followed by compact JSON, on one line.
func FormatMarker(m Marker) (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	b, err := marshalNoEscape(m)
	if err != nil {
		return "", ErrInvalidPlan.Detailf("marker: %v", err)
	}
	return MarkerSentinel + string(b), nil
}

// ParseMarker classifies a comment. It is total: it never returns an error, and
// every non-[MarkerOurs] answer is a refusal rather than a diagnosis.
//
// Surrounding whitespace is tolerated because a comment round-trips through
// psql and through humans. Nothing else is.
func ParseMarker(comment string) (Marker, MarkerStatus) {
	s := strings.TrimSpace(comment)
	switch {
	case s == "":
		return Marker{}, MarkerAbsent
	case strings.HasPrefix(s, MarkerSentinel):
		var m Marker
		if err := json.Unmarshal([]byte(strings.TrimPrefix(s, MarkerSentinel)), &m); err != nil {
			return Marker{}, MarkerUnreadable
		}
		if m.Run == "" || m.Role == "" {
			// A marker that names no run and no role proves nothing, and
			// treating it as ours would authorize a drop on the strength of the
			// sentinel alone.
			return Marker{}, MarkerUnreadable
		}
		return m, MarkerOurs
	case strings.HasPrefix(s, markerPrefix):
		return Marker{}, MarkerUnreadable
	}
	return Marker{}, MarkerForeign
}

// RenderComment renders COMMENT ON INDEX <index> IS '<marker>'.
//
// The literal is escaped by doubling the single quote, which is the whole
// escape: PostgreSQL processes no backslash escapes inside a standard string
// literal when standard_conforming_strings is on, which it is by default on
// every supported version (NFR-COMPAT-1).
func RenderComment(index ObjectName, marker string) string {
	return "COMMENT ON INDEX " + index.Quoted() + " IS " + QuoteLiteral(marker)
}

// ---------------------------------------------------------------------------
// Which nodes write a marker, and what they write
// ---------------------------------------------------------------------------

// MarkerTarget is the object one node marks and the ownership facts to record
// about it.
type MarkerTarget struct {
	// Index is the object the COMMENT is written on.
	Index ObjectName
	// Role is [MarkerRoleParent] or [MarkerRoleLeaf].
	Role string
	// Parent is the partitioned index a leaf belongs to, or empty.
	Parent string
	// Rewrite reports that the node updates an existing marker rather than
	// establishing one: index.reindex_concurrently stamps `reindexed` onto the
	// marker already on the object.
	Rewrite bool
}

// MarkerTargetFor reports the marker a node writes after its statement returns,
// if any. Like dispatch, it switches on kind alone.
//
// The kinds that mark, and why each one does:
//
//   - index.create_parent_invalid claims the parent index.
//   - index.create_concurrently claims the leaf, immediately after the build.
//   - index.attach rewrites the same leaf marker unconditionally. That is the
//     backstop for the one window the checkpoint cannot cover: a crash between
//     the CREATE INDEX CONCURRENTLY and its COMMENT, where the run is never
//     resumed and the tree is re-planned instead. The attach still runs, and it
//     marks the leaf.
//   - index.reindex_concurrently stamps `reindexed` and `reindex_run` onto the
//     marker already there, which is what makes reindex resumable per leaf
//     (FR-PLAN-5) and what lets FR-LB-4's gate ask a catalog question.
//
// The two drop kinds write nothing: there is nothing left to mark. The three
// kinds that issue no DDL write nothing either.
func MarkerTargetFor(n *Node) (MarkerTarget, bool, error) {
	if n == nil {
		return MarkerTarget{}, false, ErrInvalidPlan.Detailf("cannot mark a nil node")
	}
	switch n.Kind {
	case KindIndexCreateParentInvalid:
		p, ok := n.Params.(*CreateParentInvalidParams)
		if !ok {
			return MarkerTarget{}, false, paramsMismatch(n)
		}
		return MarkerTarget{Index: p.Index, Role: MarkerRoleParent}, true, nil

	case KindIndexCreateConcurrently:
		p, ok := n.Params.(*CreateConcurrentlyParams)
		if !ok {
			return MarkerTarget{}, false, paramsMismatch(n)
		}
		t := MarkerTarget{Index: p.Index, Role: MarkerRoleLeaf}
		if p.ParentIndex != nil {
			t.Parent = p.ParentIndex.String()
		}
		return t, true, nil

	case KindIndexAttach:
		p, ok := n.Params.(*AttachParams)
		if !ok {
			return MarkerTarget{}, false, paramsMismatch(n)
		}
		return MarkerTarget{
			Index:  p.ChildIndex,
			Role:   MarkerRoleLeaf,
			Parent: p.ParentIndex.String(),
		}, true, nil

	case KindIndexReindexConcurrently:
		p, ok := n.Params.(*ReindexConcurrentlyParams)
		if !ok {
			return MarkerTarget{}, false, paramsMismatch(n)
		}
		t := MarkerTarget{Index: p.Index, Role: MarkerRoleLeaf, Rewrite: true}
		if p.ParentIndex != nil {
			t.Parent = p.ParentIndex.String()
		}
		return t, true, nil
	}
	return MarkerTarget{}, false, nil
}

func paramsMismatch(n *Node) error {
	return ErrInvalidPlan.Detailf("node %q of kind %q carries params of type %T", n.ID, n.Kind, n.Params)
}

// RenderMarkerStatement renders the COMMENT statement a node writes after its
// primary statement, using base for the run, plan, operation and timestamp
// fields the caller owns. It returns ok false for the kinds that mark nothing.
//
// prior is the marker already on the object, for the rewrite case; pass the
// zero value when there is none. Only [MarkerTarget.Rewrite] kinds read it, and
// they preserve its creation facts so that a reindex does not erase who built
// the index.
//
// A rewrite over a comment that is [MarkerForeign] or [MarkerUnreadable]
// returns ok false rather than a statement. The rebuild has already succeeded
// and the index is healthy; overwriting somebody else's comment to record that
// fact is not a trade this tool makes. The leaf simply reads as un-reindexed on
// the next plan and is rebuilt again, which is wasteful, safe and convergent.
func RenderMarkerStatement(n *Node, base Marker, prior Marker, priorStatus MarkerStatus) (string, bool, error) {
	t, ok, err := MarkerTargetFor(n)
	if err != nil || !ok {
		return "", false, err
	}
	m := base
	m.Role = t.Role
	if t.Parent != "" {
		m.Parent = t.Parent
	}
	if t.Rewrite {
		switch priorStatus {
		case MarkerForeign, MarkerUnreadable:
			return "", false, nil
		case MarkerOurs:
			// Keep who built it and when; record only that we rebuilt it.
			m.Op, m.Run, m.At = prior.Op, prior.Run, prior.At
			if prior.Parent != "" {
				m.Parent = prior.Parent
			}
			if prior.Role != "" {
				m.Role = prior.Role
			}
		}
		m.Reindexed = base.At
		m.ReindexRun = base.Run
	}
	text, err := FormatMarker(m)
	if err != nil {
		return "", false, err
	}
	return RenderComment(t.Index, text), true, nil
}
