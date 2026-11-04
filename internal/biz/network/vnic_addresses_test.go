package biz

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestVNicRequestedAddresses(t *testing.T) {
	ip, mac, err := normalizeVNicAddresses(" 10.42.1.71 ", "02-AB-CD-EF-00-71")
	if err != nil || ip != "10.42.1.71" || mac != "02:ab:cd:ef:00:71" {
		t.Fatal(ip, mac, err)
	}
	for _, invalid := range []struct{ ip, mac string }{
		{"::1", ""}, {"10.42.1.999", ""}, {"", "01:00:00:00:00:01"}, {"", "00:00:00:00:00:00"}, {"", "02:00:00:01"},
	} {
		if _, _, err = normalizeVNicAddresses(invalid.ip, invalid.mac); ReasonOf(err) != InvalidArgument {
			t.Fatalf("accepted invalid address %+v: %v", invalid, err)
		}
	}
	for _, invalid := range []string{"10.42.2.71", "10.42.1.0", "10.42.1.255", "10.42.1.1"} {
		if err = ValidateVNicRequestedIP(invalid, "10.42.1.0/24", "10.42.1.1"); ReasonOf(err) != InvalidArgument {
			t.Fatalf("accepted unavailable Subnet address %s: %v", invalid, err)
		}
	}
	if err = ValidateVNicRequestedIP(ip, "10.42.1.0/24", "10.42.1.1"); err != nil {
		t.Fatal(err)
	}
	automatic := CreateVNic{TenantID: "tenant", Name: "nic", SubnetID: "subnet"}
	custom := automatic
	custom.RequestedIP, custom.RequestedMAC = ip, mac
	if automatic.Fingerprint() == custom.Fingerprint() {
		t.Fatal("requested addresses absent from idempotency intent")
	}
}

type addressWorkerRepository struct {
	WorkRepository
	work     Work
	progress Progress
	mutation string
}

func (r *addressWorkerRepository) Claim(context.Context, string, time.Duration) (Work, bool, error) {
	return r.work, true, nil
}
func (r *addressWorkerRepository) BeginMutation(_ context.Context, _ Work, action, _ string) error {
	r.mutation = action
	return nil
}
func (r *addressWorkerRepository) Finish(_ context.Context, _ Work, p Progress) error {
	r.progress = p
	return nil
}

type addressWorkerProvider struct {
	ResourceProvider
	observation ProviderObservation
	calls       int
	target      ProviderTarget
}

func (p *addressWorkerProvider) Observe(context.Context, ProviderTarget) (ProviderObservation, error) {
	return p.observation, nil
}
func (p *addressWorkerProvider) EnsureVNic(_ context.Context, target ProviderTarget) (ProviderObservation, error) {
	p.calls++
	p.target = target
	return ProviderObservation{Exists: true, Identity: "original-vnic", Ready: reportedReady(true)}, nil
}

func TestVNicWorkerResumesOwnDeclarationAndUsesOnlyParentReady(t *testing.T) {
	for _, needsDeclaration := range []bool{false, true} {
		r := &addressWorkerRepository{work: Work{Now: time.Now(), ActiveOperation: true, KnownIdentity: "original-vnic", Resource: ResourceWork{Kind: "vnic", State: Provisioning, RequestedIP: "10.42.1.71", RequestedMAC: "02:ab:cd:ef:00:71"}, Operation: Operation{Kind: "create_vnic"}}}
		p := &addressWorkerProvider{observation: ProviderObservation{Exists: true, Identity: "original-vnic", Ready: reportedReady(false), StatusReason: Reason("Waiting"), NeedsUpdate: needsDeclaration}}
		w, err := NewWorker(r, p, uuid.NewString(), DefaultWorkerPolicy())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
		if needsDeclaration {
			if r.progress.State != Available || r.progress.OperationState != Succeeded || r.mutation != "update" || p.calls != 1 || p.target.RequestedIP != r.work.Resource.RequestedIP {
				t.Fatal(r.progress, p.target, p.calls)
			}
		} else if r.progress.State != Provisioning || r.progress.OperationState == OpFailed || p.calls != 0 {
			t.Fatal("Ready=False became a Res terminal failure", r.progress, p.calls)
		}
	}
}

func TestVNicWorkerCommitsParentBeforeResumingRequestedIP(t *testing.T) {
	r := &addressWorkerRepository{work: Work{Now: time.Now(), ActiveOperation: true, PendingAction: "create", Resource: ResourceWork{Kind: "vnic", State: Provisioning, RequestedIP: "10.42.1.71"}, Operation: Operation{Kind: "create_vnic"}}}
	p := &addressWorkerProvider{observation: ProviderObservation{Exists: true, Identity: "original-vnic", NeedsUpdate: true}}
	w, err := NewWorker(r, p, uuid.NewString(), DefaultWorkerPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.calls != 0 || r.mutation != "" || !r.progress.ClearPending || r.progress.Identity != "original-vnic" || !r.progress.Observed || r.progress.State != Provisioning {
		t.Fatal("child mutation began before original parent UID was committed", r.progress, p.calls)
	}
}
