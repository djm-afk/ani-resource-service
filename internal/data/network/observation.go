package data

import (
	"context"
	"fmt"
	"strings"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func (p *Postgres) NotifyResource(ctx context.Context, tenant, id string) error {
	_, err := p.queries.NotifyResource(ctx, sqlcgen.NotifyResourceParams{TenantID: tenant, ResourceID: id})
	return databaseFailure(err)
}

func (p *Postgres) NotifyAttachment(ctx context.Context, tenant, id string) error {
	_, err := p.queries.NotifyAttachment(ctx, sqlcgen.NotifyAttachmentParams{TenantID: tenant, AttachmentID: id})
	return databaseFailure(err)
}

// NotifyObservedObject only resolves exact keys through current Res bindings.
// Repeated notifications are harmless queue requests, not product mutations.
func (p *Postgres) NotifyObservedObject(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) error {
	targets, err := p.queries.LookupObservationTargets(ctx, sqlcgen.LookupObservationTargetsParams{
		ClusterID: p.placement.ClusterID, ApiGroup: gvr.Group, Resource: gvr.Resource, Namespace: namespace, Name: name,
	})
	if err != nil {
		return databaseFailure(err)
	}
	for _, target := range targets {
		switch target.TargetType {
		case "resource":
			_, err = p.queries.NotifyResource(ctx, sqlcgen.NotifyResourceParams{TenantID: target.TenantID, ResourceID: target.TargetID})
		case "platform_network":
			_, err = p.queries.NotifyPlatformNetwork(ctx, sqlcgen.NotifyPlatformNetworkParams{ResourceID: target.TargetID})
		case "platform":
			_, err = p.queries.NotifyPlatform(ctx, sqlcgen.NotifyPlatformParams{ResourceID: target.TargetID})
		case "attachment":
			_, err = p.queries.NotifyAttachment(ctx, sqlcgen.NotifyAttachmentParams{TenantID: target.TenantID, AttachmentID: target.TargetID})
		case "virtual_switch":
			_, err = p.queries.NotifyVirtualSwitch(ctx, sqlcgen.NotifyVirtualSwitchParams{ResourceID: target.TargetID})
		case "default_egress":
			_, err = p.queries.NotifyDefaultEgress(ctx, sqlcgen.NotifyDefaultEgressParams{ClusterID: p.placement.ClusterID, ResourceID: target.TargetID})
		case "sg_security_group", "sg_eip", "sg_container", "sg_vm", "sg_vnic":
			_, err = p.queries.NotifySecurityGroupTarget(ctx, sqlcgen.NotifySecurityGroupTargetParams{TenantID: target.TenantID, TargetType: strings.TrimPrefix(target.TargetType, "sg_"), TargetID: target.TargetID})
		default:
			return fmt.Errorf("unknown network observation target type %q", target.TargetType)
		}
		if err != nil {
			return databaseFailure(err)
		}
	}
	return nil
}

// Startup recovers parked, persisted work once. It does not discover products
// from Kubernetes and cannot revive completed deleted/released lifecycles.
func (p *Postgres) WakeManagedTasks(ctx context.Context) error {
	return databaseFailure(p.queries.WakeManagedTasks(ctx, sqlcgen.WakeManagedTasksParams{ClusterID: p.placement.ClusterID}))
}
