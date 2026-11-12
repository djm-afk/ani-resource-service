//go:build instanceintegration

package instance_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	acc "github.com/zhangzhe-ctrl/ani-accelerator-service/api/gen/go/accelerator/v1"
	cpb "github.com/zhangzhe-ctrl/ani-resource-service/api/compute/v1"
	ipb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	compute "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/compute"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	cservice "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/compute"
	iservice "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/instance"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// The repository/transactions, handlers and authenticated listener are real.
// Only image metadata and workload-environment dependencies are controlled.
type observedCatalog struct {
	compute.PresetRepository
	reads atomic.Int32
}

func (r *observedCatalog) GetPreset(ctx context.Context, id string) (compute.Preset, error) {
	r.reads.Add(1)
	return r.PresetRepository.GetPreset(ctx, id)
}

type observedImage struct{ calls atomic.Int32 }

func (r *observedImage) Resolve(ctx context.Context, tenant, id, scope string) (biz.ResolvedImage, error) {
	r.calls.Add(1)
	return (fixtureImage{}).Resolve(ctx, tenant, id, scope)
}

type observedEnvironment struct{ calls atomic.Int32 }

func (r *observedEnvironment) ValidateEnvironment(ctx context.Context, tenant, namespace, sa, storage string) error {
	r.calls.Add(1)
	return nil
}

