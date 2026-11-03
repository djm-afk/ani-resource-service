-- Persist tenant-selected Subnet settings independently of KCN allocation status.
ALTER TABLE network_subnets
 ADD COLUMN exclude_ips text[] NOT NULL DEFAULT '{}',
 ADD COLUMN enable_dhcp boolean NOT NULL DEFAULT false,
 ADD COLUMN dns_servers text[] NOT NULL DEFAULT '{}';
