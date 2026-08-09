package planner

import (
	"context"
	"sort"

	"github.com/atulsinha87/partitionctl/engine/protocol"
)

// FakeCatalog is a complete in-memory [CatalogReader].
//
// It exists so that every planner and every operation can be tested without a
// live PostgreSQL. It is not a mock: it derives the partition tree from
// ParentOID links exactly the way pg_partition_tree() does, so a test declares
// relations and gets a tree, including the level and isleaf columns. A test
// that builds an unsupported topology here gets the same rejection it would get
// from the server.
//
// The zero value is not usable; call [NewFakeCatalog].
type FakeCatalog struct {
	// Role is what CurrentRole returns.
	Role string
	// Database is what CurrentDatabase returns.
	Database string
	// ServerVersion is what ServerVersionNum returns.
	ServerVersion int
	// ReadOnly is what AssertReadOnly reports. True by default, because the
	// supported configuration is a read-only transaction.
	ReadOnly bool

	// Relations is every relation in the fake catalog, keyed by OID.
	Relations map[uint32]Relation
	// Strategies maps a partitioned table's OID to its strategy.
	Strategies map[uint32]protocol.PartitionStrategy
	// Indexes is every index in the fake catalog.
	Indexes []Index
	// Comments maps an index's identity form (schema.name) to the comment on
	// it, which is where the ownership marker lives. Use [FakeCatalog.Mark] to
	// set a well-formed one.
	Comments map[string]string
	// Roles maps a role OID to its name.
	Roles map[uint32]string
	// Members maps a role OID to whether [FakeCatalog.Role] holds its
	// privileges. A missing entry means "not a member", which is the safe
	// default and the one that exercises FR-PLAN-10.
	Members map[uint32]bool

	// MembersByRole overrides Members for a named role. It exists because
	// membership is a question about a *specific* role, and a plan is
	// frequently executed by a different role than the one that planned it, so
	// a fake that cannot tell two roles apart cannot exercise FR-PLAN-10 at
	// execution time at all. A role with no entry here falls back to Members.
	MembersByRole map[string]map[uint32]bool

	// Err, when non-nil, is returned by every method. It is how a test
	// exercises the "catalog unreachable" path.
	Err error

	// Calls counts the calls to each method, so a test can assert that
	// discovery is O(1) queries in the number of partitions (NFR-PERF-1).
	Calls map[string]int
}

// NewFakeCatalog returns an empty fake with usable defaults: a supported server
// version, a read-only session, and a connected role named "migrator".
func NewFakeCatalog() *FakeCatalog {
	return &FakeCatalog{
		Role:          "migrator",
		Database:      "appdb",
		ServerVersion: 160000,
		ReadOnly:      true,
		Relations:     map[uint32]Relation{},
		Strategies:    map[uint32]protocol.PartitionStrategy{},
		Roles:         map[uint32]string{},
		Members:       map[uint32]bool{},
		Calls:         map[string]int{},
	}
}

// AddRole registers a role and whether the connected role holds its privileges.
func (f *FakeCatalog) AddRole(oid uint32, name string, member bool) *FakeCatalog {
	f.Roles[oid] = name
	f.Members[oid] = member
	return f
}

// AddRelation registers a relation. Its IsDefault flag is derived from its
// partition bound if it was not set explicitly, mirroring what the SQL reader
// does.
func (f *FakeCatalog) AddRelation(r Relation) *FakeCatalog {
	r.IsDefault = r.IsDefault || IsDefaultBound(r.PartitionBound)
	if r.Owner == "" {
		r.Owner = f.Roles[r.OwnerOID]
	}
	f.Relations[r.OID] = r
	return f
}

// AddIndex registers an index.
func (f *FakeCatalog) AddIndex(i Index) *FakeCatalog {
	f.Indexes = append(f.Indexes, i)
	return f
}

