package vm

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	compute "github.com/zhangzhe-ctrl/ani-resource-service/api/compute/v1"
	console "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/console/v1"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type ComputeResolver interface {
	ResolveComputePreset(context.Context, *compute.ResolveComputePresetRequest) (*compute.ComputeSelection, error)
}
type AddressReader interface {
	CurrentAddresses(context.Context, biz.VM) ([]biz.Address, error)
}
type Config struct {
	ClusterID, ServiceAccount, StorageClass, StorageClassUID, BackendStorageClass, MediaStorageClass string
	Media                                                                                            []*pb.MediaReference
	DefaultDNSServers                                                                                []string
	LogLimits                                                                                        biz.LogLimits
	// ImageUpload is the fixed deployment input for the tenant image lane.
	ImageUpload ImageUploadConfig
}
type Service struct {
	pb.UnimplementedVMServiceServer
	console.UnimplementedVMConsoleServiceServer
	repo           biz.Repository
	compute        ComputeResolver
	securityGroups biz.SecurityGroupPort
	environment    biz.EnvironmentPort
	native         biz.NativePort
	runtime        biz.RuntimePort
	config         Config
	media          map[string]*pb.MediaReference
	images         biz.ImageCatalogPort
	addresses      AddressReader
	relations      biz.RelationsPort
	logs           biz.LogPort
	logMu          sync.Mutex
	logTotal       int
	logTenants     map[string]int
	logVMs         map[string]int
}

func (s *Service) SetAddressReader(r AddressReader) { s.addresses = r }
func (s *Service) project(ctx context.Context, v biz.VM) (*pb.VirtualMachine, error) {
	if s.addresses != nil {
		addresses, e := s.addresses.CurrentAddresses(ctx, v)
		v.Addresses = addresses
		if e != nil {
			v.Addresses = nil
			v.Reason = "VM_DEPENDENCY_NOT_READY"
		}
	}
	return wireVM(v)
}
func NewService(r biz.Repository, c ComputeResolver, e biz.EnvironmentPort, n biz.NativePort, cfg Config) (*Service, error) {
	if r == nil || c == nil || e == nil || n == nil || !biz.UUID(cfg.ClusterID) || cfg.ServiceAccount == "" || cfg.StorageClass == "" || cfg.StorageClassUID == "" || biz.ValidateLogLimits(cfg.LogLimits) != nil {
		return nil, biz.ErrInvalid
	}
	logs, _ := n.(biz.LogPort)
	s := &Service{repo: r, compute: c, environment: e, native: n, config: cfg, media: map[string]*pb.MediaReference{}, logs: logs, logTenants: map[string]int{}, logVMs: map[string]int{}}
	if e := ValidateStaticMediaKeys(cfg.Media); e != nil {
		return nil, e
	}
	for _, m := range cfg.Media {
		if m == nil || m.MediaKey == "" || s.media[m.MediaKey] != nil {
			return nil, biz.ErrInvalid
		}
		s.media[m.MediaKey] = proto.Clone(m).(*pb.MediaReference)
	}
	return s, nil
}

// rpcStatus builds the wire status for a rejected VM request. The stable
// reason rides an errdetails.ErrorInfo detail so gateway encoders (Kratos
// errors.FromError) lift it into the public `reason` field instead of leaving
// callers to parse it out of `message`.
func rpcStatus(code codes.Code, message, reason string) error {
	st := status.New(code, message)
	if reason == "" {
		return st.Err()
	}
	if with, err := st.WithDetails(&errdetails.ErrorInfo{Reason: reason}); err == nil {
		return with.Err()
	}
	return st.Err()
}

