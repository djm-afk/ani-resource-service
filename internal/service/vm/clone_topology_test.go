package vm

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/api/compute/v1"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
)

// cloneSourceFrozenSpec builds the frozen DISK_IMAGE spec that every accepted
// source VM stores in Spec.Canonical.
func cloneSourceFrozenSpec(t *testing.T, sockets, threads uint32) *pb.CreateSpec {
	t.Helper()
	start := false
	preset, cluster := uuid.NewString(), uuid.NewString()
	selection := &compute.ComputeSelection{SchemaVersion: 1, PresetId: preset, Code: "cpu-test", Name: "CPU test", ComputeType: "cpu", CpuMillis: 2000, MemoryMib: 2048}
	digest, err := compute.SelectionDigest(selection)
	if err != nil {
		t.Fatal(err)
	}
	selection.SelectionDigest = digest
	vpc, subnet := "vpc_"+strings.ReplaceAll(uuid.NewString(), "-", ""), "subnet_"+strings.ReplaceAll(uuid.NewString(), "-", "")
	return &pb.CreateSpec{
		Intent: &pb.CreateVMData{Name: "source", PresetId: preset, SourceKind: "DISK_IMAGE", MediaKey: "media-key", SystemDiskSizeGb: 20,
			Network: &pb.NetworkSelection{VpcId: vpc, SubnetId: subnet}, StartAfterCreate: &start},
		Compute: selection, ClusterId: cluster, Namespace: "tenant-ns", NamespaceUid: "namespace-uid", ServiceAccount: "default",
		Storage: &pb.StoragePolicy{StorageClass: "block", StorageClassUid: uuid.NewString(), VolumeMode: "Block", AccessModes: []string{"ReadWriteOnce"}},
		Media: &pb.MediaReference{MediaKey: "media-key", Kind: "DISK_IMAGE", ClusterId: cluster, Namespace: "image-ns",
			PvcName: "image-pvc", PvcUid: "image-uid", Checksum: strings.Repeat("a", 64), VolumeMode: "Block", ContentType: "kubevirt",
			VirtualCapacityBytes: 1, RequestedBytes: 1, OsFamily: "linux", OsVersion: "22.04", Architecture: "amd64",
			Firmware: "bios", RootBus: "virtio", DataBus: "scsi", NicModel: "virtio"},
		Network: &pb.FrozenNetwork{VpcId: vpc, SubnetId: subnet, NadName: "main", NadConfigDigest: strings.Repeat("c", 64)},
		Sockets: sockets, Cores: 2, Threads: threads, BootDevice: "SYSTEM_DISK",
	}
}

// TestCloneSourceTopologyIsFrozenSingleSocket records why the clone resolve path
// needs no extra topology guard: an accepted source VM stores only a canonical
// spec whose Sockets and Threads are both 1, so the inherited values are single
// by construction. If that contract ever widens, this test fails and the clone
// branch must reject the source itself.
func TestCloneSourceTopologyIsFrozenSingleSocket(t *testing.T) {
	if _, err := pb.CanonicalCreateSpec(cloneSourceFrozenSpec(t, 1, 1)); err != nil {
		t.Fatalf("single-socket source spec must be accepted: %v", err)
	}
	for _, test := range []struct {
		name             string
		sockets, threads uint32
	}{
		{name: "multi socket", sockets: 2, threads: 1},
		{name: "multi thread", sockets: 1, threads: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := pb.CanonicalCreateSpec(cloneSourceFrozenSpec(t, test.sockets, test.threads)); !errors.Is(err, pb.ErrInvalidRequest) {
				t.Fatalf("multi-socket source spec = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

// TestCloneTargetSpecRequiresSingleSocket pins the target-side contract the
// frozen inheritance relies on.
func TestCloneTargetSpecRequiresSingleSocket(t *testing.T) {
	spec := cloneSourceFrozenSpec(t, 1, 1)
	sourceID := uuid.NewString()
	spec.Intent = &pb.CreateVMData{Name: "target", SourceKind: "VM_CLONE",
		CloneSource: &pb.VMCloneSource{SourceVmId: sourceID, ExpectedSourceVersion: 1}}
	spec.Media = &pb.MediaReference{OsFamily: "linux", OsVersion: "22.04", Architecture: "amd64",
		Firmware: "bios", RootBus: "virtio", DataBus: "scsi", NicModel: "virtio"}
	spec.CloneSource = &pb.FrozenVMCloneSource{SourceVmId: sourceID, SourceName: "source", SourceVersion: 1,
		SourceNativeName: "vm-source", SourceNativeUid: "native-uid",
		Disks: []*pb.VMCloneDisk{{SourceDiskId: uuid.NewString(), VolumeName: "disk-system", Role: "system", Name: "system", SizeGb: 20}}}
	if err := pb.ValidateCreateSpec(spec); err != nil {
		t.Fatalf("single-socket clone spec must pass: %v", err)
	}
	spec.Sockets = 2
	if err := pb.ValidateCreateSpec(spec); !errors.Is(err, pb.ErrInvalidRequest) {
		t.Fatalf("multi-socket clone spec = %v, want ErrInvalidRequest", err)
	}
}