// SetStrategy registers a partitioned table's strategy.
func (f *FakeCatalog) SetStrategy(oid uint32, s protocol.PartitionStrategy) *FakeCatalog {
	f.Strategies[oid] = s
	return f
}

func (f *FakeCatalog) call(name string) error {
	if f.Calls == nil {
		f.Calls = map[string]int{}
	}
	f.Calls[name]++
	return f.Err
}

// AssertReadOnly implements [ReadOnlyAsserter].
func (f *FakeCatalog) AssertReadOnly(ctx context.Context) error {
	if err := f.call("AssertReadOnly"); err != nil {
		return err
	}
	if !f.ReadOnly {
		return ErrNotReadOnly.Detailf("fake catalog is configured writable")
	}
	return nil
}

// CurrentRole implements [CatalogReader].
func (f *FakeCatalog) CurrentRole(ctx context.Context) (string, error) {
	if err := f.call("CurrentRole"); err != nil {
		return "", err
	}
	return f.Role, nil
}

// CurrentDatabase implements [CatalogReader].
func (f *FakeCatalog) CurrentDatabase(ctx context.Context) (string, error) {
	if err := f.call("CurrentDatabase"); err != nil {
		return "", err
	}
	return f.Database, nil
}

// ServerVersionNum implements [CatalogReader].
func (f *FakeCatalog) ServerVersionNum(ctx context.Context) (int, error) {
	if err := f.call("ServerVersionNum"); err != nil {
		return 0, err
	}
	return f.ServerVersion, nil
}

// LookupRelation implements [CatalogReader]. An empty schema matches any
// schema, which stands in for search_path resolution.
func (f *FakeCatalog) LookupRelation(ctx context.Context, name protocol.ObjectName) (Relation, error) {
	if err := f.call("LookupRelation"); err != nil {
		return Relation{}, err
	}
	var found []Relation
	for _, r := range f.Relations {
		if r.Name.Name != name.Name {
			continue
		}
		if name.Schema != "" && r.Name.Schema != name.Schema {
			continue
		}
		found = append(found, r)
	}
	sortRelations(found)
	switch len(found) {
	case 0:
		return Relation{}, ErrRelationNotFound.Detailf(
			"%s does not exist, or is not visible to the connected role", name.String())
	case 1:
		return found[0], nil
	default:
		return Relation{}, ErrAmbiguousRelation.Detailf(
			"%q resolves to %d relations; qualify it with a schema", name.String(), len(found))
	}
}

// PartitionTree implements [CatalogReader] by walking ParentOID links, which is
// the relationship pg_partition_tree() reports.
func (f *FakeCatalog) PartitionTree(ctx context.Context, rootOID uint32) ([]TreeEntry, error) {
	if err := f.call("PartitionTree"); err != nil {
		return nil, err
	}
	root, ok := f.Relations[rootOID]
	if !ok {
		return nil, ErrRelationNotFound.Detailf("oid %d does not exist", rootOID)
	}

	children := map[uint32][]Relation{}
	for _, r := range f.Relations {
		if r.ParentOID != 0 {
			children[r.ParentOID] = append(children[r.ParentOID], r)
		}
	}

	var out []TreeEntry
	var walk func(r Relation, level int)
	walk = func(r Relation, level int) {
		out = append(out, TreeEntry{
			Relation: r,
			Level:    level,
			IsLeaf:   !r.Kind.IsPartitioned(),
		})
		kids := children[r.OID]
		sortRelations(kids)
		for _, k := range kids {
			walk(k, level+1)
		}
	}
	walk(root, 0)

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Level != out[j].Level {
			return out[i].Level < out[j].Level
		}
		if out[i].Name.Schema != out[j].Name.Schema {
			return out[i].Name.Schema < out[j].Name.Schema
		}
		return out[i].Name.Name < out[j].Name.Name
	})
	return out, nil
}

