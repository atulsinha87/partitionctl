#!/bin/bash
# Both PartitionCTL products, back to back, against live PostgreSQL.
#
#   1. the Go CLI          plan -> dry-run -> execute -> verify, with a plan artifact
#   2. the Liquibase plugin  one changeset, no binary, no plan file
#
# They do the same job and share no code. Run it with:  bash docs/experiments/demo-both.sh
set -u
cd "$(cd "$(dirname "$0")/../.." && pwd)"

B=$'\033[1m'; C=$'\033[36m'; G=$'\033[32m'; Y=$'\033[33m'; R=$'\033[0m'
banner() { printf '\n%s%s\n%s\n%s%s\n' "$B$C" "$(printf '=%.0s' {1..78})" "$*" "$(printf '=%.0s' {1..78})" "$R"; }
step()   { printf '\n%s$ %s%s\n' "$Y" "$*" "$R"; }
pause()  { printf '\n%s-- press return to continue --%s' "$B" "$R"; read -r _; }

export PARTITIONCTL_DSN='postgres://postgres:pw@localhost:5432/postgres?sslmode=disable'
CONN="-driver postgres -state file -state-dir build/state -actor $(whoami)"

# Preflight. An earlier version of this script piped every command through grep, so a missing
# `mvn` produced total silence and the script then announced success anyway. Check up front and
# say plainly what is missing.
missing=0
for tool in docker mvn python3; do
    command -v "$tool" >/dev/null 2>&1 || { printf 'MISSING: %s is not on PATH\n' "$tool"; missing=1; }
done
for container in partitionctl-pg partitionctl-lb; do
    docker ps --format '{{.Names}}' 2>/dev/null | grep -qx "$container" \
        || { printf 'MISSING: container %s is not running (make db-reset / make lb-fixture)\n' "$container"; missing=1; }
done
[ -x ./build/partitionctl ] || { printf 'MISSING: ./build/partitionctl (run: make build)\n'; missing=1; }
if [ "$missing" -ne 0 ]; then
    printf '\nFix the above and re-run.\n'; exit 1
fi

banner " THE PROBLEM"
cat <<'EOF'
CREATE INDEX CONCURRENTLY is rejected outright on a partitioned parent.
Watch PostgreSQL say so:
EOF
step "psql -c 'CREATE INDEX CONCURRENTLY demo ON public.orders (created_at)'"
docker exec partitionctl-pg psql -U postgres -c \
  "CREATE INDEX CONCURRENTLY demo ON public.orders (created_at)" 2>&1 | head -3
cat <<'EOF'

The documented workaround is: build an INVALID parent index, then one
CREATE INDEX CONCURRENTLY per leaf, then ATTACH each one. PostgreSQL validates
the parent on the final attach. For 12 partitions that is 51 steps that must be
ordered, resumed after failure, and verified. Both products below do exactly
that; neither asks you to write it.
EOF
pause

# ---------------------------------------------------------------- PRODUCT ONE
banner " PRODUCT 1 of 2 - the Go CLI     (standalone, no Java, no Liquibase)"
docker exec partitionctl-pg psql -U postgres -q -c \
  "DROP INDEX IF EXISTS public.orders_created_at_idx" >/dev/null 2>&1

step "cat examples/local/orders-idx.json"
cat examples/local/orders-idx.json

step "partitionctl plan -spec examples/local/orders-idx.json -o build/demo.plan"
./build/partitionctl plan -spec examples/local/orders-idx.json \
    -o build/demo.plan -force -driver postgres 2>&1 | grep -vE '^  note:'
pause

step "partitionctl execute --dry-run build/demo.plan       # live pre-flight, issues no DDL"
./build/partitionctl execute -dry-run $CONN build/demo.plan 2>&1 | sed -n '1,12p'
pause

step "partitionctl execute build/demo.plan"
./build/partitionctl execute $CONN build/demo.plan 2>&1 | python3 -c "
import sys, json
for line in sys.stdin:
    line = line.strip()
    if not line.startswith('{'):
        print(line); continue
    r = json.loads(line)
    if r.get('state') == 'DONE' and r.get('kind') in ('index.create_concurrently', 'index.attach'):
        print('  %s  %-34s %s' % (r['ts'][11:19], r['node_id'], r['kind']))
"

step "partitionctl verify --end-state build/demo.plan       # read back from pg_catalog"
./build/partitionctl verify -end-state $CONN build/demo.plan 2>&1 | tail -3
pause

# ---------------------------------------------------------------- PRODUCT TWO
banner " PRODUCT 2 of 2 - the Liquibase plugin     (no binary, no plan file)"
docker exec partitionctl-lb psql -U postgres -q -c \
  "DROP INDEX IF EXISTS public.idx_orders_created" >/dev/null 2>&1
docker exec partitionctl-lb psql -U postgres -q -c \
  "TRUNCATE databasechangelog" >/dev/null 2>&1

cat <<'EOF'
The entire install is one Maven dependency:

    <dependency>
      <groupId>io.github.atulsinha87</groupId>
      <artifactId>liquibase-partitionctl</artifactId>
    </dependency>

