-- Requested addresses are immutable creation intent, separate from KCN facts.
ALTER TABLE network_vnics
 ADD COLUMN requested_ip text NOT NULL DEFAULT '',
 ADD COLUMN requested_mac text NOT NULL DEFAULT ''
 CHECK(requested_mac='' OR requested_mac ~ '^[0-9a-f]{2}(:[0-9a-f]{2}){5}$');

-- Only manual product VNICs participate. Deleting retains the claim until
-- original-provider cleanup completes; stopping/releasing a consumer does not.
CREATE UNIQUE INDEX network_vnics_manual_mac_unique
 ON network_vnics(tenant_id,subnet_id,lower(COALESCE(NULLIF(requested_mac,''),mac_address)))
 WHERE state<>'deleted' AND (requested_mac<>'' OR mac_address<>'');