// PartitionStrategy implements [CatalogReader].
func (f *FakeCatalog) PartitionStrategy(ctx context.Context, rootOID uint32) (protocol.PartitionStrategy, error) {
	if err := f.call("PartitionStrategy"); err != nil {
		return "", err
	}
	s, ok := f.Strategies[rootOID]
	if !ok {
		r := f.Relations[rootOID]
		return "", topologyErr(CodeNotPartitioned, r.Name.String(),
			"the relation has no pg_partitioned_table row, so it is not partitioned")
	}
	return s, nil
}

// IndexesOnRelations implements [CatalogReader].
func (f *FakeCatalog) IndexesOnRelations(ctx context.Context, tableOIDs []uint32) ([]Index, error) {
	if err := f.call("IndexesOnRelations"); err != nil {
		return nil, err
	}
	want := make(map[uint32]struct{}, len(tableOIDs))
	for _, oid := range tableOIDs {
		want[oid] = struct{}{}
	}
	var out []Index
	for _, idx := range f.Indexes {
		if _, ok := want[idx.TableOID]; !ok {
			continue
		}
		// The real reader joins obj_description into the same row, so a comment
		// set through Comment or Mark has to ride along here too. Without this
		// the batched path — the one every planner actually uses — would never
		// see a marker, and a test that called Mark would silently be testing
		// nothing (NFR-PERF-1 is why the marker travels with the index state).
		if idx.Comment == "" {
			if c, ok := f.Comments[idx.Name.String()]; ok {
				idx.Comment = c
			}
		}
		out = append(out, idx)
	}
	sortIndexes(out)
	return out, nil
}

// LookupIndex implements [CatalogReader].
func (f *FakeCatalog) LookupIndex(ctx context.Context, name protocol.ObjectName) (Index, error) {
	if err := f.call("LookupIndex"); err != nil {
		return Index{}, err
	}
	var found []Index
	for _, idx := range f.Indexes {
		if idx.Name.Name != name.Name {
			continue
		}
		if name.Schema != "" && idx.Name.Schema != name.Schema {
			continue
		}
		found = append(found, idx)
	}
	sortIndexes(found)
	switch len(found) {
	case 0:
		return Index{}, ErrIndexNotFound.Detailf(
			"%s does not exist, or is not visible to the connected role", name.String())
	case 1:
		return found[0], nil
	default:
		return Index{}, ErrAmbiguousRelation.Detailf(
			"index %q resolves to %d indexes; qualify it with a schema", name.String(), len(found))
	}
}

// RoleMemberships implements [CatalogReader]. An OID with no registered role is
// omitted from the result, exactly as it would be if pg_roles had no such row.
func (f *FakeCatalog) RoleMemberships(ctx context.Context, role string, ownerOIDs []uint32) (map[uint32]RoleMembership, error) {
	if err := f.call("RoleMemberships"); err != nil {
		return nil, err
	}
	out := make(map[uint32]RoleMembership, len(ownerOIDs))
	for _, oid := range ownerOIDs {
		name, known := f.Roles[oid]
		if !known {
			continue
		}
		out[oid] = RoleMembership{OwnerOID: oid, OwnerName: name, IsMember: f.isMember(role, oid)}
	}
	return out, nil
}

// isMember answers membership for a specific role, falling back to the
// role-agnostic Members map.
func (f *FakeCatalog) isMember(role string, ownerOID uint32) bool {
	if perRole, ok := f.MembersByRole[role]; ok {
		return perRole[ownerOID]
	}
	return f.Members[ownerOID]
}

// SetRoleMember registers whether a named role holds an owning role's
// privileges, independently of [FakeCatalog.Role].
func (f *FakeCatalog) SetRoleMember(role string, ownerOID uint32, member bool) *FakeCatalog {
	if f.MembersByRole == nil {
		f.MembersByRole = map[string]map[uint32]bool{}
	}
	if f.MembersByRole[role] == nil {
		f.MembersByRole[role] = map[uint32]bool{}
	}
	f.MembersByRole[role][ownerOID] = member
	return f
}

