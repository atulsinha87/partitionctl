package state

import (
	"strconv"
	"strings"
)

// runColumns is the run projection, used by every read so the scan order is
// defined in exactly one place.
const runColumns = `run_id, plan_id, plan_digest, operation, database_name, table_schema, table_name, ` +
	`index_schema, index_name, has_index, status, actor, node_count, cancel_requested, ` +
	`cancel_requested_at, cancel_actor, cancel_note, last_error, started_at, updated_at, finished_at`

const nodeColumns = `run_id, node_id, kind, state, object_schema, object_name, attempts, ` +
	`last_error, error_kind, started_at, updated_at`

const authorizationColumns = `authorization_id, run_id, node_id, mode, database_name, ` +
	`object_schema, object_name, relation_schema, relation_name, has_relation, ` +
	`confirmation, evidence, granted_at`

const auditColumns = `event_id, run_id, seq, node_id, event_type, detail, occurred_at`

const leaseColumns = `run_id, holder, acquired_at, heartbeat_at, ttl_seconds`

// sqlText holds every statement the SQL store issues, rendered against one
// quoted schema.
//
// Building them up front rather than formatting per call has two payoffs: the
// schema identifier is quoted exactly once, at construction, so no call site
// can forget (NFR-SEC-4); and the whole SQL surface is a value a test can
// assert on without a database, which is what "test SQL generation without a
// live DB" needs.
type sqlText struct {
	schema       string
	quotedSchema string

	ddl []string

	insertRun        string
	selectRun        string
	updateRunStatus  string
	updateRunCancel  string
	selectRunningFor string

	insertNode     string
	selectNode     string
	selectNodes    string
	transitionNode string

	insertAuthorization  string
	selectAuthorizations string

	upsertLease string
	heartbeat   string
	selectLease string
	deleteLease string

	insertAudit string
	selectAudit string

	tryAdvisoryLock string
	advisoryUnlock  string
}

func newSQLText(schema string) (sqlText, error) {
	if schema == "" {
		schema = DefaultSchema
	}
	q, err := quoteSchema(schema)
	if err != nil {
		return sqlText{}, err
	}
	ddl, err := SchemaDDL(schema)
	if err != nil {
		return sqlText{}, err
	}
	t := sqlText{schema: schema, quotedSchema: q, ddl: ddl}

	t.insertRun = `INSERT INTO ` + q + `.run (` + runColumns + `) VALUES ` +
		placeholders(21)

	t.selectRun = `SELECT ` + runColumns + ` FROM ` + q + `.run WHERE run_id = $1`

	// The CAS on status is what turns a lost update into an error instead of a
	// silent overwrite: `cancel` and `resume` can reach the same run at once.
	// $7 clears an outstanding cancellation request as part of the transition,
	// which is what adoption needs so a resumed run does not stop again on its
	// first node boundary (AC-24).
	t.updateRunStatus = `UPDATE ` + q + `.run SET status = $2, updated_at = $3, finished_at = $4, ` +
		`last_error = CASE WHEN $5 = '' THEN last_error ELSE $5 END, ` +
		`cancel_requested = CASE WHEN $7 THEN false ELSE cancel_requested END, ` +
		`cancel_requested_at = CASE WHEN $7 THEN NULL ELSE cancel_requested_at END, ` +
		`cancel_actor = CASE WHEN $7 THEN '' ELSE cancel_actor END, ` +
		`cancel_note = CASE WHEN $7 THEN '' ELSE cancel_note END ` +
		`WHERE run_id = $1 AND status = $6 RETURNING ` + runColumns

	t.updateRunCancel = `UPDATE ` + q + `.run SET cancel_requested = true, cancel_requested_at = $2, ` +
		`cancel_actor = $3, cancel_note = $4, updated_at = $2 ` +
		`WHERE run_id = $1 RETURNING ` + runColumns

	t.selectRunningFor = `SELECT ` + runColumns + ` FROM ` + q + `.run ` +
		`WHERE database_name = $1 AND table_schema = $2 AND table_name = $3 AND status = 'RUNNING' ` +
		`ORDER BY started_at LIMIT 1`

	t.insertNode = `INSERT INTO ` + q + `.node_state (` + nodeColumns + `) VALUES ` + placeholders(11)

	t.selectNode = `SELECT ` + nodeColumns + ` FROM ` + q + `.node_state WHERE run_id = $1 AND node_id = $2`

	t.selectNodes = `SELECT ` + nodeColumns + ` FROM ` + q + `.node_state WHERE run_id = $1 ORDER BY node_id`

	// started_at is stamped on the first entry into RUNNING and never moved:
	// COALESCE keeps the original when $7 is NULL and when it already has one.
	t.transitionNode = `UPDATE ` + q + `.node_state SET state = $3, attempts = attempts + $4, ` +
		`last_error = $5, error_kind = $6, started_at = COALESCE(started_at, $7), updated_at = $8 ` +
		`WHERE run_id = $1 AND node_id = $2 AND state = $9 RETURNING ` + nodeColumns

	t.insertAuthorization = `INSERT INTO ` + q + `.authorization (` + authorizationColumns + `) VALUES ` +
		`($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb, $13)`

	t.selectAuthorizations = `SELECT ` + authorizationColumns + ` FROM ` + q + `.authorization ` +
		`WHERE run_id = $1 ORDER BY granted_at, authorization_id`

	// The WHERE on DO UPDATE is the fence: an unexpired lease held by someone
	// else updates no row, and no row back means ErrLeaseLost.
	t.upsertLease = `INSERT INTO ` + q + `.lease (` + leaseColumns + `) VALUES ($1, $2, $3, $3, $4) ` +
		`ON CONFLICT (run_id) DO UPDATE SET ` +
		`holder = EXCLUDED.holder, ` +
		`acquired_at = CASE WHEN lease.holder = EXCLUDED.holder THEN lease.acquired_at ELSE EXCLUDED.acquired_at END, ` +
		`heartbeat_at = EXCLUDED.heartbeat_at, ` +
		`ttl_seconds = EXCLUDED.ttl_seconds ` +
		`WHERE lease.holder = EXCLUDED.holder ` +
		`OR lease.heartbeat_at + make_interval(secs => lease.ttl_seconds) <= EXCLUDED.heartbeat_at ` +
		`RETURNING ` + leaseColumns

	t.heartbeat = `UPDATE ` + q + `.lease SET heartbeat_at = $3 WHERE run_id = $1 AND holder = $2 ` +
		`RETURNING ` + leaseColumns

	t.selectLease = `SELECT ` + leaseColumns + ` FROM ` + q + `.lease WHERE run_id = $1`

	t.deleteLease = `DELETE FROM ` + q + `.lease WHERE run_id = $1 AND holder = $2`

	// Seq is per run and assigned server-side in the same statement as the
	// insert, so two appenders cannot both compute the same next value and
	// then both write it: the UNIQUE (run_id, seq) constraint rejects the
	// loser. There is no UPDATE and no DELETE anywhere in this file (INV-3).
	t.insertAudit = `INSERT INTO ` + q + `.audit_event (` + auditColumns + `) ` +
		`SELECT $1 || ':evt:' || lpad(t.next_seq::text, 8, '0'), $1, t.next_seq, $2, $3, $4::jsonb, $5 ` +
		`FROM (SELECT COALESCE(MAX(seq), 0) + 1 AS next_seq FROM ` + q + `.audit_event WHERE run_id = $1) AS t ` +
		`RETURNING ` + auditColumns

	t.selectAudit = `SELECT ` + auditColumns + ` FROM ` + q + `.audit_event ` +
		`WHERE run_id = $1 AND seq > $2 ORDER BY seq`

	t.tryAdvisoryLock = `SELECT pg_try_advisory_lock($1, $2)`
	t.advisoryUnlock = `SELECT pg_advisory_unlock($1, $2)`

	return t, nil
}