func rpcError(e error) error {
	if e == nil {
		return nil
	}
	code := codes.Unavailable
	reason := ""
	switch {
	case errors.Is(e, biz.ErrDenied):
		code, reason = codes.PermissionDenied, biz.ErrDenied.Error()
	case errors.Is(e, biz.ErrInvalid), errors.Is(e, pb.ErrInvalidRequest):
		code, reason = codes.InvalidArgument, biz.ErrInvalid.Error()
	case errors.Is(e, biz.ErrNotFound):
		code, reason = codes.NotFound, biz.ErrNotFound.Error()
	case errors.Is(e, biz.ErrIdempotency):
		code, reason = codes.AlreadyExists, biz.ErrIdempotency.Error()
	case errors.Is(e, biz.ErrVersion):
		code, reason = codes.FailedPrecondition, biz.ErrVersion.Error()
	case errors.Is(e, biz.ErrBusy):
		code, reason = codes.FailedPrecondition, biz.ErrBusy.Error()
	case errors.Is(e, biz.ErrOwnership):
		code, reason = codes.FailedPrecondition, biz.ErrOwnership.Error()
	case errors.Is(e, biz.ErrChanged):
		code, reason = codes.FailedPrecondition, biz.ErrChanged.Error()
	case errors.Is(e, biz.ErrConsole):
		code, reason = codes.FailedPrecondition, biz.ErrConsole.Error()
	case errors.Is(e, biz.ErrNetworkAdmissionClosed):
		code, reason = codes.FailedPrecondition, biz.ErrNetworkAdmissionClosed.Error()
	case errors.Is(e, biz.ErrLogTarget):
		code, reason = codes.FailedPrecondition, biz.ErrLogTarget.Error()
	case errors.Is(e, biz.ErrCloneSourceUnavailable):
		code, reason = codes.FailedPrecondition, biz.ErrCloneSourceUnavailable.Error()
	case errors.Is(e, biz.ErrLogLimit):
		code, reason = codes.ResourceExhausted, biz.ErrLogLimit.Error()
	}
	if code == codes.Unavailable {
		// A not-ready clone source keeps its stable reason so the caller can tell
		// "retry later" from "cannot clone"; ordinary dependencies stay generic.
		if errors.Is(e, biz.ErrCloneSourceNotReady) {
			return rpcStatus(code, e.Error(), biz.ErrCloneSourceNotReady.Error())
		}
		return rpcStatus(code, "VM_DEPENDENCY_NOT_READY", "VM_DEPENDENCY_NOT_READY")
	}
	return rpcStatus(code, e.Error(), reason)
}
func (s *Service) ResolveCreateSpec(ctx context.Context, r *pb.ResolveCreateSpecRequest) (*pb.ResolveCreateSpecResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	if r.VmContractVersion != pb.ContractVersion || pb.ValidateUserIntent(r.Data) != nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	intent := proto.Clone(r.Data).(*pb.CreateVMData)
	if intent.SourceKind == "VM_CLONE" {
		return s.resolveCloneCreateSpec(ctx, r, intent)
	}
	intent.Dns, _ = pb.NormalizeDNSSelection(intent.Dns)
	dns, e := s.resolveDNS(intent.Dns)
	if e != nil {
		return nil, rpcError(e)
	}
	ns, e := s.environment.ResolveNamespace(ctx, r.TenantId, r.Namespace)
	if e != nil {
		return nil, rpcError(e)
	}
	if r.NamespaceUid != "" && r.NamespaceUid != ns.UID {
		return nil, rpcError(biz.ErrChanged)
	}
	selection, e := s.compute.ResolveComputePreset(ctx, &compute.ResolveComputePresetRequest{TenantId: r.TenantId, PresetId: r.Data.PresetId, Usage: compute.Usage_VIRTUAL_MACHINE})
	if e != nil {
		return nil, e
	}
	media, e := s.resolveMedia(ctx, r.TenantId, r.Data.MediaKey)
	if e != nil {
		return nil, rpcError(e)
	}
	if e = s.native.ValidateSource(ctx, domainSource(media)); e != nil {
		return nil, rpcError(e)
	}
	if e = s.environment.ValidateStorage(ctx, s.config.StorageClass, s.config.StorageClassUID); e != nil {
		return nil, rpcError(e)
	}
	selectionNetwork, e := s.environment.ValidateNetwork(ctx, r.TenantId, s.config.ClusterID, ns.Name, biz.NetworkSelection{VPCID: r.Data.Network.VpcId, SubnetID: r.Data.Network.SubnetId})
	if e != nil {
		return nil, rpcError(e)
	}
	if len(r.Data.SecurityGroupIds) > 0 {
		if s.securityGroups == nil {
			return nil, rpcError(biz.ErrDependency)
		}
		if _, e = s.securityGroups.ResolveSecurityGroups(ctx, r.TenantId, selectionNetwork.VPCID, ns.Name, r.Data.SecurityGroupIds); e != nil {
			return nil, rpcError(e)
		}
	}
	serviceAccount := selectionNetwork.ServiceAccount
	if serviceAccount == "" {
		serviceAccount = s.config.ServiceAccount
	}
	spec := &pb.CreateSpec{SecurityGroupIds: append([]string(nil), r.Data.SecurityGroupIds...), Intent: intent, Compute: selection, ClusterId: s.config.ClusterID, Namespace: ns.Name, NamespaceUid: ns.UID, ServiceAccount: serviceAccount, Storage: &pb.StoragePolicy{StorageClass: s.config.StorageClass, StorageClassUid: s.config.StorageClassUID, VolumeMode: "Block", AccessModes: []string{"ReadWriteOnce"}, BackendStorageClass: s.config.BackendStorageClass, MediaStorageClass: s.config.MediaStorageClass}, Media: media, Network: &pb.FrozenNetwork{VpcId: selectionNetwork.VPCID, SubnetId: selectionNetwork.SubnetID, NadName: selectionNetwork.NADName, NadUid: selectionNetwork.NADUID, NadConfigDigest: selectionNetwork.NADDigest}, Sockets: 1, Cores: uint32((selection.CpuMillis + 999) / 1000), Threads: 1, BootDevice: "SYSTEM_DISK", Dns: dns}
	if media.Kind == "INSTALL_ISO" {
		spec.BootDevice = "INSTALL_MEDIA"
	}
	if media.DriverMediaKey != "" {
		driver, e := s.resolveMedia(ctx, r.TenantId, media.DriverMediaKey)
		if e != nil {
			return nil, rpcError(e)
		}
		spec.DriverMedia = proto.Clone(driver).(*pb.MediaReference)
		if e = s.native.ValidateSource(ctx, domainSource(driver)); e != nil {
			return nil, rpcError(e)
		}
	}
	digest, e := pb.CreateSpecDigest(spec)
	if e != nil {
		return nil, rpcError(e)
	}
	return &pb.ResolveCreateSpecResponse{CreateSpec: spec, VmContractVersion: pb.ContractVersion, SelectionDigest: selection.SelectionDigest, VmCreateDigest: digest}, nil
}

// cloneSourceHeld reports whether another clone still holds this source. A
// repository without the read-only port cannot answer, which keeps resolve
// working for owner implementations that predate the clone table.
func cloneSourceHeld(ctx context.Context, r biz.Repository, tenant, source string) (bool, error) {
	sources, ok := r.(biz.CloneSourceHeldPort)
	if !ok {
		return false, nil
	}
	return sources.SourceHeld(ctx, tenant, source)
}

