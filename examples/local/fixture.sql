-- Local development fixture for PartitionCTL.
--
-- A single-level RANGE-partitioned table with twelve monthly leaves and enough
-- rows that a concurrent index build takes long enough to interrupt on purpose.
--
-- Deliberately *not* included, because the planner rejects each of them at plan
-- time and that rejection is the behaviour worth keeping (TRD FR-PLAN-2,
-- FR-PLAN-3): a DEFAULT partition, a HASH strategy, or a second level of
-- partitioning.

DROP TABLE IF EXISTS public.orders CASCADE;

CREATE TABLE public.orders (
    id         bigserial,
    created_at timestamptz NOT NULL,
    status     text        NOT NULL,
    amount     numeric(12,2)
) PARTITION BY RANGE (created_at);

DO $$
DECLARE
    m date;
BEGIN
    FOR m IN
        SELECT generate_series('2026-01-01'::date, '2026-12-01'::date, '1 month')::date
    LOOP
        EXECUTE format(
            'CREATE TABLE public.orders_%s PARTITION OF public.orders
                 FOR VALUES FROM (%L) TO (%L)',
            to_char(m, 'YYYY_MM'), m, m + interval '1 month');
    END LOOP;
END $$;

-- Roughly one million rows spread evenly across the twelve leaves. Drop the
-- step to '5 seconds' if you want a build slow enough to Ctrl-C comfortably.
INSERT INTO public.orders (created_at, status, amount)
SELECT ts,
       CASE WHEN random() < 0.08 THEN 'deleted' ELSE 'open' END,
       round((random() * 500)::numeric, 2)
FROM generate_series('2026-01-01'::timestamptz,
                     '2026-12-31 23:59:59'::timestamptz,
                     '30 seconds'::interval) AS ts;

ANALYZE public.orders;

SELECT c.relname AS partition,
       to_char(c.reltuples, 'FM999,999,999') AS est_rows
FROM pg_catalog.pg_inherits i
JOIN pg_catalog.pg_class c ON c.oid = i.inhrelid
WHERE i.inhparent = 'public.orders'::regclass
ORDER BY c.relname;
