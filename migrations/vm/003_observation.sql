CREATE TABLE vm.observation_work (
 tenant_id uuid NOT NULL, vm_id uuid NOT NULL,
 notification_generation bigint NOT NULL DEFAULT 1 CHECK(notification_generation>0),
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 not_before timestamptz NOT NULL DEFAULT now(),
 failures integer NOT NULL DEFAULT 0 CHECK(failures>=0),
 lease_token uuid, lease_until timestamptz,
 PRIMARY KEY(tenant_id,vm_id),
 FOREIGN KEY(tenant_id,vm_id) REFERENCES vm.virtual_machines(tenant_id,id)
);
CREATE INDEX vm_observation_due ON vm.observation_work(next_attempt_at);
CREATE INDEX vm_native_observation_target ON vm.virtual_machines
 ((current_configuration #>> '{Native,Namespace}'),(current_configuration #>> '{Native,Name}'));

-- Only accepted intent or native identity changes wake an observer. Writing
-- its Observed fields must never requeue the same VM in a hot loop.
CREATE FUNCTION vm.schedule_observation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF TG_OP='INSERT' THEN
  INSERT INTO vm.observation_work(tenant_id,vm_id) VALUES(NEW.tenant_id,NEW.id);
 ELSIF NEW.version IS DISTINCT FROM OLD.version
    OR NEW.closing IS DISTINCT FROM OLD.closing
    OR NEW.current_configuration #> '{Native}' IS DISTINCT FROM OLD.current_configuration #> '{Native}' THEN
  INSERT INTO vm.observation_work(tenant_id,vm_id) VALUES(NEW.tenant_id,NEW.id)
  ON CONFLICT(tenant_id,vm_id) DO UPDATE SET
   notification_generation=vm.observation_work.notification_generation+1,
   next_attempt_at=GREATEST(vm.observation_work.not_before,LEAST(vm.observation_work.next_attempt_at,now()));
 END IF;
 RETURN NEW;
END;
$$;
CREATE TRIGGER vm_observation_insert AFTER INSERT ON vm.virtual_machines
 FOR EACH ROW EXECUTE FUNCTION vm.schedule_observation();
CREATE TRIGGER vm_observation_intent AFTER UPDATE OF version,closing,current_configuration ON vm.virtual_machines
 FOR EACH ROW EXECUTE FUNCTION vm.schedule_observation();

-- Existing VMs are observed after the explicit migration, without a new user
-- operation or a rewrite of their frozen configuration.
INSERT INTO vm.observation_work(tenant_id,vm_id)
 SELECT tenant_id,id FROM vm.virtual_machines;
