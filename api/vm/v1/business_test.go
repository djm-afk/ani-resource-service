package vmv1

import (
	"bytes"
	"strings"
	"testing"

	"github.com/google/uuid"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/api/compute/v1"
	"google.golang.org/protobuf/proto"
)

func fixedSpec(t *testing.T) *CreateSpec {
	t.Helper()
	start := true
	c := &compute.ComputeSelection{SchemaVersion: 1, PresetId: uuid.NewString(), Code: "cpu-small", Name: "CPU small", ComputeType: "cpu", CpuMillis: 1500, MemoryMib: 512}
	d, e := compute.SelectionDigest(c)
	if e != nil {
		t.Fatal(e)
	}
	c.SelectionDigest = d
	cluster := uuid.NewString()
	vpc, subnet := "vpc_"+strings.ReplaceAll(uuid.NewString(), "-", ""), "subnet_"+strings.ReplaceAll(uuid.NewString(), "-", "")
	return &CreateSpec{Intent: &CreateVMData{Name: "Linux", PresetId: c.PresetId, SourceKind: "DISK_IMAGE", MediaKey: "ubuntu-24.04", SystemDiskSizeGb: 8, Network: &NetworkSelection{VpcId: vpc, SubnetId: subnet}, StartAfterCreate: &start}, Compute: c, ClusterId: cluster, Namespace: "tenant-test", NamespaceUid: uuid.NewString(), ServiceAccount: "default", Storage: &StoragePolicy{StorageClass: "ani-block", StorageClassUid: uuid.NewString(), VolumeMode: "Block", AccessModes: []string{"ReadWriteOnce"}}, Media: &MediaReference{MediaKey: "ubuntu-24.04", Kind: "DISK_IMAGE", ClusterId: cluster, Namespace: "vm-media", PvcName: "ubuntu", PvcUid: uuid.NewString(), Checksum: Digest([]byte("fixed-source")), VolumeMode: "Block", ContentType: "kubevirt", VirtualCapacityBytes: 3758096384, RequestedBytes: 8 * BytesPerGB, OsFamily: "linux", OsVersion: "24.04", Architecture: "amd64", Firmware: "bios", RootBus: "virtio", DataBus: "scsi", NicModel: "virtio"}, Network: &FrozenNetwork{VpcId: vpc, SubnetId: subnet, NadName: "main", NadUid: uuid.NewString(), NadConfigDigest: Digest([]byte("managed-nad"))}, Sockets: 1, Cores: 2, Threads: 1, BootDevice: "SYSTEM_DISK"}
}
func TestFrozenSpecCanonicalRoundTripAndUserDigest(t *testing.T) {
	s := fixedSpec(t)
	b, e := CanonicalCreateSpec(s)
	if e != nil {
		t.Fatal(e)
	}
	copy, e := DecodeCreateSpec(b)
	if e != nil {
		t.Fatalf("decode canonical %s: %v", b, e)
	}
	again, e := CanonicalCreateSpec(copy)
	if e != nil || !bytes.Equal(b, again) {
		t.Fatalf("unstable canonical %v", e)
	}
	user, e := RequestDigest(s.Intent)
	if e != nil {
		t.Fatal(e)
	}
	original, e := CreateSpecDigest(s)
	if e != nil {
		t.Fatal(e)
	}
	s.Media.PvcUid = uuid.NewString()
	changed, _ := CreateSpecDigest(s)
	sameUser, _ := RequestDigest(s.Intent)
	if original == changed || user != sameUser {
		t.Fatal("user intent and frozen source identities are not separate")
	}
	if copy.Compute.CpuMillis != 1500 || copy.Cores != 2 || copy.Compute.MemoryMib != 512 {
		t.Fatal("compute units/topology changed")
	}
}

