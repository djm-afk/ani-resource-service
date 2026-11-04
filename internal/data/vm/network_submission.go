package vm

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

func (p *Postgres) UseNetworkNotifier(notify func(context.Context, string, string) error) {
	p.notifyNetwork = notify
}

func networkNotificationPending(n biz.NetworkPlan) bool {
	return n.AttachmentID != "" && n.NotifiedSnapshotRevision < n.SnapshotRevision
}

func (p *Postgres) notifyNetworkSubmission(ctx context.Context, v biz.VM) error {
	if p.notifyNetwork == nil || !networkNotificationPending(v.Network) {
		return nil
	}
	call, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := p.notifyNetwork(call, v.TenantID, v.Network.AttachmentID); err != nil {
		return err
	}
	// This delivery ACK is independent of VM and Attachment lifecycle state.
	// A concurrent newer snapshot cannot be acknowledged by this send.
	_, err := p.pool.Exec(ctx, `UPDATE vm.virtual_machines SET current_configuration=
 jsonb_set(current_configuration,'{Network,NotifiedSnapshotRevision}',to_jsonb($4::bigint))
 WHERE tenant_id=$1 AND id=$2 AND current_configuration #>> '{Network,AttachmentID}'=$3
 AND (current_configuration #>> '{Network,SnapshotRevision}')::bigint=$4`, v.TenantID, v.ID, v.Network.AttachmentID, v.Network.SnapshotRevision)
	return err
}

// DispatchNetworkNotification reuses the existing observer loop to retry a
// committed snapshot whose notification was interrupted. It reads only Res
// records, including closed consumers, and performs no native observation.
func (p *Postgres) DispatchNetworkNotification(ctx context.Context) (bool, error) {
	if p.notifyNetwork == nil {
		return false, nil
	}
	var raw []byte
	err := p.pool.QueryRow(ctx, `SELECT current_configuration FROM vm.virtual_machines
 WHERE COALESCE(current_configuration #>> '{Network,AttachmentID}','')<>''
 AND COALESCE((current_configuration #>> '{Network,NotifiedSnapshotRevision}')::bigint,0)
     < COALESCE((current_configuration #>> '{Network,SnapshotRevision}')::bigint,0)
 ORDER BY updated_at,tenant_id,id LIMIT 1`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	v, err := vmFrom(raw)
	if err != nil {
		return false, err
	}
	return true, p.notifyNetworkSubmission(ctx, v)
}