// resolveCloneCreateSpec freezes the accepted source business record for a
// VM_CLONE create. It never queries the current preset catalog, an image, a
// driver source or today's default DNS.
func (s *Service) resolveCloneCreateSpec(ctx context.Context, r *pb.ResolveCreateSpecRequest, intent *pb.CreateVMData) (*pb.ResolveCreateSpecResponse, error) {
	if intent.CloneSource == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	src, e := s.repo.Get(ctx, r.TenantId, intent.CloneSource.SourceVmId)
	if e != nil {
		return nil, rpcError(e)
	}
	if src.Closing || src.Version != intent.CloneSource.ExpectedSourceVersion {
		return nil, rpcError(biz.ErrVersion)
	}
	// A source another clone still holds is refused here, before the caller
	// accepts and accounts for the request. The owner re-checks the same fact
	// inside acceptance; this read only keeps the common case from being
	// accepted and then stalling without a landed target.
	if held, e := cloneSourceHeld(ctx, s.repo, r.TenantId, intent.CloneSource.SourceVmId); e != nil {
		return nil, rpcError(e)
	} else if held {
		return nil, rpcError(biz.ErrBusy)
	}
	sourceSpec, e := pb.DecodeCreateSpec(src.Spec.Canonical)
	if e != nil {
		return nil, rpcError(biz.ErrChanged)
	}
	if sourceSpec.Compute == nil || sourceSpec.Compute.CpuMillis != src.Spec.CPUMillis || sourceSpec.Compute.MemoryMib != src.Spec.MemoryMiB {
		return nil, rpcError(biz.ErrChanged)
	}
	ns, e := s.environment.ResolveNamespace(ctx, r.TenantId, r.Namespace)
	if e != nil {
		return nil, rpcError(e)
	}
	if r.NamespaceUid != "" && r.NamespaceUid != ns.UID {
		return nil, rpcError(biz.ErrChanged)
	}
	if ns.Name != src.Spec.Namespace || (src.Spec.NamespaceUID != "" && ns.UID != src.Spec.NamespaceUID) {
		return nil, rpcError(biz.ErrChanged)
	}
	storage := &pb.StoragePolicy{StorageClass: src.Spec.StorageClass, StorageClassUid: src.Spec.StorageClassUID, VolumeMode: "Block", AccessModes: []string{"ReadWriteOnce"}, BackendStorageClass: src.Spec.BackendStorageClass, MediaStorageClass: src.Spec.MediaStorageClass}
	if e = s.environment.ValidateStorage(ctx, storage.StorageClass, storage.StorageClassUid); e != nil {
		return nil, rpcError(e)
	}
	sourceNetwork := biz.NetworkSelection{VPCID: src.Spec.Network.VPCID, SubnetID: src.Spec.Network.SubnetID}
	if sourceNetwork.VPCID == "" || sourceNetwork.SubnetID == "" {
		return nil, rpcError(biz.ErrChanged)
	}
	selectionNetwork, e := s.environment.ValidateNetwork(ctx, r.TenantId, s.config.ClusterID, ns.Name, sourceNetwork)
	if e != nil {
		return nil, rpcError(e)
	}
	if len(src.SecurityGroupIDs) > 0 {
		if s.securityGroups == nil {
			return nil, rpcError(biz.ErrDependency)
		}
		if _, e = s.securityGroups.ResolveSecurityGroups(ctx, r.TenantId, selectionNetwork.VPCID, ns.Name, src.SecurityGroupIDs); e != nil {
			return nil, rpcError(e)
		}
	}
	serviceAccount := selectionNetwork.ServiceAccount
	if serviceAccount == "" {
		serviceAccount = s.config.ServiceAccount
	}
	plan := make([]*pb.VMCloneDisk, 0, len(src.Disks))
	for _, d := range src.Disks {
		if d.Tombstone || !d.Attached {
			continue
		}
		if d.Role != "system" && d.Role != "data" {
			return nil, rpcError(biz.ErrChanged)
		}
		if d.PVC.Name == "" || d.PVC.UID == "" {
			return nil, rpcError(biz.ErrDependency)
		}
		plan = append(plan, &pb.VMCloneDisk{SourceDiskId: d.ID, VolumeName: d.VolumeName, Role: d.Role, Name: d.Name, SizeGb: d.SizeGB})
	}
	if len(plan) == 0 {
		return nil, rpcError(biz.ErrChanged)
	}
	var dns *pb.FrozenDNS
	if src.Spec.DNS != nil {
		dns = &pb.FrozenDNS{Mode: src.Spec.DNS.Mode, Nameservers: append([]string(nil), src.Spec.DNS.Nameservers...)}
	}
	media := &pb.MediaReference{OsFamily: src.Spec.Media.OSFamily, OsVersion: src.Spec.Media.OSVersion, Architecture: src.Spec.Media.Architecture, Firmware: src.Spec.Media.Firmware, RootBus: src.Spec.Media.RootBus, DataBus: src.Spec.Media.DataBus, NicModel: src.Spec.Media.NICModel, PersistentTpm: src.Spec.Media.PersistentTPM, PersistentUefi: src.Spec.Media.PersistentUEFI}
	sockets := sourceSpec.Sockets
	if sockets == 0 {
		sockets = 1
	}
	threads := sourceSpec.Threads
	if threads == 0 {
		threads = 1
	}
	cores := src.Spec.Cores
	if cores == 0 {
		cores = uint32((sourceSpec.Compute.CpuMillis + 999) / 1000)
	}
	spec := &pb.CreateSpec{
		SecurityGroupIds: append([]string(nil), src.SecurityGroupIDs...),
		Intent:           intent,
		Compute:          sourceSpec.Compute,
		ClusterId:        s.config.ClusterID,
		Namespace:        ns.Name,
		NamespaceUid:     ns.UID,
		ServiceAccount:   serviceAccount,
		Storage:          storage,
		Media:            media,
		Network:          &pb.FrozenNetwork{VpcId: selectionNetwork.VPCID, SubnetId: selectionNetwork.SubnetID, NadName: selectionNetwork.NADName, NadUid: selectionNetwork.NADUID, NadConfigDigest: selectionNetwork.NADDigest},
		Sockets:          sockets,
		Cores:            cores,
		Threads:          threads,
		BootDevice:       "SYSTEM_DISK",
		Dns:              dns,
		CloneSource:      &pb.FrozenVMCloneSource{SourceVmId: src.ID, SourceName: src.Name, SourceVersion: src.Version, SourceNativeName: src.Native.Name, SourceNativeUid: src.Native.UID, Disks: plan},
	}
	eligibilitySource := biz.VM{Spec: biz.Spec{Namespace: ns.Name, CloneSource: &biz.CloneSource{SourceVMID: src.ID, SourceVersion: src.Version, Native: src.Native}}}
	for _, d := range plan {
		eligibilitySource.Spec.CloneSource.Disks = append(eligibilitySource.Spec.CloneSource.Disks, biz.CloneDisk{SourceDiskID: d.SourceDiskId, VolumeName: d.VolumeName, Role: d.Role, Name: d.Name, SizeGB: d.SizeGb})
	}
	eligibility, ok := s.native.(biz.CloneNativePort)
	if !ok {
		return nil, rpcError(biz.ErrDependency)
	}
	result, e := eligibility.ObserveSourceCloneEligibility(ctx, eligibilitySource, src)
	if e != nil {
		return nil, rpcError(e)
	}
	// Structural rejection is a stable failed precondition; a dependency that is
	// simply not observed yet stays retryable.
	if len(result.Unsupported) > 0 {
		return nil, rpcError(result.UnsupportedError())
	}
	if len(result.NotReady) > 0 {
		return nil, rpcError(result.NotReadyError())
	}
	digest, e := pb.CreateSpecDigest(spec)
	if e != nil {
		return nil, rpcError(e)
	}
	return &pb.ResolveCreateSpecResponse{CreateSpec: spec, VmContractVersion: pb.ContractVersion, SelectionDigest: sourceSpec.Compute.SelectionDigest, VmCreateDigest: digest}, nil
}