func TestComputeRPCUsesTrustedScopeAndPersistentCatalog(t *testing.T) {
	f := database(t)
	tenant, cluster, requestID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	observed := &observedCatalog{PresetRepository: f.presets}
	resolver, e := compute.NewResolver(observed, cluster)
	if e != nil {
		t.Fatal(e)
	}
	catalog, e := cservice.NewService(f.presets, resolver, cluster)
	if e != nil {
		t.Fatal(e)
	}
	images, environment := &observedImage{}, &observedEnvironment{}
	instances, e := iservice.NewService(f.repo, resolver, images, environment, iservice.Config{NamespacePrefix: "tenant-", ClusterID: cluster, ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", Shell: "/bin/sh", UID: 1000, GID: 1000})
	if e != nil {
		t.Fatal(e)
	}
	identity := testenv.NewServiceTLS(t)
	tls, e := server.GovernanceTLS(identity.CAFile, identity.CertFile, identity.KeyFile)
	if e != nil {
		t.Fatal(e)
	}
	transport, e := server.NewGRPCServer(&conf.Server_GRPC{Network: "tcp", Addr: "127.0.0.1:0", Timeout: durationpb.New(5 * time.Second)}, server.NewReadiness(), tls, false, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if e != nil {
		t.Fatal(e)
	}
	cpb.RegisterComputePresetServiceServer(transport, catalog)
	ipb.RegisterInstanceServiceServer(transport, instances)
	endpoint, e := transport.Endpoint()
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- transport.Start(context.Background()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if e := transport.Stop(ctx); e != nil {
			t.Error(e)
		}
		if e := <-done; e != nil {
			t.Error(e)
		}
	})
	conn, e := grpc.NewClient(endpoint.Host, grpc.WithTransportCredentials(credentials.NewTLS(identity.Client)))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { conn.Close() })
	client, instanceClient := cpb.NewComputePresetServiceClient(conn), ipb.NewInstanceServiceClient(conn)
	platform := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-ani-scope", "platform", "x-ani-actor", "governance:user:42", "x-ani-request-id", requestID))
	tenantContext := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-ani-tenant-id", tenant, "x-ani-actor", "governance:user:42", "x-ani-request-id", requestID))
	var cpu, gpu *cpb.Preset
	for _, kind := range []string{"cpu", "gpu"} {
		t.Run(kind, func(t *testing.T) {
			data := &cpb.PresetData{Code: kind, Name: kind + " candidate", ComputeType: kind, CpuMillis: 1250, MemoryMib: 1536}
			if kind == "gpu" {
				data.Gpu = &cpb.GpuPresetReference{ProfileId: uuid.NewString(), ProfileVersion: 2, SpecDigest: strings.Repeat("a", 64), DevicesPerInstance: 1, ClusterId: cluster, PoolId: uuid.NewString(), Mode: acc.SupplyMode_SHARED_FIXED, ModelKey: "t4", MemoryMibPerDevice: 8192, CoreLimitPercent: 25}
			}
			p, e := client.AdminCreatePreset(platform, &cpb.AdminCreatePresetRequest{Data: data})
			if e != nil || p.Enabled {
				t.Fatal("persistent create/default disabled", p, e)
			}
			if _, e = client.ResolveComputePreset(tenantContext, &cpb.ResolveComputePresetRequest{TenantId: tenant, PresetId: p.Id, Usage: cpb.Usage_TRAINING}); status.Code(e) != codes.FailedPrecondition {
				t.Fatal("disabled preset resolved", e)
			}
			yes := true
			p, e = client.AdminSetPresetEnabled(platform, &cpb.AdminSetPresetEnabledRequest{Id: p.Id, Enabled: &yes})
			if e != nil {
				t.Fatal(e)
			}
			var original *cpb.ComputeSelection
			for _, usage := range []cpb.Usage{cpb.Usage_CONTAINER_INSTANCE, cpb.Usage_TRAINING, cpb.Usage_INFERENCE} {
				before := observed.reads.Load()
				selection, e := client.ResolveComputePreset(tenantContext, &cpb.ResolveComputePresetRequest{TenantId: tenant, PresetId: p.Id, Usage: usage})
				if e != nil || cpb.ValidateSelection(selection) != nil || selection.CpuMillis != 1250 || selection.MemoryMib != 1536 || selection.ComputeType != kind || observed.reads.Load() != before+1 {
					t.Fatal("single persistent compute resolution", selection, e)
				}
				if kind == "gpu" && (selection.Gpu == nil || selection.Gpu.DevicesPerUnit != 1 || selection.Gpu.ProfileId != data.Gpu.ProfileId || selection.Gpu.ClusterId != cluster) {
					t.Fatal("GPU reference/units changed", selection)
				}
				if original != nil && !proto.Equal(original, selection) {
					t.Fatal("usage changed quantitative selection")
				}
				original = selection
			}
			before := observed.reads.Load()
			raw := &ipb.CreateInstanceData{Name: "workspace", ImageId: "img_" + strings.Repeat("a", 32), ImageScope: "platform", PresetId: p.Id}
			spec, e := instanceClient.ResolveCreateSpec(tenantContext, &ipb.ResolveCreateSpecRequest{TenantId: tenant, Data: raw})
			if e != nil || observed.reads.Load() != before+1 || spec.Cpu != "1250m" || spec.Memory != "1536Mi" || spec.PresetCode != p.Code || spec.PresetName != p.Name || spec.WorkspaceMountPath != "/workspace" {
				t.Fatal("container did not reuse one compute read", spec, e)
			}
			expected := &ipb.CreateSpec{Name: raw.Name, ImageId: raw.ImageId, ImageScope: raw.ImageScope, PresetId: p.Id, ImageReference: "registry.invalid/work@sha256:" + strings.Repeat("a", 64), Cpu: "1250m", Memory: "1536Mi", ClusterId: cluster, Namespace: "tenant-" + tenant, ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", Shell: "/bin/sh", Uid: 1000, Gid: 1000, InstanceKind: "container", ComputeType: kind, Scheduling: &ipb.SchedulingOptions{PodPlacement: ipb.SchedulingOptions_NONE.Enum()}, PresetName: p.Name, PresetCode: p.Code, WorkspaceMountPath: "/workspace"}
			if g := data.Gpu; g != nil {
				expected.GpuSelection = &ipb.GpuPresetReference{ProfileId: g.ProfileId, ProfileVersion: g.ProfileVersion, SpecDigest: g.SpecDigest, DevicesPerInstance: g.DevicesPerInstance, ClusterId: g.ClusterId, PoolId: g.PoolId, Mode: g.Mode, ModelKey: g.ModelKey, MemoryMibPerDevice: g.MemoryMibPerDevice, CoreLimitPercent: g.CoreLimitPercent}
			}
			actualCanonical, e := ipb.CanonicalBusinessPayload(spec)
			if e != nil {
				t.Fatal(e)
			}
			expectedCanonical, e := ipb.CanonicalBusinessPayload(expected)
			if e != nil || string(actualCanonical) != string(expectedCanonical) {
				t.Fatal("active v2 canonical bytes changed", e)
			}
			actualHash, e := ipb.BusinessPayloadDigest(spec)
			if e != nil {
				t.Fatal(e)
			}
			expectedHash, e := ipb.BusinessPayloadDigest(expected)
			if e != nil || actualHash != expectedHash {
				t.Fatal("active v2 digest changed", e)
			}
			if kind == "cpu" {
				cpu = p
			} else {
				gpu = p
			}
		})
	}
	if cpu == nil || gpu == nil {
		t.Fatal("CPU/GPU setup failed")
	}
	if images.calls.Load() != 2 || environment.calls.Load() != 2 {
		t.Fatal("public compute resolution invoked container dependencies")
	}
	page, e := client.ListPresets(tenantContext, &cpb.ListPresetsRequest{TenantId: tenant, Usage: cpb.Usage_CONTAINER_INSTANCE, PageSize: 1})
	if e != nil || page.Total != 1 || !page.HasGpuCandidates || len(page.Items) != 1 || page.Items[0].Id != cpu.Id {
		t.Fatal("GPU empty intersection before count/page", page, e)
	}
	page, e = client.ListPresets(tenantContext, &cpb.ListPresetsRequest{TenantId: tenant, Usage: cpb.Usage_TRAINING, Page: 2, PageSize: 1, AllowedGpuProfiles: []*cpb.GpuPresetReference{gpu.Gpu}})
	if e != nil || page.Total != 2 || len(page.Items) != 1 {
		t.Fatal("GPU allowed intersection before count/page", page, e)
	}
	if _, e = client.AdminUpdatePreset(platform, &cpb.AdminUpdatePresetRequest{Id: cpu.Id, Data: &cpb.PresetData{Name: "renamed"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"name"}}}); e != nil {
		t.Fatal(e)
	}
	got, e := client.AdminGetPreset(platform, &cpb.AdminGetPresetRequest{Id: cpu.Id})
	if e != nil || got.Name != "renamed" || got.CpuMillis != 1250 {
		t.Fatal("FieldMask update lost final object", got, e)
	}
	if page, e = client.AdminListPresets(platform, &cpb.AdminListPresetsRequest{}); e != nil || page.Total != 2 {
		t.Fatal("platform list", page, e)
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		req  *cpb.ResolveComputePresetRequest
		code codes.Code
	}{
		{"missing identity", context.Background(), &cpb.ResolveComputePresetRequest{TenantId: tenant, PresetId: cpu.Id, Usage: cpb.Usage_CONTAINER_INSTANCE}, codes.Unauthenticated},
		{"platform resolve", platform, &cpb.ResolveComputePresetRequest{TenantId: tenant, PresetId: cpu.Id, Usage: cpb.Usage_CONTAINER_INSTANCE}, codes.PermissionDenied},
		{"cross tenant", tenantContext, &cpb.ResolveComputePresetRequest{TenantId: uuid.NewString(), PresetId: cpu.Id, Usage: cpb.Usage_CONTAINER_INSTANCE}, codes.PermissionDenied},
		{"unknown usage", tenantContext, &cpb.ResolveComputePresetRequest{TenantId: tenant, PresetId: cpu.Id, Usage: cpb.Usage(99)}, codes.InvalidArgument},
		{"unspecified usage", tenantContext, &cpb.ResolveComputePresetRequest{TenantId: tenant, PresetId: cpu.Id}, codes.InvalidArgument},
		{"missing preset", tenantContext, &cpb.ResolveComputePresetRequest{TenantId: tenant, PresetId: uuid.NewString(), Usage: cpb.Usage_INFERENCE}, codes.NotFound},
		{"duplicate actor", metadata.AppendToOutgoingContext(tenantContext, "x-ani-actor", "governance:user:43"), &cpb.ResolveComputePresetRequest{TenantId: tenant, PresetId: cpu.Id, Usage: cpb.Usage_TRAINING}, codes.Unauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, e := client.ResolveComputePreset(tc.ctx, tc.req); status.Code(e) != tc.code {
				t.Fatal(e, tc.code)
			}
		})
	}
	if _, e = client.AdminDeletePreset(tenantContext, &cpb.AdminDeletePresetRequest{Id: cpu.Id}); status.Code(e) != codes.PermissionDenied {
		t.Fatal("tenant managed catalog", e)
	}
	for _, name := range []string{"AdminListPresets", "AdminGetPreset", "AdminCreatePreset", "AdminUpdatePreset", "AdminSetPresetEnabled", "AdminDeletePreset", "ListPresets"} {
		if e := conn.Invoke(tenantContext, "/instance.v1.InstanceService/"+name, &emptypb.Empty{}, &emptypb.Empty{}); status.Code(e) != codes.PermissionDenied {
			t.Fatal("old RPC remained available", name, e)
		}
	}
	if _, e = client.AdminDeletePreset(platform, &cpb.AdminDeletePresetRequest{Id: cpu.Id}); e != nil {
		t.Fatal(e)
	}
	if _, e = client.ResolveComputePreset(tenantContext, &cpb.ResolveComputePresetRequest{TenantId: tenant, PresetId: cpu.Id, Usage: cpb.Usage_TRAINING}); status.Code(e) != codes.NotFound {
		t.Fatal("deleted preset still resolves", e)
	}
}
