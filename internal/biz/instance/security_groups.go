package instance

import (
	"context"
	"regexp"
	"time"

	"github.com/google/uuid"
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
		return ErrNetworkUnavailable
	}
	seen := map[string]bool{}
	valid := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	for _, name := range names {
		if name == "" || len(name) > 63 || !valid.MatchString(name) || seen[name] {
			return ErrNetworkUnavailable
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

// SecurityGroupProvider commits the managed template first, then the current
// controller-owned Pod, and returns only after fresh identity checks match.
type SecurityGroupProvider interface {
	SetSecurityGroups(context.Context, Instance, []string) error
}

func (w *Worker) UseSecurityGroups(port SecurityGroupPort) error {
	if port == nil {
		return ErrInvalid
	}
	w.securityGroups = port
	return nil
}
func (w *Worker) initializeSecurityGroups(ctx context.Context, i *Instance) error {
	ids := append([]string{}, i.Spec.SecurityGroupIDs...)
	if len(ids) > 0 {
		if w.securityGroups == nil || i.Spec.Network == nil {
			return ErrNetworkUnavailable
		}
		names, err := w.securityGroups.SyncSecurityGroupBinding(ctx, i.TenantID, "container", i.ID, i.Spec.Network.VPCID, i.Spec.Namespace, i.Version, ids)
		if err != nil {
			return err
		}
		if err := ValidateSecurityGroupNames(ids, names); err != nil {
			return err
		}
		i.SecurityGroupNames = names
	}
	i.SecurityGroupIDs = ids
	i.SecurityGroupRevision = i.Version
	i.SecurityGroupsInitialized = true
	return nil
}
func (w *Worker) stepSecurityGroups(ctx context.Context, work Work) (bool, error) {
	p := Progress{Instance: work.Instance, Operation: cloneOperation(work.Operation), Next: 5 * time.Second}
	o := p.Operation
	if o == nil || o.Execution == nil || p.Instance.Spec.Network == nil {
		return true, ErrInvalid
	}
	provider, ok := w.provider.(SecurityGroupProvider)
	if !ok || w.securityGroups == nil {
		p.Instance.Reason = ErrNetworkUnavailable.Error()
		o.Reason = p.Instance.Reason
		o.State = "blocked"
		return true, w.repo.Finish(ctx, work, p)
	}
	ids := o.Execution.SecurityGroupIDs
	m := p.Instance.Pending
	if m == nil {
		fresh := Mutation{Kind: "set_security_groups", OperationID: o.ID, Version: o.Version, ID: uuid.NewString(), Phase: o.Execution.Phase, PhaseSequence: o.Execution.PhaseSequence}
		if err := w.repo.BeginMutation(ctx, work, fresh); err != nil {
			return true, err
		}
		m = &fresh
		p.Instance.Pending = m
	} else if m.Kind != "set_security_groups" || m.OperationID != o.ID || m.Version != o.Version {
		return true, ErrVersion
	}
	names, err := w.securityGroups.SyncSecurityGroupBinding(ctx, p.Instance.TenantID, "container", p.Instance.ID, p.Instance.Spec.Network.VPCID, p.Instance.Spec.Namespace, p.Instance.Version, ids)
	if err != nil {
		p.Instance.Reason = err.Error()
		o.Reason = p.Instance.Reason
		o.State = "blocked"
		return true, w.repo.Finish(ctx, work, p)
	}
	if err := ValidateSecurityGroupNames(ids, names); err != nil {
		p.Instance.Reason = err.Error()
		o.Reason = p.Instance.Reason
		o.State = "blocked"
		return true, w.repo.Finish(ctx, work, p)
	}
	if port, ok := w.network.(ManualNetworkPort); ok {
		if err = port.SyncNetworkInterfaces(ctx, p.Instance, ids); err != nil {
			block(&p, err.Error())
			return true, w.repo.Finish(ctx, work, p)
		}
	} else if len(p.Instance.NetworkInterfaces) > 0 {
		block(&p, ErrNetworkUnavailable.Error())
		return true, w.repo.Finish(ctx, work, p)
	}
	err = provider.SetSecurityGroups(ctx, p.Instance, names)
	if err != nil {
		p.Instance.Reason = err.Error()
		o.Reason = err.Error()
		o.State = "blocked"
		return true, w.repo.Finish(ctx, work, p)
	}
	if err = w.securityGroups.ConfirmSecurityGroupBinding(ctx, p.Instance.TenantID, "container", p.Instance.ID, p.Instance.Version, ids); err != nil {
		p.Instance.Reason = err.Error()
		o.Reason = p.Instance.Reason
		o.State = "blocked"
		return true, w.repo.Finish(ctx, work, p)
	}
	p.Instance.SecurityGroupIDs = append([]string{}, ids...)
	p.Instance.SecurityGroupNames = append([]string{}, names...)
	p.Instance.SecurityGroupRevision = p.Instance.Version
	p.Instance.SecurityGroupsInitialized = true
	p.Instance.Pending = nil
	p.Instance.Reason = ""
	p.Receipt = &MutationReceipt{Mutation: *m, Outcome: "succeeded"}
	o.State = "completed"
	o.Reason = ""
	o.Execution.Phase = "done"
	p.Next = 30 * time.Second
	return true, w.repo.Finish(ctx, work, p)
}
