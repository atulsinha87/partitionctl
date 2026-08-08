package state

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

func TestSchemaDDL(t *testing.T) {
	tests := []struct {
		name    string
		schema  string
		wantQ   string
		wantErr error
	}{
		{name: "default", schema: "", wantQ: `"partitionctl"`},
		{name: "explicit default", schema: DefaultSchema, wantQ: `"partitionctl"`},
		{name: "custom", schema: "pctl_state", wantQ: `"pctl_state"`},
		{name: "mixed case is preserved by quoting", schema: "PctlState", wantQ: `"PctlState"`},
		{name: "empty is not reachable", schema: strings.Repeat("x", 64), wantErr: protocol.ErrInvalidIdentifier},
		{name: "a NUL byte is refused", schema: "bad\x00schema", wantErr: protocol.ErrInvalidIdentifier},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ddl, err := SchemaDDL(tc.schema)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SchemaDDL: %v", err)
			}
			if len(ddl) == 0 {
				t.Fatal("no statements")
			}
			for i, stmt := range ddl {
				if strings.Contains(stmt, "%[1]s") {
					t.Errorf("statement %d still carries the schema placeholder:\n%s", i, stmt)
				}
			}
			if !strings.Contains(ddl[0], tc.wantQ) {
				t.Errorf("first statement does not name %s:\n%s", tc.wantQ, ddl[0])
			}
		})
	}
}

// A schema name is an identifier, so it is quoted, never concatenated raw
// (NFR-SEC-4). The classic injection attempt must end up inert inside quotes.
func TestSchemaDDLQuotesTheIdentifier(t *testing.T) {
	ddl, err := SchemaDDL(`we"ird`)
	if err != nil {
		t.Fatalf("SchemaDDL: %v", err)
	}
	if !strings.Contains(ddl[0], `"we""ird"`) {
		t.Fatalf("the embedded quote was not doubled:\n%s", ddl[0])
	}
}

func TestSchemaDDLContents(t *testing.T) {
	ddl, err := SchemaDDL(DefaultSchema)
	if err != nil {
		t.Fatalf("SchemaDDL: %v", err)
	}
	all := strings.Join(ddl, "\n")

	tests := []struct {
		name string
		want string
	}{
		{name: "schema is created", want: "CREATE SCHEMA IF NOT EXISTS"},
		{name: "audit table", want: "audit_event ("},
		{name: "per-run audit sequence is unique", want: "UNIQUE (run_id, seq)"},
		{name: "schema version is recorded", want: "schema_version"},
		{name: "a node record names the object it claims", want: "object_schema text NOT NULL DEFAULT ''"},
		{name: "the claim lookup has an index", want: "node_state_object_idx"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(all, tc.want) {
				t.Errorf("the bootstrap DDL does not contain %q", tc.want)
			}
		})
	}
}

// Bootstrap is the adoption cost of the SQL store: it runs against the
// customer's production database, and every privilege it needs is one more
// conversation with a DBA. It is CREATE SCHEMA, CREATE TABLE, CREATE INDEX and
// one seeded row, and nothing else. INV-3 and INV-6 are enforced by this
// package's Go API, which has no path that violates either.
func TestBootstrapNeedsNothingBeyondCreateTable(t *testing.T) {
	ddl, err := SchemaDDL(DefaultSchema)
	if err != nil {
		t.Fatalf("SchemaDDL: %v", err)
	}
	for i, stmt := range ddl {
		upper := strings.ToUpper(strings.TrimSpace(stmt))
		switch {
		case strings.HasPrefix(upper, "CREATE SCHEMA"),
			strings.HasPrefix(upper, "CREATE TABLE"),
			strings.HasPrefix(upper, "CREATE INDEX"),
			strings.HasPrefix(upper, "INSERT INTO"),
			// ALTER TABLE on a table this package created needs ownership,
			// which creating it already conferred. It is the in-place upgrade
			// from schema version 1 and buys no new conversation with a DBA.
			strings.HasPrefix(upper, "ALTER TABLE"):
		default:
			t.Errorf("bootstrap statement %d needs a privilege beyond CREATE TABLE:\n%s", i, stmt)
		}
	}
	if n := len(ddl); n > 13 {
		t.Errorf("bootstrap is %d statements; keep it small enough for a DBA to read", n)
	}
}

