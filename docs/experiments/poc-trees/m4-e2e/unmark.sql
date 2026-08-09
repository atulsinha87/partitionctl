-- Strips the partitionctl ownership marker from a built tree, so the drop can be shown to
-- refuse without it. This is the negative control for the create change's COMMENT ON INDEX.
DO $$
DECLARE r record;
BEGIN
  FOR r IN SELECT n.nspname, c.relname FROM pg_class c
             JOIN pg_namespace n ON n.oid = c.relnamespace
            WHERE c.relname LIKE 'idx_orders_created%' LOOP
    EXECUTE format('COMMENT ON INDEX %I.%I IS NULL', r.nspname, r.relname);
  END LOOP;
END $$;
SELECT c.relname, coalesce(obj_description(c.oid,'pg_class'),'(none)') AS comment
  FROM pg_class c WHERE c.relname LIKE 'idx_orders_created%' ORDER BY 1 LIMIT 4;
