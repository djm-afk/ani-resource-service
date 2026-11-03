package vm

import (
	"context"
	"encoding/json"
	"regexp"
)

const SecurityGroupsAnnotation = "networking.kubercloud.com/security_groups"

var securityGroupIDPattern = regexp.MustCompile(`^sg_[0-9a-f]{32}$`)

func ValidateSecurityGroupIDs(ids []string) error {
	if len(ids) > 16 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !securityGroupIDPattern.MatchString(id) || seen[id] {
			return ErrInvalid
		}
		seen[id] = true
	}
	return nil
}

func ValidateSecurityGroupNames(ids, names []string) error {
	if len(ids) != len(names) {
		return ErrDependency
	}
	seen := map[string]bool{}
	valid := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	for _, name := range names {
		if name == "" || len(name) > 63 || !valid.MatchString(name) || seen[name] {
			return ErrDependency
		}
		seen[name] = true
	}
	return nil
}

// SecurityGroupPort resolves only trusted tenant/VPC/namespace selections and
// keeps the Network-owned reference alive until the owner confirms release.
type SecurityGroupPort interface {
	ResolveSecurityGroups(context.Context, string, string, string, []string) ([]string, error)
	SyncSecurityGroupBinding(context.Context, string, string, string, string, string, int64, []string) ([]string, error)
	ConfirmSecurityGroupBinding(context.Context, string, string, string, int64, []string) error
	ReleaseSecurityGroupBinding(context.Context, string, string, string, int64) error
}

type SecurityGroupNativePort interface {
	SetSecurityGroups(context.Context, VM, []string) error
}

func (w *Worker) UseSecurityGroups(port SecurityGroupPort) error {
	if port == nil {
		return ErrInvalid
	}
	w.securityGroups = port
	return nil
}
func (w *Worker) initializeSecurityGroups(ctx context.Context, v *VM) error {
	ids := append([]string{}, v.Spec.SecurityGroupIDs...)
	if len(ids) > 0 {
		if w.securityGroups == nil {
			return ErrDependency
		}
		names, err := w.securityGroups.SyncSecurityGroupBinding(ctx, v.TenantID, "vm", v.ID, v.Spec.Network.VPCID, v.Spec.Namespace, v.Version, ids)
		if err != nil {
			return err
		}
		if err := ValidateSecurityGroupNames(ids, names); err != nil {
			return err
		}
		v.SecurityGroupNames = names
	}
	v.SecurityGroupIDs = ids
	v.SecurityGroupsInitialized = true
	return nil
}

type SecurityGroupIntent struct {
	IDs []string `json:"security_group_ids"`
}

func (w *Worker) setSecurityGroups(ctx context.Context, o *Operation, v *VM) error {
	var intent SecurityGroupIntent
	if json.Unmarshal(o.Intent, &intent) != nil || ValidateSecurityGroupIDs(intent.IDs) != nil {
		return ErrInvalid
	}
	provider, ok := w.native.(SecurityGroupNativePort)
	if !ok || w.securityGroups == nil {
		return ErrDependency
	}
	if err := w.checkpoint(ctx, o, *v, "set-security-groups", v.Native); err != nil {
		return err
	}
	names, err := w.securityGroups.SyncSecurityGroupBinding(ctx, v.TenantID, "vm", v.ID, v.Spec.Network.VPCID, v.Spec.Namespace, v.Version, intent.IDs)
	if err != nil {
		return err
	}
	if err := ValidateSecurityGroupNames(intent.IDs, names); err != nil {
		return err
	}
	if port, ok := w.network.(ManualNetworkPort); ok {
		if err = port.SyncNetworkInterfaces(ctx, *v, intent.IDs); err != nil {
			return err
		}
	} else if len(v.NetworkInterfaces) > 0 {
		return ErrDependency
	}
	if err = provider.SetSecurityGroups(ctx, *v, names); err != nil {
		return err
	}
	if err = w.securityGroups.ConfirmSecurityGroupBinding(ctx, v.TenantID, "vm", v.ID, v.Version, intent.IDs); err != nil {
		return err
	}
	v.SecurityGroupIDs = append([]string{}, intent.IDs...)
	v.SecurityGroupNames = append([]string{}, names...)
	v.SecurityGroupsInitialized = true
	o.Pending = nil
	o.State = "succeeded"
	return nil
}
