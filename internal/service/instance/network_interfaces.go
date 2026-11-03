package instance

import (
	"context"
	"errors"
	"github.com/google/uuid"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	networkpb "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func networkChoice(c biz.NetworkInterfaceChoice) *networkpb.NetworkInterfaceChoice {
	return &networkpb.NetworkInterfaceChoice{Mode: c.Mode, VnicId: c.VNicID, VnicName: c.Name, ProviderUid: c.ProviderUID, MacAddress: c.MACAddress, Ips: append([]string{}, c.IPs...)}
}
func (s *Service) GetNetworkInterfaces(ctx context.Context, r *pb.GetInstanceRequest) (*networkpb.NetworkInterfaces, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if err := biz.RequireTenant(ctx, r.TenantId); err != nil {
		return nil, rpcError(err)
	}
	i, err := s.repo.Get(ctx, r.TenantId, r.InstanceId)
	if err != nil {
		return nil, rpcError(err)
	}
	if i.Spec.Network == nil || i.Spec.InstanceKind != biz.KindContainer {
		return &networkpb.NetworkInterfaces{Version: i.Version, Supported: false, UnsupportedReason: "INSTANCE_NETWORK_NOT_SUPPORTED"}, nil
	}

	cfg := biz.PrimaryNetworkInterface(i)
	item := &networkpb.NetworkInterface{ConfigurationState: "ready", Slot: "primary", RequestedRevision: cfg.RequestedRevision, AppliedRevision: cfg.AppliedRevision, PendingRestart: cfg.PendingRestart()}
	if reader, ok := s.network.(interface {
		ResolveNetworkInterfaceChoice(context.Context, biz.Instance, biz.NetworkInterfaceChoice) (biz.NetworkInterfaceChoice, error)
	}); ok {
		var err error
		cfg.Desired, err = reader.ResolveNetworkInterfaceChoice(ctx, i, cfg.Desired)
		if err != nil {
			item.ObservationReason = biz.ErrNetworkUnavailable.Error()
		}
		cfg.Applied, err = reader.ResolveNetworkInterfaceChoice(ctx, i, cfg.Applied)
		if err != nil {
			item.ObservationReason = biz.ErrNetworkUnavailable.Error()
		}
	}
	if i.Observed != biz.Stopped && i.Desired != biz.Deleted && i.Network != nil {
		n := *i.Network
		confirmed := false
		if s.network != nil {
			if current, err := s.network.Observe(ctx, i); err == nil {
				n.CurrentIPs = current.CurrentIPs
				n.CurrentVNicName = current.CurrentVNicName
				n.CurrentVNicUID = current.CurrentVNicUID
				n.CurrentMACAddress = current.CurrentMACAddress
				n.CurrentMode = current.CurrentMode
				n.ObservedAt = current.ObservedAt
				n.NetworkState = current.State
				n.Reason = current.Reason
				confirmed = current.Confirmed
			} else {
				n.Reason = biz.ErrNetworkUnavailable.Error()
			}
		}
		item.CurrentVnicName = n.CurrentVNicName
		item.CurrentMacAddress = n.CurrentMACAddress
		item.CurrentIps = append([]string{}, n.CurrentIPs...)
		if !n.ObservedAt.IsZero() {
			item.ObservedAt = timestamppb.New(n.ObservedAt)
		}
		if n.Reason != "" {
			item.ObservationReason = n.Reason
		}
		applied := n.CurrentMode == cfg.Applied.Mode
		if n.CurrentMode == "manual" {
			applied = applied && n.CurrentVNicUID == cfg.Applied.ProviderUID && n.CurrentVNicName == cfg.Applied.ProviderName
			if applied {
				item.CurrentVnicName = cfg.Applied.Name
			}
		}
		item.EffectiveNow = !cfg.PendingRestart() && confirmed && applied && i.Observed == biz.Running && n.NetworkState == "attached" && len(item.CurrentIps) > 0 && item.ObservationReason == ""
	}
	if i.LastOperationID != "" {
		op, err := s.repo.GetOperation(ctx, i.TenantID, i.LastOperationID)
		if err != nil {
			return nil, rpcError(err)
		}
		if op.Action == "set-network-interface" {
			item.OperationId = op.ID
			item.OperationReason = op.Reason
			if op.State == "failed" {
				item.ConfigurationState = "failed"
			} else if !biz.TerminalOperation(op.State) {
				item.ConfigurationState = "configuring"
				item.PendingRestart = false
				item.EffectiveNow = false
			}
		}
	}
	item.Desired, item.Applied = networkChoice(cfg.Desired), networkChoice(cfg.Applied)

	return &networkpb.NetworkInterfaces{Items: []*networkpb.NetworkInterface{item}, Version: i.Version, Supported: true}, nil
}
func (s *Service) SetNetworkInterface(ctx context.Context, r *pb.SetNetworkInterfaceRequest) (*pb.OperationResponse, error) {
	if r == nil || len(r.ProtoReflect().GetUnknown()) > 0 {
		return nil, rpcError(biz.ErrInvalid)
	}
	if err := biz.RequireTenant(ctx, r.TenantId); err != nil {
		return nil, rpcError(err)
	}
	raw, err := pb.CanonicalNetworkInterfaceIntent(r.InstanceId, r.Slot, r.Mode, r.VnicId, r.ExpectedVersion)
	if err != nil || !biz.UUID(r.IdempotencyKey) {
		return nil, rpcError(biz.ErrInvalid)
	}
	c := biz.Command{TenantID: r.TenantId, ID: r.InstanceId, Action: "set-network-interface", Key: r.IdempotencyKey, ExpectedVersion: r.ExpectedVersion, CanonicalIntent: raw, RequestHash: pb.CommandIntentDigest(raw), NetworkInterface: &biz.NetworkInterfaceIntent{Slot: r.Slot, Mode: r.Mode, VNicID: r.VnicId}}
	replay, ok := s.repo.(biz.CommandReplayRepository)
	if !ok {
		return nil, rpcError(biz.ErrNetworkUnavailable)
	}
	a, err := replay.LookupCommandReplay(ctx, c)
	if err == nil {
		return ack(a), nil
	}
	if !errors.Is(err, biz.ErrNotFound) {
		return nil, rpcError(err)
	}
	c.OperationID = uuid.NewString()
	a, err = s.repo.AcceptCommand(ctx, c)
	return ack(a), rpcError(err)
}
