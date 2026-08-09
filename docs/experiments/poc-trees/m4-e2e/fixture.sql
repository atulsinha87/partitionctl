-- The end-to-end fixture: 12 monthly RANGE partitions of a real-shaped orders table,
-- 1,200,000 rows, 100,000 per partition. Big enough that CREATE INDEX CONCURRENTLY is
-- genuine work rather than a catalog no-op, small enough to iterate on.
\set ON_ERROR_STOP 1

DROP TABLE IF EXISTS public.orders CASCADE;
DROP TABLE IF EXISTS public.databasechangelog CASCADE;
DROP TABLE IF EXISTS public.databasechangeloglock CASCADE;
DROP TABLE IF EXISTS public.e2e_gate_ran CASCADE;

CREATE TABLE public.orders (
    id           bigserial,
    created_at   date        NOT NULL,
    customer_id  bigint      NOT NULL,
    region       text        NOT NULL,
    status       text        NOT NULL,
    amount       numeric(12,2) NOT NULL
) PARTITION BY RANGE (created_at);

DO $$
DECLARE m int;
BEGIN
  FOR m IN 1..12 LOOP
    EXECUTE format(
      'CREATE TABLE public.orders_2024_%1$s PARTITION OF public.orders '
      || 'FOR VALUES FROM (%2$L) TO (%3$L)',
      lpad(m::text, 2, '0'),
      make_date(2024, m, 1),
      CASE WHEN m = 12 THEN make_date(2025, 1, 1) ELSE make_date(2024, m + 1, 1) END);
  END LOOP;
END $$;

INSERT INTO public.orders (created_at, customer_id, region, status, amount)
SELECT make_date(2024, m, 1 + (g % 27)),
       (g % 5000)::bigint,
       (ARRAY['emea','apac','amer','latam'])[1 + (g % 4)],
       (ARRAY['new','paid','shipped','archived'])[1 + (g % 4)],
       ((g % 99999) / 100.0)::numeric(12,2)
  FROM generate_series(1, 12) m,
       generate_series(1, 100000) g;

ANALYZE public.orders;

SELECT count(*) AS rows_loaded FROM public.orders;
SELECT count(*) AS leaf_partitions
  FROM pg_class c JOIN pg_inherits i ON i.inhrelid = c.oid
  JOIN pg_class p ON p.oid = i.inhparent
 WHERE p.relname = 'orders' AND c.relkind = 'r';
