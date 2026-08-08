#!/usr/bin/env bash
#
# End-to-end PartitionCTL demo against a throwaway PostgreSQL container.
# Every command is printed before it runs.
#
#   ./examples/local/demo.sh          run it
#   ./examples/local/demo.sh -p       pause between steps, press enter to advance
#
# Requires docker, and a driver registered in cmd/partitionctl/main.go
# (`make driver` if `make driver-status` says none).
#
# This script asserts. Every step declares the exit code it expects and the
# script fails on the first mismatch, so a zero exit from `make demo-all` is
# evidence rather than decoration. It previously used `set -uo pipefail` with no
# status checks anywhere, which meant it could not fail: two steps that claimed
# to demonstrate the tamper guard (exit 10) and the drift guard (exit 11) were
# both printing exit 0 and nobody noticed.

set -uo pipefail
cd "$(dirname "$0")/../.."

PAUSE=0
[[ "${1:-}" == "-p" ]] && PAUSE=1

B=$'\033[1m'; C=$'\033[36m'; G=$'\033[32m'; R=$'\033[31m'; Y=$'\033[33m'; Z=$'\033[0m'

FAILURES=0

banner() { printf '\n%s══ %s ══%s\n' "$B" "$1" "$Z"; [[ $PAUSE == 1 ]] && read -rsp "   press enter" && echo; }

# run: expects success. A non-zero status fails the demo.
run() {
  printf '\n%s$ %s%s\n' "$C" "$*" "$Z"
  "$@"
  local rc=$?
  if (( rc != 0 )); then
    printf '%sFAIL: expected exit 0, got %d%s\n' "$R" "$rc" "$Z"
    FAILURES=$((FAILURES + 1))
  fi
  return 0
}

# runx <expected> <cmd...>: expects a specific non-zero exit code.
# The guards below are the whole safety story, so "it refused" is not enough —
# it has to refuse for the stated reason, which the exit code encodes.
runx() {
  local want=$1; shift
  printf '\n%s$ %s%s\n' "$C" "$*" "$Z"
  "$@"
  local rc=$?
  if (( rc == want )); then
    printf '%sexit %d (expected)%s\n' "$G" "$rc" "$Z"
  else
    printf '%sFAIL: expected exit %d, got %d%s\n' "$R" "$want" "$rc" "$Z"
    FAILURES=$((FAILURES + 1))
  fi
  return 0
}

psql_do() { docker exec -i partitionctl-pg psql -q -v ON_ERROR_STOP=1 -U postgres -d postgres "$@"; }

export PARTITIONCTL_DSN='postgres://postgres:pw@localhost:5432/postgres?sslmode=disable'
PCTL=build/partitionctl
CONN=(-driver postgres -state file -state-dir build/state -actor "${USER:-demo}")
PLAN=build/migration.plan
SPEC=examples/local/orders-idx.json

banner "0. build, and confirm a driver is registered"
run make build
run make driver-status

banner "1. throwaway PostgreSQL, 12 partitions, ~1M rows"
run make db-reset
rm -rf build/state

banner "2. plan — reads the catalog, writes an artifact, issues no DDL"
run $PCTL plan -spec "$SPEC" -o "$PLAN" -force "${CONN[@]}"

banner "3. render — the runbook, generated offline from the artifact"
run $PCTL render "$PLAN"

banner "4. execute --dry-run — live pre-flight, still no DDL"
run $PCTL execute -dry-run "${CONN[@]}" "$PLAN"

banner "5. execute — the real thing"
run $PCTL execute "${CONN[@]}" "$PLAN"

banner "6. verify --end-state — assert the end state from the catalog"
run $PCTL verify -end-state "${CONN[@]}" "$PLAN"

banner "7. what PostgreSQL actually has now"
run docker exec -i partitionctl-pg psql -U postgres -d postgres -c \
  "SELECT c.relname, c.relkind, i.indisvalid AS valid, (h.inhparent IS NOT NULL) AS attached
     FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid
     LEFT JOIN pg_inherits h ON h.inhrelid = c.oid
    WHERE c.relname LIKE 'orders_created_at_idx%' ORDER BY c.relkind DESC, c.relname;"

banner "8. idempotence — running a finished plan again is a no-op (AC-7)"
run $PCTL execute "${CONN[@]}" "$PLAN"

# ---------------------------------------------------------------------------
# The two guards. Both need an artifact with real work left in it.
#
# They used to run against build/migration.plan, which by this point is
# COMPLETE. A completed plan short-circuits on AC-7 ("already complete") and
# returns 0 before either the digest check or the drift check is reached, so
# both steps printed exit 0 and were read as passes. Re-planning the same spec
# does not help either: the index now exists, so the new plan has zero build
# nodes and nothing to tamper with.
#
# So the guards use a second index that does not exist yet. The plan below is
# never executed — it exists to be corrupted and to go stale, which is the only
# thing under test here.
# ---------------------------------------------------------------------------

cat > build/guard-idx.json <<'JSON'
{
  "operation": "create-index",
  "table": "public.orders",
  "index": "orders_guard_idx",
  "definition": { "method": "btree", "columns": [{ "name": "status" }] },
  "pace_seconds": 2,
  "pace_reason": "demonstration only; this plan is never executed"
}
JSON

