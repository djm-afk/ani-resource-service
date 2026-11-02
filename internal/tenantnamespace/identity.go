// Package tenantnamespace is the pure identity contract for pre-existing tenant
// Namespaces. It performs no I/O and never creates, adopts or repairs objects.
package tenantnamespace

import (
	"errors"
	"github.com/google/uuid"
)

const (
	Prefix         = "tenant-"
	TenantLabel    = "network.ani.io/tenant-id"
	ManagedByLabel = "network.ani.io/managed-by"
	Owner          = "ani-network-service"
)

var (
	ErrTenant     = errors.New("canonical nonzero resource tenant UUID required")
	ErrOwnership  = errors.New("TENANT_NAMESPACE_OWNERSHIP_MISMATCH")
	ErrNotActive  = errors.New("TENANT_NAMESPACE_NOT_ACTIVE")
	ErrUIDChanged = errors.New("TENANT_NAMESPACE_UID_CHANGED")
)

type Identity struct{ TenantID, ClusterID, Name, UID string }
type Object struct {
	Name, UID, Phase string
	Labels           map[string]string
	Deleting         bool
}

func Name(tenant string) (string, error) {
	id, err := uuid.Parse(tenant)
	if err != nil || id == uuid.Nil || id.String() != tenant {
		return "", ErrTenant
	}
	return Prefix + tenant, nil
}

// Validate checks ownership and original identity. Active is required for new
// acceptance; callers reading or closing original executions may pass false.
func Validate(tenant string, object Object, frozenUID string, active bool) error {
	name, err := Name(tenant)
	if err != nil {
		return err
	}
	if object.Name != name || object.UID == "" || object.Labels[TenantLabel] != tenant || object.Labels[ManagedByLabel] != Owner {
		return ErrOwnership
	}
	if frozenUID != "" && object.UID != frozenUID {
		return ErrUIDChanged
	}
	if active && (object.Phase != "Active" || object.Deleting) {
		return ErrNotActive
	}
	return nil
}
