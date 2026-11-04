package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type preflightEnvironment struct {
	biz.EnvironmentPort
	namespaceErr, networkErr error
	namespaceUID             string
	changeNetwork            bool
	checks                   int
}

func (e *preflightEnvironment) ResolveNamespace(_ context.Context, _, name string) (biz.ObjectRef, error) {
	e.checks++
	return biz.ObjectRef{Name: name, UID: e.namespaceUID}, e.namespaceErr
}

func (e *preflightEnvironment) ValidateNetwork(_ context.Context, _, _, _ string, selection biz.NetworkSelection) (biz.NetworkSelection, error) {
	e.checks++
	if e.changeNetwork {
		selection.NADUID = "replacement-nad"
	}
	return selection, e.networkErr
}

func (e *preflightEnvironment) ValidateExistingNetwork(_ context.Context, _ biz.VM) error {
	e.checks++
	if e.changeNetwork {
		return biz.ErrChanged
	}
	return e.networkErr
}

type preflightNative struct {
	biz.NativePort
	backendErr error
}

func (n *preflightNative) ValidateBackendConfiguration(context.Context, biz.Spec) error {
	return n.backendErr
}

func (n *preflightNative) ObserveBackendState(context.Context, biz.VM, []biz.BackendState) ([]biz.BackendState, error) {
	panic("preflight must not observe runtime backend state")
}

func (n *preflightNative) BackendStateRemoved(context.Context, biz.VM, biz.BackendState) (bool, error) {
	panic("preflight must not remove backend state")
}

func TestVMCommandPreflightLogsSafeStageAndKeepsPublicError(t *testing.T) {
	const secret = "postgres://vm:secret-password@db/private?token=secret-token"
	for _, action := range []string{"start", "restart"} {
		for _, failure := range []struct {
			name, stage, reason string
			code                codes.Code
		}{
			{"namespace-read", "namespace", "DEADLINE_EXCEEDED", codes.Unavailable},
			{"namespace-identity", "namespace_identity", "VM_DEPENDENCY_CHANGED", codes.FailedPrecondition},
			{"backend", "backend_configuration", "VM_DEPENDENCY_NOT_READY", codes.Unavailable},
			{"network", "network", "VM_DEPENDENCY_NOT_READY", codes.Unavailable},
			{"network-identity", "network_identity", "VM_DEPENDENCY_CHANGED", codes.FailedPrecondition},
		} {
			t.Run(action+"/"+failure.name, func(t *testing.T) {
				var output bytes.Buffer
				old := slog.Default()
				slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
				t.Cleanup(func() { slog.SetDefault(old) })
				tenant, id, cluster := uuid.NewString(), uuid.NewString(), uuid.NewString()
				v := biz.VM{ID: id, TenantID: tenant, Version: 2, Spec: biz.Spec{Namespace: "tenant-" + tenant, NamespaceUID: "namespace-uid", ClusterID: cluster, Canonical: []byte(secret), Media: biz.Source{PersistentUEFI: true}, Network: biz.NetworkSelection{VPCID: "vpc-a", SubnetID: "subnet-a", NADUID: "nad-uid"}}}
				repo := &snapshotServiceRepo{current: v}
				environment := &preflightEnvironment{namespaceUID: v.Spec.NamespaceUID}
				native := &preflightNative{}
				switch failure.name {
				case "namespace-read":
					environment.namespaceErr = errors.Join(context.DeadlineExceeded, errors.New(secret))
				case "namespace-identity":
					environment.namespaceUID = "replacement-namespace"
				case "backend":
					native.backendErr = errors.New(secret)
				case "network":
					environment.networkErr = errors.New(secret)
				case "network-identity":
					environment.changeNetwork = true
				}
				service, err := NewService(repo, dnsCompute{}, environment, native, Config{ClusterID: cluster, ServiceAccount: "workload", StorageClass: "block", StorageClassUID: "sc-uid"})
				if err != nil {
					t.Fatal(err)
				}
				ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: "user:1"})
				request := &pb.ChangeVMRequest{TenantId: tenant, VmId: id, IdempotencyKey: secret, ExpectedVersion: v.Version}
				if action == "start" {
					_, err = service.Start(ctx, request)
				} else {
					_, err = service.Restart(ctx, request)
				}
				if status.Code(err) != failure.code || repo.accepts != 0 {
					t.Fatalf("preflight changed admission: error=%v accepts=%d", err, repo.accepts)
				}
				if failure.code == codes.Unavailable && status.Convert(err).Message() != "VM_DEPENDENCY_NOT_READY" {
					t.Fatalf("public dependency error changed: %v", err)
				}
				if strings.Contains(output.String(), secret) || strings.Contains(err.Error(), secret) || strings.Contains(output.String(), "secret-password") || strings.Contains(output.String(), "secret-token") {
					t.Fatal("preflight exposed credentials or full spec")
				}
				var record map[string]any
				if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
					t.Fatalf("missing structured preflight failure: %v; log=%q", err, output.String())
				}
				if record["stage"] != failure.stage || record["reason"] != failure.reason || record["action"] != action || record["tenant_id"] != tenant || record["vm_id"] != id {
					t.Fatalf("preflight failure cannot be located: %+v", record)
				}
			})
		}
	}
}

func TestVMCommandPreflightKeepsAuthorizationAndReplayAheadOfChecks(t *testing.T) {
	tenant, id, cluster := uuid.NewString(), uuid.NewString(), uuid.NewString()
	repo := &snapshotServiceRepo{current: biz.VM{ID: id, TenantID: tenant}}
	environment := &preflightEnvironment{}
	service, err := NewService(repo, dnsCompute{}, environment, &preflightNative{}, Config{ClusterID: cluster, ServiceAccount: "workload", StorageClass: "block", StorageClassUID: "sc-uid"})
	if err != nil {
		t.Fatal(err)
	}
	request := &pb.ChangeVMRequest{TenantId: tenant, VmId: id, IdempotencyKey: "start", ExpectedVersion: 2}
	if _, err = service.Start(context.Background(), request); status.Code(err) != codes.PermissionDenied || environment.checks != 0 || repo.reads != 0 {
		t.Fatalf("unauthorized command reached dependencies: %v", err)
	}
	raw, err := pb.CanonicalCommandIntent(request)
	if err != nil {
		t.Fatal(err)
	}
	repo.digest = pb.Digest(raw)
	repo.replay = &biz.Operation{ID: uuid.NewString(), VMID: id, Action: "start", IntentVersion: 3, State: "accepted"}
	ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: "user:1"})
	if operation, err := service.Start(ctx, request); err != nil || operation.Id != repo.replay.ID || environment.checks != 0 || repo.accepts != 0 {
		t.Fatalf("accepted replay reopened dependencies: %v %+v", err, operation)
	}
}