// Every bootstrap statement must be safe to run twice, because FR-STATE-3 says
// the schema is created on first use and every process start is a first use.
func TestSchemaDDLIsIdempotent(t *testing.T) {
	ddl, err := SchemaDDL(DefaultSchema)
	if err != nil {
		t.Fatalf("SchemaDDL: %v", err)
	}
	for i, stmt := range ddl {
		upper := strings.ToUpper(stmt)
		switch {
		case strings.HasPrefix(upper, "CREATE SCHEMA IF NOT EXISTS"),
			strings.HasPrefix(upper, "CREATE TABLE IF NOT EXISTS"),
			strings.HasPrefix(upper, "CREATE INDEX IF NOT EXISTS"),
			strings.HasPrefix(upper, "ALTER TABLE") && everyAddColumnIsGuarded(upper),
			strings.Contains(upper, "ON CONFLICT"):
			// Re-runnable.
		default:
			t.Errorf("statement %d is not idempotent:\n%s", i, stmt)
		}
	}
}

// FR-STATE-3: the schema is created on first use, and only once.
func TestEnsureSchemaBootstrapsOnceOnFirstUse(t *testing.T) {
	ctx := context.Background()
	db, fake := openFakeDB(t)
	s, err := NewSQLStore(db, SQLOptions{})
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}

	// Construction issues nothing: `status` against an unreachable target must
	// fail at the read it needed, not at construction (AC-25).
	if len(fake.Calls()) != 0 {
		t.Fatalf("NewSQLStore issued %d statements, want none", len(fake.Calls()))
	}

	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	want, err := SchemaDDL(DefaultSchema)
	if err != nil {
		t.Fatalf("SchemaDDL: %v", err)
	}
	got := fake.Queries()
	if len(got) != len(want) {
		t.Fatalf("issued %d statements, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("statement %d:\n got %s\nwant %s", i, got[i], want[i])
		}
	}

	fake.Reset()
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("second EnsureSchema: %v", err)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("the second bootstrap issued %d statements, want none", n)
	}
}

// A bootstrap that fails must not mark itself done, or the next call proceeds
// against a schema that does not exist.
func TestEnsureSchemaRetriesAfterFailure(t *testing.T) {
	ctx := context.Background()
	db, fake := openFakeDB(t)
	boom := errors.New("permission denied for database")
	fake.Fail("CREATE SCHEMA", boom)

	s, err := NewSQLStore(db, SQLOptions{})
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}
	if err := s.EnsureSchema(ctx); !errors.Is(err, ErrStoreIO) {
		t.Fatalf("err = %v, want ErrStoreIO", err)
	}
	fake.Reset()
	if err := s.EnsureSchema(ctx); err == nil {
		t.Fatal("the second attempt was skipped even though the first failed")
	}
	if fake.CountMatching("CREATE SCHEMA") == 0 {
		t.Fatal("the second attempt issued no DDL")
	}
}

// SkipBootstrap is for deployments where a DBA ran the DDL and the role cannot
// CREATE.
func TestSkipBootstrap(t *testing.T) {
	ctx := context.Background()
	db, fake := openFakeDB(t)
	s, err := NewSQLStore(db, SQLOptions{SkipBootstrap: true})
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("issued %d statements with SkipBootstrap, want none", n)
	}
}

