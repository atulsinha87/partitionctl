-- Every verdict in the end-to-end report is read from here, never from Liquibase's log.
\pset pager off

\echo '--- tree summary ---'
WITH RECURSIVE t AS (
    SELECT c.oid, c.relkind FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'public' AND c.relname = 'orders'
    UNION ALL
    SELECT c2.oid, c2.relkind FROM t JOIN pg_inherits i ON i.inhparent = t.oid
      JOIN pg_class c2 ON c2.oid = i.inhrelid),
p AS (SELECT c.oid, i.indisvalid FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid
       WHERE c.relname = 'idx_orders_created' AND c.relkind = 'I')
SELECT (SELECT count(*) FROM t WHERE relkind = 'r')                       AS leaves,
       (SELECT count(*) FROM pg_inherits WHERE inhparent = (SELECT oid FROM p)) AS attached_children,
       (SELECT count(*) FROM pg_inherits ii JOIN pg_index ix ON ix.indexrelid = ii.inhrelid
         WHERE ii.inhparent = (SELECT oid FROM p) AND ix.indisvalid)      AS attached_and_valid,
       (SELECT indisvalid FROM p)                                         AS parent_valid,
       (SELECT count(*) FROM pg_class WHERE relname ~ '_cc(new|old)[0-9]*$') AS reindex_leftovers;

\echo ''
\echo '--- every child index of idx_orders_created: validity, attachment, shape, marker, relfilenode ---'
SELECT ic.relname,
       ix.indisvalid                                    AS valid,
       (ii.inhparent IS NOT NULL)                       AS attached,
       ix.indisunique                                   AS is_unique,
       am.amname                                        AS method,
       pg_get_expr(ix.indpred, ix.indrelid)             AS predicate,
       ic.relfilenode,
       CASE WHEN obj_description(ic.oid, 'pg_class') LIKE 'partitionctl owner=liquibase%'
            THEN 'MARKED' ELSE coalesce(obj_description(ic.oid, 'pg_class'), '(none)') END AS marker
  FROM pg_class lc
  JOIN pg_inherits li ON li.inhrelid = lc.oid
  JOIN pg_class lp ON lp.oid = li.inhparent AND lp.relname = 'orders'
  JOIN pg_index ix ON ix.indrelid = lc.oid
  JOIN pg_class ic ON ic.oid = ix.indexrelid
  LEFT JOIN pg_inherits ii ON ii.inhrelid = ic.oid
  LEFT JOIN pg_am am ON am.oid = ic.relam
 ORDER BY ic.relname;

\echo ''
\echo '--- the parent index itself ---'
SELECT c.relname, i.indisvalid, i.indisunique, am.amname,
       pg_get_expr(i.indpred, i.indrelid) AS predicate,
       pg_get_indexdef(c.oid) AS definition
  FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid
  LEFT JOIN pg_am am ON am.oid = c.relam
 WHERE c.relname = 'idx_orders_created';

\echo ''
\echo '--- changelog rows ---'
SELECT id, author, exectype FROM databasechangelog ORDER BY orderexecuted;

\echo ''
\echo '--- did the gated changeset body run? (rows > 0 = the gate passed) ---'
SELECT count(*) AS gate_body_rows FROM e2e_gate_ran;
