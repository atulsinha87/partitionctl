package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
)

// FingerprintPrefix names the hash algorithm inside the fingerprint string.
const FingerprintPrefix = "sha256:"

// topologyDomain separates the fingerprint's hash input from every other
// SHA-256 in the system, so a value can never be reinterpreted as a digest.
const topologyDomain = "partitionctl.topology.v1"

// PartitionStrategy is a partitioned table's strategy, as reported by
// pg_partitioned_table.
type PartitionStrategy string

// The partition strategies. v0.1 supports RANGE and LIST; HASH is a hard
// planner error (FR-PLAN-3, AC-11).
const (
	StrategyRange PartitionStrategy = "RANGE"
	StrategyList  PartitionStrategy = "LIST"
	StrategyHash  PartitionStrategy = "HASH"
)

// SupportedInV01 reports whether v0.1 can plan against this strategy.
func (s PartitionStrategy) SupportedInV01() bool {
	return s == StrategyRange || s == StrategyList
}

// RelationState is the catalog state of one relation that the fingerprint
// covers.
//
// The field set is deliberately structural. Statistics such as relpages change
// on every autovacuum, so including them would make drift detection fire
// constantly and train operators to pass --allow-drift (R6). Index state is
// excluded for a sharper reason: the executor's own DDL changes it, so a
// fingerprint that covered indexes could not survive its own run and resume
// would always report drift.
type RelationState struct {
	// OID is pg_class.oid. It is the identity: a partition dropped and
	// recreated with the same name is a different relation.
	OID uint32 `json:"oid"`
	// Schema is the containing schema.
	Schema string `json:"schema"`
	// Name is the relation name.
	Name string `json:"name"`
	// RelKind is pg_class.relkind: 'p' for a partitioned table, 'r' for a
	// leaf.
	RelKind string `json:"relkind"`
	// OwnerOID is pg_class.relowner. It changes when ownership changes, which
	// invalidates the role-membership check made at plan time (FR-PLAN-10).
	OwnerOID uint32 `json:"owner_oid,omitempty"`
	// ParentOID is the partitioned parent, or 0 for the root.
	ParentOID uint32 `json:"parent_oid,omitempty"`
	// PartitionBound is the partition bound expression, as returned by
	// pg_get_expr on relpartbound. A bound that moved is drift even if the
	// partition set is unchanged.
	PartitionBound string `json:"partition_bound,omitempty"`
	// IsDefault marks a DEFAULT partition, which v0.1 rejects (FR-PLAN-3).
	IsDefault bool `json:"is_default,omitempty"`
}

// Name of the relation in schema.name form, for messages.
func (r RelationState) String() string {
	return ObjectName{Schema: r.Schema, Name: r.Name}.String()
}

// TopologyInput is what the fingerprint is computed over: the discovered
// partition tree and its relevant catalog state (FR-PLANFILE-4).
type TopologyInput struct {
	// Root is the partitioned parent table.
	Root RelationState `json:"root"`
	// Strategy is the root's partitioning strategy.
	Strategy PartitionStrategy `json:"strategy"`
	// Partitions are the discovered leaf partitions. Order does not affect the
	// fingerprint.
	Partitions []RelationState `json:"partitions"`
}

// Validate checks the input is well formed enough to fingerprint.
func (t TopologyInput) Validate() error {
	if t.Root.Name == "" {
		return ErrInvalidPlan.Detailf("topology: root relation has no name")
	}
	seen := make(map[uint32]RelationState, len(t.Partitions))
	for i, p := range t.Partitions {
		if p.Name == "" {
			return ErrInvalidPlan.Detailf("topology: partition %d has no name", i)
		}
		if prev, dup := seen[p.OID]; dup {
			return ErrInvalidPlan.Detailf(
				"topology: partitions %s and %s share OID %d", prev, p, p.OID)
		}
		seen[p.OID] = p
	}
	return nil
}

// Fingerprint hashes the topology (FR-PLANFILE-4).
//
// The result is independent of the order of Partitions: each partition is
// hashed to its own digest, the digests are sorted, and the sorted list is
// hashed. Sorting rather than combining with XOR matters, because XOR lets a
// duplicated element cancel itself out.
func (t TopologyInput) Fingerprint() (string, error) {
	if err := t.Validate(); err != nil {
		return "", err
	}

	rootCanon, err := canonicalOf(t.Root)
	if err != nil {
		return "", err
	}
	rootSum := sha256.Sum256(rootCanon)

	elems := make([][]byte, 0, len(t.Partitions))
	for _, p := range t.Partitions {
		canon, err := canonicalOf(p)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(canon)
		elems = append(elems, sum[:])
	}
	sort.Slice(elems, func(i, j int) bool { return bytes.Compare(elems[i], elems[j]) < 0 })

	h := sha256.New()
	writeLengthPrefixed(h, []byte(topologyDomain))
	writeLengthPrefixed(h, []byte(t.Strategy))
	h.Write(rootSum[:])
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(elems)))
	h.Write(count[:])
	for _, e := range elems {
		h.Write(e)
	}
	return FingerprintPrefix + hex.EncodeToString(h.Sum(nil)), nil
}

