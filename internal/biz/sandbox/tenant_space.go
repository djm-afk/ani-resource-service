package sandbox

import "context"

const TenantSpaceActor = "governance:tenant-space"

func RequireTenantSpaceController(ctx context.Context, tenant string) error {
	if err := RequireTenant(ctx, tenant); err != nil {
		return err
	}
	caller, _ := ctx.Value(callerKey{}).(Caller)
	if caller.Actor != TenantSpaceActor {
		return ErrDenied
	}
	return nil
}

// TenantSpace is the durable fence for shared namespace provisioning. A closed
// record is a tombstone, not proof that the shared namespace was deleted.
type TenantSpace struct {
	TenantID, OperationID, Phase string
	PendingID, PendingKind       string
	NamespaceUID                 string
}

// TenantSpaceMutation is a journaled single provider write. Dispatch is true
// only for the caller that committed a new pending record; replay never grants
// another permission to send a potentially late Kubernetes write.
type TenantSpaceMutation struct {
	ID, Kind, Outcome, Receipt string
	Dispatch                   bool
}

type TenantSpaceJournal interface {
	OpenTenantSpace(context.Context, string, string) (TenantSpace, error)
	CloseTenantSpace(context.Context, string, string) (TenantSpace, error)
	GetTenantSpace(context.Context, string) (TenantSpace, error)
	PinTenantSpaceNamespace(context.Context, string, string, string) (TenantSpace, error)
	BeginTenantSpaceMutation(context.Context, string, string, string, string) (TenantSpaceMutation, error)
	FinishTenantSpaceMutation(context.Context, string, string, string, string, string) (TenantSpaceMutation, error)
}
