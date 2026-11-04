-- Imported platform network products are a read-only directory. Their
-- consumer records retain real tenants and their existing operation history.
CREATE TABLE network_platform_vpcs (
 vpc_id text PRIMARY KEY CHECK(vpc_id ~ '^vpc_[0-9a-f]{32}$'),
 cluster_id text NOT NULL UNIQUE, cluster_uid text NOT NULL CHECK(cluster_uid<>''),
 namespace text NOT NULL CHECK(namespace='kcn-system'),
 provider_name text NOT NULL CHECK(provider_name='kcn-cluster'),
 provider_uid text NOT NULL CHECK(provider_uid<>''),
 name text NOT NULL, description text NOT NULL DEFAULT '', cidr text NOT NULL DEFAULT '',
 published boolean NOT NULL DEFAULT false, accept_new boolean NOT NULL DEFAULT false,
 state text NOT NULL DEFAULT 'provisioning' CHECK(state IN ('provisioning','available','degraded','failed')),
 reason text NOT NULL DEFAULT '', version bigint NOT NULL DEFAULT 1 CHECK(version>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 observed_at timestamptz, UNIQUE(cluster_id,vpc_id)
);
CREATE TABLE network_platform_subnets (
 subnet_id text PRIMARY KEY CHECK(subnet_id ~ '^subnet_[0-9a-f]{32}$'),
 vpc_id text NOT NULL, cluster_id text NOT NULL,
 namespace text NOT NULL CHECK(namespace<>''), provider_name text NOT NULL CHECK(provider_name<>''),
 provider_uid text NOT NULL CHECK(provider_uid<>''), binding_id uuid NOT NULL UNIQUE,
 name text NOT NULL, description text NOT NULL DEFAULT '', cidr text NOT NULL DEFAULT '', gateway text NOT NULL DEFAULT '',
 exclude_ips text[] NOT NULL DEFAULT '{}', dns_servers text[] NOT NULL DEFAULT '{}', enable_dhcp boolean NOT NULL DEFAULT false,
 available_ip_range text, used_ip_range text,
 published boolean NOT NULL DEFAULT false, accept_new boolean NOT NULL DEFAULT false,
 state text NOT NULL DEFAULT 'provisioning' CHECK(state IN ('provisioning','available','degraded','failed')),
 reason text NOT NULL DEFAULT '', version bigint NOT NULL DEFAULT 1 CHECK(version>0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), updated_at timestamptz NOT NULL DEFAULT clock_timestamp(), observed_at timestamptz,
 FOREIGN KEY(cluster_id,vpc_id) REFERENCES network_platform_vpcs(cluster_id,vpc_id),
 UNIQUE(cluster_id,namespace,provider_name), UNIQUE(vpc_id,subnet_id), UNIQUE(vpc_id,subnet_id,binding_id)
);
CREATE INDEX network_platform_subnets_directory ON network_platform_subnets(vpc_id,created_at DESC,subnet_id DESC) WHERE published;
CREATE TABLE network_platform_network_observations (
 resource_id text PRIMARY KEY, vpc_id text UNIQUE REFERENCES network_platform_vpcs(vpc_id),
 subnet_id text UNIQUE REFERENCES network_platform_subnets(subnet_id),
 next_run_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 lease_owner uuid, lease_until timestamptz, lease_epoch bigint NOT NULL DEFAULT 0 CHECK(lease_epoch>=0),
 requested_generation bigint NOT NULL DEFAULT 1, processed_generation bigint NOT NULL DEFAULT 0,
 retry_not_before timestamptz NOT NULL DEFAULT '1970-01-01 UTC', retry_attempt integer NOT NULL DEFAULT 0 CHECK(retry_attempt>=0),
 observation_error text NOT NULL DEFAULT '',
 CHECK(num_nonnulls(vpc_id,subnet_id)=1), CHECK(resource_id=coalesce(vpc_id,subnet_id)),
 CHECK((lease_owner IS NULL)=(lease_until IS NULL)),
 CHECK(0<=processed_generation AND processed_generation<=requested_generation)
);
CREATE INDEX network_platform_network_observations_due ON network_platform_network_observations(next_run_at,resource_id);
ALTER TABLE network_vnics ALTER COLUMN vpc_id DROP NOT NULL, ALTER COLUMN subnet_id DROP NOT NULL,
 ADD COLUMN platform_vpc_id text, ADD COLUMN platform_subnet_id text,
 ADD CONSTRAINT network_vnics_network_scope CHECK(
 (vpc_id IS NOT NULL AND subnet_id IS NOT NULL AND platform_vpc_id IS NULL AND platform_subnet_id IS NULL) OR
 (vpc_id IS NULL AND subnet_id IS NULL AND platform_vpc_id IS NOT NULL AND platform_subnet_id IS NOT NULL)),
 ADD FOREIGN KEY(platform_vpc_id,platform_subnet_id) REFERENCES network_platform_subnets(vpc_id,subnet_id);
ALTER TABLE network_attachments ALTER COLUMN vpc_id DROP NOT NULL, ALTER COLUMN subnet_id DROP NOT NULL,
 ADD COLUMN platform_vpc_id text, ADD COLUMN platform_subnet_id text,
 ADD CONSTRAINT network_attachments_network_scope CHECK(
 (vpc_id IS NOT NULL AND subnet_id IS NOT NULL AND platform_vpc_id IS NULL AND platform_subnet_id IS NULL) OR
 (vpc_id IS NULL AND subnet_id IS NULL AND platform_vpc_id IS NOT NULL AND platform_subnet_id IS NOT NULL)),
 ADD FOREIGN KEY(platform_vpc_id,platform_subnet_id,binding_id) REFERENCES network_platform_subnets(vpc_id,subnet_id,binding_id);
CREATE INDEX network_vnics_platform_subnet ON network_vnics(tenant_id,platform_subnet_id) WHERE state<>'deleted';
CREATE UNIQUE INDEX network_vnics_platform_manual_mac_unique ON network_vnics(platform_subnet_id,lower(requested_mac)) WHERE platform_subnet_id IS NOT NULL AND state<>'deleted' AND requested_mac<>'';
CREATE INDEX network_attachments_platform_subnet ON network_attachments(tenant_id,platform_subnet_id) WHERE state<>'released';
CREATE INDEX network_attachments_platform_vpc ON network_attachments(tenant_id,platform_vpc_id) WHERE state<>'released';
ALTER TABLE network_tenant_namespaces ADD COLUMN basic_isolation_name text NOT NULL DEFAULT '',
 ADD COLUMN basic_isolation_uid text NOT NULL DEFAULT '', ADD CHECK(basic_isolation_uid='' OR basic_isolation_name<>'');
