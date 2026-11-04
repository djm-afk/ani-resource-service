package data

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
)

func (p *Postgres) acceptSwitchVlan(ctx context.Context, i biz.PlatformIntent, a biz.Attribution) (biz.PlatformResource, error) {
	if i.Vlan.ClusterID != p.placement.ClusterID {
		return biz.PlatformResource{}, biz.Fail(biz.InvalidArgument, "cluster is not configured for this service")
	}
	sw, err := p.GetVirtualSwitch(ctx, i.Vlan.VirtualSwitchID)
	if err != nil {
		return biz.PlatformResource{}, err
	}
	infra := p.virtualSwitchInfrastructure
	if infra == nil {
		return biz.PlatformResource{}, biz.Fail(biz.DependencyUnavailable, "virtual switch inventory is unavailable")
	}
	// No SQL write transaction is held during external inventory collection.
	membership, err := infra.VirtualSwitchInventory(ctx, p.placement.ClusterID, sw, i.Vlan.DeviceName)
	if err != nil {
		return biz.PlatformResource{}, switchAdmissionFailure(err)
	}
	if len(membership.Members) == 0 || membership.Fingerprint == "" {
		return biz.PlatformResource{}, biz.Fail(biz.DependencyUnavailable, "switch member selection is required")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	defer tx.Rollback(ctx)
	q := p.queries.WithTx(tx)
	if err = q.LockPlatformKey(ctx, sqlcgen.LockPlatformKeyParams{ClusterID: p.placement.ClusterID, OperationKind: i.Kind, IdempotencyKey: i.IdempotencyKey}); err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	if v, found, err := platformReplay(ctx, q, p.placement.ClusterID, i); found || err != nil {
		return v, err
	}
	if err = q.LockPlatformCluster(ctx, sqlcgen.LockPlatformClusterParams{ClusterID: p.placement.ClusterID}); err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	s, err := q.LockVirtualSwitch(ctx, sqlcgen.LockVirtualSwitchParams{ResourceID: sw.ID})
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	if s.Version != sw.Version || (s.State == "deleting" || s.State == "deleted") || (s.ClusterID != "" && s.ClusterID != p.placement.ClusterID) {
		return biz.PlatformResource{}, biz.Fail(biz.ResourceBusy, "virtual switch binding changed or is unavailable")
	}
	now, err := q.DatabaseTime(ctx)
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}

	if s.BindingID == nil {
		for j := range membership.Members {
			membership.Members[j].State = "pending"
		}
		nodes, err := json.Marshal(membership.Members)
		if err != nil {
			return biz.PlatformResource{}, err
		}
		bid := uuid.NewString()
		s, err = q.BindVirtualSwitch(ctx, sqlcgen.BindVirtualSwitchParams{ResourceID: s.ResourceID, Version: s.Version, ClusterID: p.placement.ClusterID, BindingID: &bid, Fingerprint: membership.Fingerprint, Nodes: nodes})
		if err != nil {
			return biz.PlatformResource{}, databaseFailure(err)
		}
		if err = q.ScheduleVirtualSwitch(ctx, sqlcgen.ScheduleVirtualSwitchParams{ResourceID: s.ResourceID}); err != nil {
			return biz.PlatformResource{}, databaseFailure(err)
		}
	} else {
		var frozen []biz.VirtualSwitchNode
		if err = json.Unmarshal(s.Nodes, &frozen); err != nil {
			return biz.PlatformResource{}, databaseFailure(err)
		}
		if !sameSwitchMembers(frozen, membership.Members) {
			return biz.PlatformResource{}, biz.Fail(biz.ResourceBusy, "frozen switch membership changed")
		}
	}
	device, err := q.GetActiveSwitchDevice(ctx, sqlcgen.GetActiveSwitchDeviceParams{ClusterID: p.placement.ClusterID, DeviceName: i.Vlan.DeviceName})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	if err == nil {
		if textValue(device.VirtualSwitchID) != s.ResourceID || textValue(device.SwitchBindingID) != textValue(s.BindingID) {
			return biz.PlatformResource{}, biz.Fail(biz.ResourceInUse, "device has an active registration owned by another binding")
		}
		var frozenDevice []biz.PreparedDeviceNode
		if err = json.Unmarshal(device.NodeInventory, &frozenDevice); err != nil {
			return biz.PlatformResource{}, databaseFailure(err)
		}
		if !sameSwitchDeviceIdentity(frozenDevice, membership.Inventory.Items) {
			return biz.PlatformResource{}, biz.Fail(biz.ResourceBusy, "registered device membership changed")
		}
	} else {
		id, op := newEgressID("device"), uuid.NewString()
		nodes, err := json.Marshal(membership.Inventory.Items)
		if err != nil {
			return biz.PlatformResource{}, err
		}
		_, err = q.InsertPlatformResource(ctx, sqlcgen.InsertPlatformResourceParams{ResourceID: id, Kind: "device", ClusterID: p.placement.ClusterID, Name: i.Vlan.DeviceName, CreatedAt: now, LastOperationID: op, ProviderName: "device-" + id[7:], BindingID: uuid.NewString()})
		if err != nil {
			return biz.PlatformResource{}, databaseFailure(err)
		}
		if err = q.InsertSwitchDevice(ctx, sqlcgen.InsertSwitchDeviceParams{ResourceID: id, ClusterID: p.placement.ClusterID, DeviceName: i.Vlan.DeviceName, InventoryFingerprint: membership.Inventory.Fingerprint, NodeInventory: nodes, VirtualSwitchID: &s.ResourceID, SwitchBindingID: s.BindingID}); err != nil {
			return biz.PlatformResource{}, databaseFailure(err)
		}
		if err = q.InsertPlatformOperation(ctx, sqlcgen.InsertPlatformOperationParams{ResourceID: id, OperationID: op, Kind: "adopt_device", State: string(biz.Succeeded), CreatedAt: now, CompletedAt: &now}); err != nil {
			return biz.PlatformResource{}, databaseFailure(err)
		}
		if err = affected(q.RegisterSwitchDevice(ctx, sqlcgen.RegisterSwitchDeviceParams{ResourceID: id})); err != nil {
			return biz.PlatformResource{}, err
		}
		if err = q.InsertPlatformHistory(ctx, sqlcgen.InsertPlatformHistoryParams{HistoryID: uuid.NewString(), ResourceID: id, OperationID: &op, Event: "internal_registration", ResourceState: string(biz.Available), OperationState: string(biz.Succeeded), ActorRef: a.Actor, CallerRef: a.DirectCaller, CreatedAt: now}); err != nil {
			return biz.PlatformResource{}, databaseFailure(err)
		}
		device.ResourceID = id
	}
	id, op := newEgressID("vlan"), uuid.NewString()
	r, err := q.InsertPlatformResource(ctx, sqlcgen.InsertPlatformResourceParams{ResourceID: id, Kind: "vlan", ClusterID: p.placement.ClusterID, Name: i.Name, Description: i.Description, CreatedAt: now, LastOperationID: op, ProviderName: "vlan-" + id[5:], BindingID: uuid.NewString()})
	if err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	if err = q.InsertSwitchVlan(ctx, sqlcgen.InsertSwitchVlanParams{ResourceID: id, ClusterID: p.placement.ClusterID, DeviceID: device.ResourceID, VlanID: i.Vlan.VlanID, VirtualSwitchID: &s.ResourceID}); err != nil {
		return biz.PlatformResource{}, switchAdmissionFailure(err)
	}
	if err = q.InsertPlatformOperation(ctx, sqlcgen.InsertPlatformOperationParams{ResourceID: id, OperationID: op, Kind: i.Kind, State: string(biz.Queued), CreatedAt: now, NextAttemptAt: &now}); err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	if err = q.InsertPlatformReconciliation(ctx, sqlcgen.InsertPlatformReconciliationParams{ResourceID: id, NextRunAt: now}); err != nil {
		return biz.PlatformResource{}, databaseFailure(err)
	}
	v, err := platformSnapshot(ctx, q, r)
	if err != nil {
		return v, err
	}
	v.ObservationStale = true
	if err = savePlatformAcceptance(ctx, q, i, a, v, now, string(biz.Queued)); err != nil {
		return v, err
	}
	if err = q.InsertVirtualSwitchHistory(ctx, sqlcgen.InsertVirtualSwitchHistoryParams{HistoryID: uuid.NewString(), ResourceID: s.ResourceID, Event: "vlan_accepted", ActorRef: a.Actor, CallerRef: a.DirectCaller, Detail: []byte(`{"vlan_id":"` + id + `"}`)}); err != nil {
		return v, databaseFailure(err)
	}
	return v, databaseFailure(tx.Commit(ctx))
}

func sameSwitchMembers(a, b []biz.VirtualSwitchNode) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]biz.VirtualSwitchNode{}
	for _, n := range a {
		seen[n.NodeUID] = n
	}
	if len(seen) != len(a) {
		return false
	}
	for _, n := range b {
		old, ok := seen[n.NodeUID]
		if !ok || old.NodeName != n.NodeName || old.Priority != n.Priority || old.Allocated != n.Allocated {
			return false
		}
	}
	return true
}

func sameSwitchDeviceIdentity(frozen, current []biz.PreparedDeviceNode) bool {
	if len(frozen) == 0 || len(frozen) != len(current) {
		return false
	}
	identities := map[string]biz.PreparedDeviceNode{}
	for _, n := range frozen {
		key := n.NodeUID + "/" + n.Name
		if n.NodeUID == "" || n.NodeName == "" || n.Name == "" || identities[key].NodeUID != "" {
			return false
		}
		identities[key] = n
	}
	for _, n := range current {
		key := n.NodeUID + "/" + n.Name
		old, ok := identities[key]
		if !ok || n.NodeName != old.NodeName {
			return false
		}
		delete(identities, key)
	}
	return len(identities) == 0
}
