package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	networkbiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	networkdata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/vm"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/vm"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

type vmSettings struct {
	DatabaseDSNFile, ConsoleAddress, GuestCredentialKeyFile string
	NetworkServerSocket                                     string
	Service                                                 service.Config
	Networks                                                []networkdata.ManagedVMNAD
}
type vmComponents struct {
	repo              *data.Postgres
	service           *service.Service
	worker            *biz.Worker
	observer          *biz.Observer
	observationWorker *server.VMObservationServer
	observation       *data.StateObservation
	console           *server.InstanceTerminalServer
	imageService      *service.ImageService
	imageWorker       *biz.ImageWorker
	referenceWorker   *biz.ReferenceWorker
	environment       biz.EnvironmentPort
}

// ImageLanes is the background lane list the composition root appends to the
// shared worker server, so no second scheduler framework appears. The reference
// lane runs even when uploads are disabled: an in-flight pin still has to settle.
func (c *vmComponents) ImageLanes() []server.Stepper {
	if c == nil || c.imageWorker == nil {
		return nil
	}
	return []server.Stepper{c.imageWorker, c.referenceWorker}
}

func (c *vmComponents) Close() {
	if c != nil && c.repo != nil {
		c.repo.Close()
	}
}
func runVMMigration() error {
	raw, e := readImagePrivateFile(os.Getenv("ANI_VM_OWNER_DSN_FILE"))
	if e != nil {
		return e
	}
	defer clear(raw)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return data.Migrate(ctx, strings.TrimSpace(string(raw)), os.Getenv("ANI_VM_RUNTIME_ROLE"))
}
func wireVM(ctx context.Context, c *conf.Network, compute *computeComponents, repository *networkdata.Postgres, provider *networkdata.KCProvider, attachments *networkbiz.Attachments, instances *instanceComponents, legacy networkbiz.InstanceConsumer, manualNetwork *networkbiz.Network) (*vmComponents, networkbiz.InstanceConsumer, error) {
	path := os.Getenv("ANI_VM_CONFIG_FILE")
	if path == "" {
		return nil, networkbiz.NewVMConsumerRouter(legacy, nil), nil
	}
	raw, e := readImagePrivateFile(path)
	if e != nil {
		return nil, nil, e
	}
	defer clear(raw)
	var cfg vmSettings
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e = d.Decode(&cfg); e != nil || d.Decode(new(any)) != io.EOF || compute == nil || !filepath.IsAbs(cfg.DatabaseDSNFile) || filepath.Clean(cfg.DatabaseDSNFile) != filepath.Clean(compute.settings.DatabaseDSNFile) || cfg.Service.ClusterID != c.ClusterId {
		return nil, nil, fmt.Errorf("VM requires managed compute database and cluster configuration")
	}
	if e = provider.ConfigureVMNetworks(cfg.Networks); e != nil {
		return nil, nil, e
	}
	if e = provider.ConfigureVMAutoNetwork(networkdata.VMAutoNetworkConfig{ServiceAccount: cfg.Service.ServiceAccount, ServerSocket: cfg.NetworkServerSocket}); e != nil {
		return nil, nil, e
	}
	if e = repository.SetVMNetworkResolver(provider); e != nil {
		return nil, nil, e
	}
	dsn, e := readImagePrivateFile(cfg.DatabaseDSNFile)
	if e != nil {
		return nil, nil, e
	}
	defer clear(dsn)
	repo, e := data.OpenPostgres(ctx, strings.TrimSpace(string(dsn)))
	if e != nil {
		return nil, nil, e
	}
	repo.UseNetworkNotifier(attachments.Notify)
	valid := false
	defer func() {
		if !valid {
			repo.Close()
		}
	}()
	if cfg.GuestCredentialKeyFile != "" {
		key, e := readImagePrivateFile(cfg.GuestCredentialKeyFile)
		if e != nil {
			return nil, nil, e
		}
		defer clear(key)
		if e = repo.ConfigureGuestCredentialKey(key); e != nil {
			return nil, nil, e
		}
	}
	clientConfig, e := clientcmd.BuildConfigFromFlags("", c.Kubeconfig)
	if e != nil {
		return nil, nil, e
	}
	clientConfig.Timeout = 10 * time.Second
	client, e := dynamic.NewForConfig(clientConfig)
	if e != nil {
		return nil, nil, e
	}
	native, e := data.NewNative(client, clientConfig)
	if e != nil {
		return nil, nil, e
	}
	native.SetGuestCredentialReader(repo.GuestPasswordHash)
	if e = native.ConfigureLogs(clientConfig, cfg.Service.LogLimits); e != nil {
		return nil, nil, e
	}
	if e = service.ValidateStaticMediaKeys(cfg.Service.Media); e != nil {
		return nil, nil, e
	}
	networkPort, e := data.NewNetwork(attachments, repo)
	if e != nil {
		return nil, nil, e
	}
	if e = networkPort.UseManualVNics(manualNetwork); e != nil {
		return nil, nil, e
	}
	access := vmNetworkAccess{repository: repository, provider: provider}
	repo.UseNetworkAdmission(access.accept)
	networkPort.UseTenantPreparation(access.prepare)
	environment, e := data.NewEnvironment(native, "tenant-", access.resolve)
	if e != nil {
		return nil, nil, e
	}
	environment.UseExistingNetworkValidator(access.validateExisting)
	handler, e := service.NewService(repo, compute.service, environment, native, cfg.Service)
	if e != nil {
		return nil, nil, e
	}
	handler.SetAddressReader(networkPort)
	runtimeReader, e := data.NewRuntimeReader(clientConfig)
	if e != nil {
		return nil, nil, e
	}
	handler.SetRuntimeReader(runtimeReader)
	handler.SetRelationsReader(data.NewDetailRelations(repository))
	if instances != nil && instances.service != nil {
		instances.service.SetVMMetricScope(func(ctx context.Context, tenant, id string) (string, string, bool, error) {
			v, e := repo.Get(ctx, tenant, id)
			if errors.Is(e, biz.ErrNotFound) {
				return "", "", false, nil
			}
			if e != nil {
				return "", "", false, e
			}
			return v.Spec.ClusterID, v.Spec.Namespace, true, nil
		})
	}
	worker, e := biz.NewWorker(repo, native, networkPort)
	if e != nil {
		return nil, nil, e
	}
	component := &vmComponents{repo: repo, service: handler, worker: worker, environment: environment}
	component.observation, e = data.NewStateObservation(clientConfig, repo)
	if e != nil {
		return nil, nil, e
	}
	component.observer, e = biz.NewObserver(repo, component.observation)
	if e != nil {
		return nil, nil, e
	}
	component.observationWorker = server.NewVMObservationServer(component.observer, slog.Default(), time.Second)
	if instances != nil {
		instances.terminal.RegisterVMConsole(handler)
	} else {
		component.console, e = server.NewInstanceTerminalServer(cfg.ConsoleAddress, os.Getenv("ANI_NETWORK_CLIENT_CA"), os.Getenv("ANI_NETWORK_TLS_CERT"), os.Getenv("ANI_NETWORK_TLS_KEY"), nil, handler)
		if e != nil {
			return nil, nil, e
		}
	}
	if e = wireVMImages(component, cfg.Service, client, c.ClusterId); e != nil {
		return nil, nil, e
	}
	valid = true
	return component, networkbiz.NewVMConsumerRouter(legacy, networkPort), nil
}

