//go:build instanceintegration

package instance_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/instance/v1"
	imagebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	networkbiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	imagedata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/image"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/instance"
	networkdata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	service "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/instance"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

// The boundary models controller annotation projection and KCN status. Target
// observations use the real provider's single-current-VNic reader; SG catalog,
// authorization, references and operations use real SQL.
type verticalSecurityGroupBoundary struct {
	provider networkbiz.SecurityGroupProvider
	notebook *notebookNetworkBoundary
}

func (b *verticalSecurityGroupBoundary) ApplySecurityGroup(ctx context.Context, w networkbiz.SecurityGroupWork) (networkbiz.SecurityGroupResult, error) {
	if w.TargetType != "security_group" {
		return b.provider.ApplySecurityGroup(ctx, w)
	}
	return networkbiz.SecurityGroupResult{ProviderUID: "controlled-kcn-" + w.TargetID, State: string(networkbiz.Available), Complete: true}, nil
}
func (b *verticalSecurityGroupBoundary) mirrorCNI(ctx context.Context, namespace string) error {
	pods, err := b.notebook.client.Resource(verticalPodGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	b.notebook.api.Mu.Lock()
	defer b.notebook.api.Mu.Unlock()
	for index := range pods.Items {
		pod := pods.Items[index].DeepCopy()
		ann := pod.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann["networking.kubercloud.com/vnic"] = namespace + "/nic"
		pod.SetAnnotations(ann)
		if err = b.notebook.client.Tracker().Update(verticalPodGVR, pod, namespace); err != nil {
			return err
		}
		b.notebook.api.Objects["pods/"+namespace+"/"+pod.GetName()] = pod.Object
		if nic := b.notebook.api.Objects["vnics/"+namespace+"/nic"]; nic != nil {
			names := []any{}
			applied := map[string]any{}
			if ann[biz.SecurityGroupsAnnotation] != "" {
				for _, name := range strings.Split(ann[biz.SecurityGroupsAnnotation], ",") {
					names = append(names, name)
					applied[name] = int64(1)
				}
			}
			spec := nic["spec"].(map[string]any)
			spec["securityGroups"] = names
			if err = unstructured.SetNestedMap(nic, applied, "status", "appliedSecurityGroups"); err != nil {
				return err
			}
		}
	}
	return nil
}
func (b *verticalSecurityGroupBoundary) installNotebookProjection(t *testing.T) {
	t.Helper()
	b.notebook.client.PrependReactor("patch", "notebooks", func(action ktesting.Action) (bool, runtime.Object, error) {
		patchAction := action.(ktesting.PatchAction)
		old, err := b.notebook.client.Tracker().Get(verticalNotebookGVR, action.GetNamespace(), patchAction.GetName())
		if err != nil {
			return true, nil, err
		}
		raw, err := json.Marshal(old)
		if err != nil {
			return true, nil, err
		}
		patch, err := jsonpatch.DecodePatch(patchAction.GetPatch())
		if err != nil {
			return true, nil, err
		}
		raw, err = patch.Apply(raw)
		if err != nil {
			return true, nil, err
		}
		root := &unstructured.Unstructured{}
		if err = json.Unmarshal(raw, &root.Object); err != nil {
			return true, nil, err
		}
		root.SetResourceVersion(uuid.NewString())
		if err = b.notebook.client.Tracker().Update(verticalNotebookGVR, root, root.GetNamespace()); err != nil {
			return true, nil, err
		}
		b.notebook.notebook = root
		object, err := b.notebook.client.Tracker().Get(verticalSTSGVR, root.GetNamespace(), root.GetName())
		if err != nil {
			return true, nil, err
		}
		set := object.(*unstructured.Unstructured).DeepCopy()
		if err = unstructured.SetNestedStringMap(set.Object, root.GetAnnotations(), "spec", "template", "metadata", "annotations"); err != nil {
			return true, nil, err
		}
		if err = b.notebook.client.Tracker().Update(verticalSTSGVR, set, set.GetNamespace()); err != nil {
			return true, nil, err
		}
		b.notebook.api.Mu.Lock()
		b.notebook.api.Objects["notebooks/"+root.GetNamespace()+"/"+root.GetName()] = root.DeepCopy().Object
		b.notebook.api.Objects["statefulsets/"+set.GetNamespace()+"/"+set.GetName()] = set.Object
		b.notebook.api.Mu.Unlock()
		return true, root, nil
	})
}

// The catalog/credential rows are prepared input, like the frozen image
// reference in AcceptCreate. Ensure itself uses real Image SQL and AES-GCM;
// only its Kubernetes writer and namespace observation are controlled.
type verticalPullSecretKubernetes struct {
	clusterID string
	boundary  *notebookNetworkBoundary
}

func (k verticalPullSecretKubernetes) VerifyNamespace(_ context.Context, claim imagebiz.NamespaceClaim) error {
	if claim.ClusterID != k.clusterID || claim.Namespace != "tenant-"+claim.TenantID {
		return imagebiz.Fail(imagebiz.PermissionDenied, "pull secret fixture scope rejected")
	}
	k.boundary.api.Mu.Lock()
	defer k.boundary.api.Mu.Unlock()
	ns := k.boundary.api.Objects["namespaces//"+claim.Namespace]
	if ns == nil {
		return imagebiz.Fail(imagebiz.PermissionDenied, "tenant namespace was not prepared by Network")
	}
	object := &unstructured.Unstructured{Object: ns}
	if object.GetUID() == "" || object.GetLabels()["network.ani.io/tenant-id"] != claim.TenantID || (claim.NamespaceUID != "" && claim.NamespaceUID != string(object.GetUID())) {
		return imagebiz.Fail(imagebiz.PermissionDenied, "namespace identity changed")
	}
	return nil
}
func (k verticalPullSecretKubernetes) EnsurePullSecret(ctx context.Context, target imagebiz.PullSecretTarget, plain imagebiz.Secret) (int64, error) {
	if target.ClusterID != k.clusterID || target.Namespace != "tenant-"+target.TenantID || target.Generation < 1 || len(plain) == 0 {
		return 0, imagebiz.Fail(imagebiz.PermissionDenied, "pull secret write scope rejected")
	}
	resource := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	client := k.boundary.client.Resource(resource).Namespace(target.Namespace)
	existing, err := client.Get(ctx, target.SecretName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return 0, err
	}
	if err == nil {
		if existing.GetLabels()["ani.io/managed-by"] != biz.Owner || existing.GetLabels()["ani.io/resource-tenant-id"] != target.TenantID {
			return 0, imagebiz.Fail(imagebiz.PermissionDenied, "foreign Secret")
		}
		generation, _ := strconv.ParseInt(existing.GetAnnotations()["image.ani.io/credential-generation"], 10, 64)
		if generation >= target.Generation {
			return generation, nil
		}
	}
	config, e := json.Marshal(map[string]any{"auths": map[string]any{target.RegistryAuthority: map[string]string{"username": target.Username, "password": string(plain)}}})
	if e != nil {
		return 0, e
	}
	defer clear(config)
	object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": target.SecretName, "namespace": target.Namespace, "uid": uuid.NewString(), "resourceVersion": "1", "labels": map[string]any{"ani.io/managed-by": biz.Owner, "ani.io/resource-tenant-id": target.TenantID}, "annotations": map[string]any{"image.ani.io/credential-generation": strconv.FormatInt(target.Generation, 10)}}, "type": "kubernetes.io/dockerconfigjson", "data": map[string]any{".dockerconfigjson": base64.StdEncoding.EncodeToString(config)}}}
	if apierrors.IsNotFound(err) {
		_, e = client.Create(ctx, object, metav1.CreateOptions{})
	} else {
		object.SetUID(existing.GetUID())
		object.SetResourceVersion(existing.GetResourceVersion())
		_, e = client.Update(ctx, object, metav1.UpdateOptions{})
	}
	return target.Generation, e
}

type verticalFrozenImageRegistry struct{ imagebiz.Registry } // Accepted frozen input does not re-resolve Harbor.
func configureVerticalPullSecret(t *testing.T, f *fixture, c biz.Create, boundary *notebookNetworkBoundary, notebook *data.Notebook) {
	t.Helper()
	ctx := context.Background()
	owner, err := pgxpool.New(ctx, f.ownerDSN)
	if err != nil {
		t.Fatal("isolated Image owner connection unavailable")
	}
	defer owner.Close()
	// Image deliberately forbids another domain's DML and PUBLIC database TEMP.
	// Provision a separate runtime identity in this already isolated test DB;
	// its owner identity is used only for explicit migration and prepared input.
	runtimeURL, err := url.Parse(f.ownerDSN)
	if err != nil {
		t.Fatal("invalid isolated owner DSN")
	}
	password, _ := runtimeURL.User.Password()
	imageRole := "sg_image_runtime_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	adminDSN, err := os.ReadFile(os.Getenv("INSTANCE_TEST_ADMIN_DSN_FILE"))
	if err != nil {
		t.Fatal("isolated fixture admin DSN unavailable")
	}
	defer clear(adminDSN)
	adminURL, err := url.Parse(string(adminDSN))
	if err != nil {
		t.Fatal("invalid isolated admin DSN")
	}
	admin, err := pgxpool.New(ctx, adminURL.String())
	if err != nil {
		t.Fatal("isolated fixture admin unavailable")
	}
	t.Cleanup(admin.Close)
	literal := "'" + strings.ReplaceAll(password, "'", "''") + "'"
	if _, err = admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{imageRole}.Sanitize()+" LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE PASSWORD "+literal); err != nil {
		t.Fatal("isolated Image runtime role creation failed")
	}
	t.Cleanup(func() {
		cleanupURL := *adminURL
		cleanupURL.Path = runtimeURL.Path
		cleanupPool, e := pgxpool.New(context.Background(), cleanupURL.String())
		if e != nil {
			t.Error("Image role cleanup connection unavailable")
			return
		}
		_, e = cleanupPool.Exec(context.Background(), "DROP OWNED BY "+pgx.Identifier{imageRole}.Sanitize())
		cleanupPool.Close()
		if e != nil {
			t.Error("Image role cleanup grants failed")
			return
		}
		if _, e = admin.Exec(context.Background(), "DROP ROLE "+pgx.Identifier{imageRole}.Sanitize()); e != nil {
			t.Error("Image runtime role cleanup failed")
		}
	})
	dbName := strings.TrimPrefix(runtimeURL.Path, "/")
	if _, err = owner.Exec(ctx, "REVOKE CREATE,TEMPORARY ON DATABASE "+pgx.Identifier{dbName}.Sanitize()+" FROM PUBLIC; REVOKE CREATE ON SCHEMA public FROM PUBLIC"); err != nil {
		t.Fatal("isolated database privilege preparation failed")
	}
	if err = imagedata.ApplyImageMigrations(ctx, imagedata.OwnerConfig{DSN: f.ownerDSN, RuntimeRole: imageRole}); err != nil {
		t.Fatal(err)
	}
	runtimeURL.User = url.UserPassword(imageRole, password)
	space, installation := uuid.NewString(), uuid.NewString()
	cipher, err := imagedata.NewAESGCMKeyring("vertical-fixture", map[string][]byte{"vertical-fixture": []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Seal(ctx, imagebiz.SecretAAD{InstallationID: installation, TenantID: c.TenantID, SpaceID: space, Purpose: "pull", Scope: imagebiz.TenantImages, Generation: 1}, imagebiz.Secret("controlled-test-only"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `INSERT INTO image.spaces(space_id,owner_scope,tenant_id,installation_id,registry_authority,project_name,harbor_project_id,state) VALUES($1,'tenant',$2,$3,'registry.invalid','prepared-vertical',1,'available')`, space, c.TenantID, installation); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `INSERT INTO image.credentials(space_id,owner_scope,tenant_id,purpose,state,generation,robot_id,robot_name,username,never_expires,secret_ciphertext,secret_key_id) VALUES($1,'tenant',$2,'pull','active',1,1,'prepared-pull','robot$prepared',true,$3,$4)`, space, c.TenantID, encrypted.Ciphertext, encrypted.KeyID); err != nil {
		t.Fatal(err)
	}
	repo, err := imagedata.OpenPostgres(ctx, runtimeURL.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repo.Close)
	runtime, err := imagebiz.NewRuntime(repo, repo, repo, verticalFrozenImageRegistry{}, cipher, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	kubernetes := verticalPullSecretKubernetes{clusterID: c.Spec.ClusterID, boundary: boundary}
	if err = runtime.UsePullSecretWriter(kubernetes, kubernetes, nil); err != nil {
		t.Fatal(err)
	}
	if err = notebook.ConfigureImages(runtime); err != nil {
		t.Fatal(err)
	}
}

type verticalNotebookDiagnostics struct {
	*data.Notebook
	stage     string
	lastError error
}

func (n *verticalNotebookDiagnostics) EnsureWorkspace(ctx context.Context, i biz.Instance) (string, error) {
	id, err := n.Notebook.EnsureWorkspace(ctx, i)
	if err != nil {
		n.stage, n.lastError = "EnsureWorkspace", err
	}
	return id, err
}
func (n *verticalNotebookDiagnostics) CreateNotebook(ctx context.Context, i biz.Instance) (string, error) {
	id, err := n.Notebook.CreateNotebook(ctx, i)
	if err != nil {
		n.stage, n.lastError = "CreateNotebook", err
	}
	return id, err
}

func TestInstanceSecurityGroupsVerticalRealCatalogCreateReplaceClearDelete(t *testing.T) {
	f := database(t)
	c := networkCreate()
	ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: c.TenantID, Actor: "governance:sg-vertical"})
	ctx = networkbiz.WithEgressCaller(ctx, networkbiz.EgressCaller{TenantID: c.TenantID, Attribution: networkbiz.Attribution{Actor: "governance:sg-vertical", DirectCaller: "controlled-vertical"}})
	nf := testenv.NewDatabase(t)
	nrepo, err := networkdata.OpenPostgres(ctx, nf.RuntimeDSN, networkdata.Placement{ClusterID: c.Spec.ClusterID, NamespacePrefix: "tenant-"})
	if err != nil {
		t.Fatal(err)
	}
	defer nrepo.Close()
	if err = nrepo.ConfigureVPCCIDRPresets([]string{"10.42.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	api := &testenv.KC{}
	httpServer := httptest.NewServer(api)
	defer httpServer.Close()
	nprovider, err := networkdata.NewKCProvider(nrepo, &rest.Config{Host: httpServer.URL, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	policy := networkbiz.DefaultWorkerPolicy()
	policy.ObserveEvery = time.Millisecond
	policy.RetryMin = time.Millisecond
	networkUC, err := networkbiz.NewNetwork(nrepo, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	resourceWorker, err := networkbiz.NewWorker(nrepo, nprovider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	vpc, err := networkUC.CreateVPC(ctx, networkbiz.CreateVPC{TenantID: c.TenantID, Name: "sg-vertical", CIDR: "10.42.0.0/16", IdempotencyKey: "sg-vertical"})
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 100 && vpc.State != networkbiz.Available; round++ {
		if _, err = resourceWorker.Step(ctx); err != nil {
			t.Fatal(err)
		}
		vpc, err = networkUC.GetVPC(ctx, c.TenantID, vpc.ID)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if vpc.State != networkbiz.Available {
		t.Fatal("VPC did not converge", vpc)
	}
	subnet, err := networkUC.CreateSubnet(ctx, networkbiz.CreateSubnet{TenantID: c.TenantID, VPCID: vpc.ID, Name: "sg-vertical", CIDR: "10.42.1.0/24", IdempotencyKey: "sg-vertical"})
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 100 && subnet.State != networkbiz.Available; round++ {
		if _, err = resourceWorker.Step(ctx); err != nil {
			t.Fatal(err)
		}
		subnet, err = networkUC.GetSubnet(ctx, c.TenantID, subnet.ID)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if subnet.State != networkbiz.Available {
		t.Fatal("Subnet did not converge", subnet)
	}
	c.Spec.Network = &biz.NetworkSelection{VPCID: vpc.ID, SubnetID: subnet.ID}
	boundary := newNotebookNetworkBoundary(t, api)
	sgBoundary := &verticalSecurityGroupBoundary{provider: nprovider, notebook: boundary}
	sgBoundary.installNotebookProjection(t)
	groups := networkbiz.NewSecurityGroups(nrepo)
	sgWorker := networkbiz.NewSecurityGroupWorker(nrepo, sgBoundary, uuid.NewString(), policy)
	createGroup := func(key string) networkbiz.SecurityGroup {
		t.Helper()
		accepted, e := groups.Mutate(ctx, networkbiz.SecurityGroupIntent{Action: "create", Name: key, IdempotencyKey: key})
		if e != nil {
			t.Fatal(e)
		}
		if _, e = sgWorker.Step(ctx); e != nil {
			t.Fatal(e)
		}
		got, e := groups.Get(ctx, c.TenantID, accepted.SecurityGroup.ID)
		if e != nil || got.State != networkbiz.Available {
			t.Fatal(got, e)
		}
		return got
	}
	first, second := createGroup("first"), createGroup("second")
	c.Spec.SecurityGroupIDs = []string{first.ID}
	intake, err := service.NewService(f.repo, computeResolver(t, f, c.Spec.ClusterID), frozenNetworkIntakeBoundary{}, frozenNetworkIntakeBoundary{}, service.Config{NamespacePrefix: "tenant-", ClusterID: c.Spec.ClusterID, ServiceAccount: c.Spec.ServiceAccount, StorageClass: c.Spec.StorageClass, WorkspaceSize: c.Spec.WorkspaceSize, Shell: c.Spec.Shell, UID: c.Spec.UID, GID: c.Spec.GID, NewIntentsEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = intake.UseSecurityGroups(nrepo); err != nil {
		t.Fatal(err)
	}
	s := c.Spec
	request := &pb.AcceptCreateRequest{Context: &pb.CommandContext{TenantId: c.TenantID, ResourceId: c.ID, OperationId: c.OperationID}, Spec: &pb.CreateSpec{Name: s.Name, ImageId: s.ImageID, ImageScope: s.ImageScope, PresetId: s.PresetID, ImageReference: s.ImageReference, Cpu: s.CPU, Memory: s.Memory, ClusterId: s.ClusterID, Namespace: s.Namespace, ServiceAccount: s.ServiceAccount, StorageClass: s.StorageClass, WorkspaceSize: s.WorkspaceSize, Shell: s.Shell, Uid: s.UID, Gid: s.GID, InstanceKind: s.InstanceKind, ComputeType: s.ComputeType, Scheduling: &pb.SchedulingOptions{PodPlacement: pb.SchedulingOptions_NONE.Enum()}, WorkspaceMountPath: s.WorkspaceMountPath, Network: &pb.NetworkSelection{VpcId: vpc.ID, SubnetId: subnet.ID}, SecurityGroupIds: []string{first.ID}}}
	request.Context.RequestDigest, err = pb.CommandDigest(request.Context, request.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if ack, e := intake.AcceptCreate(ctx, request); e != nil || !ack.GetDurableOwnerAck().GetAccepted() {
		t.Fatal("real create admission failed", ack, e)
	}
	attachments := networkbiz.NewAttachments(nrepo)
	f.repo.UseNetworkNotifier(attachments.Notify)
	adapter, err := data.NewInstanceNetwork(networkUC, attachments, c.Spec.ClusterID, "tenant-")
	if err != nil {
		t.Fatal(err)
	}
	notebook, err := data.NewNotebook(boundary.client)
	if err != nil {
		t.Fatal(err)
	}
	configureVerticalPullSecret(t, f, c, boundary, notebook)
	diagnostics := &verticalNotebookDiagnostics{Notebook: notebook}
	worker, err := biz.NewWorker(f.repo, diagnostics, "sg-vertical")
	if err != nil {
		t.Fatal(err)
	}
	_ = worker.UseNetwork(adapter)
	_ = worker.UseSecurityGroups(nrepo)
	consumer, err := data.NewInstanceNetworkConsumer(f.repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	nworker, err := networkbiz.NewAttachmentWorker(nrepo, consumer, nprovider, uuid.NewString(), policy)
	if err != nil {
		t.Fatal(err)
	}
	drive := func(want string) biz.Instance {
		t.Helper()
		var got biz.Instance
		for round := 0; round < 140; round++ {
			got = step(t, f, worker, c)
			if err = sgBoundary.mirrorCNI(ctx, c.Spec.Namespace); err != nil {
				t.Fatal(err)
			}
			if _, err = nf.Owner.Exec(ctx, "UPDATE network_attachments SET next_check_at=clock_timestamp()"); err != nil {
				t.Fatal(err)
			}
			if _, err = nworker.Step(ctx); err != nil {
				t.Fatal(err)
			}
			if got.Observed == want && got.ActiveOperation == nil && (want != biz.Deleted || got.Network.Cleaned) {
				return got
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatalf("SG vertical did not reach %s: observed=%s reason=%s mutation_stage=%s mutation_error=%v active=%+v", want, got.Observed, got.Reason, diagnostics.stage, diagnostics.lastError, got.ActiveOperation)
		return got
	}
	assertCurrent := func(i biz.Instance, ids []string, names string) {
		t.Helper()
		if !slices.Equal(i.SecurityGroupIDs, ids) {
			t.Fatal("current selection differs", i.SecurityGroupIDs, ids)
		}
		root, e := boundary.client.Resource(verticalNotebookGVR).Namespace(c.Spec.Namespace).Get(ctx, c.Spec.NotebookName, metav1.GetOptions{})
		if e != nil || root.GetAnnotations()[biz.SecurityGroupsAnnotation] != names || root.GetAnnotations()["instance.ani.io/intent-version"] != strconv.FormatInt(i.Version, 10) {
			t.Fatal("template differs", root, e)
		}
		pod, e := boundary.client.Resource(verticalPodGVR).Namespace(c.Spec.Namespace).Get(ctx, i.PodName, metav1.GetOptions{})
		if e != nil {
			t.Fatal(e)
		}
		if value, present := pod.GetAnnotations()[biz.SecurityGroupsAnnotation]; !present || value != names || pod.GetAnnotations()["instance.ani.io/intent-version"] != strconv.FormatInt(i.Version, 10) {
			t.Fatal("current Pod differs", pod.GetAnnotations())
		}
		// Instance has already completed its configuration operation. Applied
		// output is refreshed separately by the SG worker from current KCN status.
		var binding networkbiz.SecurityGroupBinding
		for round := 0; round < 100; round++ {
			if _, e = sgWorker.Step(ctx); e != nil {
				t.Fatal(e)
			}
			var body []byte
			if e = nf.Owner.QueryRow(ctx, `SELECT payload FROM network_security_group_targets WHERE tenant_id=$1 AND target_type='container' AND target_id=$2`, c.TenantID, c.ID).Scan(&body); e != nil {
				t.Fatal(e)
			}
			if e = json.Unmarshal(body, &binding); e != nil {
				t.Fatal(e)
			}
			if binding.State == "applied" && slices.Equal(binding.AppliedSecurityGroupIDs, ids) {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatal("SG worker did not project the current VNic status", binding)
	}
	i := drive(biz.Running)
	assertCurrent(i, []string{first.ID}, first.ProviderName)
	originalPod := i.PodUID
	secretName, err := imagebiz.PullSecretName(c.TenantID, c.Spec.ClusterID, c.Spec.Namespace, "registry.invalid")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := boundary.client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "secrets"}).Namespace(c.Spec.Namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil || secret.GetLabels()["ani.io/resource-tenant-id"] != c.TenantID {
		t.Fatal("real Image Ensure did not persist the scoped Secret", err)
	}
	podForPull, err := boundary.client.Resource(verticalPodGVR).Namespace(c.Spec.Namespace).Get(ctx, i.PodName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pulls, _, err := unstructured.NestedSlice(podForPull.Object, "spec", "imagePullSecrets")
	if err != nil || len(pulls) != 1 || pulls[0].(map[string]any)["name"] != secretName {
		t.Fatal("workload did not consume the real Image Ensure receipt", err)
	}
	if _, err = groups.Mutate(ctx, networkbiz.SecurityGroupIntent{Action: "delete", ID: first.ID}); networkbiz.ReasonOf(err) != networkbiz.ResourceInUse {
		t.Fatal("created consumer did not protect its group", err)
	}
	set := func(ids []string) {
		t.Helper()
		ack, e := intake.SetSecurityGroups(ctx, &pb.SetSecurityGroupsRequest{TenantId: c.TenantID, InstanceId: c.ID, ExpectedVersion: i.Version, IdempotencyKey: uuid.NewString(), SecurityGroupIds: ids})
		if e != nil || !ack.GetDurableOwnerAck().GetAccepted() {
			t.Fatal("real set admission failed", ack, e)
		}
		i = drive(biz.Running)
	}
	set([]string{second.ID})
	// Normal background reconciliation after completion must not turn a live
	// security-group change into an unrelated Notebook lifecycle mutation.
	for range 3 {
		i = drive(biz.Running)
	}
	assertCurrent(i, []string{second.ID}, second.ProviderName)
	if i.PodUID != originalPod {
		t.Fatal("changing groups restarted the Pod")
	}
	old, e := groups.Get(ctx, c.TenantID, first.ID)
	if e != nil || old.BoundResourceCount != 0 {
		t.Fatal("replacement leaked old ref", old, e)
	}
	set([]string{})
	assertCurrent(i, []string{}, "")
	if !slices.Equal(i.Spec.SecurityGroupIDs, []string{first.ID}) {
		t.Fatal("change rewrote immutable CREATE", i.Spec.SecurityGroupIDs)
	}
	command(t, f, c, i, "delete")
	i = drive(biz.Deleted)
	for _, g := range []networkbiz.SecurityGroup{first, second} {
		got, e := groups.Get(ctx, c.TenantID, g.ID)
		if e != nil || got.BoundResourceCount != 0 {
			t.Fatal("delete leaked reference", got, e)
		}
	}
	if !i.ComputeCleaned || !i.WorkspaceCleaned || !i.Network.Cleaned {
		t.Fatal("delete did not finish real owner/network cleanup", i)
	}
}