func TestNewSQLStoreRejectsBadInput(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		nilDB  bool
	}{
		{name: "nil db", nilDB: true},
		{name: "over-long schema", schema: strings.Repeat("s", 64)},
		{name: "empty-ish schema with a NUL", schema: "a\x00b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var db = openNilDB(t)
			if tc.nilDB {
				db = nil
			}
			if _, err := NewSQLStore(db, SQLOptions{Schema: tc.schema}); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

// TestSkipBootstrapIssuesNoDDLOnARead is the store-side half of AC-1: "plan
// never opens a write transaction" and TRD §7.2.12's "plan connects read-only".
//
// The planner reads the claim table whenever it finds an index that is not
// healthy and carries no ownership marker, which is exactly the post-crash
// re-plan. That read goes through ready() -> EnsureSchema, so with the default
// options `plan` issues the whole bootstrap against the target. An operator
// planning as a deliberately read-only role, or against a hot standby, gets a
// bootstrap failure instead of a plan, with no indication that a WRITE was
// attempted.
func TestSkipBootstrapIssuesNoDDLOnARead(t *testing.T) {
	ctx := context.Background()
	db, fake := openFakeDB(t)
	s, err := NewSQLStore(db, SQLOptions{SkipBootstrap: true})
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}

	// The read itself may fail (the fake answers no rows); what matters is
	// what was issued.
	_, _, _ = ClaimsObjectIn(ctx, s, "appdb", protocol.NewObjectName("public", "orders_idx_p1"))

	for _, q := range fake.Queries() {
		upper := strings.ToUpper(strings.TrimSpace(q))
		for _, verb := range []string{"CREATE ", "REVOKE ", "GRANT ", "ALTER ", "DROP ", "COMMENT "} {
			if strings.HasPrefix(upper, verb) {
				t.Errorf("a read-only claim lookup issued DDL: %s", q)
			}
		}
	}
}

// everyAddColumnIsGuarded reports whether an ALTER TABLE adds no column without
// IF NOT EXISTS. A single unguarded clause makes the whole statement fail on the
// second run, which is every run after the first (FR-STATE-3).
func everyAddColumnIsGuarded(upperStmt string) bool {
	n := strings.Count(upperStmt, "ADD COLUMN")
	return n > 0 && n == strings.Count(upperStmt, "ADD COLUMN IF NOT EXISTS")
}

// m1Schema is the shape a schema has after a version-1 binary bootstrapped it,
// transcribed from `git show 0c9e0f2:engine/state/sql_ddl.go` (the M1 commit).
//
// Only the table and column names matter here; types and constraints do not,
// because the question this fixture answers is the one PostgreSQL answers with
// SQLSTATE 42703 — does the column the next statement names exist.
func m1Schema() map[string]map[string]bool {
	return catalogOf(map[string][]string{
		"meta": {"key", "value"},
		"run": {
			"run_id", "plan_id", "plan_digest", "operation", "database_name",
			"table_schema", "table_name", "index_schema", "index_name", "has_index",
			"status", "actor", "node_count", "cancel_requested", "cancel_requested_at",
			"cancel_actor", "cancel_note", "last_error", "started_at", "updated_at",
			"finished_at",
		},
		// The two columns M2 added -- object_schema and object_name -- are
		// deliberately absent. That absence is the bug under test.
		"node_state": {
			"run_id", "node_id", "kind", "state", "attempts", "last_error",
			"error_kind", "started_at", "updated_at",
		},
		"lease": {"run_id", "holder", "acquired_at", "heartbeat_at", "ttl_seconds"},
		"provenance": {
			"provenance_id", "run_id", "node_id", "plan_digest", "database_name",
			"object_schema", "object_name", "object_kind", "relation_schema",
			"relation_name", "has_relation", "actor", "recorded_at",
		},
		"authorization": {
			"authorization_id", "run_id", "node_id", "mode", "database_name",
			"object_schema", "object_name", "relation_schema", "relation_name",
			"has_relation", "confirmation", "evidence", "granted_at",
			"provenance_id", "reindex_run_id",
		},
		"audit_event": {
			"event_id", "run_id", "seq", "node_id", "event_type", "detail", "occurred_at",
		},
	})
}

func catalogOf(tables map[string][]string) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(tables))
	for name, cols := range tables {
		set := make(map[string]bool, len(cols))
		for _, c := range cols {
			set[c] = true
		}
		out[name] = set
	}
	return out
}

// An installation that ran a version-1 binary has a node_state with no object
// columns. Bootstrap runs on every process start, so if it cannot bring that
// schema forward, every command fails for good -- `status` included, which is
// the sharper half: the operator cannot even read the run history the upgrade
// was supposed to preserve.
//
// TestSchemaDDLIsIdempotent does not cover this. It proves each statement is
// re-runnable against a schema of the *same* shape, which is the case that was
// never at risk.
func TestBootstrapUpgradesAVersion1Schema(t *testing.T) {
	ddl, err := SchemaDDL(DefaultSchema)
	if err != nil {
		t.Fatalf("SchemaDDL: %v", err)
	}
	if err := replayDDL(ddl, m1Schema()); err != nil {
		t.Fatalf("bootstrap cannot upgrade a schema an M1 binary created: %v", err)
	}
}