func domainSource(m *pb.MediaReference) biz.Source {
	if m == nil {
		return biz.Source{}
	}
	return biz.Source{Key: m.MediaKey, Kind: m.Kind, ClusterID: m.ClusterId, Namespace: m.Namespace, PVCName: m.PvcName, PVCUID: m.PvcUid, Checksum: m.Checksum, VolumeMode: m.VolumeMode, ContentType: m.ContentType, VirtualBytes: m.VirtualCapacityBytes, RequestedBytes: m.RequestedBytes, OSFamily: m.OsFamily, OSVersion: m.OsVersion, Architecture: m.Architecture, Firmware: m.Firmware, RootBus: m.RootBus, DataBus: m.DataBus, NICModel: m.NicModel, PersistentTPM: m.PersistentTpm, PersistentUEFI: m.PersistentUefi}
}
func domainSpec(vmid string, p *pb.CreateSpec) (biz.Spec, error) {
	raw, e := pb.CanonicalCreateSpec(p)
	if e != nil {
		return biz.Spec{}, e
	}
	digest, e := pb.CreateSpecDigest(p)
	if e != nil {
		return biz.Spec{}, e
	}
	request, e := pb.RequestDigest(p.Intent)
	if e != nil {
		return biz.Spec{}, e
	}
	s := biz.Spec{SecurityGroupIDs: append([]string(nil), p.SecurityGroupIds...), Canonical: raw, Digest: digest, RequestDigest: request, Name: p.Intent.Name, PresetID: p.Compute.PresetId, SelectionDigest: p.Compute.SelectionDigest, ClusterID: p.ClusterId, Namespace: p.Namespace, NamespaceUID: p.NamespaceUid, ServiceAccount: p.ServiceAccount, StorageClass: p.Storage.StorageClass, StorageClassUID: p.Storage.StorageClassUid, BackendStorageClass: p.Storage.BackendStorageClass, MediaStorageClass: p.Storage.MediaStorageClass, CPUMillis: p.Compute.CpuMillis, MemoryMiB: p.Compute.MemoryMib, Cores: p.Cores, Media: domainSource(p.Media), Network: biz.NetworkSelection{VPCID: p.Network.VpcId, SubnetID: p.Network.SubnetId, NADName: p.Network.NadName, NADUID: p.Network.NadUid, NADDigest: p.Network.NadConfigDigest}, BootDevice: p.BootDevice, StartAfterCreate: p.Intent.GetStartAfterCreate()}
	if p.DriverMedia != nil {
		d := domainSource(p.DriverMedia)
		s.Driver = &d
	}
	if p.Dns != nil {
		s.DNS = &biz.DNS{Mode: p.Dns.Mode, Nameservers: append([]string(nil), p.Dns.Nameservers...)}
	}
	if g := p.Intent.GuestInitialization; g != nil {
		s.GuestInitialization = &biz.GuestInitialization{Method: g.Method, Username: g.Username, SSHPublicKey: g.SshPublicKey, PasswordSet: g.PasswordSet, CredentialIntentDigest: g.CredentialIntentDigest}
	}
	if p.Intent.SourceKind == "VM_CLONE" {
		if p.CloneSource == nil {
			return biz.Spec{}, pb.ErrInvalidRequest
		}
		clone := &biz.CloneSource{SourceVMID: p.CloneSource.SourceVmId, SourceName: p.CloneSource.SourceName, SourceVersion: p.CloneSource.SourceVersion, Native: biz.ObjectRef{Kind: "VirtualMachine", Namespace: p.Namespace, Name: p.CloneSource.SourceNativeName, UID: p.CloneSource.SourceNativeUid}}
		for _, d := range p.CloneSource.Disks {
			clone.Disks = append(clone.Disks, biz.CloneDisk{SourceDiskID: d.SourceDiskId, VolumeName: d.VolumeName, Role: d.Role, Name: d.Name, SizeGB: d.SizeGb})
		}
		s.CloneSource = clone
		// The target disks come from the frozen source plan, never from the empty
		// user size inputs, and never with a planned DataVolume name.
		s.InitialDisks = make([]biz.Disk, 0, len(clone.Disks))
		for _, d := range clone.Disks {
			disk := biz.NewCloneTargetDisk(vmid, d)
			disk.DV.Namespace = p.Namespace
			s.InitialDisks = append(s.InitialDisks, disk)
		}
		return s, nil
	}
	s.InitialDisks = []biz.Disk{biz.NewInitialDisk(vmid, "system", "system", "system", p.Intent.SystemDiskSizeGb)}
	for _, d := range p.Intent.DataDisks {
		s.InitialDisks = append(s.InitialDisks, biz.NewInitialDisk(vmid, d.ClientDiskKey, d.Name, "data", d.SizeGb))
	}
	for i := range s.InitialDisks {
		s.InitialDisks[i].DV.Namespace = p.Namespace
	}
	return s, nil
}
func (s *Service) AcceptCreate(ctx context.Context, r *pb.AcceptCreateRequest) (*pb.Acceptance, error) {
	if r == nil || r.Context == nil || r.VmContractVersion != pb.ContractVersion {
		return nil, rpcError(biz.ErrInvalid)
	}
	c := r.Context
	if e := biz.RequireTenant(ctx, c.TenantId); e != nil {
		return nil, rpcError(e)
	}
	spec, e := domainSpec(c.ResourceId, r.CreateSpec)
	if e != nil {
		return nil, rpcError(e)
	}
	if spec.Digest != r.VmCreateDigest || spec.RequestDigest != c.RequestDigest {
		return nil, rpcError(biz.ErrIdempotency)
	}
	if e = pb.ValidateGuestCredentials(r.CreateSpec, r.GuestCredentials); e != nil {
		return nil, rpcError(e)
	}
	password := ""
	if r.GuestCredentials != nil {
		password = r.GuestCredentials.Password
	}
	o, e := s.repo.AcceptCreate(ctx, biz.Acceptance{TenantID: c.TenantId, VMID: c.ResourceId, OperationID: c.OperationId, RequestDigest: c.RequestDigest, Spec: spec, Password: password})
	if e != nil {
		return nil, rpcError(e)
	}
	return &pb.Acceptance{OperationId: o.ID, ResourceId: o.VMID, Accepted: true}, nil
}
func (s *Service) AcceptDelete(ctx context.Context, r *pb.AcceptDeleteRequest) (*pb.Acceptance, error) {
	if r == nil || r.Context == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	c := r.Context
	if e := biz.RequireTenant(ctx, c.TenantId); e != nil {
		return nil, rpcError(e)
	}
	d, e := pb.DeleteRequestDigest(r)
	if e != nil {
		return nil, rpcError(e)
	}
	if d != c.RequestDigest {
		return nil, rpcError(biz.ErrIdempotency)
	}
	o, e := s.repo.AcceptDelete(ctx, biz.Acceptance{TenantID: c.TenantId, VMID: c.ResourceId, OperationID: c.OperationId, RequestDigest: c.RequestDigest, CreateOperationID: r.CreateOperationId, ExpectedVersion: r.ExpectedVersion})
	if e != nil {
		return nil, rpcError(e)
	}
	return &pb.Acceptance{OperationId: o.ID, ResourceId: o.VMID, Accepted: true}, nil
}
func (s *Service) GetVM(ctx context.Context, r *pb.GetVMRequest) (*pb.VirtualMachine, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	v, e := s.repo.Get(ctx, r.TenantId, r.VmId)
	if e != nil {
		return nil, rpcError(e)
	}
	return s.projectDetail(ctx, v, r.IncludeRuntime == nil || r.GetIncludeRuntime())
}
func wireVM(v biz.VM) (*pb.VirtualMachine, error) {
	spec, e := pb.DecodeCreateSpec(v.Spec.Canonical)
	queryReason := ""
	if e != nil {
		// A stored, already accepted spec is a read projection, not a new
		// admission request. A newer writer may have fields or source kinds
		// this reader does not implement. Project known fields without
		// rewriting the original canonical bytes or weakening command validation.
		queryReason = "VM_STORED_SPEC_COMPATIBILITY"
		spec = new(pb.CreateSpec)
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(v.Spec.Canonical, spec); err != nil {
			// Keep the row's identity/state visible even when a known field has
			// an unreadable type; never present a partially decoded frozen spec.
			spec = nil
			queryReason = "VM_STORED_SPEC_UNREADABLE"
		}
		// Do not log stored payloads or parser errors which can quote their values.
		slog.Warn("vm stored spec query projection", "tenant_id", v.TenantID, "vm_id", v.ID, "reason", queryReason)
	}
	out := &pb.VirtualMachine{SecurityGroupIds: append([]string(nil), v.SecurityGroupIDs...), Id: v.ID, Name: v.Name, Version: v.Version, OsFamily: v.Spec.Media.OSFamily, OsVersion: v.Spec.Media.OSVersion, FrozenSpec: spec, DesiredState: v.Desired, ObservedState: v.Observed, Reason: v.Reason, ObservedAt: observed(v.ObservedAt), LastOperationId: v.LastOperationID, Closing: v.Closing, BootDevice: v.Spec.BootDevice, CreatedAt: observed(v.CreatedAt)}
	if out.Reason == "" {
		out.Reason = queryReason
	}
	if !v.SecurityGroupsInitialized {
		out.SecurityGroupIds = append([]string(nil), v.Spec.SecurityGroupIDs...)
	}
	out.Disks = wireDisks(v)
	for _, a := range v.Addresses {
		out.Addresses = append(out.Addresses, &pb.CurrentAddress{NicName: a.NIC, Address: a.Address, ObservedAt: observed(a.ObservedAt)})
	}
	wireMedia(v, out)
	return out, nil
}

