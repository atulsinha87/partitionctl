package state

import (
	"context"
	"errors"
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

// The DDL must actually install the two in-database guards, because the Go API
// is only half of the enforcement story for INV-3 and INV-6.
func TestSchemaDDLInstallsTheInvariantGuards(t *testing.T) {
	ddl, err := SchemaDDL(DefaultSchema)
	if err != nil {
		t.Fatalf("SchemaDDL: %v", err)
	}
	all := strings.Join(ddl, "\n")

	tests := []struct {
		name string
		want string
	}{
		{name: "INV-3 trigger function", want: "audit_event_append_only() RETURNS trigger"},
		{name: "INV-3 trigger", want: "BEFORE UPDATE OR DELETE ON"},
		{name: "INV-3 revoke", want: "REVOKE UPDATE, DELETE, TRUNCATE ON"},
		{name: "INV-6 trigger function", want: "run_digest_immutable() RETURNS trigger"},
		{name: "INV-6 trigger", want: "BEFORE UPDATE ON"},
		{name: "schema is created", want: "CREATE SCHEMA IF NOT EXISTS"},
		{name: "audit table", want: "audit_event ("},
		{name: "per-run audit sequence is unique", want: "UNIQUE (run_id, seq)"},
		{name: "schema version is recorded", want: "schema_version"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(all, tc.want) {
				t.Errorf("the bootstrap DDL does not contain %q", tc.want)
			}
		})
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
			strings.HasPrefix(upper, "CREATE OR REPLACE FUNCTION"),
			strings.HasPrefix(upper, "CREATE OR REPLACE TRIGGER"),
			strings.HasPrefix(upper, "REVOKE"),
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
// The planner reads provenance whenever it finds an index that is not healthy,
// which is exactly the post-crash re-plan. That read goes through ready() ->
// EnsureSchema, so with the default options `plan` issues CREATE SCHEMA, CREATE
// TABLE, CREATE FUNCTION, CREATE TRIGGER and REVOKE against the target. An
// operator planning as a deliberately read-only role, or against a hot standby,
// gets a bootstrap failure instead of a plan, with no indication that a WRITE
// was attempted.
func TestSkipBootstrapIssuesNoDDLOnARead(t *testing.T) {
	ctx := context.Background()
	db, fake := openFakeDB(t)
	s, err := NewSQLStore(db, SQLOptions{SkipBootstrap: true})
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}

	// The read itself may fail (the fake answers no rows); what matters is
	// what was issued.
	_, _ = s.FindProvenance(ctx, ProvenanceQuery{
		Object:   protocol.NewObjectName("public", "orders_idx_p1"),
		Database: "appdb",
	})

	for _, q := range fake.Queries() {
		upper := strings.ToUpper(strings.TrimSpace(q))
		for _, verb := range []string{"CREATE ", "REVOKE ", "GRANT ", "ALTER ", "DROP ", "COMMENT "} {
			if strings.HasPrefix(upper, verb) {
				t.Errorf("a read-only provenance lookup issued DDL: %s", q)
			}
		}
	}
}

// TestAuditTrailRefusesTruncate is INV-3: "the audit trail is append-only".
//
// A BEFORE UPDATE OR DELETE ... FOR EACH ROW trigger does not fire on TRUNCATE,
// and REVOKE ... FROM PUBLIC has no effect on the table's owner, which is the
// role that ran the bootstrap and normally the role PartitionCTL connects as.
// So the fastest way to erase the entire trail was the one path left unguarded,
// while the slower DELETE was correctly refused.
func TestAuditTrailRefusesTruncate(t *testing.T) {
	ddl, err := SchemaDDL(DefaultSchema)
	if err != nil {
		t.Fatalf("SchemaDDL: %v", err)
	}
	joined := strings.Join(ddl, "\n")

	if !strings.Contains(joined, "BEFORE TRUNCATE") {
		t.Error("no BEFORE TRUNCATE trigger on audit_event; TRUNCATE erases the trail (INV-3)")
	}
	// A TRUNCATE trigger is only legal, and only fires, at statement level.
	truncateStmt := ""
	for _, s := range ddl {
		if strings.Contains(s, "BEFORE TRUNCATE") {
			truncateStmt = s
		}
	}
	if truncateStmt == "" {
		t.Fatal("no TRUNCATE trigger statement found")
	}
	if !strings.Contains(truncateStmt, "FOR EACH STATEMENT") {
		t.Errorf("the TRUNCATE trigger is not statement-level, so it never fires:\n%s", truncateStmt)
	}
	if !strings.Contains(truncateStmt, "audit_event_append_only()") {
		t.Errorf("the TRUNCATE trigger does not raise the append-only exception:\n%s", truncateStmt)
	}
}