// The same replay against an empty database, which keeps the fixture above
// honest: if the replayer were too permissive to catch anything, or the DDL
// grew a statement that referenced a column no CREATE TABLE declares, this
// fails too.
func TestBootstrapReplaysOnAnEmptyDatabase(t *testing.T) {
	ddl, err := SchemaDDL(DefaultSchema)
	if err != nil {
		t.Fatalf("SchemaDDL: %v", err)
	}
	catalog := map[string]map[string]bool{}
	if err := replayDDL(ddl, catalog); err != nil {
		t.Fatalf("bootstrap failed on an empty database: %v", err)
	}
	// And it must be re-runnable against what it just built.
	if err := replayDDL(ddl, catalog); err != nil {
		t.Fatalf("bootstrap is not re-runnable against its own output: %v", err)
	}
	for _, want := range []string{"object_schema", "object_name"} {
		if !catalog["node_state"][want] {
			t.Errorf("node_state has no %s column after bootstrap", want)
		}
	}
}

// The replayer proves it can see the failure it exists to catch: drop the
// upgrade statement and the version-1 schema must break exactly where a real
// PostgreSQL breaks, on the index over the missing columns.
func TestReplayerCatchesTheMissingUpgrade(t *testing.T) {
	ddl, err := SchemaDDL(DefaultSchema)
	if err != nil {
		t.Fatalf("SchemaDDL: %v", err)
	}
	var withoutUpgrade []string
	for _, stmt := range ddl {
		if strings.HasPrefix(strings.TrimSpace(strings.ToUpper(stmt)), "ALTER TABLE") {
			continue
		}
		withoutUpgrade = append(withoutUpgrade, stmt)
	}
	if len(withoutUpgrade) == len(ddl) {
		t.Fatal("no ALTER TABLE in the bootstrap DDL, so there is no upgrade to remove")
	}
	err = replayDDL(withoutUpgrade, m1Schema())
	if err == nil {
		t.Fatal("replaying a version-1 schema without the upgrade succeeded; the replayer is blind")
	}
	if !strings.Contains(err.Error(), "object_schema") {
		t.Errorf("expected the failure to name the missing column, got: %v", err)
	}
}

// replayDDL applies bootstrap statements to an in-memory catalog with the same
// answers PostgreSQL gives to the only question that matters here: does the
// column this statement names exist?
//
// The load-bearing rule is that CREATE TABLE IF NOT EXISTS against an existing
// table is a no-op *whatever shape that table has*. That is precisely why a
// schema created by an older binary is not brought forward by re-running the
// CREATE TABLEs, and why the upgrade has to be an explicit ALTER.
func replayDDL(stmts []string, catalog map[string]map[string]bool) error {
	for i, raw := range stmts {
		stmt := collapse(raw)
		upper := strings.ToUpper(stmt)
		var err error
		switch {
		case strings.HasPrefix(upper, "CREATE SCHEMA"):
		case strings.HasPrefix(upper, "CREATE TABLE"):
			err = replayCreateTable(stmt, catalog)
		case strings.HasPrefix(upper, "ALTER TABLE"):
			err = replayAlterTable(stmt, catalog)
		case strings.HasPrefix(upper, "CREATE INDEX"):
			err = replayCreateIndex(stmt, catalog)
		case strings.HasPrefix(upper, "INSERT INTO"):
			err = replayInsert(stmt, catalog)
		default:
			err = fmt.Errorf("the replayer does not model this statement")
		}
		if err != nil {
			return fmt.Errorf("statement %d of %d: %w:\n%s", i+1, len(stmts), err, stmt)
		}
	}
	return nil
}

