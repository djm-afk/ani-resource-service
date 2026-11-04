package data

import (
	"errors"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

func TestCachedSnapshotPendingKeepsOriginalEvidenceTime(t *testing.T) {
	now := time.Now()
	options := DefaultObservationOptions()
	for _, tc := range []struct {
		name             string
		age              time.Duration
		missing, pending bool
	}{
		{name: "initial collection", missing: true, pending: true},
		{name: "fresh collection", age: time.Second},
		{name: "last accepted instant", age: options.AuditInterval + options.AuditJitter},
		{name: "next audit not published", age: options.AuditInterval + options.AuditJitter + time.Nanosecond, pending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collected := now.Add(-tc.age)
			o := &KCObservation{options: options}
			if !tc.missing {
				o.view = &auditView{collected: collected}
			}
			view, err := o.cachedSnapshot(now)
			if tc.pending {
				if view != nil || !errors.Is(err, biz.ErrObservationPending) || !errors.Is(readFailure(err), biz.ErrObservationPending) {
					t.Fatalf("local wait lost its type through Provider error wrapping: view=%+v, err=%v", view, err)
				}
			} else if err != nil || view != o.view {
				t.Fatalf("valid shared view was not reused: %+v %v", view, err)
			}
			if o.view != nil && !o.view.collected.Equal(collected) {
				t.Fatal("cache access renewed the original collection timestamp")
			}
		})
	}
}

func TestEgressAuditInvalidIsTypedSharedObservationWait(t *testing.T) {
	// readEgressSet compares this stable error identity for its bounded critical
	// retry; ordinary callers receive the same cause through readFailure.
	err := readFailure(errEgressAuditInvalid)
	var failure *biz.ProviderError
	if !errors.Is(err, biz.ErrObservationPending) || !errors.Is(err, errEgressAuditInvalid) || !errors.As(err, &failure) || failure.Kind != biz.ProviderTemporary {
		t.Fatal("invalid shared audit lost its local-wait classification", err)
	}
	if errors.Is(readFailure(errors.New("Kubernetes LIST failed")), biz.ErrObservationPending) {
		t.Fatal("real Provider error was classified as a shared audit wait")
	}
}
