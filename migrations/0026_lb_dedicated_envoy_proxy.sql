-- New LB incarnations own a dedicated EnvoyProxy. Existing installations and
-- Gateway classes are never rewritten or adopted by this schema migration.
DO $$ DECLARE guard text; BEGIN
 FOR guard IN SELECT conname FROM pg_constraint
  WHERE conrelid='network_lb_components'::regclass AND contype='c'
   AND pg_get_constraintdef(oid) LIKE '%kind%'
 LOOP EXECUTE format('ALTER TABLE network_lb_components DROP CONSTRAINT %I',guard); END LOOP;
END $$;
ALTER TABLE network_lb_components
 ADD CHECK (kind IN ('envoy_proxy','route','tcp_route','udp_route','policy','client_policy','patch_policy','backend')),
 ADD CHECK ((kind='backend')=(member_id IS NOT NULL)),
 ADD CHECK ((kind IN ('route','tcp_route','udp_route','policy','client_policy','patch_policy'))=(listener_id IS NOT NULL));
CREATE UNIQUE INDEX network_lb_one_active_envoy_proxy
 ON network_lb_components(tenant_id,lb_id) WHERE kind='envoy_proxy' AND deleted_at IS NULL;
