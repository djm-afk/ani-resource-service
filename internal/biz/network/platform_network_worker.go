package biz

import "context"

// PlatformNetworkWorkSpec is an imported original identity, separate from
// operation-owned platform resources. The worker has no mutation path for it.
type PlatformNetworkWorkSpec struct {
	Kind, ClusterID, Namespace, Name, UID string
}

type PlatformNetworkFacts struct {
	CIDR, Gateway string
	SubnetOptions
	SubnetIPRanges
}

func (w *Worker) stepPlatformNetwork(ctx context.Context, work Work) (err error) {
	progress := Progress{State: work.Resource.State, Reason: work.Resource.Reason, Identity: work.KnownIdentity}
	defer func() {
		if w.observer != nil {
			w.observer(ctx, work, progress, err)
		}
	}()
	callCtx, cancel := context.WithTimeout(ctx, w.policy.RequestTimeout)
	observed, readErr := w.provider.Observe(callCtx, ProviderTarget{ResourceID: work.Resource.ID, Kind: work.Resource.Kind, KnownIdentity: work.KnownIdentity, PlatformNetwork: work.PlatformNetwork})
	cancel()
	if readErr != nil {
		progress.ProviderError = readErr
		progress.ObservationError = observationReason(readErr)
		progress.Backoff = true
	} else if !observed.Exists {
		progress.ObservationError = ProviderMissing
		progress.Backoff = true
	} else if observed.Identity != work.KnownIdentity || observed.Identity == "" {
		progress.ObservationError = ProviderOwnership
		progress.Backoff = true
	} else {
		progress.Observed = true
		progress.PlatformNetwork = observed.PlatformNetwork
		if observed.Ready != nil {
			if *observed.Ready {
				progress.State, progress.Reason = Available, ""
			} else {
				progress.State = Provisioning
				if work.Resource.State == Available || work.Resource.State == Degraded {
					progress.State = Degraded
				}
				progress.Reason = observed.StatusReason
				if progress.Reason == "" {
					progress.Reason = ProviderNotReady
				}
			}
		}
		if observed.Ready == nil {
			progress.Reason = observed.StatusReason
			if progress.Reason == "" {
				progress.Reason = ProviderStatusUnreported
			}
		}
		// A successful Unknown/False is a completed read, not a poll condition.
	}
	if progress.Backoff {
		progress.NextDelay = w.retryDelay(work.Attempt + 1)
	}
	return w.finish(ctx, work, progress)
}
