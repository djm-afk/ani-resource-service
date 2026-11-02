package sandbox

import "context"

// TenantSpaceWorkRepository schedules accepted space operations and commits
// readiness. Journal methods separately fence every provider write.
type TenantSpaceWorkRepository interface {
	TenantSpaceJournal
	ClaimTenantSpace(context.Context) (TenantSpace, error)
	MarkTenantSpaceReady(context.Context, string, string, string) error
}

type TenantSpaceProvisioner interface {
	ProvisionTenantSpace(context.Context, string, string, TenantSpaceJournal) (string, error)
}

type TenantSpaceWorker struct {
	repo     TenantSpaceWorkRepository
	provider TenantSpaceProvisioner
}

func NewTenantSpaceWorker(repo TenantSpaceWorkRepository, provider TenantSpaceProvisioner) (*TenantSpaceWorker, error) {
	if repo == nil || provider == nil {
		return nil, ErrInvalid
	}
	return &TenantSpaceWorker{repo: repo, provider: provider}, nil
}

func (w *TenantSpaceWorker) Step(ctx context.Context) (bool, error) {
	space, err := w.repo.ClaimTenantSpace(ctx)
	if err == ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	ctx = WithCaller(ctx, Caller{TenantID: space.TenantID, Actor: TenantSpaceActor})
	uid, err := w.provider.ProvisionTenantSpace(ctx, space.TenantID, space.OperationID, w.repo)
	if err != nil {
		return true, err
	}
	return true, w.repo.MarkTenantSpaceReady(ctx, space.TenantID, space.OperationID, uid)
}
