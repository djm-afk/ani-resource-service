package data_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

const securityGroupsLiveAPI = "https://172.16.101.10:6443"
const securityGroupsLiveClusterUID = "5277649d-e28d-4a0a-ae76-8354365c63bc"

var securityGroupsLiveGVR = schema.GroupVersionResource{Group: "networking.kubercloud.com", Version: "v1", Resource: "securitygroups"}
var securityGroupsLiveNamespaces = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// TestSecurityGroupsKCNLiveLifecycle is opt-in and targets only the explicitly
// authorized three-node ANI lab. It runs the real UC, restricted runtime role,
// PostgreSQL reconciliation/operations and KCProvider; no KCN boundary double is
// installed. The fixture does not start or change a shared Resource service.
func TestSecurityGroupsKCNLiveLifecycle(t *testing.T) {
	if os.Getenv("NETWORK_SG_LIVE") != "1" {
		t.Skip("opt-in live SG lifecycle requires NETWORK_SG_LIVE=1 and scripts/integration's isolated PostgreSQL")
	}
	if os.Getenv("NETWORK_TEST_ADMIN_DSN") == "" {
		t.Fatal("live SG lifecycle requires scripts/integration's isolated PostgreSQL")
	}
	kubeconfig := os.Getenv("NETWORK_SG_LIVE_KUBECONFIG")
	if kubeconfig == "" {
		t.Fatal("NETWORK_SG_LIVE_KUBECONFIG must name an existing authorized private kubeconfig")
	}
	directory := os.Getenv("NETWORK_SG_LIVE_EVIDENCE_DIR")
	manifest := os.Getenv("NETWORK_SG_LIVE_SOURCE_MANIFEST_SHA256")
	if !filepath.IsAbs(directory) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(manifest) {
		t.Fatal("absolute evidence directory and frozen source manifest SHA256 are required")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	config, err := clientcmd.BuildConfigFromFlags(securityGroupsLiveAPI, kubeconfig)
	if err != nil {
		t.Fatal("existing live kubeconfig could not be loaded")
	}
	config.Timeout, config.QPS, config.Burst = 15*time.Second, 4, 4
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal("live Kubernetes client configuration failed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	identity, err := securityGroupsLiveIdentity(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	identity["source_manifest_sha256"] = manifest
	identity["source_commit"] = os.Getenv("NETWORK_SG_LIVE_SOURCE_COMMIT")
	securityGroupsLiveWrite(t, directory, "identity.json", identity)

	fixture := testenv.NewDatabase(t)
	repository, err := data.OpenPostgres(ctx, fixture.RuntimeDSN, data.Placement{ClusterID: securityGroupsLiveClusterUID, NamespacePrefix: "sg-live-"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repository.Close)
	provider, err := data.NewKCProvider(repository, config)
	if err != nil {
		t.Fatal(err)
	}
	groups := biz.NewSecurityGroups(repository)
	policy := biz.DefaultWorkerPolicy()
	policy.Lease, policy.RequestTimeout = 45*time.Second, 20*time.Second
	worker := biz.NewSecurityGroupWorker(repository, provider, uuid.NewString(), policy)
	tenant := uuid.NewString()
	namespace := "sg-live-" + tenant
	caller := func(parent context.Context) context.Context {
		return biz.WithEgressCaller(parent, biz.EgressCaller{TenantID: tenant, Attribution: biz.Attribution{Actor: "security-groups-live-lifecycle", DirectCaller: "resource-live-driver"}})
	}
	callCtx := caller(ctx)
	if _, err = client.Resource(securityGroupsLiveNamespaces).Get(ctx, namespace, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("task namespace is not provably absent; refusing adoption", namespace)
	}
	var current biz.SecurityGroup
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		cleanupCtx = caller(cleanupCtx)
		if current.ID != "" {
			g, readErr := groups.Get(cleanupCtx, tenant, current.ID)
			if readErr != nil {
				t.Error("cleanup could not read its SG", readErr)
				return
			}
			if g.State != biz.Deleted {
				if g.State != biz.Deleting {
					if _, deleteErr := groups.Mutate(cleanupCtx, biz.SecurityGroupIntent{Action: "delete", ID: g.ID, ExpectedVersion: g.Version}); deleteErr != nil {
						t.Error("cleanup UC delete acceptance failed", deleteErr)
						return
					}
				}
				if _, cleanupErr := securityGroupsLiveDrive(cleanupCtx, groups, worker, tenant, g.ID, biz.Deleted); cleanupErr != nil {
					t.Error("cleanup did not complete through the real Resource worker", cleanupErr)
					return
				}
			}
		}
		if cleanupErr := securityGroupsLiveDeleteNamespace(cleanupCtx, client, tenant, namespace); cleanupErr != nil {
			t.Error("task namespace cleanup failed", cleanupErr)
			return
		}
		securityGroupsLiveWrite(t, directory, "cleanup.json", map[string]any{"namespace": namespace, "namespace_absent": true, "security_group_id": current.ID, "cleaned_at": time.Now().UTC()})
	})

	accepted, err := groups.Mutate(callCtx, biz.SecurityGroupIntent{Action: "create", Name: "sg-live-web", Description: "task-owned real KCN lifecycle", IdempotencyKey: "live-create", Rules: []biz.SecurityGroupRule{
		{Direction: "ingress", Protocol: "tcp", Action: "allow", Priority: 3000, RemoteAddresses: []string{"192.0.2.0/24"}, PortRanges: []biz.SecurityGroupPortRange{{Min: 80, Max: 80}}},
	}})
	if err != nil {
		t.Fatal("real UC acceptance failed", err)
	}
	current = accepted.SecurityGroup
	if current.Namespace != namespace {
		t.Fatal("accepted SG left the task namespace", current.Namespace)
	}
	securityGroupsLiveWrite(t, directory, "create-accepted.json", accepted)
	current, err = securityGroupsLiveDrive(callCtx, groups, worker, tenant, current.ID, biz.Available)
	if err != nil {
		securityGroupsLiveCapture(t, directory, "create-failed.json", client, groups, callCtx, current)
		t.Fatal("real KCN creation did not become available", err)
	}
	created := securityGroupsLiveCapture(t, directory, "create-ready.json", client, groups, callCtx, current)
	securityGroupsLiveAssertApplied(t, created, 80)
	initialUID, initialGeneration := created.GetUID(), created.GetGeneration()
	var rule biz.SecurityGroupRule
	for _, candidate := range current.Rules {
		if !candidate.IsDefault {
			rule = candidate
		}
	}
	if rule.ID == "" {
		t.Fatal("created allow rule has no durable Resource identity")
	}
	rule.PortRanges = []biz.SecurityGroupPortRange{{Min: 443, Max: 443}}
	updated, err := groups.Mutate(callCtx, biz.SecurityGroupIntent{Action: "update_rule", ID: current.ID, RuleID: rule.ID, ExpectedVersion: current.Version, IdempotencyKey: "live-update-https", Rule: rule})
	if err != nil {
		t.Fatal("real rule update acceptance failed", err)
	}
	current = updated.SecurityGroup
	securityGroupsLiveWrite(t, directory, "update-accepted.json", updated)
	current, err = securityGroupsLiveDrive(callCtx, groups, worker, tenant, current.ID, biz.Available)
	if err != nil {
		securityGroupsLiveCapture(t, directory, "update-failed.json", client, groups, callCtx, current)
		t.Fatal("real KCN rule update did not become available", err)
	}
	applied := securityGroupsLiveCapture(t, directory, "update-ready.json", client, groups, callCtx, current)
	securityGroupsLiveAssertApplied(t, applied, 443)
	if applied.GetUID() != initialUID || applied.GetGeneration() <= initialGeneration {
		t.Fatal("rule update replaced the SG UID or did not advance its generation")
	}
	deletion, err := groups.Mutate(callCtx, biz.SecurityGroupIntent{Action: "delete", ID: current.ID, ExpectedVersion: current.Version})
	if err != nil {
		t.Fatal("real delete acceptance failed", err)
	}
	current = deletion.SecurityGroup
	securityGroupsLiveWrite(t, directory, "delete-accepted.json", deletion)
	current, err = securityGroupsLiveDrive(callCtx, groups, worker, tenant, current.ID, biz.Deleted)
	if err != nil {
		securityGroupsLiveCapture(t, directory, "delete-failed.json", client, groups, callCtx, current)
		t.Fatal("real KCN finalizer deletion did not complete", err)
	}
	if _, err = client.Resource(securityGroupsLiveGVR).Namespace(namespace).Get(callCtx, current.ProviderName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("Resource reported deletion while its SG still exists", err)
	}
	op, err := groups.Operation(callCtx, tenant, current.LastOperationID)
	if err != nil || op.State != biz.Succeeded {
		t.Fatal("real deletion operation did not complete", op, err)
	}
	securityGroupsLiveWrite(t, directory, "delete-complete.json", map[string]any{"group": current, "operation": op, "provider_absent": true})
	t.Logf("PASS real Resource UC/PostgreSQL/worker/KCProvider SG lifecycle; cluster=%s namespace=%s uid=%s generation=%d->%d; no traffic assertion", securityGroupsLiveClusterUID, namespace, initialUID, initialGeneration, applied.GetGeneration())
}

func securityGroupsLiveIdentity(ctx context.Context, client dynamic.Interface) (map[string]any, error) {
	ns, err := client.Resource(securityGroupsLiveNamespaces).Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil || string(ns.GetUID()) != securityGroupsLiveClusterUID {
		return nil, errors.New("live SG target cluster UID differs from the authorized ANI lab")
	}
	nodes, err := client.Resource(schema.GroupVersionResource{Version: "v1", Resource: "nodes"}).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	want := map[string]string{"ani-01": "201e41ac-c50d-4c77-b118-72d1ed93e111", "ani-02": "73564fa7-98ad-45d4-9576-be59f6d5d245", "ani-03": "af992ed9-cbe1-4f64-8fac-7433808bfab2"}
	addresses := []string{}
	identity := []map[string]any{}
	for _, node := range nodes.Items {
		if want[node.GetName()] != string(node.GetUID()) {
			return nil, errors.New("live SG target node UID differs from the authorized ANI lab")
		}
		entries, _, _ := unstructured.NestedSlice(node.Object, "status", "addresses")
		for _, raw := range entries {
			entry, ok := raw.(map[string]any)
			if ok && entry["type"] == "InternalIP" {
				address, _ := entry["address"].(string)
				addresses = append(addresses, address)
				identity = append(identity, map[string]any{"name": node.GetName(), "uid": node.GetUID(), "internal_ip": address})
			}
		}
	}
	slices.Sort(addresses)
	if len(nodes.Items) != 3 || !slices.Equal(addresses, []string{"172.16.101.10", "172.16.101.11", "172.16.101.12"}) {
		return nil, errors.New("live SG target node addresses differ from the authorized ANI lab")
	}
	crd, err := client.Resource(schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}).Get(ctx, "securitygroups.networking.kubercloud.com", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
	fields := map[string]bool{}
	for _, raw := range versions {
		v, ok := raw.(map[string]any)
		if !ok || v["name"] != "v1" || v["served"] != true {
			continue
		}
		for _, name := range []string{"observedGeneration", "appliedEIPs", "appliedVNics", "allowSameGroupTraffic"} {
			_, present, _ := unstructured.NestedMap(v, "schema", "openAPIV3Schema", "properties", "status", "properties", name)
			fields[name] = present
		}
	}
	for _, available := range fields {
		if !available {
			return nil, errors.New("live KCN CRD lacks the frozen SG status contract; KCN upgrade is outside this test")
		}
	}
	if len(fields) != 4 {
		return nil, errors.New("live KCN v1 SG contract is unavailable")
	}
	controller, err := client.Resource(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}).Namespace("kcn-system").Get(ctx, "kcn-controller", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	containers, _, _ := unstructured.NestedSlice(controller.Object, "spec", "template", "spec", "containers")
	images := []string{}
	for _, raw := range containers {
		if c, ok := raw.(map[string]any); ok {
			image, _ := c["image"].(string)
			images = append(images, image)
		}
	}
	return map[string]any{"api": securityGroupsLiveAPI, "cluster_uid": ns.GetUID(), "nodes": identity, "kcn_controller_images": images, "security_group_crd_uid": crd.GetUID(), "status_contract_fields": fields, "captured_at": time.Now().UTC()}, nil
}

func securityGroupsLiveDrive(ctx context.Context, groups *biz.SecurityGroups, worker *biz.SecurityGroupWorker, tenant, id string, want biz.ResourceState) (biz.SecurityGroup, error) {
	deadline := time.Now().Add(100 * time.Second)
	var current biz.SecurityGroup
	for time.Now().Before(deadline) {
		var err error
		current, err = groups.Get(ctx, tenant, id)
		if err != nil {
			return current, err
		}
		if current.State == want {
			op, opErr := groups.Operation(ctx, tenant, current.LastOperationID)
			if opErr != nil || op.State != biz.Succeeded {
				return current, fmt.Errorf("resource state %s lacks a successful durable operation: %v", want, opErr)
			}
			return current, nil
		}
		if _, err = worker.Step(ctx); err != nil {
			return current, err
		}
		select {
		case <-ctx.Done():
			return current, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return current, fmt.Errorf("SG did not reach %s: state=%s reason=%s", want, current.State, current.Reason)
}

func securityGroupsLiveCapture(t *testing.T, directory, name string, client dynamic.Interface, groups *biz.SecurityGroups, ctx context.Context, group biz.SecurityGroup) *unstructured.Unstructured {
	t.Helper()
	o, err := client.Resource(securityGroupsLiveGVR).Namespace(group.Namespace).Get(ctx, group.ProviderName, metav1.GetOptions{})
	op, opErr := groups.Operation(ctx, group.TenantID, group.LastOperationID)
	record := map[string]any{"resource": group, "operation": op, "captured_at": time.Now().UTC()}
	if err != nil {
		record["provider_read_error"] = err.Error()
	} else {
		record["provider_object"] = o.Object
	}
	if opErr != nil {
		record["operation_read_error"] = opErr.Error()
	}
	securityGroupsLiveWrite(t, directory, name, record)
	return o
}

func securityGroupsLiveAssertApplied(t *testing.T, o *unstructured.Unstructured, port int64) {
	t.Helper()
	if o == nil {
		t.Fatal("expected task SG object was not readable")
	}
	observed, _, _ := unstructured.NestedInt64(o.Object, "status", "observedGeneration")
	conditions, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	good := map[string]bool{}
	for _, raw := range conditions {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := c["type"].(string)
		good[name] = c["status"] == "True"
	}
	trust, found, _ := unstructured.NestedBool(o.Object, "spec", "allowSameGroupTraffic")
	ingress, _, _ := unstructured.NestedSlice(o.Object, "spec", "ingressRules")
	egress, _, _ := unstructured.NestedSlice(o.Object, "spec", "egressRules")
	if !good["Ready"] || o.GetGeneration() < 1 || observed != o.GetGeneration() || !found || trust || len(egress) != 0 || len(ingress) != 2 {
		t.Fatal("live KCN SG did not apply the exact ingress-only default policy", o.Object)
	}
	allow, drop := false, false
	for _, raw := range ingress {
		r, ok := raw.(map[string]any)
		if !ok {
			t.Fatal("invalid live ingress rule", raw)
		}
		priority, _, _ := unstructured.NestedInt64(r, "priority")
		if r["protocol"] == "all" && r["action"] == "drop" && priority == 1 {
			drop = true
		}
		if r["protocol"] == "tcp" && r["action"] == "allow" && priority == 3000 {
			ports, _, _ := unstructured.NestedSlice(r, "portRanges")
			if len(ports) == 1 {
				p, _ := ports[0].(map[string]any)
				min, _, _ := unstructured.NestedInt64(p, "min")
				max, _, _ := unstructured.NestedInt64(p, "max")
				allow = min == port && max == port
			}
		}
	}
	if !allow || !drop {
		t.Fatal("live ingress rule update was not present", ingress)
	}
}

func securityGroupsLiveDeleteNamespace(ctx context.Context, client dynamic.Interface, tenant, namespace string) error {
	if !strings.HasPrefix(namespace, "sg-live-") || namespace != "sg-live-"+tenant {
		return errors.New("refusing namespace cleanup outside this task")
	}
	endpoint := client.Resource(securityGroupsLiveNamespaces)
	ns, err := endpoint.Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if ns.GetLabels()["network.ani.io/managed-by"] != "ani-network-service" || ns.GetLabels()["network.ani.io/tenant-id"] != tenant || ns.GetUID() == "" {
		return errors.New("refusing namespace cleanup: task ownership labels or UID changed")
	}
	remaining, err := client.Resource(securityGroupsLiveGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil || len(remaining.Items) != 0 {
		return errors.New("refusing namespace deletion before real Resource SG cleanup finishes")
	}
	uid, rv := ns.GetUID(), ns.GetResourceVersion()
	if ns.GetDeletionTimestamp() == nil {
		if err = endpoint.Delete(ctx, namespace, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	for {
		_, err = endpoint.Get(ctx, namespace, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func securityGroupsLiveWrite(t *testing.T, directory, name string, value any) {
	t.Helper()
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(directory, name), append(encoded, '\n'), 0600)
	}
	if err != nil {
		t.Error("could not save bounded live SG evidence", name, err)
	}
}
