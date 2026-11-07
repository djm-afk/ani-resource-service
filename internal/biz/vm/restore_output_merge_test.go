package vm

import (
	"errors"
	"reflect"
	"testing"
)

func TestRestoreOutputMergePreservesMissingIdentitiesAndCleanup(t *testing.T) {
	for _, uid := range []string{"", "registered-pvc", "replacement-pvc"} {
		for _, kind := range []string{"disk", "media", "backend", "current-disk", "current-media"} {
			t.Run(kind+"/"+uid, func(t *testing.T) {
				v, _ := snapshotFixture(t)
				ref := ObjectRef{Kind: "PersistentVolumeClaim", Namespace: v.Spec.Namespace, Name: "restored-output", UID: "registered-pvc"}
				extra := ObjectRef{Kind: "PersistentVolumeClaim", Namespace: v.Spec.Namespace, Name: "pending-cleanup", UID: "cleanup-pvc"}
				disk := Disk{ID: "disk", VolumeName: "disk-volume", Serial: "disk", PVC: ref, RequestedBytes: 4096, ObservedCapacityBytes: 8192, Cleanup: []ObjectRef{extra}, Preparation: "Bound", Tombstone: true}
				media := MediaCopy{ID: "media", PVC: ref, SourceDigest: "frozen-source", Cleanup: []ObjectRef{extra}, Ready: true, Tombstone: true}
				backend := BackendState{PVC: ref, Owner: v.Native, PV: ObjectRef{Kind: "PersistentVolume", Name: "diagnostic-pv", UID: "pv"}, VolumeName: "persistent-state", RequestedBytes: 4096, CapacityBytes: 8192, VolumeMode: "Filesystem", Present: true, Current: true}
				observed := ref
				observed.UID = uid
				x := RestoreObservation{}
				switch kind {
				case "disk":
					v.RetiredDisks = []Disk{disk}
					x.Disks = []Disk{{ID: "disk", PVC: observed}}
				case "media":
					v.RetiredMediaCopies = []MediaCopy{media}
					x.MediaCopies = []MediaCopy{{ID: "media", PVC: observed}}
				case "backend":
					v.BackendStates = []BackendState{backend}
					x.BackendStates = []BackendState{{PVC: observed, Owner: v.Native, VolumeName: backend.VolumeName}}
				case "current-disk":
					v.Disks = []Disk{disk}
					x.Disks = []Disk{{ID: "disk", PVC: observed}}
				case "current-media":
					v.MediaCopies = []MediaCopy{media}
					x.MediaCopies = []MediaCopy{{ID: "media", PVC: observed}}
				}
				err := recordRestoreVolumes(&v, x)
				if uid == "replacement-pvc" {
					if !errors.Is(err, ErrOwnership) {
						t.Fatal("replacement accepted", err)
					}
					return
				}
				if err != nil {
					t.Fatal("missing or same identity rejected", err)
				}
				switch kind {
				case "disk":
					if v.RetiredDisks[0].PVC != ref || !reflect.DeepEqual(v.RetiredDisks[0].Cleanup, []ObjectRef{extra}) {
						t.Fatal(v.RetiredDisks)
					}
					if uid == "" && !reflect.DeepEqual(v.RetiredDisks[0], disk) {
						t.Fatal("missing observation erased disk history", v.RetiredDisks)
					}
				case "media":
					if v.RetiredMediaCopies[0].PVC != ref || !reflect.DeepEqual(v.RetiredMediaCopies[0].Cleanup, []ObjectRef{extra}) {
						t.Fatal(v.RetiredMediaCopies)
					}
					if uid == "" && !reflect.DeepEqual(v.RetiredMediaCopies[0], media) {
						t.Fatal("missing observation erased media history", v.RetiredMediaCopies)
					}
				case "backend":
					got := v.BackendStates[0]
					if got.PVC != ref || got.Owner != backend.Owner || got.PV != backend.PV {
						t.Fatal(got)
					}
					if uid == "" && (got.Present || got.Current || got.CapacityBytes != backend.CapacityBytes || got.VolumeMode != backend.VolumeMode) {
						t.Fatal("missing observation lost backend history", got)
					}
				case "current-disk":
					if len(v.RetiredDisks) != 0 || !reflect.DeepEqual(v.Disks[0], disk) {
						t.Fatal("current disk retired or overwritten", v)
					}
				case "current-media":
					if len(v.RetiredMediaCopies) != 0 || !reflect.DeepEqual(v.MediaCopies[0], media) {
						t.Fatal("current media retired or overwritten", v)
					}
				}
			})
		}
	}
}

func TestRestoreOutputMergeRejectsForeignScope(t *testing.T) {
	for _, kind := range []string{"wrong-kind", "wrong-namespace", "wrong-backend-owner"} {
		t.Run(kind, func(t *testing.T) {
			v, _ := snapshotFixture(t)
			ref := ObjectRef{Kind: "PersistentVolumeClaim", Namespace: v.Spec.Namespace, Name: "restored", UID: "known"}
			v.BackendStates = []BackendState{{PVC: ref, Owner: v.Native}}
			observed := BackendState{PVC: ref, Owner: v.Native}
			observed.PVC.UID = ""
			switch kind {
			case "wrong-kind":
				observed.PVC.Kind = "Secret"
			case "wrong-namespace":
				observed.PVC.Namespace = "foreign"
			case "wrong-backend-owner":
				observed.Owner.UID = "foreign"
			}
			if err := recordRestoreVolumes(&v, RestoreObservation{BackendStates: []BackendState{observed}}); !errors.Is(err, ErrOwnership) {
				t.Fatal(err)
			}
		})
	}
}
