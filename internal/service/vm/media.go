package vm

import (
	"context"
	"errors"

	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func (s *Service) mediaCommand(ctx context.Context, message proto.Message, action string) (*pb.Operation, error) {
	if message == nil || !message.ProtoReflect().IsValid() {
		return nil, rpcError(biz.ErrInvalid)
	}
	fields := message.ProtoReflect().Descriptor().Fields()
	value := message.ProtoReflect()
	get := func(name string) string { return value.Get(fields.ByName(protoreflect.Name(name))).String() }
	tenant, id, key := get("tenant_id"), get("vm_id"), get("idempotency_key")
	if e := biz.RequireTenant(ctx, tenant); e != nil {
		return nil, rpcError(e)
	}
	version := value.Get(fields.ByName("expected_version")).Int()
	if !biz.UUID(id) || key == "" || len(key) > 256 || version < 1 {
		return nil, rpcError(biz.ErrInvalid)
	}
	raw, e := pb.CanonicalCommandIntent(message)
	if e != nil {
		return nil, rpcError(e)
	}
	command := biz.Command{TenantID: tenant, VMID: id, Action: action, Key: key, ExpectedVersion: version, RequestDigest: pb.Digest(raw), Intent: raw}
	// Original accepted intent wins even if the catalog/source disappeared,
	// version advanced, or final deletion subsequently closed the VM.
	if operation, e := s.repo.LookupReplay(ctx, command); !errors.Is(e, biz.ErrNotFound) {
		if e != nil {
			return nil, rpcError(e)
		}
		return wireOp(operation), nil
	}
	current, e := s.repo.Get(ctx, tenant, id)
	if e != nil {
		return nil, rpcError(e)
	}
	if current.Closing {
		return nil, rpcError(biz.ErrBusy)
	}
	if current.Version != version {
		return nil, rpcError(biz.ErrVersion)
	}
	if e = biz.InitializeMedia(&current); e != nil {
		return nil, rpcError(e)
	}
	mutation := biz.MediaMutation{UserIntent: raw}
	switch action {
	case "set-media":
		request := message.(*pb.SetMediaRequest)
		mutation.Slot = request.Slot
		if request.Slot != "INSTALL" && request.Slot != "DRIVER" || request.MediaKey == "" {
			return nil, rpcError(biz.ErrInvalid)
		}
		// Re-insertion of one registered retained copy uses its original source
		// and native identity, and does not depend on the administrator's source.
		for _, copy := range current.MediaCopies {
			if copy.Source.Key == request.MediaKey && !copy.Tombstone && copy.DV.UID != "" {
				if mutation.Copy != nil {
					return nil, rpcError(biz.ErrChanged)
				}
				frozen := copy
				mutation.Copy = &frozen
			}
		}
		if mutation.Copy == nil {
			// The same resolution the picker and the create path use, so an ISO the
			// tenant uploaded can be mounted for the first time as well as booted.
			configured, e := s.resolveMedia(ctx, current.TenantID, request.MediaKey)
			if e != nil {
				return nil, rpcError(e)
			}
			source := domainSource(configured)
			if e = biz.ValidateMediaSource(current, request.Slot, source); e != nil {
				return nil, rpcError(e)
			}
			if e = s.native.ValidateSource(ctx, source); e != nil {
				return nil, rpcError(e)
			}
			copy := biz.NewMediaCopy(current, source)
			mutation.Copy = &copy
		} else if e = biz.ValidateMediaSource(current, request.Slot, mutation.Copy.Source); e != nil {
			return nil, rpcError(e)
		}
	case "eject-media":
		mutation.Slot = message.(*pb.EjectMediaRequest).Slot
		if mutation.Slot != "INSTALL" && mutation.Slot != "DRIVER" {
			return nil, rpcError(biz.ErrInvalid)
		}
	case "set-boot":
		mutation.BootDevice = message.(*pb.SetBootDeviceRequest).BootDevice
		if mutation.BootDevice != "SYSTEM_DISK" && mutation.BootDevice != "INSTALL_MEDIA" {
			return nil, rpcError(biz.ErrInvalid)
		}
	default:
		return nil, rpcError(biz.ErrInvalid)
	}
	repository, ok := s.repo.(biz.MediaRepository)
	if !ok {
		return nil, rpcError(biz.ErrDependency)
	}
	operation, e := repository.AcceptMedia(ctx, command, mutation)
	if e != nil {
		return nil, rpcError(e)
	}
	return wireOp(operation), nil
}
func (s *Service) SetMedia(ctx context.Context, r *pb.SetMediaRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.mediaCommand(ctx, r, "set-media")
}
func (s *Service) EjectMedia(ctx context.Context, r *pb.EjectMediaRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.mediaCommand(ctx, r, "eject-media")
}
func (s *Service) SetBootDevice(ctx context.Context, r *pb.SetBootDeviceRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.mediaCommand(ctx, r, "set-boot")
}
func wireMedia(v biz.VM, out *pb.VirtualMachine) {
	if v.CurrentBootDevice != "" {
		out.BootDevice = v.CurrentBootDevice
	}
	for _, slot := range v.MediaSlots {
		state := &pb.MediaState{Slot: slot.Slot, EffectiveNow: slot.EffectiveNow, PendingRestart: slot.PendingRestart}
		if copy, found := biz.FindMediaCopy(v, slot.DesiredCopyID); found {
			state.DesiredMedia = copy.Source.Key
		}
		if copy, found := biz.FindMediaCopy(v, slot.ObservedCopyID); found {
			state.ObservedMedia = copy.Source.Key
		}
		out.Media = append(out.Media, state)
	}
}
