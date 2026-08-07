package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atulsinha/partitionctl/engine/executor"
	"github.com/atulsinha/partitionctl/engine/planner"
	"github.com/atulsinha/partitionctl/engine/protocol"
	"github.com/atulsinha/partitionctl/engine/state"
	"github.com/atulsinha/partitionctl/engine/verifier"
)

// This file is the harness the CLI package was missing. adapters/cli is where
// the operative logic for nine of the fourteen M1 acceptance criteria lives, so
// engine-level tests do not establish those criteria: they assert on the
// executor's error before the CLI wraps it, or on a store API the CLI reaches
// through two adapters.
//
// App is already fully injectable (Env, Now, OpenDB, NewTarget, NewStore,
// Signals), and planner.FakeCatalog plus state.OpenFileStore make a complete
// target with no PostgreSQL, so a harness costs little and buys end-to-end
// coverage of exit codes, refusals and cleanup gating.

// ctx is the context every test uses. Go 1.22 has no testing.T.Context.
func ctx() context.Context { return context.Background() }

const (
	ownerOID  uint32 = 10
	rootOID   uint32 = 100
	leafBase  uint32 = 200
	indexBase uint32 = 900

	testTable  = "public.orders"
	testIndex  = "orders_created_at_idx"
	testDBName = "appdb"
)

func obj(schema, name string) protocol.ObjectName { return protocol.NewObjectName(schema, name) }

// ---------------------------------------------------------------------------
// Fake SQL executor
// ---------------------------------------------------------------------------

// fakeSQL records every statement the executor dispatches and can be told to
// fail a specific one, which is how a test reaches the failure paths that only
// arise during a run.
type fakeSQL struct {
	mu    sync.Mutex
	stmts []executor.Statement

	// FailOn returns a non-nil error for a statement whose SQL contains the
	// key, simulating a server-side failure.
	FailOn map[string]error

	// OnExec, when set, runs before each statement. It is how a test mutates
	// the world mid-run (a concurrent rebuild, a lost lease).
	OnExec func(executor.Statement) error

	// Effects applies a successful statement to the fake catalog, so the
	// verifier reads a world the run actually changed.
	Effects func(executor.Statement)
}

func (f *fakeSQL) Exec(_ context.Context, stmt executor.Statement) error {
	f.mu.Lock()
	f.stmts = append(f.stmts, stmt)
	f.mu.Unlock()

	if f.OnExec != nil {
		if err := f.OnExec(stmt); err != nil {
			return err
		}
	}
	for key, err := range f.FailOn {
		if strings.Contains(stmt.SQL, key) {
			return err
		}
	}
	if f.Effects != nil {
		f.Effects(stmt)
	}
	return nil
}

// SQLTexts returns the SQL of every statement issued, in order.
func (f *fakeSQL) SQLTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.stmts))
	for i, s := range f.stmts {
		out[i] = s.SQL
	}
	return out
}