banner "9. tamper with the approved artifact — must refuse, exit 10 (AC-2)"
run $PCTL plan -spec build/guard-idx.json -o build/fresh.plan -force "${CONN[@]}"
cp build/fresh.plan build/tampered.plan
# The pause is `"seconds"` in the artifact. `pace_seconds` is a field of the
# SPECIFICATION and appears nowhere in a plan, so the old pattern matched
# nothing and left the file byte-identical to the original.
sed -i '' 's/"seconds": 2/"seconds": 9/' build/tampered.plan 2>/dev/null \
  || sed -i 's/"seconds": 2/"seconds": 9/' build/tampered.plan
if cmp -s build/fresh.plan build/tampered.plan; then
  printf '%sFAIL: the tamper edited nothing; this step would prove nothing%s\n' "$R" "$Z"
  FAILURES=$((FAILURES + 1))
else
  printf '%stampered %s pause node(s): "seconds": 2 -> 9%s\n' "$Y" \
    "$(grep -c '"seconds": 9' build/tampered.plan)" "$Z"
fi
runx 10 $PCTL execute "${CONN[@]}" build/tampered.plan

banner "10. add a partition after planning — drift, exit 11 (AC-3)"
run psql_do -c "CREATE TABLE public.orders_2027_01 PARTITION OF public.orders
                  FOR VALUES FROM ('2027-01-01') TO ('2027-02-01');"
runx 11 $PCTL execute "${CONN[@]}" build/fresh.plan

banner "11. re-plan absorbs the new partition (FR-PLAN-5)"
run $PCTL plan -spec "$SPEC" -o build/drift.plan -force "${CONN[@]}"
# KNOWN DEFECT — this artifact is deliberately NOT executed here.
#
# PostgreSQL creates AND attaches a child index automatically when a partition
# is added to a table that already carries a partitioned index, using its own
# name (orders_2027_01_created_at_idx). InspectChildren keys its lookup only on
# the name PartitionCTL would generate, so it cannot see that index and reports
# the leaf as needing a build. Executing this plan does a full redundant CREATE
# INDEX CONCURRENTLY and then dies on the ATTACH with SQLSTATE 55000 — only one
# child index may be attached per (partition, partitioned index) — leaving a
# stray duplicate behind that only a human can remove. Reproduced on
# PostgreSQL 17.10; pre-existing since M1, not an M2/M3 regression.
printf '%snote: this plan is not executed — see the comment above; re-planning is correct,\n' "$Y"
printf '      executing it hits SQLSTATE 55000 on the ATTACH (known, pre-existing).%s\n' "$Z"

banner "12. reject an unsupported topology — DEFAULT partition, exit 15 (AC-11)"
run psql_do -c "CREATE TABLE public.orders_default PARTITION OF public.orders DEFAULT;"
runx 15 $PCTL plan -spec "$SPEC" -o build/bad.plan -force "${CONN[@]}"
run psql_do -c "DROP TABLE public.orders_default;"

# Return to the clean 12-partition tree the remaining steps describe.
run psql_do -c "DROP TABLE public.orders_2027_01;"

# ---------------------------------------------------------------------------
# The other two operations, end to end. Previously this was one step that ran
# both and showed them failing as unbuilt.
# ---------------------------------------------------------------------------

banner "13. reindex-index — rebuild every leaf online, one partition at a time"
cat > build/reindex.json <<'JSON'
{
  "operation": "reindex-index",
  "table": "public.orders",
  "index": "orders_created_at_idx",
  "pace_seconds": 1,
  "pace_reason": "let autovacuum catch up between partitions"
}
JSON
run $PCTL plan -spec build/reindex.json -o build/reindex.plan -force "${CONN[@]}"
run $PCTL execute "${CONN[@]}" build/reindex.plan
run $PCTL verify -end-state "${CONN[@]}" build/reindex.plan

banner "14. an unwind is refused where none exists"
# One create-shaped body used to serve every operation here, so this printed
# `DROP INDEX "public"."orders_created_at_idx";` as the "rollback" of a reindex:
# a statement destroying a production index the run never created.
runx 1 $PCTL render -rollback -confirm-exclusive-lock build/reindex.plan

banner "15. drop-index — the one operation that is not online (FR-DROP-5)"
cat > build/drop.json <<'JSON'
{
  "operation": "drop-index",
  "table": "public.orders",
  "index": "orders_created_at_idx",
  "confirm_exclusive_lock": true,
  "note": "demo teardown"
}
JSON
run $PCTL plan -spec build/drop.json -o build/drop.plan -force "${CONN[@]}"
run $PCTL execute "${CONN[@]}" build/drop.plan
# The drop's end state is absence. `verify --end-state` used to run
# create-index's gate for every operation and so reported FAIL here, on a drop
# that had worked perfectly.
run $PCTL verify -end-state "${CONN[@]}" build/drop.plan

banner "16. the index and all 12 children are gone"
run docker exec -i partitionctl-pg psql -U postgres -d postgres -c \
  "SELECT count(*) AS remaining FROM pg_class
    WHERE relname LIKE 'orders_created_at_idx%';"

if (( FAILURES == 0 )); then
  printf '\n%sdone — every step exited as expected.%s  teardown: make db-down && make demo-clean\n' "$G" "$Z"
  exit 0
fi
printf '\n%s%d step(s) did not behave as expected.%s\n' "$R" "$FAILURES" "$Z"
exit 1