// wireDisks projects the persisted disk inventory only. Disk reads must never
// depend on spec decoding, relations or guest runtime collection.
func wireDisks(v biz.VM) []*pb.Disk {
	var out []*pb.Disk
	for _, d := range v.Disks {
		if !d.Tombstone {
			out = append(out, &pb.Disk{DiskId: d.ID, Role: d.Role, Name: d.Name, SizeGb: d.SizeGB, RequestedBytes: d.RequestedBytes, ObservedCapacityBytes: d.ObservedCapacityBytes, VolumeName: d.VolumeName, Serial: d.Serial, Attached: d.Attached, PreparationState: d.Preparation, Reason: d.Reason})
		}
	}
	return out
}
func observed(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
func wireOp(o biz.Operation) *pb.Operation {
	return &pb.Operation{Id: o.ID, VmId: o.VMID, Action: o.Action, IntentVersion: o.IntentVersion, State: o.State, Reason: o.Reason, ObservedAt: observed(o.ObservedAt)}
}
func (s *Service) ListVMs(ctx context.Context, r *pb.ListVMsRequest) (*pb.ListVMsResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	all, total, e := s.repo.List(ctx, r.TenantId, biz.ListFilter{Page: r.Page, PageSize: r.PageSize, State: r.State, Keyword: r.Keyword})
	if e != nil {
		return nil, rpcError(e)
	}
	out := &pb.ListVMsResponse{Total: total}
	for _, v := range all {
		x, e := s.project(ctx, v)
		if e != nil {
			return nil, e
		}
		out.Items = append(out.Items, x)
	}
	return out, nil
}
func (s *Service) GetOperation(ctx context.Context, r *pb.GetOperationRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	o, e := s.repo.GetOperation(ctx, r.TenantId, r.VmId, r.OperationId)
	if e != nil {
		return nil, rpcError(e)
	}
	return wireOp(o), nil
}
func (s *Service) LookupCommandReplay(ctx context.Context, r *pb.LookupCommandReplayRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	if !validAction(r.Action) {
		return nil, rpcError(biz.ErrInvalid)
	}
	o, e := s.repo.LookupReplay(ctx, biz.Command{TenantID: r.TenantId, VMID: r.VmId, Action: r.Action, Key: r.IdempotencyKey, RequestDigest: pb.Digest(r.CanonicalIntent)})
	if e != nil {
		return nil, rpcError(e)
	}
	return wireOp(o), nil
}
func validAction(a string) bool {
	switch a {
	case "start", "stop", "restart", "force-stop", "create-disk", "attach-disk", "detach-disk", "delete-disk", "set-media", "eject-media", "set-boot", "create-snapshot", "delete-snapshot", "restore-snapshot", "set-security-groups", "set-network-interface":
		return true
	}
	return false
}

// Preflight errors keep their public contract; operator logs identify the
// failed dependency without copying provider messages or the frozen VM spec.
func commandPreflightError(ctx context.Context, c biz.Command, stage string, err error) error {
	reason := "VM_DEPENDENCY_NOT_READY"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		reason = "DEADLINE_EXCEEDED"
	case errors.Is(err, context.Canceled):
		reason = "CANCELED"
	case errors.Is(err, biz.ErrChanged):
		reason = "VM_DEPENDENCY_CHANGED"
	case errors.Is(err, biz.ErrOwnership):
		reason = "VM_OBJECT_OWNERSHIP_CONFLICT"
	case errors.Is(err, biz.ErrNotFound):
		reason = "VM_NOT_FOUND"
	case errors.Is(err, biz.ErrInvalid):
		reason = "VM_INVALID_REQUEST"
	}
	slog.WarnContext(ctx, "VM command preflight failed", "tenant_id", c.TenantID, "vm_id", c.VMID, "action", c.Action, "stage", stage, "reason", reason)
	return rpcError(err)
}