func TestInitialNADUIDRemainsAnExplicitEmptyV1Field(t *testing.T) {
	s := fixedSpec(t)
	original, err := CanonicalCreateSpec(s)
	if err != nil {
		t.Fatal(err)
	}
	originalUID := s.Network.NadUid
	s.Network.NadUid = ""
	raw, err := CanonicalCreateSpec(s)
	if err != nil || !bytes.Contains(raw, []byte(`"nad_uid":""`)) || ContractVersion != 1 {
		t.Fatalf("initial network intent changed its v1 field set: %s %v", raw, err)
	}
	decoded, err := DecodeCreateSpec(raw)
	if err != nil || decoded.Network.NadUid != "" {
		t.Fatalf("empty initial UID did not survive canonical decode: %v", err)
	}
	again, err := CanonicalCreateSpec(decoded)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatal("empty initial UID changed canonical bytes on replay", err)
	}
	s.Network.NadUid = originalUID
	restored, err := CanonicalCreateSpec(s)
	if err != nil || !bytes.Equal(original, restored) {
		t.Fatal("existing nonempty UID canonical changed", err)
	}
}
func TestFrozenSpecRejectsUnknownAndInvalidCapacity(t *testing.T) {
	for _, name := range []string{"absent-start", "zero-size", "overflow-size", "wrong-topology", "filesystem-root", "source-minimum", "nested-unknown"} {
		t.Run(name, func(t *testing.T) {
			s := fixedSpec(t)
			switch name {
			case "absent-start":
				s.Intent.StartAfterCreate = nil
			case "zero-size":
				s.Intent.SystemDiskSizeGb = 0
			case "overflow-size":
				s.Intent.SystemDiskSizeGb = 1 << 62
			case "wrong-topology":
				s.Cores = 1
			case "filesystem-root":
				s.Storage.VolumeMode = "Filesystem"
			case "source-minimum":
				s.Intent.SystemDiskSizeGb = 7
			case "nested-unknown":
				s.Media.ProtoReflect().SetUnknown([]byte{0xf8, 0x07, 0x01})
			}
			if _, e := CanonicalCreateSpec(s); e == nil {
				t.Fatal("invalid frozen spec accepted")
			}
		})
	}
}
func TestCommandReplayIntentIncludesVersionAndConfirmation(t *testing.T) {
	r := &ForceStopRequest{TenantId: uuid.NewString(), VmId: uuid.NewString(), IdempotencyKey: "key", ExpectedVersion: 4, DestructiveConfirmation: true}
	b, e := CanonicalCommandIntent(r)
	if e != nil {
		t.Fatal(e)
	}
	for _, mutate := range []func(*ForceStopRequest){func(x *ForceStopRequest) { x.ExpectedVersion++ }, func(x *ForceStopRequest) { x.DestructiveConfirmation = false }, func(x *ForceStopRequest) { x.VmId = uuid.NewString() }} {
		x := proto.Clone(r).(*ForceStopRequest)
		mutate(x)
		other, e := CanonicalCommandIntent(x)
		if e != nil {
			t.Fatal(e)
		}
		if bytes.Equal(b, other) {
			t.Fatal("changed intent aliases original operation")
		}
	}
	if _, e = CanonicalCommandIntent(&compute.ResolveComputePresetRequest{}); e == nil {
		t.Fatal("unknown command message accepted")
	}
}

func TestGuestInitializationCapabilityAndWriteOnlyPassword(t *testing.T) {
	s := fixedSpec(t)
	old, e := CanonicalCreateSpec(s)
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(old, []byte("guest_initialization")) || bytes.Contains(old, []byte("initialization_capability")) {
		t.Fatal("absent optional fields changed legacy canonical payload")
	}
	s.Media.InitializationCapability = CloudInitNoCloud
	s.Media.InitializationVerified = true
	s.Intent.GuestInitialization = &GuestInitialization{Method: CloudInitNoCloud, Username: "vmuser", PasswordSet: true, CredentialIntentDigest: Digest([]byte("opaque-test-hmac"))}
	if e = ValidateGuestCredentials(s, &GuestCredentials{Password: "console-test-123"}); e != nil {
		t.Fatal(e)
	}
	raw, e := CanonicalCreateSpec(s)
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(raw, []byte("console-test-123")) {
		t.Fatal("password leaked into frozen spec")
	}
	decoded, e := DecodeCreateSpec(raw)
	if e != nil || !proto.Equal(s, decoded) {
		t.Fatalf("initialization did not round trip: %v", e)
	}
	for _, name := range []string{"unverified", "no-capability", "windows", "iso", "root-user", "bad-user", "private-key", "key-options", "no-credentials", "missing-password", "short-password"} {
		t.Run(name, func(t *testing.T) {
			x := proto.Clone(s).(*CreateSpec)
			credentials := &GuestCredentials{Password: "console-test-123"}
			switch name {
			case "unverified":
				x.Media.InitializationVerified = false
			case "no-capability":
				x.Media.InitializationCapability = "NONE"
				x.Media.InitializationVerified = false
			case "windows":
				x.Media.OsFamily = "windows"
			case "iso":
				x.Intent.SourceKind = "INSTALL_ISO"
				x.Media.Kind = "INSTALL_ISO"
				x.BootDevice = "INSTALL_MEDIA"
			case "root-user":
				x.Intent.GuestInitialization.Username = "root"
			case "bad-user":
				x.Intent.GuestInitialization.Username = "user\ncommands"
			case "private-key":
				x.Intent.GuestInitialization.SshPublicKey = "-----BEGIN OPENSSH PRIVATE KEY-----"
			case "key-options":
				x.Intent.GuestInitialization.SshPublicKey = "command=\"touch /tmp/x\" ssh-ed25519 invalid"
			case "no-credentials":
				x.Intent.GuestInitialization.PasswordSet = false
				x.Intent.GuestInitialization.CredentialIntentDigest = ""
				credentials = nil
			case "missing-password":
				credentials = nil
			case "short-password":
				credentials.Password = "short"
			}
			if ValidateGuestCredentials(x, credentials) == nil {
				t.Fatal("invalid initialization accepted")
			}
		})
	}
}
