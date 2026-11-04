package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	instancebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	networkbiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	vmbiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	networkdata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	networkservice "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/network"
)

type securityGroupReferenceStore interface {
	ResolveSecurityGroups(context.Context, string, string, string, []string) ([]string, error)
	SyncSecurityGroupBinding(context.Context, string, string, string, string, string, int64, []string) ([]string, error)
	ConfirmSecurityGroupBinding(context.Context, string, string, string, int64, []string) error
	ReleaseSecurityGroupBinding(context.Context, string, string, string, int64) error
}

// The composition root translates Network failures to each owner's existing
// dependency vocabulary without coupling either business package to Network.
type ownerSecurityGroups struct {
	store    securityGroupReferenceStore
	mapError func(error) error
}

func (p ownerSecurityGroups) ResolveSecurityGroups(ctx context.Context, tenant, vpc, namespace string, ids []string) ([]string, error) {
	names, err := p.store.ResolveSecurityGroups(ctx, tenant, vpc, namespace, ids)
	return names, p.mapError(err)
}
func (p ownerSecurityGroups) SyncSecurityGroupBinding(ctx context.Context, tenant, targetType, id, vpc, namespace string, version int64, ids []string) ([]string, error) {
	names, err := p.store.SyncSecurityGroupBinding(ctx, tenant, targetType, id, vpc, namespace, version, ids)
	return names, p.mapError(err)
}
func (p ownerSecurityGroups) ConfirmSecurityGroupBinding(ctx context.Context, tenant, targetType, id string, version int64, ids []string) error {
	return p.mapError(p.store.ConfirmSecurityGroupBinding(ctx, tenant, targetType, id, version, ids))
}
func (p ownerSecurityGroups) ReleaseSecurityGroupBinding(ctx context.Context, tenant, targetType, id string, version int64) error {
	return p.mapError(p.store.ReleaseSecurityGroupBinding(ctx, tenant, targetType, id, version))
}

func instanceSecurityGroupError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	target := instancebiz.ErrNetworkUnavailable
	switch networkbiz.ReasonOf(err) {
	case networkbiz.InvalidArgument, networkbiz.TenantRequired:
		target = instancebiz.ErrInvalid
	case networkbiz.PermissionDenied:
		target = instancebiz.ErrDenied
	case networkbiz.ResourceNotFound:
		target = instancebiz.ErrNetworkNotFound
	case networkbiz.VersionConflict:
		target = instancebiz.ErrVersion
	case networkbiz.ProviderOwnership, networkbiz.ProviderStateMismatch, networkbiz.PlacementMismatch, networkbiz.AttachmentConflict, networkbiz.ResourceInUse:
		target = instancebiz.ErrNetworkConflict
	case networkbiz.ResourceBusy:
		target = instancebiz.ErrOperationInProgress
	}
	return fmt.Errorf("%w: %v", target, err)
}

func vmSecurityGroupError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	target := vmbiz.ErrDependency
	switch networkbiz.ReasonOf(err) {
	case networkbiz.InvalidArgument, networkbiz.TenantRequired:
		target = vmbiz.ErrInvalid
	case networkbiz.PermissionDenied:
		target = vmbiz.ErrDenied
	case networkbiz.VersionConflict:
		target = vmbiz.ErrVersion
	case networkbiz.ProviderOwnership:
		target = vmbiz.ErrOwnership
	case networkbiz.ProviderStateMismatch, networkbiz.PlacementMismatch, networkbiz.AttachmentConflict:
		target = vmbiz.ErrChanged
	case networkbiz.ResourceBusy, networkbiz.ResourceInUse:
		target = vmbiz.ErrBusy
	}
	return fmt.Errorf("%w: %v", target, err)
}

func wireSecurityGroups(repository *networkdata.Postgres, provider *networkdata.KCProvider, instances *instanceComponents, vms *vmComponents, policy networkbiz.WorkerPolicy) (*networkservice.NetworkSecurityGroupService, *networkbiz.SecurityGroupWorker, error) {
	if repository == nil || provider == nil {
		return nil, nil, fmt.Errorf("security groups require Network repository and provider")
	}
	if instances != nil {
		port := ownerSecurityGroups{store: repository, mapError: instanceSecurityGroupError}
		if err := instances.service.UseSecurityGroups(port); err != nil {
			return nil, nil, err
		}
		if err := instances.worker.UseSecurityGroups(port); err != nil {
			return nil, nil, err
		}
	}
	if vms != nil {
		port := ownerSecurityGroups{store: repository, mapError: vmSecurityGroupError}
		if err := vms.service.UseSecurityGroups(port); err != nil {
			return nil, nil, err
		}
		if err := vms.worker.UseSecurityGroups(port); err != nil {
			return nil, nil, err
		}
	}
	service := networkservice.NewNetworkSecurityGroupService(networkbiz.NewSecurityGroups(repository))
	worker := networkbiz.NewSecurityGroupWorker(repository, provider, uuid.NewString(), policy)
	return service, worker, nil
}

var _ instancebiz.SecurityGroupPort = ownerSecurityGroups{}
var _ vmbiz.SecurityGroupPort = ownerSecurityGroups{}