func (s *Service) command(ctx context.Context, m proto.Message, action string) (*pb.Operation, error) {
	x := m.ProtoReflect()
	tenant := x.Get(x.Descriptor().Fields().ByName("tenant_id")).String()
	if e := biz.RequireTenant(ctx, tenant); e != nil {
		return nil, rpcError(e)
	}
	raw, e := pb.CanonicalCommandIntent(m)
	if e != nil {
		return nil, rpcError(e)
	}
	c := biz.Command{TenantID: tenant, VMID: x.Get(x.Descriptor().Fields().ByName("vm_id")).String(), Action: action, Key: x.Get(x.Descriptor().Fields().ByName("idempotency_key")).String(), ExpectedVersion: x.Get(x.Descriptor().Fields().ByName("expected_version")).Int(), Intent: raw, RequestDigest: pb.Digest(raw)}
	if !biz.UUID(c.VMID) || c.Key == "" || len(c.Key) > 256 {
		return nil, rpcError(biz.ErrInvalid)
	}
	if o, e := s.repo.LookupReplay(ctx, c); !errors.Is(e, biz.ErrNotFound) {
		if e != nil {
			return nil, rpcError(e)
		}
		return wireOp(o), nil
	}
	if action == "set-security-groups" {
		if err := s.validateSecurityGroups(ctx, c, m.(*pb.SetSecurityGroupsRequest).SecurityGroupIds); err != nil {
			return nil, rpcError(err)
		}
	}
	if action == "force-stop" && !m.(*pb.ForceStopRequest).DestructiveConfirmation {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e = s.validateSnapshotCommand(ctx, c); e != nil {
		return nil, rpcError(e)
	}
	if action == "create-disk" || action == "attach-disk" || action == "detach-disk" || action == "delete-disk" {
		v, e := s.repo.Get(ctx, c.TenantID, c.VMID)
		if e != nil {
			return nil, rpcError(e)
		}
		if e = biz.ValidateDiskCommand(v, action, raw); e != nil {
			return nil, rpcError(e)
		}
		if action != "create-disk" {
			intent, _ := biz.ParseDiskIntent(raw)
			i, _ := biz.FindDisk(v, intent.DiskID)
			n, ok := s.native.(biz.DiskNativePort)
			if !ok {
				return nil, rpcError(biz.ErrDependency)
			}
			if action == "delete-disk" {
				clear, e := n.DiskUnreferenced(ctx, v, v.Disks[i])
				if e != nil {
					return nil, rpcError(e)
				}
				if !clear {
					return nil, rpcError(biz.ErrBusy)
				}
			} else {
				observed, e := n.ValidateDiskAttachment(ctx, v, v.Disks[i], action == "attach-disk")
				if e != nil {
					return nil, rpcError(e)
				}
				// Reuse the observation the validation already paid for instead of
				// issuing a second full native observation on the accept path.
				if action == "detach-disk" && observed.Running && !intent.GuestPrepared {
					return nil, rpcError(biz.ErrInvalid)
				}
			}
		}
	}
	if action == "start" || action == "restart" {
		v, e := s.repo.Get(ctx, c.TenantID, c.VMID)
		if e != nil {
			return nil, rpcError(e)
		}
		ns, e := s.environment.ResolveNamespace(ctx, c.TenantID, v.Spec.Namespace)
		if e != nil {
			return nil, commandPreflightError(ctx, c, "namespace", e)
		}
		if ns.UID != v.Spec.NamespaceUID {
			return nil, commandPreflightError(ctx, c, "namespace_identity", biz.ErrChanged)
		}
		if e := s.environment.ValidateExistingNetwork(ctx, v); e != nil {
			stage := "network"
			if errors.Is(e, biz.ErrChanged) || errors.Is(e, biz.ErrOwnership) {
				stage = "network_identity"
			}
			return nil, commandPreflightError(ctx, c, stage, e)
		}
	}
	o, e := s.repo.AcceptCommand(ctx, c)
	if e != nil {
		return nil, rpcError(e)
	}
	return wireOp(o), nil
}
func (s *Service) Start(ctx context.Context, r *pb.ChangeVMRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.command(ctx, r, "start")
}
func (s *Service) Stop(ctx context.Context, r *pb.ChangeVMRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.command(ctx, r, "stop")
}
func (s *Service) Restart(ctx context.Context, r *pb.ChangeVMRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.command(ctx, r, "restart")
}
func (s *Service) ForceStop(ctx context.Context, r *pb.ForceStopRequest) (*pb.Operation, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	return s.command(ctx, r, "force-stop")
}
func (s *Service) ListDisks(ctx context.Context, r *pb.GetVMRequest) (*pb.ListDisksResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	// Disks are a persisted projection. Reusing GetVM here used to pay the full
	// detail projection (relations plus guest runtime) for data it never reads.
	v, e := s.repo.Get(ctx, r.TenantId, r.VmId)
	if e != nil {
		return nil, rpcError(e)
	}
	return &pb.ListDisksResponse{Items: wireDisks(v)}, nil
}
func (s *Service) ListMedia(ctx context.Context, r *pb.ListMediaRequest) (*pb.ListMediaResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	out := &pb.ListMediaResponse{}
	for _, m := range s.config.Media {
		out.Items = append(out.Items, proto.Clone(m).(*pb.MediaReference))
	}
	// Tenant-uploaded READY images join the same picker with no contract change:
	// the response keeps its shape, ordering of static entries stays first.
	dynamic, e := s.dynamicMedia(ctx, r.TenantId)
	if e != nil {
		return nil, rpcError(e)
	}
	out.Items = append(out.Items, dynamic...)
	return out, nil
}
func (s *Service) resolveConsole(ctx context.Context, tenant, id string, version int64) (*console.VMConsoleBinding, error) {
	v, e := s.repo.Get(ctx, tenant, id)
	if e != nil {
		return nil, e
	}
	if v.Closing || v.Version != version || version < 1 {
		return nil, biz.ErrConsole
	}
	ns, e := s.environment.ResolveNamespace(ctx, tenant, v.Spec.Namespace)
	if e != nil {
		return nil, e
	}
	if ns.UID != v.Spec.NamespaceUID {
		return nil, biz.ErrConsole
	}
	if v.LastOperationID != "" {
		o, e := s.repo.GetOperation(ctx, tenant, id, v.LastOperationID)
		if e != nil {
			return nil, e
		}
		if o.State != "succeeded" && o.State != "failed" && (o.Action == "stop" || o.Action == "force-stop" || o.Action == "restart" || o.Action == "restore-snapshot" || o.Action == biz.DeleteAction) {
			return nil, biz.ErrConsole
		}
	}
	var x biz.Observation
	if native, ok := s.native.(biz.ConsoleNativePort); ok {
		x, e = native.ObserveConsole(ctx, v)
	} else {
		x, e = s.native.Observe(ctx, v)
	}
	if e != nil {
		return nil, e
	}
	if !x.Running || x.VMI == nil || x.VM.UID != v.Native.UID {
		return nil, biz.ErrConsole
	}
	current, e := s.repo.Get(ctx, tenant, id)
	if e != nil {
		return nil, e
	}
	if current.Closing || current.Version != version || current.Native.UID != v.Native.UID {
		return nil, biz.ErrConsole
	}
	return &console.VMConsoleBinding{TenantId: tenant, VmId: id, Version: version, ClusterId: v.Spec.ClusterID, Namespace: v.Spec.Namespace, NamespaceUid: v.Spec.NamespaceUID, VmName: v.Native.Name, VmUid: v.Native.UID, VmiName: x.VMI.Name, VmiUid: x.VMI.UID}, nil
}
func (s *Service) ResolveConsoleTarget(ctx context.Context, r *pb.ResolveConsoleTargetRequest) (*console.VMConsoleBinding, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return nil, rpcError(e)
	}
	v, e := s.resolveConsole(ctx, r.TenantId, r.VmId, r.ExpectedVersion)
	return v, rpcError(e)
}
func (s *Service) ValidateConsoleSession(ctx context.Context, r *console.VMConsoleBinding) (*console.ConsoleSessionValidation, error) {
	if r == nil || !biz.UUID(r.TenantId) || !biz.UUID(r.VmId) || len(r.ProtoReflect().GetUnknown()) != 0 {
		return nil, rpcError(biz.ErrConsole)
	}
	for _, v := range []string{r.ClusterId, r.Namespace, r.NamespaceUid, r.VmName, r.VmUid, r.VmiName, r.VmiUid} {
		if strings.TrimSpace(v) == "" {
			return nil, rpcError(biz.ErrConsole)
		}
	}
	current, e := s.resolveConsole(ctx, r.TenantId, r.VmId, r.Version)
	if e != nil {
		return nil, rpcError(e)
	}
	if !proto.Equal(current, r) {
		return nil, rpcError(biz.ErrConsole)
	}
	return &console.ConsoleSessionValidation{Valid: true}, nil
}

