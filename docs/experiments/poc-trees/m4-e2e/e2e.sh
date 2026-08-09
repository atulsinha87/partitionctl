#!/bin/bash
#
# The end-to-end walkthrough: create, gate, reindex, drop, from ONE changelog, with the
# catalog read between every stage. Every verdict below comes from pg_catalog, never from
# Liquibase's log -- a green log line over a broken tree is exactly the failure this
# project has shipped before.
#
# Driven by `make lb-e2e`. Standalone use:
#   docker run -d --name partitionctl-lb -e POSTGRES_PASSWORD=pw -p 5559:5432 postgres:17
#   ./e2e.sh
#
set -uo pipefail
BASE="$(cd "$(dirname "$0")" && pwd)"
CT="${LB_PG_CONTAINER:-partitionctl-lb}"
PSQL=(docker exec -i "$CT" psql -q -U postgres -d postgres)

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
run() { "$BASE/run.sh" e2e.xml update "$@" 2>&1 \
        | perl -MTime::HiRes=time -ne 'BEGIN{$|=1;$s=time()} printf("%7.2f  %s", time()-$s, $_)' \
        | grep -E 'partitionctl|Running Changeset|BUILD|ERROR'; }

say "fixture: 12 RANGE partitions, 1,200,000 rows"
"${PSQL[@]}" -v ON_ERROR_STOP=1 < "$BASE/fixture.sql" | tail -6

say "STAGE 1  createPartitionedTableIndex  (one progress line per partition)"
run -Dliquibase.toTag=created

say "STAGE 1 verdict, read from pg_catalog"
"${PSQL[@]}" -c "
WITH RECURSIVE t AS (
    SELECT c.oid, c.relkind FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
     WHERE n.nspname='public' AND c.relname='orders'
    UNION ALL SELECT c2.oid, c2.relkind FROM t JOIN pg_inherits i ON i.inhparent=t.oid
      JOIN pg_class c2 ON c2.oid=i.inhrelid),
p AS (SELECT c.oid, i.indisvalid FROM pg_class c JOIN pg_index i ON i.indexrelid=c.oid
       WHERE c.relname='idx_orders_created' AND c.relkind='I')
SELECT (SELECT count(*) FROM t WHERE relkind='r') AS leaves,
       (SELECT count(*) FROM pg_inherits WHERE inhparent=(SELECT oid FROM p)) AS attached,
       (SELECT count(*) FROM pg_inherits ii JOIN pg_index ix ON ix.indexrelid=ii.inhrelid
         WHERE ii.inhparent=(SELECT oid FROM p) AND ix.indisvalid) AS attached_and_valid,
       (SELECT indisvalid FROM p) AS parent_valid,
       (SELECT count(*) FROM pg_class c2
         WHERE c2.relname LIKE 'idx_orders_created_%'
           AND obj_description(c2.oid,'pg_class') LIKE 'partitionctl owner=liquibase%')
         AS children_carrying_the_ownership_marker;"

# The marker is what makes STAGE 4 possible at all, so snapshot the relfilenodes now and
# check both after the reindex.
"${PSQL[@]}" -c "DROP TABLE IF EXISTS before_reindex;
CREATE TABLE before_reindex AS
  SELECT ic.relname, ic.relfilenode FROM pg_class lc
    JOIN pg_inherits li ON li.inhrelid=lc.oid
    JOIN pg_class lp ON lp.oid=li.inhparent AND lp.relname='orders'
    JOIN pg_index ix ON ix.indrelid=lc.oid
    JOIN pg_class ic ON ic.oid=ix.indexrelid;" >/dev/null

say "STAGE 2  partitionctlIndexGate guarding a changeset that depends on the index"
run -Dliquibase.toTag=gated

say "STAGE 2 verdict: did the gated body actually execute?"
"${PSQL[@]}" -c "SELECT count(*) AS gate_body_rows, min(note) AS note FROM e2e_gate_ran;"

say "STAGE 3  reindexPartitionedTableIndex"
run -Dliquibase.toTag=reindexed

say "STAGE 3 verdict: every leaf rebuilt (relfilenode moved) and still ours (marker survived)"
"${PSQL[@]}" -c "
SELECT count(*) AS leaves,
       count(*) FILTER (WHERE b.relfilenode <> ic.relfilenode) AS rebuilt,
       count(*) FILTER (WHERE obj_description(ic.oid,'pg_class')
                              LIKE 'partitionctl owner=liquibase%') AS still_marked
  FROM before_reindex b JOIN pg_class ic ON ic.relname=b.relname;"

say "STAGE 4  dropPartitionedTableIndex  (marker + confirmExclusiveLock + reindex gate)"
run

say "STAGE 4 verdict: nothing left on any partition, and the data is untouched"
"${PSQL[@]}" -c "
SELECT (SELECT count(*) FROM pg_class WHERE relname LIKE 'idx_orders_created%') AS indexes_named_like_it,
       (SELECT count(*) FROM pg_class lc
          JOIN pg_inherits li ON li.inhrelid=lc.oid
          JOIN pg_class lp ON lp.oid=li.inhparent AND lp.relname='orders'
          JOIN pg_index ix ON ix.indrelid=lc.oid) AS indexes_on_any_partition,
       (SELECT count(*) FROM public.orders) AS rows_still_there;"
"${PSQL[@]}" -c "SELECT id, author, exectype FROM databasechangelog ORDER BY orderexecuted;"

printf '\n\033[1m== done\033[0m\n'
