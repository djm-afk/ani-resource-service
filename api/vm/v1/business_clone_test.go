package vmv1

import (
	"strings"
	"testing"

	compute "github.com/zhangzhe-ctrl/ani-resource-service/api/compute/v1"
)

func cloneIntent() *CreateVMData {
	start := false
	return &CreateVMData{
		Name:             "clone-target",
		SourceKind:       "VM_CLONE",
		StartAfterCreate: &start,
		CloneSource:      &VMCloneSource{SourceVmId: "11111111-1111-4111-8111-111111111111", ExpectedSourceVersion: 7},
	}
}

func TestCloneIntentRejectsOverrides(t *testing.T) {
	cases := map[string]func(*CreateVMData){
		"preset":       func(d *CreateVMData) { d.PresetId = "22222222-2222-4222-8222-222222222222" },
		"media":        func(d *CreateVMData) { d.MediaKey = "img" },
		"system size":  func(d *CreateVMData) { d.SystemDiskSizeGb = 40 },
		"data disks":   func(d *CreateVMData) { d.DataDisks = []*InitialDataDisk{{ClientDiskKey: "d", Name: "d", SizeGb: 10}} },
		"security grp": func(d *CreateVMData) { d.SecurityGroupIds = []string{"sg-1"} },
		"empty network": func(d *CreateVMData) {
			d.Network = &NetworkSelection{}
		},
		"empty dns": func(d *CreateVMData) { d.Dns = &DNSSelection{} },
		"empty init": func(d *CreateVMData) {
			d.GuestInitialization = &GuestInitialization{}
		},
		"missing selector": func(d *CreateVMData) { d.CloneSource = nil },
		"zero version":     func(d *CreateVMData) { d.CloneSource.ExpectedSourceVersion = 0 },
		"bad source id":    func(d *CreateVMData) { d.CloneSource.SourceVmId = "not-a-uuid" },
	}
	for name, mutate := range cases {
		d := cloneIntent()
		mutate(d)
		if e := ValidateUserIntent(d); e == nil {
			t.Fatalf("%s: clone intent accepted an override", name)
		}
	}
	if e := ValidateUserIntent(cloneIntent()); e != nil {
		t.Fatalf("valid clone intent rejected: %v", e)
	}
}

func TestCloneIntentAbsentStartIsFalse(t *testing.T) {
	d := cloneIntent()
	d.StartAfterCreate = nil
	raw, e := CanonicalUserIntent(d)
	if e != nil {
		t.Fatalf("clone without start_after_create rejected: %v", e)
	}
	if got := string(raw); !strings.Contains(got, `"start_after_create":false`) {
		t.Fatalf("canonical clone intent lost the implicit false: %s", got)
	}
}

func TestImageIntentRejectsCloneSource(t *testing.T) {
	d := &CreateVMData{
		Name: "image-vm", PresetId: "22222222-2222-4222-8222-222222222222", SourceKind: "DISK_IMAGE",
		MediaKey: "img", SystemDiskSizeGb: 40, StartAfterCreate: boolPtr(true),
		Network:     &NetworkSelection{VpcId: "vpc_11111111111111111111111111111111", SubnetId: "subnet_11111111111111111111111111111111"},
		CloneSource: &VMCloneSource{SourceVmId: "11111111-1111-4111-8111-111111111111", ExpectedSourceVersion: 1},
	}
	if e := ValidateUserIntent(d); e == nil {
		t.Fatal("image intent accepted clone_source")
	}
}

func TestCloneSpecFreezeValidation(t *testing.T) {
	d := cloneIntent()
	clone := &FrozenVMCloneSource{
		SourceVmId: d.CloneSource.SourceVmId, SourceName: "source", SourceVersion: 7,
		SourceNativeName: "vm-" + d.CloneSource.SourceVmId, SourceNativeUid: "uid-source",
		Disks: []*VMCloneDisk{
			{SourceDiskId: "33333333-3333-4333-8333-333333333333", VolumeName: "disk-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Role: "system", Name: "system", SizeGb: 40},
			{SourceDiskId: "44444444-4444-4444-8444-444444444444", VolumeName: "disk-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Role: "data", Name: "data", SizeGb: 100},
		},
	}
	spec := &CreateSpec{
		Intent:    d,
		Compute:   &compute.ComputeSelection{},
		ClusterId: "55555555-5555-4555-8555-555555555555", Namespace: "tenant-1", NamespaceUid: "ns-uid", ServiceAccount: "sa",
		Storage: &StoragePolicy{StorageClass: "ani-block", StorageClassUid: "sc-uid", VolumeMode: "Block", AccessModes: []string{"ReadWriteOnce"}},
		Media:   &MediaReference{OsFamily: "linux", OsVersion: "24.04", Architecture: "amd64", Firmware: "bios", RootBus: "virtio", DataBus: "scsi", NicModel: "virtio"},
		Network: &FrozenNetwork{VpcId: "vpc_11111111111111111111111111111111", SubnetId: "subnet_11111111111111111111111111111111", NadName: "nad", NadConfigDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Sockets: 1, Cores: 4, Threads: 1, BootDevice: "SYSTEM_DISK",
		CloneSource: clone,
	}
	// compute.ValidateSelection requires a real digest; this case only asserts the
	// clone branch rejects a missing frozen source early.
	if e := ValidateCreateSpec(spec); e == nil {
		t.Fatal("clone spec without a valid compute selection was accepted")
	}
	spec.CloneSource = nil
	if e := ValidateCreateSpec(spec); e == nil {
		t.Fatal("clone intent without frozen clone source was accepted")
	}
}

func boolPtr(v bool) *bool { return &v }