func sortRelations(rs []Relation) {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].Name.Schema != rs[j].Name.Schema {
			return rs[i].Name.Schema < rs[j].Name.Schema
		}
		if rs[i].Name.Name != rs[j].Name.Name {
			return rs[i].Name.Name < rs[j].Name.Name
		}
		return rs[i].OID < rs[j].OID
	})
}

func sortIndexes(is []Index) {
	sort.Slice(is, func(i, j int) bool {
		if is[i].Table.String() != is[j].Table.String() {
			return is[i].Table.String() < is[j].Table.String()
		}
		if is[i].Name.String() != is[j].Name.String() {
			return is[i].Name.String() < is[j].Name.String()
		}
		return is[i].OID < is[j].OID
	})
}

// IndexComment implements [CatalogReader].
func (f *FakeCatalog) IndexComment(ctx context.Context, index protocol.ObjectName) (string, bool, error) {
	if err := f.call("IndexComment"); err != nil {
		return "", false, err
	}
	// The comment normally rides along with the index state; this method is the
	// per-index fallback, so it answers from either place.
	for _, i := range f.Indexes {
		if i.Name == index && i.Comment != "" {
			return i.Comment, true, nil
		}
	}
	comment, ok := f.Comments[index.String()]
	if !ok || comment == "" {
		// An unqualified name resolves the way the server would: by bare name,
		// since the fake holds one search path.
		if index.Schema == "" {
			for name, c := range f.Comments {
				if o, err := protocol.ParseObjectName(name); err == nil && o.Name == index.Name && c != "" {
					return c, true, nil
				}
			}
		}
		return "", false, nil
	}
	return comment, true, nil
}

// Comment sets an arbitrary comment on an index, which is how a test builds the
// "somebody else wrote this" case.
func (f *FakeCatalog) Comment(index protocol.ObjectName, comment string) *FakeCatalog {
	if f.Comments == nil {
		f.Comments = map[string]string{}
	}
	f.Comments[index.String()] = comment
	return f
}

// Mark writes a well-formed PartitionCTL ownership marker onto an index,
// claiming it for the named run. It panics on a malformed marker, which is a
// test-construction error rather than a condition to handle.
func (f *FakeCatalog) Mark(index protocol.ObjectName, run string) *FakeCatalog {
	text, err := protocol.FormatMarker(protocol.Marker{
		Run: run, Plan: "sha256:fake", Op: string(protocol.OpCreateIndex),
		Role: protocol.MarkerRoleLeaf, At: "2026-08-07T12:00:00Z",
	})
	if err != nil {
		panic("planner: FakeCatalog.Mark: " + err.Error())
	}
	return f.Comment(index, text)
}

// FakeClaims is an in-memory [ClaimLookup] keyed on object identity. It stands
// in for the node checkpoints a run that died mid-statement left behind.
type FakeClaims struct {
	// Runs maps an object's identity form (schema.name) to the run holding a
	// live claim on it.
	Runs map[string]string
	// Err, when non-nil, is returned by every lookup.
	Err error
}

// NewFakeClaims returns a claim source holding a claim on each object, all from
// one notional crashed run.
func NewFakeClaims(objects ...protocol.ObjectName) *FakeClaims {
	f := &FakeClaims{Runs: map[string]string{}}
	for _, o := range objects {
		f.Runs[o.String()] = "run-crashed"
	}
	return f
}

// ClaimsObject implements [ClaimLookup].
func (f *FakeClaims) ClaimsObject(ctx context.Context, object protocol.ObjectName) (string, bool, error) {
	if f.Err != nil {
		return "", false, f.Err
	}
	run, ok := f.Runs[object.String()]
	return run, ok, nil
}

var (
	_ CatalogReader    = (*FakeCatalog)(nil)
	_ ReadOnlyAsserter = (*FakeCatalog)(nil)
	_ ClaimLookup      = (*FakeClaims)(nil)
)
