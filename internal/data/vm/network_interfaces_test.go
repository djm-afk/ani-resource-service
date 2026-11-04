package vm

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	network "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

type interfaceBindingCall struct {
	tenant, kind, consumer, id string
	revision                   int64
	keep                       []string
}

// Only persistence is replaced: the adapter calls the real Network use cases,
// including their tenant and applied-revision validation.
type interfaceBindingRepository struct {
	network.NetworkRepository
	network.VNicRepository
	applied, released    []interfaceBindingCall
	applyErr, releaseErr error
}

func (r *interfaceBindingRepository) ApplyVNicBinding(_ context.Context, tenant, kind, consumer, id string, revision int64) error {
	r.applied = append(r.applied, interfaceBindingCall{tenant: tenant, kind: kind, consumer: consumer, id: id, revision: revision})
	return r.applyErr
}

func (r *interfaceBindingRepository) ReleaseVNicBindings(_ context.Context, tenant, kind, consumer string, keep []string) error {
	r.released = append(r.released, interfaceBindingCall{tenant: tenant, kind: kind, consumer: consumer, keep: append([]string(nil), keep...)})
	return r.releaseErr
}

func TestReleaseNetworkInterfacesPreservesBindingValidation(t *testing.T) {
	const tenant = "d3aafc08-88b2-45ad-9d2b-5679c917e0b1"
	const consumer = "db4388f9-49a5-4d24-93d0-333be79fb3c4"
	const manualID = "vnic_11111111111111111111111111111111"
	automatic := biz.NetworkInterfaceChoice{Mode: "automatic"}
	manual := biz.NetworkInterfaceChoice{Mode: "manual", VNicID: manualID}
	configured := func(choice biz.NetworkInterfaceChoice, revision int64) []biz.NetworkInterface {
		return []biz.NetworkInterface{{Slot: "primary", Desired: choice, Applied: choice, RequestedRevision: revision, AppliedRevision: revision}}
	}
	pending := configured(manual, 3)
	pending[0].Desired = automatic
	pending[0].RequestedRevision = 4
	applyFailure := errors.New("binding apply failed")
	releaseFailure := errors.New("binding release failed")
	for _, tc := range []struct {
		name                 string
		interfaces           []biz.NetworkInterface
		applyErr, releaseErr error
		wantApply            int
		wantRelease          int
		wantReason           network.Reason
		wantErr              error
	}{
		{name: "unconfigured nil", wantRelease: 1},
		{name: "unconfigured empty", interfaces: []biz.NetworkInterface{}, wantRelease: 1},
		{name: "configured automatic", interfaces: configured(automatic, 4), wantApply: 1, wantRelease: 1},
		{name: "configured manual", interfaces: configured(manual, 3), wantApply: 1, wantRelease: 1},
		{name: "invalid automatic revision", interfaces: configured(automatic, 0), wantReason: network.InvalidArgument},
		{name: "invalid manual revision", interfaces: configured(manual, 0), wantReason: network.InvalidArgument},
		{name: "pending restart", interfaces: pending, wantRelease: 1},
		{name: "apply failure", interfaces: configured(manual, 3), applyErr: applyFailure, wantApply: 1, wantErr: applyFailure},
		{name: "release failure", releaseErr: releaseFailure, wantRelease: 1, wantErr: releaseFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &interfaceBindingRepository{applyErr: tc.applyErr, releaseErr: tc.releaseErr}
			port, err := network.NewNetwork(repo, []byte(strings.Repeat("k", 32)), time.Minute, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			adapter := &Network{}
			if err := adapter.UseManualVNics(port); err != nil {
				t.Fatal(err)
			}
			keep := []string{manualID}
			err = adapter.ReleaseNetworkInterfaces(context.Background(), biz.VM{TenantID: tenant, ID: consumer, NetworkInterfaces: tc.interfaces}, keep)
			if tc.wantReason != "" {
				if network.ReasonOf(err) != tc.wantReason {
					t.Fatalf("want %s, got %v", tc.wantReason, err)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Fatalf("want error %v, got %v", tc.wantErr, err)
			}
			if len(repo.applied) != tc.wantApply || len(repo.released) != tc.wantRelease {
				t.Fatalf("apply/release calls = %d/%d, want %d/%d", len(repo.applied), len(repo.released), tc.wantApply, tc.wantRelease)
			}
			if len(repo.applied) != 0 {
				cfg := biz.PrimaryNetworkInterface(biz.VM{NetworkInterfaces: tc.interfaces})
				want := interfaceBindingCall{tenant: tenant, kind: "vm", consumer: consumer, id: cfg.Applied.VNicID, revision: cfg.AppliedRevision}
				if !reflect.DeepEqual(repo.applied[0], want) {
					t.Fatalf("applied binding = %+v, want %+v", repo.applied[0], want)
				}
			}
			if len(repo.released) != 0 {
				want := interfaceBindingCall{tenant: tenant, kind: "vm", consumer: consumer, keep: keep}
				if !reflect.DeepEqual(repo.released[0], want) {
					t.Fatalf("released binding = %+v, want %+v", repo.released[0], want)
				}
			}
		})
	}
}
