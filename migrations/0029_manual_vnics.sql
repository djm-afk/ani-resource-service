-- User-owned VNICs retain their identity independently of runtime consumers.
CREATE TABLE network_vnics (
 tenant_id uuid NOT NULL, vnic_id text NOT NULL CHECK(vnic_id ~ '^vnic_[0-9a-f]{32}$'),
 vpc_id text NOT NULL, subnet_id text NOT NULL, name text NOT NULL CHECK(char_length(name) BETWEEN 1 AND 128),
 description text NOT NULL DEFAULT '' CHECK(char_length(description)<=1024),
 state text NOT NULL CHECK(state IN ('provisioning','available','degraded','failed','deleting','deleted')),
 reason text NOT NULL DEFAULT '', version bigint NOT NULL DEFAULT 1 CHECK(version>0),
 mac_address text NOT NULL DEFAULT '', primary_ip text NOT NULL DEFAULT '', ips text[] NOT NULL DEFAULT '{}',
 security_group_ids text[] NOT NULL DEFAULT '{}', observed_at timestamptz,
 created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,last_operation_id uuid NOT NULL,
 PRIMARY KEY(tenant_id,vnic_id), UNIQUE(vnic_id),
 FOREIGN KEY(tenant_id,vpc_id,subnet_id) REFERENCES network_subnets(tenant_id,vpc_id,subnet_id)
);
CREATE INDEX network_vnics_page ON network_vnics(tenant_id,created_at DESC,vnic_id DESC);
CREATE INDEX network_vnics_subnet ON network_vnics(tenant_id,subnet_id) WHERE state<>'deleted';
CREATE TABLE network_vnic_bindings (
 tenant_id uuid NOT NULL,binding_id uuid NOT NULL,vnic_id text NOT NULL,
 consumer_kind text NOT NULL CHECK(consumer_kind IN ('vm','container')),consumer_id text NOT NULL,
 slot text NOT NULL CHECK(slot='primary'),operation_id text NOT NULL,
 requested_revision bigint NOT NULL CHECK(requested_revision>0),applied_revision bigint NOT NULL DEFAULT 0,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),released_at timestamptz,
 PRIMARY KEY(tenant_id,binding_id),
 FOREIGN KEY(tenant_id,vnic_id) REFERENCES network_vnics(tenant_id,vnic_id),
 UNIQUE(tenant_id,consumer_kind,consumer_id,slot,operation_id,vnic_id)
);
CREATE INDEX network_vnic_bindings_active ON network_vnic_bindings(tenant_id,vnic_id) WHERE released_at IS NULL;
ALTER TABLE network_operations ADD COLUMN vnic_id text;
ALTER TABLE network_operations ADD FOREIGN KEY(tenant_id,vnic_id) REFERENCES network_vnics(tenant_id,vnic_id);
ALTER TABLE network_operations DROP CONSTRAINT network_operations_one_target;
ALTER TABLE network_operations ADD CONSTRAINT network_operations_one_target CHECK(num_nonnulls(vpc_id,subnet_id,eip_id,snat_id,lb_id,vnic_id)=1);
ALTER TABLE network_reconciliations ADD COLUMN vnic_id text;
ALTER TABLE network_reconciliations ADD FOREIGN KEY(tenant_id,vnic_id) REFERENCES network_vnics(tenant_id,vnic_id);
ALTER TABLE network_reconciliations DROP CONSTRAINT network_reconciliations_one_target;
ALTER TABLE network_reconciliations ADD CONSTRAINT network_reconciliations_one_target CHECK(num_nonnulls(vpc_id,subnet_id,eip_id,snat_id,lb_id,vnic_id)=1);
ALTER TABLE network_idempotency ADD COLUMN vnic_id text;
ALTER TABLE network_idempotency ADD FOREIGN KEY(tenant_id,vnic_id) REFERENCES network_vnics(tenant_id,vnic_id);
ALTER TABLE network_idempotency DROP CONSTRAINT network_idempotency_one_target;
ALTER TABLE network_idempotency ADD CONSTRAINT network_idempotency_one_target CHECK(num_nonnulls(vpc_id,subnet_id,eip_id,snat_id,lb_id,vnic_id)=1);
ALTER TABLE network_provider_bindings ADD COLUMN vnic_id text;
ALTER TABLE network_provider_bindings ADD FOREIGN KEY(tenant_id,vnic_id) REFERENCES network_vnics(tenant_id,vnic_id);
ALTER TABLE network_provider_bindings DROP CONSTRAINT network_provider_bindings_one_target;
ALTER TABLE network_provider_bindings ADD CONSTRAINT network_provider_bindings_one_target CHECK(num_nonnulls(vpc_id,subnet_id,eip_id,snat_id,lb_id,vnic_id)=1);
ALTER TABLE network_resource_history ADD COLUMN vnic_id text;
ALTER TABLE network_resource_history ADD FOREIGN KEY(tenant_id,vnic_id) REFERENCES network_vnics(tenant_id,vnic_id);
ALTER TABLE network_resource_history DROP CONSTRAINT network_resource_history_one_target;
ALTER TABLE network_resource_history ADD CONSTRAINT network_resource_history_one_target CHECK(num_nonnulls(vpc_id,subnet_id,eip_id,snat_id,lb_id,vnic_id)=1);
ALTER TABLE network_operations ADD UNIQUE(tenant_id,vnic_id,operation_id);
ALTER TABLE network_operations ADD UNIQUE(tenant_id,vnic_id,operation_id,kind);
CREATE UNIQUE INDEX network_operations_vnic_active ON network_operations(tenant_id,vnic_id) WHERE state NOT IN ('succeeded','failed');
ALTER TABLE network_vnics ADD FOREIGN KEY(tenant_id,vnic_id,last_operation_id) REFERENCES network_operations(tenant_id,vnic_id,operation_id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE network_reconciliations ADD UNIQUE(tenant_id,vnic_id);
ALTER TABLE network_provider_bindings ADD UNIQUE(tenant_id,vnic_id);
ALTER TABLE network_resource_history ADD FOREIGN KEY(tenant_id,vnic_id,operation_id) REFERENCES network_operations(tenant_id,vnic_id,operation_id);
ALTER TABLE network_idempotency ADD FOREIGN KEY(tenant_id,vnic_id,operation_id,operation_kind) REFERENCES network_operations(tenant_id,vnic_id,operation_id,kind);
-- Preserve every existing branch of the closed kind constraints and add only VNIC.
DO $$ DECLARE c record; expr text; BEGIN
 FOR c IN SELECT conrelid::regclass AS tbl,conname,pg_get_expr(conbin,conrelid) AS expr
 FROM pg_constraint WHERE contype='c' AND conrelid IN ('network_operations'::regclass,'network_idempotency'::regclass,'network_provider_bindings'::regclass)
 AND (pg_get_expr(conbin,conrelid) LIKE '%create_vpc%' OR pg_get_expr(conbin,conrelid) LIKE '%resource_kind%')
 LOOP
  expr:=c.expr;
  EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I',c.tbl,c.conname);
  IF c.tbl='network_operations'::regclass THEN expr:='('||expr||') OR (vnic_id IS NOT NULL AND kind IN (''create_vnic'',''delete_vnic''))';
  ELSIF c.tbl='network_idempotency'::regclass THEN expr:='('||expr||') OR (vnic_id IS NOT NULL AND operation_kind=''create_vnic'')';
  ELSE expr:='('||expr||') OR (vnic_id IS NOT NULL AND resource_kind=''vnic'')'; END IF;
  EXECUTE format('ALTER TABLE %s ADD CONSTRAINT %I CHECK(%s)',c.tbl,c.conname,expr);
 END LOOP;
END $$;
ALTER TABLE network_operations ADD CHECK(kind<>'delete_vnic' OR state<>'failed');
ALTER TABLE network_security_group_targets DROP CONSTRAINT network_security_group_targets_target_type_check;
ALTER TABLE network_security_group_targets ADD CHECK(target_type IN ('eip','container','vm','vnic'));
