-- 007: one-shot data cleanup for the retired initialization_verified field.
-- Releases before 2026-11-09 froze "initialization_verified" into stored
-- canonical spec/configuration/intent/record bytes. The field no longer exists
-- on MediaReference and the strict decoder rejects it, so this migration strips
-- exactly that key from every object level once. No runtime compatibility path
-- remains: after this runs, a record still carrying the key is corrupt data.
-- Applied only by the explicit Migrate() entry point; nothing here rewrites 001-006.

CREATE OR REPLACE FUNCTION vm.jsonb_strip_retired(v jsonb) RETURNS jsonb
LANGUAGE sql IMMUTABLE AS $$
SELECT CASE jsonb_typeof(v)
  WHEN 'object' THEN (
    SELECT coalesce(jsonb_object_agg(key, vm.jsonb_strip_retired(value)), '{}'::jsonb)
    FROM jsonb_each(v)
    WHERE key <> 'initialization_verified'
  )
  WHEN 'array' THEN (
    SELECT coalesce(jsonb_agg(vm.jsonb_strip_retired(elem)), '[]'::jsonb)
    FROM jsonb_array_elements(v) AS elem
  )
  ELSE v
END
$$;

UPDATE vm.virtual_machines
   SET initial_spec = vm.jsonb_strip_retired(initial_spec)
 WHERE initial_spec::text LIKE '%initialization_verified%';

UPDATE vm.virtual_machines
   SET current_configuration = vm.jsonb_strip_retired(current_configuration)
 WHERE current_configuration::text LIKE '%initialization_verified%';

UPDATE vm.operations
   SET intent = vm.jsonb_strip_retired(intent)
 WHERE intent::text LIKE '%initialization_verified%';

UPDATE vm.operations
   SET record = vm.jsonb_strip_retired(record)
 WHERE record::text LIKE '%initialization_verified%';

DROP FUNCTION vm.jsonb_strip_retired(jsonb);
