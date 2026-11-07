package vm

import (
	"context"
	"encoding/json"
	"strings"
)

// DiskNativePort changes one owned disk through the native declarative API.
// A false result means the persistent or running configuration still converges.
// ValidateDiskAttachment returns the observation it judged on so callers can
// reuse it instead of observing the same native state twice.
type DiskNativePort interface {
	ValidateDiskAttachment(context.Context, VM, Disk, bool) (Observation, error)
	EnsureDiskAttachment(context.Context, VM, Disk, bool) (bool, error)
	DiskUnreferenced(context.Context, VM, Disk) (bool, error)
}

type DiskIntent struct {
	Name          string      `json:"name"`
	SizeGB        json.Number `json:"size_gb"`
	DiskID        string      `json:"disk_id"`
	GuestPrepared bool        `json:"guest_prepared_confirmation"`
}

func ParseDiskIntent(raw json.RawMessage) (DiskIntent, error) {
	var d DiskIntent
	if e := json.Unmarshal(raw, &d); e != nil {
		return d, ErrInvalid
	}
	return d, nil
}

func FindDisk(v VM, id string) (int, error) {
	for i, d := range v.Disks {
		if d.ID == id && !d.Tombstone {
			return i, nil
		}
	}
	return -1, ErrNotFound
}

// ValidateDiskCommand also runs inside the locked acceptance transaction.
func ValidateDiskCommand(v VM, action string, raw json.RawMessage) error {
	d, e := ParseDiskIntent(raw)
	if e != nil {
		return e
	}
	if action == "create-disk" {
		size, e := d.SizeGB.Int64()
		if e != nil || size < 1 || size > 9223372036 || strings.TrimSpace(d.Name) == "" || len(d.Name) > 128 {
			return ErrInvalid
		}
		return nil
	}
	if !UUID(d.DiskID) {
		return ErrInvalid
	}
	i, e := FindDisk(v, d.DiskID)
	if e != nil {
		return e
	}
	if v.Disks[i].Role != "data" {
		return ErrInvalid
	}
	if action == "delete-disk" && v.Disks[i].Attached {
		return ErrBusy
	}
	return nil
}

func (w *Worker) disk(ctx context.Context, o *Operation, v *VM) error {
	if e := ValidateDiskCommand(*v, o.Action, o.Intent); e != nil {
		return e
	}
	intent, _ := ParseDiskIntent(o.Intent)
	if o.Action == "create-disk" {
		// The accepted operation identity is persisted before the first native
		// request and deterministically fixes all identities after a lost ACK.
		d := NewInitialDisk(v.ID, o.ID, intent.Name, "data", 1)
		i := -1
		for k := range v.Disks {
			if v.Disks[k].ID == d.ID {
				i = k
			}
		}
		if i < 0 {
			d.SizeGB, _ = intent.SizeGB.Int64()
			d.RequestedBytes = d.SizeGB * 1000000000
			d.Attached = false
			d.DV.Namespace = v.Spec.Namespace
			v.Disks = append(v.Disks, d)
			i = len(v.Disks) - 1
		}
		if e := w.checkpoint(ctx, o, *v, "create-volume", v.Disks[i].DV); e != nil {
			return e
		}
		d, e := w.native.EnsureVolume(ctx, *v, v.Disks[i])
		if e != nil {
			return e
		}
		v.Disks[i] = d
		o.Pending = nil
		o.State = "succeeded" // WFFC is a valid prepared declaration.
		return nil
	}
	i, _ := FindDisk(*v, intent.DiskID)
	d := v.Disks[i]
	n, ok := w.native.(DiskNativePort)
	if !ok {
		return ErrDependency
	}
	if o.Action == "delete-disk" {
		clear, e := n.DiskUnreferenced(ctx, *v, d)
		if e != nil {
			return e
		}
		if !clear {
			return ErrBusy
		}
		if e = w.checkpoint(ctx, o, *v, "delete-volume", d.DV); e != nil {
			return e
		}
		done, e := w.native.DeleteVolume(ctx, *v, d)
		if e != nil {
			return e
		}
		if !done {
			return nil
		}
		v.Disks[i].Tombstone = true
	} else {
		attached := o.Action == "attach-disk"
		x, e := w.native.Observe(ctx, *v)
		if e != nil {
			return e
		}
		if !attached && x.Running && !intent.GuestPrepared {
			return ErrInvalid
		}
		for _, current := range x.Volumes {
			if current.ID == d.ID {
				d = current
				v.Disks[i] = current
			}
		}
		if _, e := n.ValidateDiskAttachment(ctx, *v, d, attached); e != nil {
			return e
		}
		if e := w.checkpoint(ctx, o, *v, o.Action, d.DV); e != nil {
			return e
		}
		done, e := n.EnsureDiskAttachment(ctx, *v, d, attached)
		if e != nil {
			return e
		}
		if !done {
			return nil
		}
		v.Disks[i].Attached = attached
	}
	o.Pending = nil
	o.State = "succeeded"
	return nil
}

// DiskPreparationPending limits background reads to unfinished inventory.
// Bound, identified disks are not subjected to a periodic storage audit.
func DiskPreparationPending(d Disk) bool {
	return !d.Tombstone && (d.PVC.UID == "" || d.Preparation != "Bound" || d.ObservedCapacityBytes <= 0 || d.ObservedCapacityBytes < d.RequestedBytes)
}

// ApplyDiskPreparation patches only physical preparation facts on the current
// locked aggregate. A remapped/retired disk makes a claimed observation stale.
func ApplyDiskPreparation(v *VM, observed []Disk) error {
	disks := append([]Disk(nil), v.Disks...)
	for _, found := range observed {
		index := -1
		for i, current := range disks {
			if current.ID == found.ID && !current.Tombstone {
				index = i
				break
			}
		}
		if index < 0 {
			return ErrVersion
		}
		current := disks[index]
		if current.VolumeName != found.VolumeName || current.Serial != found.Serial || current.RequestedBytes != found.RequestedBytes {
			return ErrVersion
		}
		dv, err := mergeRegisteredRef(current.DV, found.DV)
		if err != nil {
			return ErrVersion
		}
		pvc := current.PVC
		if current.PVC.Name == "" {
			if found.PVC.Name != "" {
				if found.PVC.Kind != "PersistentVolumeClaim" || found.PVC.Namespace != v.Spec.Namespace || current.DV.Name == "" || found.PVC.Name != current.DV.Name {
					return ErrVersion
				}
				pvc = found.PVC
			} else if found.PVC != (ObjectRef{}) {
				return ErrVersion
			}
		} else {
			pvc, err = mergeRegisteredRef(current.PVC, found.PVC)
			if err != nil {
				return ErrVersion
			}
		}
		current.DV, current.PVC = dv, pvc
		if found.Preparation != "" {
			current.Preparation = found.Preparation
		}
		if found.ObservedCapacityBytes > 0 {
			current.ObservedCapacityBytes = found.ObservedCapacityBytes
		}
		disks[index] = current
	}
	v.Disks = disks
	return nil
}
