package protocol

import "fmt"

// NodeParams is the authoritative, structured execution input for one node
// (FR-PLANFILE-6). Each [NodeKind] has exactly one concrete params type, and
// every field round-trips through JSON without loss.
//
// The interface is sealed by an unexported method: the nine kinds are the whole
// vocabulary, and adding one is a versioned engine change (TRD §7.2.2).
// Construct params with the concrete types, or with [NewParams] when the kind
// is only known at run time.
type NodeParams interface {
	// Kind returns the node kind these params belong to.
	Kind() NodeKind
	// Validate checks the params for structural completeness and validates
	// every identifier they carry.
	Validate() error

	nodeParams()
}

// NewParams returns zero-valued params of the concrete type for kind, ready to
// be unmarshalled into. It returns an error matching [ErrUnknownNodeKind] for a
// kind outside the vocabulary.
func NewParams(kind NodeKind) (NodeParams, error) {
	switch kind {
	case KindCatalogAssert:
		return &CatalogAssertParams{}, nil
	case KindIndexCreateParentInvalid:
		return &CreateParentInvalidParams{}, nil
	case KindIndexCreateConcurrently:
		return &CreateConcurrentlyParams{}, nil
	case KindIndexAttach:
		return &AttachParams{}, nil
	case KindIndexVerify:
		return &VerifyParams{}, nil
	case KindWait:
		return &WaitParams{}, nil
	case KindIndexDropConcurrently:
		return &DropConcurrentlyParams{}, nil
	case KindIndexReindexConcurrently:
		return &ReindexConcurrentlyParams{}, nil
	case KindIndexDropPartitioned:
		return &DropPartitionedParams{}, nil
	}
	return nil, CheckNodeKind(kind)
}

// ---------------------------------------------------------------------------
// Index definition, shared by the two create kinds.
// ---------------------------------------------------------------------------

// IndexDefinition is the structured form of an index's shape. It is what the
// executor re-renders into SQL; [Node.RenderedSQL] is ignored (FR-PLANFILE-7).
type IndexDefinition struct {
	// Method is the access method, for example "btree" or "gin". Empty means
	// the server default.
	Method string `json:"method,omitempty"`
	// Columns are the indexed columns or expressions, in order. At least one
	// is required.
	Columns []IndexColumn `json:"columns"`
	// Unique requests a UNIQUE index.
	Unique bool `json:"unique,omitempty"`
	// Include lists non-key INCLUDE columns, in order.
	Include []string `json:"include,omitempty"`
	// Where is the partial-index predicate, an operator-authored SQL fragment.
	// See the package doc's trust boundary note.
	Where string `json:"where,omitempty"`
	// Tablespace names a tablespace, or is empty for the default.
	Tablespace string `json:"tablespace,omitempty"`
	// StorageParams are WITH (...) options, for example {"fillfactor": "90"}.
	// Values are rendered as literals; keys are identifiers.
	StorageParams map[string]string `json:"storage_params,omitempty"`
}

// Validate checks the definition and every identifier in it.
func (d IndexDefinition) Validate() error {
	if d.Method != "" {
		// pg_am has no namespace column, so an access method is never
		// schema-qualified. Catching a dotted value here turns a mid-run 42704
		// into a plan-time refusal.
		if err := ValidateSimpleIdentifier(d.Method); err != nil {
			return fmt.Errorf("method: %w", err)
		}
	}
	if len(d.Columns) == 0 {
		return fmt.Errorf("index definition has no columns")
	}
	for i, c := range d.Columns {
		if err := c.Validate(); err != nil {
			return fmt.Errorf("column %d: %w", i, err)
		}
	}
	for i, c := range d.Include {
		if err := ValidateIdentifier(c); err != nil {
			return fmt.Errorf("include %d: %w", i, err)
		}
	}
	if d.Where != "" {
		if err := validateSQLFragment("where", d.Where); err != nil {
			return err
		}
	}
	if d.Tablespace != "" {
		if err := ValidateIdentifier(d.Tablespace); err != nil {
			return fmt.Errorf("tablespace: %w", err)
		}
	}
	for k := range d.StorageParams {
		if err := ValidateIdentifier(k); err != nil {
			return fmt.Errorf("storage param key: %w", err)
		}
	}
	return nil
}