// Issued reports whether any statement containing substr was issued.
func (f *fakeSQL) Issued(substr string) bool {
	for _, s := range f.SQLTexts() {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// DDLCount counts statements that are not session settings.
func (f *fakeSQL) DDLCount() int {
	n := 0
	for _, s := range f.SQLTexts() {
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(s)), "SET ") {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Fake verifier catalog
// ---------------------------------------------------------------------------

// fakeVerifyCatalog answers the verifier's reads from a declared index set.
type fakeVerifyCatalog struct {
	Indexes []verifier.IndexState
	// Parents maps a child index to the partitioned index it is attached to.
	Parents map[protocol.ObjectName]protocol.ObjectName
	Leaves  []protocol.ObjectName
	// Comments maps an index's identity form to the comment on it, which is
	// where the PartitionCTL ownership marker lives.
	Comments map[string]string
}

func (f *fakeVerifyCatalog) IndexComment(_ context.Context, index protocol.ObjectName) (string, bool, error) {
	c, ok := f.Comments[index.String()]
	if !ok || c == "" {
		return "", false, nil
	}
	return c, true, nil
}

// Mark writes a well-formed PartitionCTL ownership marker onto an index, which
// is what proves the object is this tool's to clean up (AC-6).
func (f *fakeVerifyCatalog) Mark(index protocol.ObjectName, run string) {
	text, err := protocol.FormatMarker(protocol.Marker{
		Run: run, Plan: "sha256:fake", Op: string(protocol.OpCreateIndex),
		Role: protocol.MarkerRoleLeaf, At: "2026-08-07T12:00:00Z",
	})
	if err != nil {
		panic("cli: fakeVerifyCatalog.Mark: " + err.Error())
	}
	if f.Comments == nil {
		f.Comments = map[string]string{}
	}
	f.Comments[index.String()] = text
}

func (f *fakeVerifyCatalog) Index(_ context.Context, name protocol.ObjectName) (verifier.IndexState, bool, error) {
	for _, s := range f.Indexes {
		if s.Name.Name == name.Name && (name.Schema == "" || s.Name.Schema == name.Schema) {
			return s, true, nil
		}
	}
	return verifier.IndexState{}, false, nil
}

func (f *fakeVerifyCatalog) IndexParent(_ context.Context, child protocol.ObjectName) (protocol.ObjectName, bool, error) {
	p, ok := f.Parents[child]
	return p, ok, nil
}

func (f *fakeVerifyCatalog) AttachedIndexes(_ context.Context, parentIndex protocol.ObjectName) ([]verifier.IndexState, error) {
	var out []verifier.IndexState
	for _, s := range f.Indexes {
		if p, ok := f.Parents[s.Name]; ok && p.Name == parentIndex.Name {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name.String() < out[j].Name.String() })
	return out, nil
}

func (f *fakeVerifyCatalog) LeafPartitions(_ context.Context, _ protocol.ObjectName) ([]protocol.ObjectName, error) {
	return f.Leaves, nil
}

func (f *fakeVerifyCatalog) TreeIndexes(_ context.Context, _ protocol.ObjectName) ([]verifier.IndexState, error) {
	return f.Indexes, nil
}

// ---------------------------------------------------------------------------
// The harness
// ---------------------------------------------------------------------------

// harness is an App wired to in-memory fakes, plus everything a test needs to
// inspect afterwards.
type harness struct {
	t *testing.T

	App    *App
	Cat    *planner.FakeCatalog
	SQL    *fakeSQL
	Verify *fakeVerifyCatalog
	Store  state.StateStore

	Stdout *bytes.Buffer
	Stderr *bytes.Buffer

	Dir      string
	StateDir string

	// StoreIntents records the intent each command asked the state store for,
	// in order. `plan` must ask for StoreReadOnly (AC-1).
	StoreIntents []StoreIntent

	// clock advances by one second per call so run ids and timestamps differ.
	now time.Time
}

// newHarness builds a target with a three-partition RANGE table and a file
// state store.
func newHarness(t *testing.T, leaves ...string) *harness {
	t.Helper()
	if len(leaves) == 0 {
		leaves = []string{"orders_2026_01", "orders_2026_02", "orders_2026_03"}
	}

	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")

	h := &harness{
		t:        t,
		Cat:      fakeTree(leaves...),
		SQL:      &fakeSQL{FailOn: map[string]error{}},
		Verify:   &fakeVerifyCatalog{Parents: map[protocol.ObjectName]protocol.ObjectName{}},
		Stdout:   &bytes.Buffer{},
		Stderr:   &bytes.Buffer{},
		Dir:      dir,
		StateDir: stateDir,
		now:      time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
	}
	for _, l := range leaves {
		h.Verify.Leaves = append(h.Verify.Leaves, obj("public", l))
	}
	h.SQL.Effects = h.applyEffects

	store, err := state.OpenFileStore(stateDir, state.FileOptions{Holder: "test/1"})
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h.Store = store

	h.App = &App{
		Stdout: h.Stdout,
		Stderr: h.Stderr,
		Env:    func(string) (string, bool) { return "", false },
		Now:    h.Now,
		OpenDB: func(context.Context, Config) (*sql.DB, error) { return nil, nil },
		NewTarget: func(context.Context, Config, *sql.DB) (*Target, error) {
			return &Target{
				Catalog: h.Cat,
				Verify:  h.Verify,
				SQL:     h.SQL,
			}, nil
		},
		NewStore: func(_ context.Context, _ Config, _ *sql.DB, intent StoreIntent) (state.StateStore, error) {
			h.StoreIntents = append(h.StoreIntents, intent)
			return nopCloseStore{h.Store}, nil
		},
		// No signal handler in a test: installing one would make the suite
		// sensitive to whatever else is running.
		Signals: func(c context.Context) (context.Context, context.CancelFunc) {
			return context.WithCancel(c)
		},
	}
	return h
}

// Now is a monotonic test clock.
func (h *harness) Now() time.Time {
	h.now = h.now.Add(time.Second)
	return h.now
}

// nopCloseStore keeps the harness's store alive across commands, since each
// command closes the store it opens.
type nopCloseStore struct{ state.StateStore }

func (nopCloseStore) Close() error { return nil }

// fakeTree builds a single-level RANGE partitioned table owned by a role the
// connected role is a member of.
func fakeTree(leaves ...string) *planner.FakeCatalog {
	f := planner.NewFakeCatalog()
	f.Database = testDBName
	f.AddRole(ownerOID, "app_owner", true)
	f.AddRelation(planner.Relation{
		OID:      rootOID,
		Name:     obj("public", "orders"),
		Kind:     planner.RelKindPartitionedTable,
		OwnerOID: ownerOID,
	})
	f.SetStrategy(rootOID, protocol.StrategyRange)
	for i, leaf := range leaves {
		f.AddRelation(planner.Relation{
			OID:            leafBase + uint32(i),
			Name:           obj("public", leaf),
			Kind:           planner.RelKindTable,
			OwnerOID:       ownerOID,
			ParentOID:      rootOID,
			RelPages:       128,
			PartitionBound: "FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')",
		})
	}
	return f
}

// ---------------------------------------------------------------------------
// Driving commands
// ---------------------------------------------------------------------------

// stateFlags point every command at the harness's file store.
func (h *harness) stateFlags() []string {
	return []string{"--state", "file", "--state-dir", h.StateDir, "--actor", "tester"}
}

// Run drives App.Run and returns the exit code, resetting the output buffers so
// each command's output can be asserted independently.
func (h *harness) Run(args ...string) int {
	h.t.Helper()
	h.Stdout.Reset()
	h.Stderr.Reset()
	full := append([]string{args[0]}, append(h.stateFlags(), args[1:]...)...)
	return h.App.Run(ctx(), full)
}

// Out is everything the last command wrote to stdout and stderr.
func (h *harness) Out() string { return h.Stdout.String() + h.Stderr.String() }

// WriteSpec writes a create-index specification and returns its path.
func (h *harness) WriteSpec(s SpecFile) string {
	h.t.Helper()
	if s.Operation == "" {
		s.Operation = "create-index"
	}
	if s.Table == "" {
		s.Table = testTable
	}
	if s.Index == "" {
		s.Index = testIndex
	}
	if len(s.Definition.Columns) == 0 {
		s.Definition = protocol.IndexDefinition{
			Method:  "btree",
			Columns: []protocol.IndexColumn{{Name: "created_at"}},
		}
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		h.t.Fatalf("marshal spec: %v", err)
	}
	path := filepath.Join(h.Dir, "spec.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		h.t.Fatalf("write spec: %v", err)
	}
	return path
}

// PlanPath is where the harness writes plan artifacts.
func (h *harness) PlanPath() string { return filepath.Join(h.Dir, "plan.json") }

// MustPlan runs `plan` and returns the artifact path.
func (h *harness) MustPlan(spec ...SpecFile) string {
	h.t.Helper()
	s := SpecFile{}
	if len(spec) > 0 {
		s = spec[0]
	}
	specPath := h.WriteSpec(s)
	out := h.PlanPath()
	if code := h.Run("plan", "--spec", specPath, "-o", out, "--force"); code != 0 {
		h.t.Fatalf("plan exited %d: %s", code, h.Out())
	}
	return out
}

// LoadPlan reads back a written plan artifact.
func (h *harness) LoadPlan(path string) *protocol.Plan {
	h.t.Helper()
	p, err := loadPlan(path)
	if err != nil {
		h.t.Fatalf("loadPlan: %v", err)
	}
	return p
}

// WritePlan seals and writes a hand-built plan, for the cases the create-index
// planner will not emit.
func (h *harness) WritePlan(p *protocol.Plan) string {
	h.t.Helper()
	if err := p.Seal(); err != nil {
		h.t.Fatalf("Seal: %v", err)
	}
	data, err := protocol.EncodePlan(p)
	if err != nil {
		h.t.Fatalf("EncodePlan: %v", err)
	}
	path := filepath.Join(h.Dir, "handbuilt.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		h.t.Fatalf("write plan: %v", err)
	}
	return path
}

// ---------------------------------------------------------------------------
// Catalog effects
// ---------------------------------------------------------------------------

// quotedIdent matches the "schema"."name" and "name" forms the renderer emits.
var quotedIdent = regexp.MustCompile(`"((?:[^"]|"")*)"`)

// identsIn returns the quoted identifiers in a statement, in order, unescaped.
func identsIn(sqlText string) []string {
	ms := quotedIdent.FindAllStringSubmatch(sqlText, -1)
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, strings.ReplaceAll(m[1], `""`, `"`))
	}
	return out
}

// applyEffects makes the fake target behave like a server: a statement that
// succeeds changes the catalog the verifier then reads. Without this an
// `index.verify` node always fails and no run can reach a terminal success, so
// AC-4's "converges to the same final catalog state" would be untestable.
//
// Effects are keyed on the node kind, never on the SQL text, because the kind
// is what the executor dispatches on.
func (h *harness) applyEffects(stmt executor.Statement) {
	idents := identsIn(stmt.SQL)
	switch stmt.Kind {
	case protocol.KindIndexCreateParentInvalid:
		// CREATE INDEX "idx" ON ONLY "public"."orders"
		if len(idents) < 3 {
			return
		}
		h.addIndex(verifier.IndexState{
			Name:     obj(idents[1], idents[0]),
			Relation: obj(idents[1], idents[2]),
			// A partitioned index stays INVALID until its last child attaches.
			Valid: false, Ready: true, Live: true, Partitioned: true,
		})

	case protocol.KindIndexCreateConcurrently:
		// CREATE INDEX CONCURRENTLY "child" ON "public"."leaf"
		if len(idents) < 3 {
			return
		}
		h.addIndex(verifier.IndexState{
			Name:     obj(idents[1], idents[0]),
			Relation: obj(idents[1], idents[2]),
			Valid:    true, Ready: true, Live: true,
		})

	case protocol.KindIndexAttach:
		// ALTER INDEX "public"."parent" ATTACH PARTITION "public"."child"
		if len(idents) < 4 {
			return
		}
		parent, child := obj(idents[0], idents[1]), obj(idents[2], idents[3])
		h.Verify.Parents[child] = parent
		// PostgreSQL marks the partitioned index valid once every leaf index
		// is attached, which is what index.verify on the parent asserts.
		if h.attachedCount(parent) >= len(h.Verify.Leaves) {
			h.setValid(parent, true)
		}

	case protocol.KindIndexDropConcurrently:
		// DROP INDEX CONCURRENTLY "public"."name"
		if len(idents) < 2 {
			return
		}
		h.removeIndex(obj(idents[0], idents[1]))
	}
}

func (h *harness) addIndex(s verifier.IndexState) {
	for i := range h.Verify.Indexes {
		if h.Verify.Indexes[i].Name == s.Name {
			h.Verify.Indexes[i] = s
			return
		}
	}
	h.Verify.Indexes = append(h.Verify.Indexes, s)
}

func (h *harness) removeIndex(name protocol.ObjectName) {
	out := h.Verify.Indexes[:0]
	for _, s := range h.Verify.Indexes {
		if s.Name != name {
			out = append(out, s)
		}
	}
	h.Verify.Indexes = out
	delete(h.Verify.Parents, name)
}

func (h *harness) setValid(name protocol.ObjectName, valid bool) {
	for i := range h.Verify.Indexes {
		if h.Verify.Indexes[i].Name == name {
			h.Verify.Indexes[i].Valid = valid
		}
	}
}

func (h *harness) attachedCount(parent protocol.ObjectName) int {
	n := 0
	for _, p := range h.Verify.Parents {
		if p == parent {
			n++
		}
	}
	return n
}

// IndexNames returns every index the fake catalog now holds, sorted. It is the
// "final catalog state" AC-4 compares between an interrupted and an
// uninterrupted run.
func (h *harness) IndexNames() []string {
	out := make([]string, 0, len(h.Verify.Indexes))
	for _, s := range h.Verify.Indexes {
		attached := ""
		if p, ok := h.Verify.Parents[s.Name]; ok {
			attached = " -> " + p.String()
		}
		out = append(out, s.Name.String()+" valid="+strconv.FormatBool(s.Valid)+attached)
	}
	sort.Strings(out)
	return out
}

// Runs returns every run the store knows about.
func (h *harness) Runs() []state.Run {
	h.t.Helper()
	runs, err := h.Store.FindRuns(ctx(), state.RunQuery{})
	if err != nil {
		h.t.Fatalf("FindRuns: %v", err)
	}
	return runs
}

// plannerRelation builds a leaf partition record for the fake catalog.
func plannerRelation(oid uint32, name string) planner.Relation {
	return planner.Relation{
		OID:            oid,
		Name:           obj("public", name),
		Kind:           planner.RelKindTable,
		OwnerOID:       ownerOID,
		ParentOID:      rootOID,
		RelPages:       128,
		PartitionBound: "FOR VALUES FROM ('2026-01-01') TO ('2026-02-01')",
	}
}