// ComputeTopologyFingerprint is the package-level form of
// [TopologyInput.Fingerprint].
func ComputeTopologyFingerprint(t TopologyInput) (string, error) { return t.Fingerprint() }

// VerifyTopology recomputes the fingerprint against live catalog state and
// reports drift (FR-PLANFILE-5, AC-3). It returns an error matching
// [ErrTopologyDrift], which maps to exit code 11.
//
// The caller may still proceed on --allow-drift; that decision is the CLI's.
func (p *Plan) VerifyTopology(live TopologyInput) error {
	if p.TopologyFingerprint == "" {
		return ErrTopologyDrift.Detailf("plan %q carries no topology fingerprint", p.PlanID)
	}
	got, err := live.Fingerprint()
	if err != nil {
		return err
	}
	if got != p.TopologyFingerprint {
		return ErrTopologyDrift.Detailf(
			"plan %q: planned topology %s, live topology %s", p.PlanID, p.TopologyFingerprint, got)
	}
	return nil
}

// TopologyChangeKind classifies one difference between two topologies.
type TopologyChangeKind string

// The kinds of drift.
const (
	TopologyRootChanged      TopologyChangeKind = "root_changed"
	TopologyStrategyChanged  TopologyChangeKind = "strategy_changed"
	TopologyPartitionAdded   TopologyChangeKind = "partition_added"
	TopologyPartitionRemoved TopologyChangeKind = "partition_removed"
	TopologyPartitionChanged TopologyChangeKind = "partition_changed"
)

// TopologyChange is one named difference, so that `execute` can tell the
// operator *what* drifted rather than only that something did (AC-3).
type TopologyChange struct {
	Change   TopologyChangeKind `json:"change"`
	Relation string             `json:"relation,omitempty"`
	OID      uint32             `json:"oid,omitempty"`
	Detail   string             `json:"detail,omitempty"`
}

func (c TopologyChange) String() string {
	s := string(c.Change)
	if c.Relation != "" {
		s += " " + c.Relation
	}
	if c.Detail != "" {
		s += ": " + c.Detail
	}
	return s
}

// DiffTopology names the differences between the planned and live topologies,
// matching partitions by OID. The result is deterministic: root and strategy
// changes first, then partition changes ordered by OID. It is empty when the
// two fingerprint identically.
func DiffTopology(planned, live TopologyInput) []TopologyChange {
	var changes []TopologyChange

	if planned.Root != live.Root {
		changes = append(changes, TopologyChange{
			Change:   TopologyRootChanged,
			Relation: live.Root.String(),
			OID:      live.Root.OID,
			Detail:   fmt.Sprintf("planned %+v, live %+v", planned.Root, live.Root),
		})
	}
	if planned.Strategy != live.Strategy {
		changes = append(changes, TopologyChange{
			Change: TopologyStrategyChanged,
			Detail: fmt.Sprintf("planned %s, live %s", planned.Strategy, live.Strategy),
		})
	}

	plannedByOID := make(map[uint32]RelationState, len(planned.Partitions))
	liveByOID := make(map[uint32]RelationState, len(live.Partitions))
	oids := make([]uint32, 0, len(planned.Partitions)+len(live.Partitions))
	for _, p := range planned.Partitions {
		if _, ok := plannedByOID[p.OID]; !ok {
			oids = append(oids, p.OID)
		}
		plannedByOID[p.OID] = p
	}
	for _, p := range live.Partitions {
		if _, ok := plannedByOID[p.OID]; !ok {
			if _, dup := liveByOID[p.OID]; !dup {
				oids = append(oids, p.OID)
			}
		}
		liveByOID[p.OID] = p
	}
	sort.Slice(oids, func(i, j int) bool { return oids[i] < oids[j] })

	for _, oid := range oids {
		was, inPlanned := plannedByOID[oid]
		now, inLive := liveByOID[oid]
		switch {
		case inPlanned && !inLive:
			changes = append(changes, TopologyChange{
				Change: TopologyPartitionRemoved, Relation: was.String(), OID: oid,
			})
		case !inPlanned && inLive:
			changes = append(changes, TopologyChange{
				Change: TopologyPartitionAdded, Relation: now.String(), OID: oid,
			})
		case was != now:
			changes = append(changes, TopologyChange{
				Change: TopologyPartitionChanged, Relation: now.String(), OID: oid,
				Detail: fmt.Sprintf("planned %+v, live %+v", was, now),
			})
		}
	}
	return changes
}

// writeLengthPrefixed frames a field so that concatenation is injective: no two
// different field sequences can produce the same byte stream.
func writeLengthPrefixed(h interface{ Write([]byte) (int, error) }, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	h.Write(n[:])
	h.Write(b)
}