func replayCreateTable(stmt string, catalog map[string]map[string]bool) error {
	table := tableAfter(stmt, "EXISTS ")
	if table == "" {
		return fmt.Errorf("cannot find the table name")
	}
	if _, exists := catalog[table]; exists {
		// The no-op that causes the bug.
		return nil
	}
	body, ok := parenGroup(stmt)
	if !ok {
		return fmt.Errorf("cannot find the column list")
	}
	cols := map[string]bool{}
	for _, part := range splitTopLevel(body) {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "PRIMARY", "UNIQUE", "FOREIGN", "CHECK", "CONSTRAINT":
			continue
		}
		cols[fields[0]] = true
	}
	catalog[table] = cols
	return nil
}

func replayAlterTable(stmt string, catalog map[string]map[string]bool) error {
	table := tableAfter(stmt, "ALTER TABLE ")
	cols, exists := catalog[table]
	if !exists {
		return fmt.Errorf("relation %q does not exist", table)
	}
	const guard = "ADD COLUMN IF NOT EXISTS "
	rest := stmt
	for {
		i := strings.Index(strings.ToUpper(rest), guard)
		if i < 0 {
			break
		}
		rest = rest[i+len(guard):]
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return fmt.Errorf("ADD COLUMN IF NOT EXISTS names no column")
		}
		cols[strings.TrimSuffix(fields[0], ",")] = true
	}
	return nil
}

func replayCreateIndex(stmt string, catalog map[string]map[string]bool) error {
	i := strings.Index(strings.ToUpper(stmt), " ON ")
	if i < 0 {
		return fmt.Errorf("cannot find the indexed relation")
	}
	table := tableName(strings.Fields(stmt[i+4:])[0])
	cols, exists := catalog[table]
	if !exists {
		return fmt.Errorf("relation %q does not exist", table)
	}
	body, ok := parenGroup(stmt[i:])
	if !ok {
		return fmt.Errorf("cannot find the index column list")
	}
	for _, c := range splitTopLevel(body) {
		c = strings.TrimSpace(c)
		if !cols[c] {
			// PostgreSQL's SQLSTATE 42703.
			return fmt.Errorf("column %q does not exist on %s", c, table)
		}
	}
	return nil
}

func replayInsert(stmt string, catalog map[string]map[string]bool) error {
	table := tableAfter(stmt, "INSERT INTO ")
	cols, exists := catalog[table]
	if !exists {
		return fmt.Errorf("relation %q does not exist", table)
	}
	body, ok := parenGroup(stmt)
	if !ok {
		return fmt.Errorf("cannot find the insert column list")
	}
	for _, c := range splitTopLevel(body) {
		c = strings.TrimSpace(c)
		if !cols[c] {
			return fmt.Errorf("column %q does not exist on %s", c, table)
		}
	}
	return nil
}

// tableAfter reads the qualified relation name that follows a marker and
// returns its bare name.
func tableAfter(stmt, marker string) string {
	i := strings.Index(strings.ToUpper(stmt), strings.ToUpper(marker))
	if i < 0 {
		return ""
	}
	fields := strings.Fields(stmt[i+len(marker):])
	if len(fields) == 0 {
		return ""
	}
	return tableName(fields[0])
}

// tableName strips the quoted schema qualifier and any trailing "(" that ran
// into the identifier.
func tableName(tok string) string {
	if i := strings.LastIndex(tok, "."); i >= 0 {
		tok = tok[i+1:]
	}
	if i := strings.Index(tok, "("); i >= 0 {
		tok = tok[:i]
	}
	return strings.Trim(tok, `"`)
}

// parenGroup returns the contents of the first balanced parenthesised group.
func parenGroup(s string) (string, bool) {
	start := strings.Index(s, "(")
	if start < 0 {
		return "", false
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[start+1 : i], true
			}
		}
	}
	return "", false
}

// splitTopLevel splits on commas that are not inside parentheses, so that
// "PRIMARY KEY (run_id, node_id)" stays one element.
func splitTopLevel(s string) []string {
	var (
		out   []string
		depth int
		cur   strings.Builder
	)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(cur.String()))
				cur.Reset()
				continue
			}
		}
		cur.WriteByte(s[i])
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, strings.TrimSpace(cur.String()))
	}
	return out
}

func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
