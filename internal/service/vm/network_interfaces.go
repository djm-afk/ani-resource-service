package vm

import (
	"context"
	networkpb "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func choice(c biz.NetworkInterfaceChoice) *networkpb.NetworkInterfaceChoice {
	return &networkpb.NetworkInterfaceChoice{Mode: c.Mode, VnicId: c.VNicID, VnicName: c.Name, ProviderUid: c.ProviderUID, MacAddress: c.MACAddress, Ips: append([]string{}, c.IPs...)}
}
func (s *Service) GetNetworkInterfaces(ctx context.Context, r *pb.GetVMRequest) (*networkpb.NetworkInterfaces, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if err := biz.RequireTenant(ctx, r.TenantId); err != nil {
		return nil, rpcError(err)
	}
	v, err := s.repo.Get(ctx, r.TenantId, r.VmId)
	if err != nil {
		return nil, rpcError(err)
	}

	cfg := biz.PrimaryNetworkInterface(v)
	item := &networkpb.NetworkInterface{ConfigurationState: "ready", Slot: "primary", RequestedRevision: cfg.RequestedRevision, AppliedRevision: cfg.AppliedRevision, PendingRestart: cfg.PendingRestart()}
	if reader, ok := s.addresses.(biz.NetworkInterfaceReadPort); ok {
		var err error
		cfg.Desired, err = reader.ResolveNetworkInterfaceChoice(ctx, v, cfg.Desired)
		if err != nil {
			item.ObservationReason = biz.ErrDependency.Error()
		}
		cfg.Applied, err = reader.ResolveNetworkInterfaceChoice(ctx, v, cfg.Applied)
		if err != nil {
			item.ObservationReason = biz.ErrDependency.Error()
		}
		current, err := reader.CurrentNetworkInterface(ctx, v)
		if err != nil {
			item.ObservationReason = biz.ErrDependency.Error()
		} else {
			item.CurrentVnicName = current.ProviderName
			item.CurrentMacAddress = current.MACAddress
			item.CurrentIps = current.IPs
			if !current.ObservedAt.IsZero() {
				item.ObservedAt = timestamppb.New(current.ObservedAt)
			}
			if current.Reason != "" {
				item.ObservationReason = current.Reason
			}
			applied := current.Mode == cfg.Applied.Mode
			if current.Mode == "manual" {
				applied = applied && current.ProviderUID == cfg.Applied.ProviderUID && current.ProviderName == cfg.Applied.ProviderName
				if applied {
					item.CurrentVnicName = cfg.Applied.Name
				}
			}
			item.EffectiveNow = !cfg.PendingRestart() && current.Confirmed && applied && len(current.IPs) > 0 && item.ObservationReason == ""
		}
	} else {
		item.ObservationReason = biz.ErrDependency.Error()
	}
	if v.LastOperationID != "" {
		op, err := s.repo.GetOperation(ctx, v.TenantID, v.ID, v.LastOperationID)
		if err != nil {
			return nil, rpcError(err)
		}
		if op.Action == "set-network-interface" {
			item.OperationId = op.ID
			item.OperationReason = op.Reason
			if op.State == "failed" {
				item.ConfigurationState = "failed"
			} else if op.State != "succeeded" {
				item.ConfigurationState = "configuring"
				item.PendingRestart = false
				item.EffectiveNow = false
			}
		}
	}
	item.Desired, item.Applied = choice(cfg.Desired), choice(cfg.Applied)

	return &networkpb.NetworkInterfaces{Items: []*networkpb.NetworkInterface{item}, Version: v.Version, Supported: true}, nil
}
func (s *Service) SetNetworkInterface(ctx context.Context, r *pb.SetNetworkInterfaceRequest) (*pb.Operation, error) {
	if r == nil || (biz.NetworkInterfaceIntent{Slot: r.Slot, Mode: r.Mode, VNicID: r.VnicId}).Validate() != nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.command(ctx, r, "set-network-interface")
}