// IndexColumn is one key column of an index. Exactly one of Name and
// Expression must be set.
type IndexColumn struct {
	// Name is a plain column name, quoted on render.
	Name string `json:"name,omitempty"`
	// Expression is an operator-authored SQL expression, for example
	// "lower(email)". It is not identifier-quoted. See the package doc's trust
	// boundary note.
	Expression string `json:"expression,omitempty"`
	// Collation names a collation, or is empty for the default.
	Collation string `json:"collation,omitempty"`
	// OpClass names an operator class, or is empty for the default.
	OpClass string `json:"opclass,omitempty"`
	// Descending requests DESC.
	Descending bool `json:"descending,omitempty"`
	// NullsFirst requests NULLS FIRST when true and NULLS LAST when false.
	// Nil leaves the server default, which depends on Descending.
	NullsFirst *bool `json:"nulls_first,omitempty"`
}

// Validate checks that exactly one of Name and Expression is set and that
// every identifier is well formed.
func (c IndexColumn) Validate() error {
	switch {
	case c.Name == "" && c.Expression == "":
		return fmt.Errorf("column has neither name nor expression")
	case c.Name != "" && c.Expression != "":
		return fmt.Errorf("column has both name %q and an expression; set exactly one", c.Name)
	case c.Name != "":
		if err := ValidateIdentifier(c.Name); err != nil {
			return err
		}
	default:
		if err := validateSQLFragment("expression", c.Expression); err != nil {
			return err
		}
	}
	if c.Collation != "" {
		// pg_collation is schema-qualified.
		if err := ValidateMaybeQualified(c.Collation); err != nil {
			return fmt.Errorf("collation: %w", err)
		}
	}
	if c.OpClass != "" {
		// pg_opclass is schema-qualified, and on managed PostgreSQL an
		// extension's opclasses routinely live outside the search path.
		if err := ValidateMaybeQualified(c.OpClass); err != nil {
			return fmt.Errorf("opclass: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// catalog.assert
// ---------------------------------------------------------------------------

// AssertionKind names a catalog predicate. The set covers the preconditions in
// TRD §7.2.13 for all three v0.1 operations.
type AssertionKind string

// The catalog predicates.
const (
	// AssertRelationIsPartitioned: pg_class.relkind = 'p'.
	AssertRelationIsPartitioned AssertionKind = "relation_is_partitioned"
	// AssertPartitionStrategy: pg_partitioned_table strategy is in Expected.
	// HASH is rejected in v0.1 (FR-PLAN-3).
	AssertPartitionStrategy AssertionKind = "partition_strategy"
	// AssertPartitionDepth: pg_partition_tree depth equals Expected[0].
	// v0.1 requires exactly 1 (FR-PLAN-2).
	AssertPartitionDepth AssertionKind = "partition_depth"
	// AssertNoDefaultPartition: the tree contains no DEFAULT partition
	// (FR-PLAN-3).
	AssertNoDefaultPartition AssertionKind = "no_default_partition"
	// AssertRoleMembership: Role is a member of the owning role of Relation
	// (FR-PLAN-10, AC-12).
	AssertRoleMembership AssertionKind = "role_membership"
	// AssertIndexNameAvailable: Index does not exist, or matches an
	// in-progress build with PartitionCTL provenance.
	AssertIndexNameAvailable AssertionKind = "index_name_available"
	// AssertIndexExists: Index exists (FR-DROP-1).
	AssertIndexExists AssertionKind = "index_exists"
	// AssertIndexIsPartitioned: Index is a partitioned index on Relation
	// (FR-DROP-1).
	AssertIndexIsPartitioned AssertionKind = "index_is_partitioned"
	// AssertIndexNotConstraintBacked: Index does not back a UNIQUE or PRIMARY
	// KEY constraint (FR-DROP-2, AC-14).
	AssertIndexNotConstraintBacked AssertionKind = "index_not_constraint_backed"
	// AssertLeavesAttached: every leaf index is attached to Index
	// (ReindexPartitionedIndex precondition).
	AssertLeavesAttached AssertionKind = "leaves_attached"
)

// Valid reports whether a is a known predicate.
func (a AssertionKind) Valid() bool {
	switch a {
	case AssertRelationIsPartitioned,
		AssertPartitionStrategy,
		AssertPartitionDepth,
		AssertNoDefaultPartition,
		AssertRoleMembership,
		AssertIndexNameAvailable,
		AssertIndexExists,
		AssertIndexIsPartitioned,
		AssertIndexNotConstraintBacked,
		AssertLeavesAttached:
		return true
	}
	return false
}

// Assertion is one catalog predicate to evaluate.
type Assertion struct {
	// Assertion names the predicate.
	Assertion AssertionKind `json:"assertion"`
	// Relation is the table the predicate is about, where applicable.
	Relation *ObjectName `json:"relation,omitempty"`
	// Index is the index the predicate is about, where applicable.
	Index *ObjectName `json:"index,omitempty"`
	// Role is the connected role, for AssertRoleMembership.
	Role string `json:"role,omitempty"`
	// Expected carries the predicate's argument as ordered strings: allowed
	// strategies for AssertPartitionStrategy, the depth for
	// AssertPartitionDepth. Strings keep the plan format free of typed unions
	// and keep canonicalization trivially stable.
	Expected []string `json:"expected,omitempty"`
	// FailureCode is the exit code the run should use when this predicate is
	// false. Unsupported topology is 15 and insufficient privilege is 16
	// (TRD §7.2.12); zero means [ExitVerificationFailed].
	FailureCode ExitCode `json:"failure_code,omitempty"`
	// Message is the operator-facing explanation to print on failure.
	Message string `json:"message,omitempty"`
}

// Validate checks the assertion.
func (a Assertion) Validate() error {
	if !a.Assertion.Valid() {
		return fmt.Errorf("unknown assertion %q", a.Assertion)
	}
	if a.Relation != nil {
		if err := a.Relation.Validate(); err != nil {
			return fmt.Errorf("relation: %w", err)
		}
	}
	if a.Index != nil {
		if err := a.Index.Validate(); err != nil {
			return fmt.Errorf("index: %w", err)
		}
	}
	if a.Role != "" {
		if err := ValidateIdentifier(a.Role); err != nil {
			return fmt.Errorf("role: %w", err)
		}
	}
	if a.Assertion == AssertRoleMembership && a.Role == "" {
		return fmt.Errorf("assertion %q requires role", a.Assertion)
	}
	return nil
}

// CatalogAssertParams evaluates catalog predicates and fails the run if any is
// false. Terminal on false; never retried.
type CatalogAssertParams struct {
	// Assertions are evaluated in order; the first false one fails the node.
	Assertions []Assertion `json:"assertions"`
}

func (p *CatalogAssertParams) Kind() NodeKind { return KindCatalogAssert }
func (p *CatalogAssertParams) nodeParams()    {}

// Validate checks every assertion.
func (p *CatalogAssertParams) Validate() error {
	if len(p.Assertions) == 0 {
		return fmt.Errorf("catalog.assert has no assertions")
	}
	for i, a := range p.Assertions {
		if err := a.Validate(); err != nil {
			return fmt.Errorf("assertion %d: %w", i, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// index.create_parent_invalid
// ---------------------------------------------------------------------------

// CreateParentInvalidParams renders CREATE INDEX <index> ON ONLY <parent>
// (...). ONLY prevents recursion into the leaves, so the statement is
// catalog-only and the resulting parent index is deliberately invalid for the
// whole build (TRD §7.2.13).
type CreateParentInvalidParams struct {
	// Parent is the partitioned table.
	Parent ObjectName `json:"parent"`
	// Index is the parent index to create.
	Index ObjectName `json:"index"`
	// Definition is the index shape.
	Definition IndexDefinition `json:"definition"`
}

func (p *CreateParentInvalidParams) Kind() NodeKind { return KindIndexCreateParentInvalid }
func (p *CreateParentInvalidParams) nodeParams()    {}

// Validate checks the parameters.
func (p *CreateParentInvalidParams) Validate() error {
	if err := validateObject("parent", p.Parent); err != nil {
		return err
	}
	if err := validateObject("index", p.Index); err != nil {
		return err
	}
	return p.Definition.Validate()
}

// ---------------------------------------------------------------------------
// index.create_concurrently
// ---------------------------------------------------------------------------

// CreateConcurrentlyParams renders CREATE INDEX CONCURRENTLY <index> ON
// <partition> (...). It runs outside any transaction block and carries no
// finite statement_timeout (FR-EXEC-5, FR-EXEC-6).
type CreateConcurrentlyParams struct {
	// Partition is the leaf table to index.
	Partition ObjectName `json:"partition"`
	// Index is the child index name. It is generated by the planner as a
	// deterministic function of the parent index name and the partition name
	// and recorded here (FR-PLAN-11, FR-PLAN-13). PostgreSQL-generated names
	// cannot be correlated on resume, so the executor uses this value verbatim
	// and never re-derives it. See [ChildIndexName].
	Index ObjectName `json:"index"`
	// Definition is the index shape, identical to the parent's.
	Definition IndexDefinition `json:"definition"`
	// ParentIndex is the partitioned index this child will later attach to. It
	// is the correlation key for provenance (INV-1) and for resume.
	ParentIndex *ObjectName `json:"parent_index,omitempty"`
}

func (p *CreateConcurrentlyParams) Kind() NodeKind { return KindIndexCreateConcurrently }
func (p *CreateConcurrentlyParams) nodeParams()    {}

// Validate checks the parameters.
func (p *CreateConcurrentlyParams) Validate() error {
	if err := validateObject("partition", p.Partition); err != nil {
		return err
	}
	if err := validateObject("index", p.Index); err != nil {
		return err
	}
	if err := validateOptionalObject("parent_index", p.ParentIndex); err != nil {
		return err
	}
	return p.Definition.Validate()
}

// ---------------------------------------------------------------------------
// index.attach
// ---------------------------------------------------------------------------

// AttachParams renders ALTER INDEX <parent_index> ATTACH PARTITION
// <child_index>. Catalog-only. When the last child attaches, PostgreSQL marks
// the parent index valid automatically; no statement is issued for that.
type AttachParams struct {
	// ParentIndex is the partitioned index.
	ParentIndex ObjectName `json:"parent_index"`
	// ChildIndex is the leaf index to attach.
	ChildIndex ObjectName `json:"child_index"`
}

func (p *AttachParams) Kind() NodeKind { return KindIndexAttach }
func (p *AttachParams) nodeParams()    {}

// Validate checks the parameters.
func (p *AttachParams) Validate() error {
	if err := validateObject("parent_index", p.ParentIndex); err != nil {
		return err
	}
	return validateObject("child_index", p.ChildIndex)
}

// ---------------------------------------------------------------------------
// index.verify
// ---------------------------------------------------------------------------

// VerifyCheckKind names a catalog assertion the verifier evaluates.
type VerifyCheckKind string

// The verification checks (FR-VER-1…4, FR-REIDX-6, FR-DROP-7).
const (
	// CheckIndexValid asserts indisvalid AND indisready AND indislive on
	// Index (FR-VER-1).
	CheckIndexValid VerifyCheckKind = "index_valid"
	// CheckIndexAttached asserts the parent-child index relationship exists in
	// pg_inherits (FR-VER-2).
	CheckIndexAttached VerifyCheckKind = "index_attached"
	// CheckParentIndexValid asserts the parent index is indisvalid
	// (FR-VER-3, FR-REIDX-8).
	CheckParentIndexValid VerifyCheckKind = "parent_index_valid"
	// CheckLeafIndexCount asserts the leaf index count equals ExpectedCount
	// (FR-VER-4).
	CheckLeafIndexCount VerifyCheckKind = "leaf_index_count"
	// CheckIndexAbsent asserts Index is absent from pg_index (FR-DROP-7).
	CheckIndexAbsent VerifyCheckKind = "index_absent"
	// CheckNoLeftoverIndexes asserts no _ccnew/_ccold index remains on
	// Relation (FR-REIDX-6).
	CheckNoLeftoverIndexes VerifyCheckKind = "no_leftover_indexes"
)

// Valid reports whether c is a known check.
func (c VerifyCheckKind) Valid() bool {
	switch c {
	case CheckIndexValid,
		CheckIndexAttached,
		CheckParentIndexValid,
		CheckLeafIndexCount,
		CheckIndexAbsent,
		CheckNoLeftoverIndexes:
		return true
	}
	return false
}

// VerifyCheck is one catalog assertion.
type VerifyCheck struct {
	// Check names the assertion.
	Check VerifyCheckKind `json:"check"`
	// Index is the index under test, where applicable.
	Index *ObjectName `json:"index,omitempty"`
	// ParentIndex is the partitioned index, for attachment and parent-validity
	// checks.
	ParentIndex *ObjectName `json:"parent_index,omitempty"`
	// Relation is the table, for leaf-count and leftover checks.
	Relation *ObjectName `json:"relation,omitempty"`
	// ExpectedCount is the expected leaf index count, for
	// CheckLeafIndexCount.
	ExpectedCount *int `json:"expected_count,omitempty"`
	// Message is the operator-facing explanation to print on failure.
	Message string `json:"message,omitempty"`
}

// Validate checks the assertion's shape.
func (c VerifyCheck) Validate() error {
	if !c.Check.Valid() {
		return fmt.Errorf("unknown check %q", c.Check)
	}
	if err := validateOptionalObject("index", c.Index); err != nil {
		return err
	}
	if err := validateOptionalObject("parent_index", c.ParentIndex); err != nil {
		return err
	}
	if err := validateOptionalObject("relation", c.Relation); err != nil {
		return err
	}
	switch c.Check {
	case CheckIndexValid, CheckIndexAbsent:
		if c.Index == nil {
			return fmt.Errorf("check %q requires index", c.Check)
		}
	case CheckIndexAttached:
		if c.Index == nil || c.ParentIndex == nil {
			return fmt.Errorf("check %q requires both index and parent_index", c.Check)
		}
	case CheckParentIndexValid:
		if c.ParentIndex == nil {
			return fmt.Errorf("check %q requires parent_index", c.Check)
		}
	case CheckLeafIndexCount:
		if c.ParentIndex == nil {
			return fmt.Errorf("check %q requires parent_index", c.Check)
		}
		if c.ExpectedCount == nil {
			return fmt.Errorf("check %q requires expected_count", c.Check)
		}
		if *c.ExpectedCount < 0 {
			return fmt.Errorf("check %q has negative expected_count %d", c.Check, *c.ExpectedCount)
		}
	case CheckNoLeftoverIndexes:
		if c.Relation == nil {
			return fmt.Errorf("check %q requires relation", c.Check)
		}
	}
	return nil
}

// VerifyParams asserts catalog state. It issues no DDL and is terminal on
// false; it is also what `verify` runs standalone (FR-VER-5, FR-CLI-14).
type VerifyParams struct {
	// Checks are evaluated in order. All must pass. The verifier reports
	// pass/fail per check so `verify --json` can list them (FR-CLI-14).
	Checks []VerifyCheck `json:"checks"`
}

func (p *VerifyParams) Kind() NodeKind { return KindIndexVerify }
func (p *VerifyParams) nodeParams()    {}

// Validate checks every assertion.
func (p *VerifyParams) Validate() error {
	if len(p.Checks) == 0 {
		return fmt.Errorf("index.verify has no checks")
	}
	for i, c := range p.Checks {
		if err := c.Validate(); err != nil {
			return fmt.Errorf("check %d: %w", i, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// wait
// ---------------------------------------------------------------------------

// WaitParams is a fixed pause emitted by the planner for pacing (FR-ORD-3).
// Every pause is a node so that it is visible in the reviewed plan artifact;
// the executor introduces no delays of its own.
type WaitParams struct {
	// Seconds is the pause duration. Zero is legal and is a no-op.
	Seconds int `json:"seconds"`
	// Reason is the operator-facing explanation for the pause.
	Reason string `json:"reason,omitempty"`
}

func (p *WaitParams) Kind() NodeKind { return KindWait }
func (p *WaitParams) nodeParams()    {}

// Validate checks the duration.
func (p *WaitParams) Validate() error {
	if p.Seconds < 0 {
		return fmt.Errorf("wait has negative seconds %d", p.Seconds)
	}
	return nil
}

// ---------------------------------------------------------------------------
// index.drop_concurrently  (destructive)
// ---------------------------------------------------------------------------

// DropReason records why the planner emitted a destructive drop. It is
// documentation and audit detail; it never authorizes anything. Authorization
// is [Node.Authorization] alone (FR-AUTH-7: no mode is inferable from naming).
type DropReason string

// The reasons a drop is planned.
const (
	// DropInvalidBuild: an INVALID index left by a failed CREATE INDEX
	// CONCURRENTLY, with PartitionCTL provenance (FR-PLAN-6, AC-5).
	DropInvalidBuild DropReason = "invalid_build"
	// DropCCNew: a *_ccnew leftover. The rebuild failed and the original is
	// intact, so the leaf still needs reindexing (FR-REIDX-3).
	DropCCNew DropReason = "cc_new"
	// DropCCOld: a *_ccold leftover. The rebuild succeeded and the old copy
	// survived, so the leaf is already complete (FR-REIDX-4).
	DropCCOld DropReason = "cc_old"
	// DropUnattachedOrphan: a leaf index built but never attached, left by an
	// abandoned create; it would survive the parent's cascade (TRD §7.2.13,
	// DropPartitionedIndex step 1).
	DropUnattachedOrphan DropReason = "unattached_orphan"
)

// Valid reports whether r is a known reason.
func (r DropReason) Valid() bool {
	switch r {
	case DropInvalidBuild, DropCCNew, DropCCOld, DropUnattachedOrphan:
		return true
	}
	return false
}

// DropConcurrentlyParams renders DROP INDEX CONCURRENTLY <index> on an
// *unattached* leaf index. Online: ShareUpdateExclusive. Destructive, so the
// node carries an [Authorization] the executor re-evaluates before dispatch.
//
// PostgreSQL forbids this form on a partitioned index and forbids dropping an
// attached child individually (TRD §7.2.10), so this kind is only ever emitted
// for unattached ordinary indexes.
type DropConcurrentlyParams struct {
	// Index is the index to drop.
	Index ObjectName `json:"index"`
	// Relation is the leaf table the index is on. AuthLeftover resolves
	// reindex-run history per relation (FR-AUTH-3), and the audit trail records
	// it.
	Relation *ObjectName `json:"relation,omitempty"`
	// Reason records why the drop was planned. Audit detail only.
	Reason DropReason `json:"reason,omitempty"`
}

func (p *DropConcurrentlyParams) Kind() NodeKind { return KindIndexDropConcurrently }
func (p *DropConcurrentlyParams) nodeParams()    {}

// Validate checks the parameters.
func (p *DropConcurrentlyParams) Validate() error {
	if err := validateObject("index", p.Index); err != nil {
		return err
	}
	if err := validateOptionalObject("relation", p.Relation); err != nil {
		return err
	}
	if p.Reason != "" && !p.Reason.Valid() {
		return fmt.Errorf("unknown drop reason %q", p.Reason)
	}
	return nil
}

// ---------------------------------------------------------------------------
// index.reindex_concurrently
// ---------------------------------------------------------------------------

// ReindexConcurrentlyParams renders REINDEX INDEX CONCURRENTLY <index> against
// one leaf partition's index. It runs outside any transaction block.
//
// The planner never emits a reindex of the partitioned parent: PostgreSQL
// rejects REINDEX CONCURRENTLY on partitioned relations, and the non-concurrent
// form takes AccessExclusiveLock per partition (FR-REIDX-2).
type ReindexConcurrentlyParams struct {
	// Index is the leaf index to rebuild.
	Index ObjectName `json:"index"`
	// Relation is the leaf table the index is on.
	Relation *ObjectName `json:"relation,omitempty"`
	// ParentIndex is the partitioned index the leaf is attached to. Verification
	// asserts the attachment survives the swap (FR-REIDX-6).
	ParentIndex *ObjectName `json:"parent_index,omitempty"`
	// EstimatedPeakBytes is the estimated peak *additional* storage: a reindex
	// transiently holds both the old and the new index (FR-REIDX-7).
	EstimatedPeakBytes int64 `json:"estimated_peak_bytes,omitempty"`
}

func (p *ReindexConcurrentlyParams) Kind() NodeKind { return KindIndexReindexConcurrently }
func (p *ReindexConcurrentlyParams) nodeParams()    {}

// Validate checks the parameters.
func (p *ReindexConcurrentlyParams) Validate() error {
	if err := validateObject("index", p.Index); err != nil {
		return err
	}
	if err := validateOptionalObject("relation", p.Relation); err != nil {
		return err
	}
	if err := validateOptionalObject("parent_index", p.ParentIndex); err != nil {
		return err
	}
	if p.EstimatedPeakBytes < 0 {
		return fmt.Errorf("estimated_peak_bytes is negative: %d", p.EstimatedPeakBytes)
	}
	return nil
}

// ---------------------------------------------------------------------------
// index.drop_partitioned  (destructive)
// ---------------------------------------------------------------------------

// DropPartitionedParams renders DROP INDEX <index> on a partitioned parent.
//
// This is the only kind that takes AccessExclusiveLock, and it takes it on the
// parent and every leaf simultaneously, because the statement cascades to every
// attached child. PostgreSQL offers no online alternative: DROP INDEX
// CONCURRENTLY is rejected on partitioned indexes and there is no ALTER INDEX
// ... DETACH PARTITION (TRD §7.2.10).
//
// The statement is atomic, so there is no resume and no progress to report. The
// executor sets lock_timeout and retries with backoff; on budget exhaustion it
// abandons cleanly, leaving the index intact (FR-DROP-6, AC-15).
type DropPartitionedParams struct {
	// Parent is the partitioned table the index is on.
	Parent ObjectName `json:"parent"`
	// Index is the partitioned index to drop.
	Index ObjectName `json:"index"`
	// LeafCount is how many leaf partitions will be locked. It exists so the
	// plan output and rendered SQL can state the blast radius (FR-DROP-5).
	LeafCount int `json:"leaf_count,omitempty"`
}

func (p *DropPartitionedParams) Kind() NodeKind { return KindIndexDropPartitioned }
func (p *DropPartitionedParams) nodeParams()    {}

// Validate checks the parameters.
func (p *DropPartitionedParams) Validate() error {
	if err := validateObject("parent", p.Parent); err != nil {
		return err
	}
	if err := validateObject("index", p.Index); err != nil {
		return err
	}
	if p.LeafCount < 0 {
		return fmt.Errorf("leaf_count is negative: %d", p.LeafCount)
	}
	return nil
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

func validateObject(field string, o ObjectName) error {
	if o.IsZero() {
		return fmt.Errorf("%s is required", field)
	}
	if err := o.Validate(); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}

func validateOptionalObject(field string, o *ObjectName) error {
	if o == nil {
		return nil
	}
	return validateObject(field, *o)
}
