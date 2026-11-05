package vm

import (
	"context"
	"time"

	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

func (s *Service) SetRelationsReader(r biz.RelationsPort) { s.relations = r }
func (s *Service) SetRuntimeReader(r biz.RuntimePort)     { s.runtime = r }
func referenceWire(r biz.ResourceReference) *pb.ResourceReference {
	return &pb.ResourceReference{Id: r.ID, Name: r.Name, State: r.State}
}
// projectDetail assembles the persisted detail plus optional guest runtime.
// includeRuntime is tri-state: unset or true collects runtime exactly as
// before; an explicit false skips guest reads because the runtime is served by
// the dedicated read entry point.
func (s *Service) projectDetail(ctx context.Context, v biz.VM, includeRuntime bool) (*pb.VirtualMachine, error) {
	out, err := s.project(ctx, v)
	if err != nil {
		return nil, err
	}
	if s.relations != nil {
		relations := s.relations.ReadRelations(ctx, v)
		out.Relations = &pb.VMRelations{Vpc: referenceWire(relations.VPC), Subnet: referenceWire(relations.Subnet)}
		for _, group := range relations.SecurityGroups {
			out.Relations.SecurityGroups = append(out.Relations.SecurityGroups, referenceWire(group))
		}
	}
	value := biz.Runtime{State: "UNAVAILABLE", AgentState: "UNKNOWN", FilesystemsState: "NOT_QUERIED"}
	reader := s.runtime
	if reader == nil {
		reader, _ = s.native.(biz.RuntimePort)
	}
	switch {
	case v.Native.UID == "":
		value.State = "NOT_RUNNING"
	case reader != nil && includeRuntime:
		value, err = reader.ReadRuntime(ctx, v)
		if err != nil {
			// A runtime dependency must not hide the persisted VM detail. Drop
			// all unverified guest fields, retaining an explicit read failure.
			value = biz.Runtime{State: "UNAVAILABLE", AgentState: "UNKNOWN", FilesystemsState: "NOT_QUERIED", ObservedAt: time.Now().UTC()}
		}
	default:
		// Runtime skipped by request; keep an explicit marker instead of a
		// stale-looking state.
		value = biz.Runtime{State: "NOT_QUERIED", AgentState: "UNKNOWN", FilesystemsState: "NOT_QUERIED"}
	}
	out.Runtime = &pb.VMRuntime{State: value.State, AgentState: value.AgentState, FilesystemsState: value.FilesystemsState, ObservedAt: observed(value.ObservedAt), StartedAt: observed(value.StartedAt), NodeName: value.NodeName, Hostname: value.Hostname, AgentVersion: value.AgentVersion, OsName: value.OSName, OsVersion: value.OSVersion, KernelRelease: value.KernelRelease, KernelVersion: value.KernelVersion}
	for _, nic := range value.Interfaces {
		out.Runtime.Interfaces = append(out.Runtime.Interfaces, &pb.GuestInterface{Name: nic.Name, GuestName: nic.GuestName, Mac: nic.MAC, Addresses: append([]string(nil), nic.Addresses...), InfoSource: nic.InfoSource})
	}
	for _, fs := range value.Filesystems {
		out.Runtime.Filesystems = append(out.Runtime.Filesystems, &pb.GuestFilesystem{DiskName: fs.DiskName, MountPoint: fs.MountPoint, FileSystemType: fs.FileSystemType, TotalBytes: fs.TotalBytes, UsedBytes: fs.UsedBytes})
	}
	return out, nil
}