func (s *Service) acquireLog(v biz.VM) (func(), error) {
	limits := s.config.LogLimits.Defaults()
	key := v.TenantID + ":" + v.ID
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logTotal >= limits.ProcessConnections || s.logTenants[v.TenantID] >= limits.TenantConnections || s.logVMs[key] >= limits.InstanceConnections {
		return nil, biz.ErrLogLimit
	}
	s.logTotal++
	s.logTenants[v.TenantID]++
	s.logVMs[key]++
	return func() {
		s.logMu.Lock()
		defer s.logMu.Unlock()
		s.logTotal--
		s.logTenants[v.TenantID]--
		s.logVMs[key]--
		if s.logTenants[v.TenantID] == 0 {
			delete(s.logTenants, v.TenantID)
		}
		if s.logVMs[key] == 0 {
			delete(s.logVMs, key)
		}
	}, nil
}

// StreamVMLogs serves the guest serial console of the running launcher. A VM
// that is not running yields an empty stream (READY then END EOF). The
// caller's lifetime owns the stream; frames are only released after READY.
func (s *Service) StreamVMLogs(r *pb.StreamVMLogsRequest, stream grpc.ServerStreamingServer[pb.VMLogFrame]) error {
	if r == nil || r.TailLines < 1 || r.TailLines > 1000 {
		return rpcError(biz.ErrInvalid)
	}
	ctx := stream.Context()
	if e := biz.RequireTenant(ctx, r.TenantId); e != nil {
		return rpcError(e)
	}
	if !biz.UUID(r.VmId) {
		return rpcError(biz.ErrInvalid)
	}
	v, e := s.repo.Get(ctx, r.TenantId, r.VmId)
	if e != nil {
		return rpcError(e)
	}
	if v.TenantID != r.TenantId || v.ID != r.VmId {
		return rpcError(biz.ErrDenied)
	}
	if v.Spec.ClusterID != s.config.ClusterID || s.logs == nil {
		return rpcError(biz.ErrLogTarget)
	}
	release, e := s.acquireLog(v)
	if e != nil {
		return rpcError(e)
	}
	defer release()
	var x biz.Observation
	if native, ok := s.native.(biz.ConsoleNativePort); ok {
		x, e = native.ObserveConsole(ctx, v)
	} else {
		x, e = s.native.Observe(ctx, v)
	}
	if e != nil {
		return rpcError(e)
	}
	limits := s.config.LogLimits.Defaults()
	emit := func(frame biz.LogFrame) error {
		if len(frame.Data) > limits.FrameBytes {
			return biz.ErrInvalid
		}
		out := &pb.VMLogFrame{Kind: pb.VMLogFrame_Kind(pb.VMLogFrame_Kind_value[frame.Kind]), Data: frame.Data, Reason: frame.Reason}
		done := make(chan error, 1)
		go func() { done <- stream.Send(out) }()
		timer := time.NewTimer(time.Duration(limits.WriteTimeoutSeconds) * time.Second)
		defer timer.Stop()
		select {
		case sendErr := <-done:
			return sendErr
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-timer.C:
			return status.Error(codes.DeadlineExceeded, "VM_LOG_WRITE_TIMEOUT")
		}
	}
	if x.VM.UID != v.Native.UID || (x.Running && x.VMI == nil) {
		return rpcError(biz.ErrLogTarget)
	}
	// A VM that is not running (stopped, starting or shutting down) has no
	// console to read: end the stream empty instead of failing the request.
	if !x.Running {
		if e = emit(biz.LogFrame{Kind: "READY"}); e != nil {
			return e
		}
		return emit(biz.LogFrame{Kind: "END", Reason: "EOF"})
	}
	call, cancel := context.WithTimeout(ctx, time.Duration(limits.LifetimeSeconds)*time.Second)
	defer cancel()
	return s.logs.StreamLogs(call, v, x, r.TailLines, r.Follow, emit)
}
