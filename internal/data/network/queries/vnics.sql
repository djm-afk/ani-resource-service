-- name: GetVNicRow :one
SELECT * FROM network_vnics WHERE tenant_id=$1 AND vnic_id=$2;
-- name: LockVNicRow :one
SELECT * FROM network_vnics WHERE tenant_id=$1 AND vnic_id=$2 FOR UPDATE;
-- name: AdvanceVNic :one
UPDATE network_vnics SET state=sqlc.arg(state),reason=sqlc.arg(reason),version=version+1,updated_at=clock_timestamp(),
 observed_at=CASE WHEN sqlc.arg(observed)::boolean THEN sqlc.narg(observed_at)::timestamptz ELSE observed_at END,
 mac_address=CASE WHEN sqlc.arg(observed)::boolean AND sqlc.arg(mac_address)::text<>'' THEN sqlc.arg(mac_address)::text ELSE mac_address END,
 primary_ip=CASE WHEN sqlc.arg(observed)::boolean AND sqlc.arg(primary_ip)::text<>'' THEN sqlc.arg(primary_ip)::text ELSE primary_ip END,
 ips=CASE WHEN sqlc.arg(observed)::boolean AND cardinality(sqlc.arg(ips)::text[])>0 THEN sqlc.arg(ips)::text[] ELSE ips END
WHERE tenant_id=sqlc.arg(tenant_id) AND vnic_id=sqlc.arg(vnic_id) AND version=sqlc.arg(version) RETURNING *;
