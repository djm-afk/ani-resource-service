ALTER TABLE network_tenant_namespaces ADD COLUMN namespace_uid text;
ALTER TABLE network_tenant_namespaces ADD CONSTRAINT network_tenant_namespaces_uid_nonempty
 CHECK (namespace_uid IS NULL OR btrim(namespace_uid) <> '');
