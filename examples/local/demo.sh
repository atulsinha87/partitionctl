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

set -uo pipefail
cd "$(dirname "$0")/../.."

PAUSE=0
[[ "${1:-}" == "-p" ]] && PAUSE=1

B=$'\033[1m'; C=$'\033[36m'; G=$'\033[32m'; R=$'\033[31m'; Z=$'\033[0m'

banner() { printf '\n%s══ %s ══%s\n' "$B" "$1" "$Z"; [[ $PAUSE == 1 ]] && read -rsp "   press enter" && echo; }
run()    { printf '\n%s$ %s%s\n' "$C" "$*" "$Z"; "$@"; }
# run a command we expect to fail, and show the exit code
runx()   { printf '\n%s$ %s%s\n' "$C" "$*" "$Z"; "$@"; local rc=$?; printf '%sexit %d%s\n' "$R" "$rc" "$Z"; return 0; }

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

banner "2. plan — reads the catalog, writes an artifact, issues no DDL"
run $PCTL plan -spec "$SPEC" -o "$PLAN" -force "${CONN[@]}"

banner "3. render — the runbook, generated offline from the artifact"
run $PCTL render "$PLAN"

banner "4. execute --dry-run — live pre-flight, still no DDL"
run $PCTL execute -dry-run "${CONN[@]}" "$PLAN"

banner "5. execute — the real thing"
run $PCTL execute "${CONN[@]}" "$PLAN"

banner "6. verify — assert the end state from the catalog"
run $PCTL verify -end-state "${CONN[@]}" "$PLAN"

banner "7. what PostgreSQL actually has now"
run docker exec -i partitionctl-pg psql -U postgres -d postgres -c \
  "SELECT c.relname, c.relkind, i.indisvalid AS valid, (h.inhparent IS NOT NULL) AS attached
     FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid
     LEFT JOIN pg_inherits h ON h.inhrelid = c.oid
    WHERE c.relname LIKE 'orders_created_at_idx%' ORDER BY c.relkind DESC, c.relname;"

banner "8. idempotence — running a finished plan again is a no-op (AC-7)"
run $PCTL execute "${CONN[@]}" "$PLAN"

banner "9. tamper with the approved artifact — must refuse, exit 10 (AC-2)"
cp "$PLAN" build/tampered.plan
sed -i '' 's/"pace_seconds": *2/"pace_seconds": 9/' build/tampered.plan 2>/dev/null \
  || sed -i 's/"pace_seconds": *2/"pace_seconds": 9/' build/tampered.plan
runx $PCTL execute "${CONN[@]}" build/tampered.plan

banner "10. add a partition after planning — drift, exit 11 (AC-3)"
run docker exec -i partitionctl-pg psql -q -U postgres -d postgres -c \
  "CREATE TABLE public.orders_2027_01 PARTITION OF public.orders
     FOR VALUES FROM ('2027-01-01') TO ('2027-02-01');"
runx $PCTL execute "${CONN[@]}" "$PLAN"

banner "11. re-plan absorbs the new partition — 1 build remains, 12 already done (FR-PLAN-5)"
run $PCTL plan -spec "$SPEC" -o build/drift.plan -force "${CONN[@]}"

banner "12. reject an unsupported topology — DEFAULT partition, exit 15 (AC-11)"
run docker exec -i partitionctl-pg psql -q -U postgres -d postgres -c \
  "CREATE TABLE public.orders_default PARTITION OF public.orders DEFAULT;"
runx $PCTL plan -spec "$SPEC" -o build/bad.plan -force "${CONN[@]}"
run docker exec -i partitionctl-pg psql -q -U postgres -d postgres -c \
  "DROP TABLE public.orders_default;"

banner "13. the other two operations — not built yet"
for op in reindex-index drop-index; do
  sed "s/create-index/$op/" "$SPEC" > "build/$op.json"
  runx $PCTL plan -spec "build/$op.json" -o "build/$op.plan" -force "${CONN[@]}"
done

printf '\n%sdone.%s  teardown: make db-down && make demo-clean\n' "$G" "$Z"
