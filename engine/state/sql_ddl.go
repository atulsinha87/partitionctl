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
const SchemaVersion = "1"

// ddlTemplates is the bootstrap DDL, with %[1]s standing for the quoted schema
// name.
//
// Every statement is idempotent, because FR-STATE-3 says the schema is created
// on first use and "first use" happens on every process start. The statements
// are separate strings rather than one script so that a driver which refuses
// multi-statement Exec still works, and so that a failure names the statement
// that failed.
var ddlTemplates = []string{
	`CREATE SCHEMA IF NOT EXISTS %[1]s`,

	`CREATE TABLE IF NOT EXISTS %[1]s.meta (
	key text PRIMARY KEY,
	value text NOT NULL
)`,

	`INSERT INTO %[1]s.meta (key, value) VALUES ('schema_version', '` + SchemaVersion + `')
	ON CONFLICT (key) DO NOTHING`,

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

	// INV-6: a run is bound to exactly one plan digest for its lifetime. The
	// Go API has no path that changes it; this is the second lock on the same
	// door, for anyone reaching the table with psql.
	`CREATE OR REPLACE FUNCTION %[1]s.run_digest_immutable() RETURNS trigger
	LANGUAGE plpgsql AS $fn$
BEGIN
	IF NEW.plan_digest IS DISTINCT FROM OLD.plan_digest THEN
		RAISE EXCEPTION
			'partitionctl: run % is bound to plan digest %, it cannot be rebound to % (INV-6)',
			OLD.run_id, OLD.plan_digest, NEW.plan_digest;
	END IF;
	IF NEW.run_id IS DISTINCT FROM OLD.run_id THEN
		RAISE EXCEPTION 'partitionctl: run_id is immutable';
	END IF;
	RETURN NEW;
END;
$fn$`,

	`CREATE OR REPLACE TRIGGER run_digest_immutable
	BEFORE UPDATE ON %[1]s.run
	FOR EACH ROW EXECUTE FUNCTION %[1]s.run_digest_immutable()`,

	`CREATE TABLE IF NOT EXISTS %[1]s.node_state (
	run_id text NOT NULL REFERENCES %[1]s.run (run_id),
	node_id text NOT NULL,
	kind text NOT NULL,
	state text NOT NULL,
	attempts integer NOT NULL DEFAULT 0,
	last_error text NOT NULL DEFAULT '',
	error_kind text NOT NULL DEFAULT '',
	started_at timestamptz,
	updated_at timestamptz NOT NULL,
	PRIMARY KEY (run_id, node_id)
)`,

	`CREATE TABLE IF NOT EXISTS %[1]s.lease (
	run_id text PRIMARY KEY REFERENCES %[1]s.run (run_id),
	holder text NOT NULL,
	acquired_at timestamptz NOT NULL,
	heartbeat_at timestamptz NOT NULL,
	ttl_seconds integer NOT NULL
)`,

	`CREATE TABLE IF NOT EXISTS %[1]s.provenance (
	provenance_id text PRIMARY KEY,
	run_id text NOT NULL REFERENCES %[1]s.run (run_id),
	node_id text NOT NULL DEFAULT '',
	plan_digest text NOT NULL DEFAULT '',
	database_name text NOT NULL DEFAULT '',
	object_schema text NOT NULL DEFAULT '',
	object_name text NOT NULL,
	object_kind text NOT NULL,
	relation_schema text NOT NULL DEFAULT '',
	relation_name text NOT NULL DEFAULT '',
	has_relation boolean NOT NULL DEFAULT false,
	actor text NOT NULL DEFAULT '',
	recorded_at timestamptz NOT NULL
)`,

	`CREATE INDEX IF NOT EXISTS provenance_object_idx
	ON %[1]s.provenance (database_name, object_schema, object_name)`,

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
	provenance_id text NOT NULL DEFAULT '',
	reindex_run_id text NOT NULL DEFAULT '',
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

	// INV-3: the audit trail is append-only. The Go API exposes no update or
	// delete path; this makes the table refuse one even from psql.
	`CREATE OR REPLACE FUNCTION %[1]s.audit_event_append_only() RETURNS trigger
	LANGUAGE plpgsql AS $fn$
BEGIN
	RAISE EXCEPTION
		'partitionctl: %[1]s.audit_event is append-only, % is not permitted (INV-3)', TG_OP;
END;
$fn$`,

	`CREATE OR REPLACE TRIGGER audit_event_append_only
	BEFORE UPDATE OR DELETE ON %[1]s.audit_event
	FOR EACH ROW EXECUTE FUNCTION %[1]s.audit_event_append_only()`,

	// TRUNCATE needs its own statement-level trigger: a FOR EACH ROW trigger
	// never fires on it, so the trigger above leaves the fastest way to erase
	// the whole trail wide open. The REVOKE below does not close it either,
	// because a REVOKE ... FROM PUBLIC has no effect on the table's owner,
	// which is the role that ran the bootstrap and is normally the same role
	// PartitionCTL connects as. Without this, `TRUNCATE audit_event` from psql
	// silently erases the trail while `DELETE FROM` is correctly refused
	// (INV-3).
	`CREATE OR REPLACE TRIGGER audit_event_no_truncate
	BEFORE TRUNCATE ON %[1]s.audit_event
	FOR EACH STATEMENT EXECUTE FUNCTION %[1]s.audit_event_append_only()`,

	`REVOKE UPDATE, DELETE, TRUNCATE ON %[1]s.audit_event FROM PUBLIC`,
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
// fmt.Sprintf is deliberately not used: the DDL contains PostgreSQL format
// specifiers of its own, the % placeholders inside RAISE EXCEPTION, and
// Sprintf would mangle them into %!(NOVERB). A plain replace has no such
// opinion about the rest of the string.
func expandSchema(tmpl, quotedSchema string) string {
	return strings.ReplaceAll(tmpl, "%[1]s", quotedSchema)
}
