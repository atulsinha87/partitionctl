package state

import (
	"strings"

	"github.com/atulsinha/partitionctl/engine/protocol"
)

// DefaultSchema is the dedicated schema the SQL state store lives in,
// created on first use (FR-STATE-3).
const DefaultSchema = "partitionctl"

// SchemaVersion is the state schema's own version, recorded in the meta table.
// It is not the plan format version: the two evolve independently, and
// conflating them would tie a state migration to a plan-format bump.
//
// Version 2 added node_state.object_schema and node_state.object_name, which is
// what the claim lookup reads. The row is written with DO UPDATE rather than
// DO NOTHING so that a schema bootstrapped by a version-1 binary and since
// upgraded in place reports the version it actually has, not the version it was
// created at.
const SchemaVersion = "2"

// ddlTemplates is the bootstrap DDL, with %[1]s standing for the quoted schema
// name.
//
// Every statement is idempotent, because FR-STATE-3 says the schema is created
// on first use and "first use" happens on every process start. The statements
// are separate strings rather than one script so that a driver which refuses
// multi-statement Exec still works, and so that a failure names the statement
// that failed.
//
// # Why there are no triggers here any more
//
// Earlier versions installed two plpgsql functions and three triggers: one
// pair enforcing INV-6 (a run's plan digest is immutable) and one enforcing
// INV-3 (the audit trail is append-only), plus a REVOKE. Both invariants are
// enforced by this package's Go API, which has no path that rewrites a plan
// digest ([RunStore.SetRunStatus] never touches it) and no update or delete
// method for an audit event at all. What the triggers bought was defence
// against an operator hand-editing PartitionCTL's own bookkeeping tables with
// psql; what they cost was CREATE FUNCTION privilege and eighteen bootstrap
// statements against the customer's production database.
//
// Bootstrap is now CREATE SCHEMA, CREATE TABLE and CREATE INDEX and nothing
// else, which is a materially smaller adoption ask (G4, NFR-SEC-1) and is
// something a DBA can read in one sitting and pre-create by hand under
// SkipBootstrap. [SchemaDDL] stays exported for exactly that.
var ddlTemplates = []string{
	`CREATE SCHEMA IF NOT EXISTS %[1]s`,

	`CREATE TABLE IF NOT EXISTS %[1]s.meta (
	key text PRIMARY KEY,
	value text NOT NULL
)`,

	`INSERT INTO %[1]s.meta (key, value) VALUES ('schema_version', '` + SchemaVersion + `')
	ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`,

	`CREATE TABLE IF NOT EXISTS %[1]s.run (
	run_id text PRIMARY KEY,
	plan_id text NOT NULL,
	plan_digest text NOT NULL,
	operation text NOT NULL,
	database_name text NOT NULL DEFAULT '',
	table_schema text NOT NULL DEFAULT '',
	table_name text NOT NULL,
	index_schema text NOT NULL DEFAULT '',
	index_name text NOT NULL DEFAULT '',
	has_index boolean NOT NULL DEFAULT false,
	status text NOT NULL,
	actor text NOT NULL DEFAULT '',
	node_count integer NOT NULL DEFAULT 0,
	cancel_requested boolean NOT NULL DEFAULT false,
	cancel_requested_at timestamptz,
	cancel_actor text NOT NULL DEFAULT '',
	cancel_note text NOT NULL DEFAULT '',
	last_error text NOT NULL DEFAULT '',
	started_at timestamptz NOT NULL,
	updated_at timestamptz NOT NULL,
	finished_at timestamptz
)`,

	`CREATE INDEX IF NOT EXISTS run_target_idx
	ON %[1]s.run (database_name, table_schema, table_name, status)`,

	`CREATE INDEX IF NOT EXISTS run_plan_digest_idx ON %[1]s.run (plan_digest)`,

	`CREATE TABLE IF NOT EXISTS %[1]s.node_state (
	run_id text NOT NULL REFERENCES %[1]s.run (run_id),
	node_id text NOT NULL,
	kind text NOT NULL,
	state text NOT NULL,
	object_schema text NOT NULL DEFAULT '',
	object_name text NOT NULL DEFAULT '',
	attempts integer NOT NULL DEFAULT 0,
	last_error text NOT NULL DEFAULT '',
	error_kind text NOT NULL DEFAULT '',
	started_at timestamptz,
	updated_at timestamptz NOT NULL,
	PRIMARY KEY (run_id, node_id)
)`,

	// The in-place upgrade from schema version 1.
	//
	// CREATE TABLE IF NOT EXISTS is a no-op against a table that already
	// exists, whatever shape it has, so a schema bootstrapped by a version-1
	// binary keeps a node_state with no object columns and every later
	// statement that names them fails. That is not a corner case: it is what
	// every existing installation looks like on the first run of this binary,
	// on the default state backend, and the failure is permanent because
	// bootstrap runs on every process start — `status` included, so an
	// operator could not even read their own run history (FR-CLI-12, AC-25).
	//
	// ADD COLUMN IF NOT EXISTS is idempotent, is one catalog write on a fresh
	// schema where the columns are already there, and needs no privilege
	// beyond ownership of a table this package created. It must precede the
	// index below, which is the statement that would otherwise fail 42703.
	`ALTER TABLE %[1]s.node_state
	ADD COLUMN IF NOT EXISTS object_schema text NOT NULL DEFAULT '',
	ADD COLUMN IF NOT EXISTS object_name text NOT NULL DEFAULT ''`,

	// The claim lookup asks "does any live node record name this object?", so
	// it reads across runs rather than within one (see ClaimsObject).
	`CREATE INDEX IF NOT EXISTS node_state_object_idx
	ON %[1]s.node_state (object_schema, object_name)`,

	`CREATE TABLE IF NOT EXISTS %[1]s.lease (
	run_id text PRIMARY KEY REFERENCES %[1]s.run (run_id),
	holder text NOT NULL,
	acquired_at timestamptz NOT NULL,
	heartbeat_at timestamptz NOT NULL,
	ttl_seconds integer NOT NULL
)`,

	`CREATE TABLE IF NOT EXISTS %[1]s.authorization (
	authorization_id text PRIMARY KEY,
	run_id text NOT NULL REFERENCES %[1]s.run (run_id),
	node_id text NOT NULL,
	mode text NOT NULL,
	database_name text NOT NULL DEFAULT '',
	object_schema text NOT NULL DEFAULT '',
	object_name text NOT NULL,
	relation_schema text NOT NULL DEFAULT '',
	relation_name text NOT NULL DEFAULT '',
	has_relation boolean NOT NULL DEFAULT false,
	confirmation text NOT NULL DEFAULT '',
	evidence jsonb NOT NULL DEFAULT '{}'::jsonb,
	granted_at timestamptz NOT NULL
)`,

	`CREATE INDEX IF NOT EXISTS authorization_run_idx ON %[1]s.authorization (run_id, granted_at)`,

	`CREATE TABLE IF NOT EXISTS %[1]s.audit_event (
	event_id text PRIMARY KEY,
	run_id text NOT NULL,
	seq bigint NOT NULL,
	node_id text NOT NULL DEFAULT '',
	event_type text NOT NULL,
	detail jsonb NOT NULL DEFAULT '{}'::jsonb,
	occurred_at timestamptz NOT NULL,
	UNIQUE (run_id, seq)
)`,
}

// SchemaDDL returns the bootstrap statements for a schema, in the order they
// must run (FR-STATE-3).
//
// It is exported so that an operator who cannot grant CREATE on the database
// can hand the statements to a DBA, and so the CLI can print them. The schema
// name is validated as an identifier and quoted, so it cannot carry SQL
// (NFR-SEC-4).
func SchemaDDL(schema string) ([]string, error) {
	q, err := quoteSchema(schema)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(ddlTemplates))
	for i, t := range ddlTemplates {
		out[i] = expandSchema(t, q)
	}
	return out, nil
}

func quoteSchema(schema string) (string, error) {
	if schema == "" {
		schema = DefaultSchema
	}
	if err := protocol.ValidateIdentifier(schema); err != nil {
		return "", err
	}
	return protocol.QuoteIdentifier(schema), nil
}

// expandSchema substitutes the quoted schema for %[1]s.
//
// fmt.Sprintf is deliberately not used. Any % this DDL grows would be a
// PostgreSQL format specifier rather than a Go one, and Sprintf would mangle it
// into %!(NOVERB). A plain replace has no opinion about the rest of the string.
func expandSchema(tmpl, quotedSchema string) string {
	return strings.ReplaceAll(tmpl, "%[1]s", quotedSchema)
}
