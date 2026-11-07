//go:build instanceintegration

package vm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/api/compute/v1"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/vm"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

type inventoryTestCompute struct{ service.ComputeResolver }
type inventoryTestEnvironment struct{ biz.EnvironmentPort }

func inventoryFrozenSpec(t *testing.T, v *biz.VM) {
	t.Helper()
	start := false
	c := &compute.ComputeSelection{SchemaVersion: 1, PresetId: uuid.NewString(), Code: "cpu-test", Name: "CPU test", ComputeType: "cpu", CpuMillis: 2000, MemoryMib: 512}
	var err error
	c.SelectionDigest, err = compute.SelectionDigest(c)
	if err != nil {
		t.Fatal(err)
	}
	vpc, subnet := "vpc_"+strings.ReplaceAll(uuid.NewString(), "-", ""), "subnet_"+strings.ReplaceAll(uuid.NewString(), "-", "")
	v.Spec.Name = "async-inventory"
	v.Spec.ServiceAccount = "default"
	v.Spec.StorageClassUID = uuid.NewString()
	v.Spec.StartAfterCreate = false
	m := v.Spec.Media
	frozen := &pb.CreateSpec{Intent: &pb.CreateVMData{Name: v.Spec.Name, PresetId: c.PresetId, SourceKind: "DISK_IMAGE", MediaKey: m.Key, SystemDiskSizeGb: v.Disks[0].SizeGB, Network: &pb.NetworkSelection{VpcId: vpc, SubnetId: subnet}, StartAfterCreate: &start}, Compute: c, ClusterId: v.Spec.ClusterID, Namespace: v.Spec.Namespace, NamespaceUid: v.Spec.NamespaceUID, ServiceAccount: "default", Storage: &pb.StoragePolicy{StorageClass: v.Spec.StorageClass, StorageClassUid: v.Spec.StorageClassUID, VolumeMode: "Block", AccessModes: []string{"ReadWriteOnce"}}, Media: &pb.MediaReference{MediaKey: m.Key, Kind: m.Kind, ClusterId: m.ClusterID, Namespace: m.Namespace, PvcName: m.PVCName, PvcUid: m.PVCUID, Checksum: m.Checksum, VolumeMode: m.VolumeMode, ContentType: m.ContentType, VirtualCapacityBytes: m.VirtualBytes, RequestedBytes: m.RequestedBytes, OsFamily: m.OSFamily, OsVersion: m.OSVersion, Architecture: m.Architecture, Firmware: m.Firmware, RootBus: m.RootBus, DataBus: m.DataBus, NicModel: m.NICModel}, Network: &pb.FrozenNetwork{VpcId: vpc, SubnetId: subnet, NadName: "main", NadConfigDigest: stringDigest('c')}, Sockets: 1, Cores: 2, Threads: 1, BootDevice: "SYSTEM_DISK"}
	v.Spec.Canonical, err = pb.CanonicalCreateSpec(frozen)
	if err != nil {
		t.Fatal(err)
	}
	v.Spec.Digest = pb.Digest(v.Spec.Canonical)
	v.Spec.InitialDisks = append([]biz.Disk(nil), v.Disks...)
}

func forceVMObservationDue(t *testing.T, db *testenv.Database, v biz.VM) {
	t.Helper()
	_, err := db.Owner.Exec(t.Context(), "UPDATE vm.observation_work SET next_attempt_at=now()-interval '1 second',not_before=now()-interval '1 second' WHERE tenant_id=$1 AND vm_id=$2", v.TenantID, v.ID)
	if err != nil {
		t.Fatal(err)
	}
}

