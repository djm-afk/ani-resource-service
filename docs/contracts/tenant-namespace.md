# Standard tenant Namespace and ModelDev pull Secret

This contract replaces configurable Namespace prefixes in Resource owners.
Sandbox tenant-space provisioning initializes the shared Namespace when it is
absent; all other resource owners use that same Namespace.

`resource_tenant_id` is an unchanged, canonical lowercase, nonzero UUID. The
Namespace name is exactly `tenant-<resource_tenant_id>`. Each owner GETs that
name on its configured cluster, checks a nonempty UID, `phase=Active`, no deletion
timestamp, and these two labels:

- `network.ani.io/tenant-id=<resource_tenant_id>`
- `network.ani.io/managed-by=ani-network-service`

Sandbox tenant-space provisioning first reads the standard name, validates and
reuses an existing owned Namespace, or journals one create if absent. It also
sets the protected Sandbox access marker for its restricted per-tenant RBAC.
Other owners do not create, patch, adopt or delete the Namespace. Missing or
foreign objects are errors for resource creation. Original execution reads and
cleanup keep their original name/UID and do not require Active; same-name
replacement never grants authority.
The pure rules and public positive/negative vectors live in
`internal/tenantnamespace/`.

Network accepts the live identity outside its database transaction and inserts
or compares that exact identity inside the existing acceptance transaction.
Migration 0025 adds nullable `network_tenant_namespaces.namespace_uid` without
changing historical migrations. Runtime rejects NULL. During a maintenance
window, use the [temporary guarded UID cutover](../runbooks/tenant-namespace-uid-window.md):
freeze target identity-table INSERT/UPDATE, apply the official migration,
atomically require non-NULL UIDs, then use `scripts/tenant-namespace-backfill`
first without `-apply`. Apply requires its reviewed SHA256 and the applied
require-UID window plan. It only fills exact existing NULL rows; existing UIDs
and other clusters are never overwritten. Verify zero NULL, update Resource,
and prove original writers retired before removing the temporary guard. The
tools neither deploy the service nor mutate Namespace objects.

`network.namespace_prefix`, `ANI_NETWORK_NAMESPACE_PREFIX`,
`NETWORK_NAMESPACE_PREFIX` and Instance's former nonempty prefix configuration
are rejected. Namespace field 4 and its source name are reserved in conf.proto.

ModelDev's authenticated `ani-modeldev-service` caller config contains only
subject and cluster allowlist. Its requested tenant must equal the trusted
metadata, and the exact standard name, original UID and cluster must be verified
before Secret writes. Its legacy namespace/owner-label allowlists are rejected.
The tenant's existing image space and pull robot remain the sole registry
credential source; missing space or wrong authority fails without initializing
Harbor. Resource writes only the deterministic Secret; it never writes an SA.
Namespace network ownership labels are separate from Secret labels:
`ani.io/managed-by=ani-resource-service`, image tenant, space and generation.
Inference retains its existing static caller branch and configuration.
