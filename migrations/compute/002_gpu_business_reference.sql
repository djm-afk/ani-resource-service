-- The offline manifest conversion runs first. Never infer new business fields
-- from resource names or silently accept legacy GPU references at runtime.
ALTER TABLE compute.presets DROP CONSTRAINT presets_check;
ALTER TABLE compute.presets ADD CONSTRAINT presets_check CHECK (
 (compute_type='cpu' AND gpu IS NULL) OR
 (compute_type='gpu' AND jsonb_typeof(gpu)='object'
  AND jsonb_strip_nulls(gpu) ?& ARRAY[
   'profile_id','profile_version','spec_digest','devices_per_instance',
   'cluster_id','pool_id','mode','model_key','memory_mib_per_device',
   'core_limit_percent','allocation_type','resource_key','compute_units_per_device','adapter_id']
  AND gpu - ARRAY[
   'profile_id','profile_version','spec_digest','devices_per_instance',
   'cluster_id','pool_id','mode','model_key','memory_mib_per_device',
   'core_limit_percent','allocation_type','resource_key','compute_units_per_device','adapter_id']='{}'::jsonb
  AND (gpu->>'profile_version')::numeric BETWEEN 1 AND 18446744073709551615
  AND jsonb_typeof(gpu->'profile_version')='number'
  AND gpu->>'profile_version' ~ '^[1-9][0-9]*$'
  AND (gpu->>'profile_id')::uuid IS NOT NULL
  AND (gpu->>'cluster_id')::uuid IS NOT NULL
  AND (gpu->>'pool_id')::uuid IS NOT NULL
  AND gpu->>'spec_digest' ~ '^[0-9a-f]{64}$'
  AND length(btrim(gpu->>'model_key'))>0
  AND length(btrim(gpu->>'resource_key'))>0
  AND jsonb_typeof(gpu->'devices_per_instance')='number'
  AND gpu->>'devices_per_instance' ~ '^[1-9][0-9]*$'
  AND (gpu->>'devices_per_instance')::numeric BETWEEN 1 AND 4294967295
  AND (gpu->>'memory_mib_per_device')::bigint>=0
  AND jsonb_typeof(gpu->'memory_mib_per_device')='number'
  AND gpu->>'memory_mib_per_device' ~ '^(0|[1-9][0-9]*)$'
  AND jsonb_typeof(gpu->'core_limit_percent')='number'
  AND gpu->>'core_limit_percent' ~ '^(0|[1-9][0-9]*)$'
  AND (gpu->>'core_limit_percent')::numeric BETWEEN 0 AND 100
  AND jsonb_typeof(gpu->'compute_units_per_device')='number'
  AND gpu->>'compute_units_per_device' ~ '^(0|[1-9][0-9]*)$'
  AND (gpu->>'compute_units_per_device')::numeric BETWEEN 0 AND 4294967295
  AND (
   (gpu->>'adapter_id'='volcano-hami-mib-v1'
    AND gpu->>'resource_key'='volcano.sh/vgpu-number'
    AND (gpu->>'compute_units_per_device')::numeric=0
    AND (gpu->>'memory_mib_per_device')::bigint>0
    AND (gpu->>'memory_mib_per_device')::bigint<=2147483647
    AND (gpu->>'devices_per_instance')::numeric<=2147483647
    AND ((gpu->>'allocation_type'='PHYSICAL' AND gpu->>'mode'='WHOLE_EXCLUSIVE' AND (gpu->>'core_limit_percent')::integer=100)
      OR (gpu->>'allocation_type'='DYNAMIC_SHARED' AND gpu->>'mode'='SHARED_FIXED' AND gpu->>'devices_per_instance'='1' AND (gpu->>'core_limit_percent')::integer BETWEEN 1 AND 99)))
   OR
   (gpu->>'adapter_id'='hygon-hcu-v1'
    AND gpu->>'resource_key' NOT LIKE 'volcano.sh/vgpu-%'
    AND (gpu->>'core_limit_percent')::integer=0
    AND ((gpu->>'allocation_type'='PHYSICAL' AND gpu->>'mode'='WHOLE_EXCLUSIVE' AND (gpu->>'compute_units_per_device')::numeric=0)
      OR (gpu->>'allocation_type'='FIXED_PARTITION' AND gpu->>'mode'='SHARED_FIXED' AND gpu->>'devices_per_instance'='1' AND (gpu->>'memory_mib_per_device')::bigint>0 AND (gpu->>'compute_units_per_device')::numeric>0)))
  )
 ));