and one changeset. runInTransaction="false" is required -- it is what lets
CREATE INDEX CONCURRENTLY run at all, and what makes partial progress survive a
failure. runAlways="true" is the default so partitions added later get indexed.
EOF

step "cat changelogs/demo-create.xml"
sed -n '/<changeSet/,/<\/changeSet>/p' docs/experiments/poc-trees/m4-e2e/changelogs/demo-create.xml

step "mvn liquibase:update -Dchangelog=changelogs/demo-create.xml"
# Keep the whole log, show the interesting lines, and judge on mvn's own exit status rather
# than on grep's. Piping straight into grep is how the previous version of this script turned
# a failure into silence.
LOG=$(mktemp -t partitionctl-demo)
(
  cd docs/experiments/poc-trees/m4-e2e \
    && MAVEN_OPTS=-Duser.timezone=UTC mvn -B liquibase:update \
         -Dchangelog=changelogs/demo-create.xml
) >"$LOG" 2>&1
MVN_STATUS=$?
grep -E "partitionctl|Running Changeset|BUILD" "$LOG" || true
if [ "$MVN_STATUS" -ne 0 ]; then
    printf '\n%sliquibase:update FAILED (exit %s). The reason, verbatim:%s\n' "$B" "$MVN_STATUS" "$R"
    grep -E "ERROR" "$LOG" | head -12
    printf '\nFull log: %s\n' "$LOG"
fi

# ---------------------------------------------------------------- THE VERDICT
banner " THE VERDICT - read from pg_catalog, not from either tool's log"
printf '%sGo CLI  (port 5432)%s\n' "$B" "$R"
docker exec partitionctl-pg psql -U postgres -c "
SELECT count(*) AS leaves,
       count(*) FILTER (WHERE ci.indisvalid)            AS valid,
       count(*) FILTER (WHERE inh.inhparent IS NOT NULL) AS attached
  FROM pg_class leaf
  JOIN pg_inherits li ON li.inhrelid = leaf.oid
  JOIN pg_class p ON p.oid = li.inhparent AND p.relname = 'orders'
  LEFT JOIN pg_class cic ON cic.relname = 'orders_created_at_idx_' || leaf.relname
  LEFT JOIN pg_index ci ON ci.indexrelid = cic.oid
  LEFT JOIN pg_inherits inh ON inh.inhrelid = cic.oid;"

printf '%sLiquibase plugin  (port 5559)%s\n' "$B" "$R"
docker exec partitionctl-lb psql -U postgres -c "
SELECT count(*) AS leaves,
       count(*) FILTER (WHERE ix.indisvalid) AS valid,
       count(*) FILTER (WHERE ii.inhparent IS NOT NULL) AS attached,
       count(*) FILTER (WHERE obj_description(ic.oid,'pg_class') LIKE 'partitionctl owner=%')
         AS ownership_markers
  FROM pg_class leaf
  JOIN pg_inherits li ON li.inhrelid = leaf.oid
  JOIN pg_class p ON p.oid = li.inhparent AND p.relname = 'orders'
  LEFT JOIN pg_class ic ON ic.relname = 'idx_orders_created_' || leaf.relname
  LEFT JOIN pg_index ix ON ix.indexrelid = ic.oid
  LEFT JOIN pg_inherits ii ON ii.inhrelid = ic.oid;"

# Judge it, do not assert it. The previous version printed "same outcome" unconditionally and
# said so under a verdict of 0 valid, 0 attached.
CLI_OK=$(docker exec partitionctl-pg psql -U postgres -tAc "
SELECT count(*) FROM pg_class leaf
  JOIN pg_inherits li ON li.inhrelid = leaf.oid
  JOIN pg_class p ON p.oid = li.inhparent AND p.relname = 'orders'
  JOIN pg_class cic ON cic.relname = 'orders_created_at_idx_' || leaf.relname
  JOIN pg_index ci ON ci.indexrelid = cic.oid AND ci.indisvalid
  JOIN pg_inherits inh ON inh.inhrelid = cic.oid;")
LB_OK=$(docker exec partitionctl-lb psql -U postgres -tAc "
SELECT count(*) FROM pg_class leaf
  JOIN pg_inherits li ON li.inhrelid = leaf.oid
  JOIN pg_class p ON p.oid = li.inhparent AND p.relname = 'orders'
  JOIN pg_class ic ON ic.relname = 'idx_orders_created_' || leaf.relname
  JOIN pg_index ix ON ix.indexrelid = ic.oid AND ix.indisvalid
  JOIN pg_inherits ii ON ii.inhrelid = ic.oid;")

if [ "$CLI_OK" = "12" ] && [ "$LB_OK" = "12" ]; then
    printf '\n%sBoth built 12 of 12 leaves, valid and attached. No shared code, no%s\n' "$G" "$R"
    printf '%sinteroperation -- pick one and stay on it.%s\n\n' "$G" "$R"
else
    printf '\n%sNOT the same outcome: Go CLI %s/12, Liquibase %s/12 valid and attached.%s\n\n' \
        "$Y" "$CLI_OK" "$LB_OK" "$R"
    exit 1
fi