// wireVMImages attaches the tenant image lane. An absent or disabled block keeps
// the deployment exactly as it was: no service, no worker, no new permissions.
func wireVMImages(component *vmComponents, cfg service.Config, client dynamic.Interface, cluster string) error {
	upload := cfg.ImageUpload
	if upload.MaxFileBytes < 0 || upload.MaxSourceGB < 0 {
		return fmt.Errorf("VM image upload limits cannot be negative")
	}
	deadline, tokenTTL, e := upload.Durations()
	if e != nil {
		return e
	}
	nativeConfig := data.ImageUploadConfig{Enabled: upload.Enabled, ExternalUploadOrigin: upload.ExternalUploadOrigin,
		ValidatorImage: upload.ValidatorImage, UploadDeadline: deadline, MaxFileBytes: upload.MaxFileBytes,
		MaxSourceGB: upload.MaxSourceGB, StorageClass: cfg.MediaStorageClass, ClusterID: cluster, TokenTTL: tokenTTL,
		VerifierActiveDeadlineSeconds: int64((15 * time.Minute).Seconds()), VerifierBackoffLimit: 0}
	imageNative, e := data.NewImageNative(client, nativeConfig)
	if e != nil {
		return e
	}
	logger := slog.Default()
	imageService, e := service.NewImageService(component.repo, imageNative, component.environment, service.ImageUploadConfig{
		Enabled: upload.Enabled, ExternalUploadOrigin: upload.ExternalUploadOrigin, ValidatorImage: upload.ValidatorImage,
		UploadDeadline: upload.UploadDeadline, MaxFileBytes: upload.MaxFileBytes, MaxSourceGB: upload.MaxSourceGB, TokenTTL: upload.TokenTTL}, cluster, logger)
	if e != nil {
		return e
	}
	imageWorker, e := biz.NewImageWorker(component.repo, imageNative, component.environment, uuid.NewString(), func() bool { return upload.Enabled }, logger)
	if e != nil {
		return e
	}
	imageWorker.SetImageClusterID(cluster)
	referenceWorker, e := biz.NewReferenceWorker(component.repo, component.repo, imageNative, logger)
	if e != nil {
		return e
	}
	component.imageService, component.imageWorker, component.referenceWorker = imageService, imageWorker, referenceWorker
	component.service.SetImageCatalog(component.repo)
	return nil
}