// placeholders renders "($1, $2, ... $n)".
func placeholders(n int) string {
	var b strings.Builder
	b.WriteByte('(')
	for i := 1; i <= n; i++ {
		if i > 1 {
			b.WriteString(", ")
		}
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(i))
	}
	b.WriteByte(')')
	return b.String()
}

// runQuery renders a [RunQuery] as SQL plus its arguments.
//
// Every value is bound, never interpolated: a RunQuery carries operator-typed
// strings such as a table name, and the identifier-quoting rule (NFR-SEC-4)
// applies to identifiers in DDL, not to values in a predicate.
func (t sqlText) runQuery(q RunQuery) (string, []any) {
	var (
		where []string
		args  []any
	)
	add := func(clause string, v any) {
		args = append(args, v)
		where = append(where, clause+"$"+strconv.Itoa(len(args)))
	}
	if q.RunID != "" {
		add("run_id = ", string(q.RunID))
	}
	if q.PlanDigest != "" {
		add("plan_digest = ", q.PlanDigest)
	}
	if q.Database != "" {
		add("database_name = ", q.Database)
	}
	if q.Table != nil {
		add("table_schema = ", q.Table.Schema)
		add("table_name = ", q.Table.Name)
	}
	if q.Index != nil {
		where = append(where, "has_index")
		add("index_schema = ", q.Index.Schema)
		add("index_name = ", q.Index.Name)
	}
	if q.Operation != "" {
		add("operation = ", string(q.Operation))
	}
	if len(q.Statuses) > 0 {
		parts := make([]string, 0, len(q.Statuses))
		for _, s := range q.Statuses {
			args = append(args, string(s))
			parts = append(parts, "$"+strconv.Itoa(len(args)))
		}
		where = append(where, "status IN ("+strings.Join(parts, ", ")+")")
	}
	if !q.FinishedSince.IsZero() {
		add("finished_at >= ", q.FinishedSince.UTC())
	}

	sb := strings.Builder{}
	sb.WriteString("SELECT ")
	sb.WriteString(runColumns)
	sb.WriteString(" FROM ")
	sb.WriteString(t.quotedSchema)
	sb.WriteString(".run")
	if len(where) > 0 {
		sb.WriteString(" WHERE ")
		sb.WriteString(strings.Join(where, " AND "))
	}
	sb.WriteString(" ORDER BY started_at, run_id")
	if q.Limit > 0 {
		sb.WriteString(" LIMIT ")
		sb.WriteString(strconv.Itoa(q.Limit))
	}
	return sb.String(), args
}
