-- Manual Intranet forwarding evidence remains diagnostic metadata. Provider
-- identity, current Ready observations and topology are still checked by runtime.
ALTER TABLE network_public_pools DROP CONSTRAINT network_pool_allocation_verification;

ALTER TABLE network_platform_operations DROP CONSTRAINT network_platform_operations_kind_check;
ALTER TABLE network_platform_operations ADD CONSTRAINT network_platform_operations_kind_check
 CHECK (kind IN ('adopt_device','create_vlan','delete_vlan','create_egress_gateway','delete_egress_gateway',
 'create_public_pool','delete_public_pool','set_pool_allocation','set_default_pool','verify_public_pool',
 'create_intranet_pool','delete_intranet_pool','set_intranet_pool_allocation','set_default_intranet_pool','verify_intranet_pool',
 'set_intranet_pool_auto_allocation'));