func TestStoppedCreateInventoryRefreshAfterIndependentStart(t *testing.T) {
	for _, initiallyPresent := range []bool{false, true} {
		t.Run(map[bool]string{false: "PVC-absent", true: "PVC-Pending-WFFC"}[initiallyPresent], func(t *testing.T) {
			ctx := t.Context()
			_, v, original := mediaNativeFixture(t)
			v.Spec.Media.Kind = "DISK_IMAGE"
			v.Spec.BootDevice = "SYSTEM_DISK"
			v.CurrentBootDevice = "SYSTEM_DISK"
			v.MediaSlots = nil
			v.MediaCopies = nil
			inventoryFrozenSpec(t, &v)
			ns, _ := original.Resource(nsResource).Get(ctx, v.Spec.Namespace, metav1.GetOptions{})
			root, _ := original.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
			root.SetLabels(labelVM(v))
			root.SetResourceVersion("1")
			if err := unstructured.SetNestedSlice(root.Object, []any{consoleTablet()}, "spec", "template", "spec", "domain", "devices", "inputs"); err != nil {
				t.Fatal(err)
			}
			source, _ := original.Resource(pvcResource).Namespace(v.Spec.Media.Namespace).Get(ctx, v.Spec.Media.PVCName, metav1.GetOptions{})
			client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{podResource: "PodList"}, ns, root, source)
			client.PrependReactor("create", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
				obj := a.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
				if obj.GetUID() == "" {
					obj.SetUID(types.UID(uuid.NewString()))
				}
				return false, nil, nil
			})
			starts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "PUT" || !strings.HasSuffix(r.URL.Path, "/start") {
					t.Errorf("unexpected power request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(400)
					return
				}
				starts++
				instance := snapshotObject("kubevirt.io/v1", "VirtualMachineInstance", v.Native.Name, v.Spec.Namespace, map[string]any{"status": map[string]any{"phase": "Running"}})
				instance.SetOwnerReferences(ownReference(v))
				if _, err := client.Resource(vmiResource).Namespace(v.Spec.Namespace).Create(ctx, instance, metav1.CreateOptions{}); err != nil {
					t.Error(err)
				}
				w.WriteHeader(http.StatusAccepted)
			}))
			defer server.Close()
			native, err := NewNative(client, &rest.Config{Host: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			d := v.Disks[0]
			d.DV.Namespace = v.Spec.Namespace
			d.PVC = biz.ObjectRef{}
			d, err = native.EnsureVolume(ctx, v, d)
			if err != nil {
				t.Fatal(err)
			}
			pvc := snapshotObject("v1", "PersistentVolumeClaim", d.DV.Name, v.Spec.Namespace, map[string]any{"spec": map[string]any{"volumeMode": "Block"}, "status": map[string]any{"phase": "Pending"}})
			yes := true
			pvc.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "cdi.kubevirt.io/v1beta1", Kind: "DataVolume", Name: d.DV.Name, UID: types.UID(d.DV.UID), Controller: &yes}})
			if initiallyPresent {
				if _, err = client.Resource(pvcResource).Namespace(v.Spec.Namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			db := testenv.NewDatabase(t)
			if err = Migrate(ctx, db.Owner.Config().ConnString(), db.RuntimeRole); err != nil {
				t.Fatal(err)
			}
			p, err := OpenPostgres(ctx, db.RuntimeDSN)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			createID := uuid.NewString()
			if _, err = p.AcceptCreate(ctx, biz.Acceptance{TenantID: v.TenantID, VMID: v.ID, OperationID: createID, RequestDigest: stringDigest('a'), Spec: v.Spec}); err != nil {
				t.Fatal(err)
			}
			op, saved, err := p.Claim(ctx, uuid.NewString(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			saved.Native = v.Native
			saved.Disks = []biz.Disk{d}
			saved.SecurityGroupsInitialized = true
			saved.Network = biz.NetworkPlan{AttachmentID: uuid.NewString()}
			if err = p.Checkpoint(ctx, op, saved); err != nil {
				t.Fatal(err)
			}
			if _, err = db.Owner.Exec(ctx, "UPDATE vm.operations SET lease_until=now()-interval '1 second',next_attempt_at=now()-interval '1 second' WHERE tenant_id=$1 AND id=$2", v.TenantID, createID); err != nil {
				t.Fatal(err)
			}
			worker, err := biz.NewWorker(p, native, powerVMNetwork{})
			if err != nil {
				t.Fatal(err)
			}
			stepOperation := func(id string) {
				t.Helper()
				for i := 0; i < 12; i++ {
					_, err = db.Owner.Exec(ctx, "UPDATE vm.operations SET next_attempt_at=now()-interval '1 second' WHERE tenant_id=$1 AND id=$2", v.TenantID, id)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = worker.Step(ctx); err != nil {
						t.Fatal(err)
					}
					o, e := p.GetOperation(ctx, v.TenantID, v.ID, id)
					if e != nil {
						t.Fatal(e)
					}
					if o.State == "succeeded" {
						return
					}
					if o.State == "failed" {
						t.Fatalf("operation failed: %+v", o)
					}
				}
				t.Fatal("operation did not converge")
			}
			stepOperation(createID)
			before, err := p.Get(ctx, v.TenantID, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			if before.Observed != "Stopped" || before.Disks[0].Preparation == "Bound" || starts != 0 {
				t.Fatal("stopped create waited for binding or started", before, starts)
			}
			storageErr := errors.New("storage read unavailable")
			failStorage := true
			client.PrependReactor("get", "*", func(a ktesting.Action) (bool, runtime.Object, error) {
				if failStorage && (a.GetResource() == dvResource || a.GetResource() == pvcResource) {
					return true, nil, storageErr
				}
				return false, nil, nil
			})
			start, err := p.AcceptCommand(ctx, biz.Command{TenantID: v.TenantID, VMID: v.ID, Action: "start", Key: "independent-start", ExpectedVersion: before.Version, RequestDigest: stringDigest('d'), Intent: []byte("{}")})
			if err != nil {
				t.Fatal(err)
			}
			stepOperation(start.ID)
			if starts != 1 {
				t.Fatal("Start did not use native subresource once", starts)
			}
			// This is the real adapter capability, not the legacy fake Observe fallback.
			running, err := native.ObserveLifecycle(ctx, before)
			if err != nil || !running.Running || len(running.Volumes) != 0 {
				t.Fatal("lifecycle coupled to unavailable storage", running, err)
			}
			observation := newStateObservation(client, nil)
			observation.synced = true
			for _, resource := range observation.resources {
				resource.valid = true
			}
			currentRoot, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
			instance, _ := client.Resource(vmiResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
			if err = observation.resources[0].index.Add(currentRoot); err != nil {
				t.Fatal(err)
			}
			if err = observation.resources[1].index.Add(instance); err != nil {
				t.Fatal(err)
			}
			observer, err := biz.NewObserver(p, observation)
			if err != nil {
				t.Fatal(err)
			}
			forceVMObservationDue(t, db, v)
			if worked, e := observer.Step(ctx); !worked || !errors.Is(e, storageErr) {
				t.Fatal("storage error not isolated to background attempt", worked, e)
			}
			still, err := p.Get(ctx, v.TenantID, v.ID)
			if err != nil || still.Observed != "Running" || still.Version != start.IntentVersion {
				t.Fatal("inventory error changed power projection", still, err)
			}
			completed, err := p.GetOperation(ctx, v.TenantID, v.ID, start.ID)
			if err != nil || completed.State != "succeeded" {
				t.Fatal("inventory error failed completed Start", completed, err)
			}
			failStorage = false
			if !initiallyPresent {
				if _, err = client.Resource(pvcResource).Namespace(v.Spec.Namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			bound, err := client.Resource(pvcResource).Namespace(v.Spec.Namespace).Get(ctx, d.DV.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if err = unstructured.SetNestedMap(bound.Object, map[string]any{"phase": "Bound", "capacity": map[string]any{"storage": "12Gi"}}, "status"); err != nil {
				t.Fatal(err)
			}
			if _, err = client.Resource(pvcResource).Namespace(v.Spec.Namespace).Update(ctx, bound, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			forceVMObservationDue(t, db, v)
			if worked, e := observer.Step(ctx); !worked || e != nil {
				t.Fatal(worked, e)
			}
			after, err := p.Get(ctx, v.TenantID, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Disks[0].PVC.UID != string(bound.GetUID()) || after.Disks[0].Preparation != "Bound" || after.Disks[0].ObservedCapacityBytes != 12<<30 || after.Version != still.Version || after.LastOperationID != still.LastOperationID {
				t.Fatal("preparation did not converge without a disk command", after)
			}
			var mirror biz.Disk
			var raw []byte
			if err = p.pool.QueryRow(ctx, "SELECT record FROM vm.disks WHERE tenant_id=$1 AND vm_id=$2 AND disk_id=$3", v.TenantID, v.ID, d.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(raw, &mirror); err != nil || mirror.PVC != after.Disks[0].PVC || mirror.Preparation != "Bound" {
				t.Fatal("disk mirror stale", mirror, err)
			}
			// A converged disk is not continuously re-read from storage.
			failStorage = true
			forceVMObservationDue(t, db, v)
			if worked, e := observer.Step(ctx); !worked || e != nil {
				t.Fatal("converged inventory was unnecessarily audited", worked, e)
			}
			client.ClearActions()
			handler, err := service.NewService(p, inventoryTestCompute{}, inventoryTestEnvironment{}, native, service.Config{ClusterID: v.Spec.ClusterID, ServiceAccount: "default", StorageClass: v.Spec.StorageClass, StorageClassUID: v.Spec.StorageClassUID, LogLimits: biz.DefaultLogLimits})
			if err != nil {
				t.Fatal(err)
			}
			caller := biz.WithCaller(ctx, biz.Caller{TenantID: v.TenantID, Actor: "test"})
			include := false
			public, err := handler.GetVM(caller, &pb.GetVMRequest{TenantId: v.TenantID, VmId: v.ID, IncludeRuntime: &include})
			if err != nil {
				t.Fatal(err)
			}
			disks, err := handler.ListDisks(caller, &pb.GetVMRequest{TenantId: v.TenantID, VmId: v.ID})
			if err != nil {
				t.Fatal(err)
			}
			if len(public.Disks) != 1 || len(disks.Items) != 1 || public.Disks[0].PreparationState != "Bound" || disks.Items[0].ObservedCapacityBytes != 12<<30 {
				t.Fatal("public projection stale", public, disks)
			}
			if len(client.Actions()) != 0 {
				t.Fatal("public inventory reads unexpectedly contacted native storage", client.Actions())
			}
			failStorage = false
			// An actual replacement remains an identity error, not a refreshed UID.
			after.Disks[0].Preparation = "Pending"
			replacement := bound.DeepCopy()
			replacement.SetUID(types.UID(uuid.NewString()))
			if err = client.Tracker().Update(pvcResource, replacement, v.Spec.Namespace); err != nil {
				t.Fatal(err)
			}
			preparation, ok := any(native).(interface {
				ObserveDiskPreparation(context.Context, biz.VM) ([]biz.Disk, error)
			})
			if !ok {
				t.Fatal("native preparation observation is missing")
			}
			if _, err = preparation.ObserveDiskPreparation(ctx, after); !errors.Is(err, biz.ErrOwnership) {
				t.Fatal("replacement adopted", err)
			}
		})
	}
}

func TestObservationPreparationFencesRemapAndPreservesIntentFields(t *testing.T) {
	p, db, v := observationRegisteredVM(t)
	ctx := t.Context()
	d := biz.NewInitialDisk(v.ID, "one", "one", "data", 1)
	d.DV.Namespace = v.Spec.Namespace
	d.DV.UID = uuid.NewString()
	v.Disks = []biz.Disk{d}
	if _, err := db.Owner.Exec(ctx, "UPDATE vm.virtual_machines SET current_configuration=jsonb_set(current_configuration,'{Disks}',$3::jsonb) WHERE tenant_id=$1 AND id=$2", v.TenantID, v.ID, encode(v.Disks)); err != nil {
		t.Fatal(err)
	}
	work, claimed, err := p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ready := d
	ready.PVC = biz.ObjectRef{Kind: "PersistentVolumeClaim", Namespace: v.Spec.Namespace, Name: d.DV.Name, UID: uuid.NewString()}
	ready.Preparation = "Bound"
	ready.ObservedCapacityBytes = 1 << 30
	// Same-version current user fields must not be overwritten by the claimed copy.
	current := d
	current.Attached = false
	current.Name = "new-name"
	current.Cleanup = []biz.ObjectRef{{Kind: "Secret", Namespace: v.Spec.Namespace, Name: "cleanup", UID: "known"}}
	if _, err = db.Owner.Exec(ctx, "UPDATE vm.virtual_machines SET current_configuration=jsonb_set(current_configuration,'{Disks}',$3::jsonb) WHERE tenant_id=$1 AND id=$2", v.TenantID, v.ID, encode([]biz.Disk{current})); err != nil {
		t.Fatal(err)
	}
	observed := stoppedObservation(claimed)
	observed.Volumes = []biz.Disk{ready}
	if err = p.SaveObservation(ctx, work, observed, nil); err != nil {
		t.Fatal(err)
	}
	saved, err := p.Get(ctx, v.TenantID, v.ID)
	if err != nil || saved.Disks[0].Attached || saved.Disks[0].Name != "new-name" || len(saved.Disks[0].Cleanup) != 1 || saved.Disks[0].Preparation != "Bound" {
		t.Fatal(saved, err)
	}
	forceVMObservationDue(t, db, v)
	work, claimed, err = p.ClaimObservation(ctx, uuid.NewString(), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	newer := claimed.Disks[0]
	newer.DV = biz.ObjectRef{}
	newer.PVC.Name = "new-restored-name"
	newer.PVC.UID = uuid.NewString()
	if _, err = db.Owner.Exec(ctx, "UPDATE vm.virtual_machines SET current_configuration=jsonb_set(current_configuration,'{Disks}',$3::jsonb) WHERE tenant_id=$1 AND id=$2", v.TenantID, v.ID, encode([]biz.Disk{newer})); err != nil {
		t.Fatal(err)
	}
	observed = stoppedObservation(claimed)
	observed.Volumes = claimed.Disks
	if err = p.SaveObservation(ctx, work, observed, nil); !errors.Is(err, biz.ErrVersion) {
		t.Fatal("stale mapping was not fenced", err)
	}
	saved, err = p.Get(ctx, v.TenantID, v.ID)
	if err != nil || saved.Disks[0].PVC != newer.PVC || saved.Disks[0].DV != newer.DV {
		t.Fatal("old preparation replaced restore mapping", saved, err)
	}
}
