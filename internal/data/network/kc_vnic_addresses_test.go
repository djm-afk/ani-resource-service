package data

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func requestedVNicAddressFixture() (sqlcgen.NetworkProviderBinding, biz.ProviderTarget, *unstructured.Unstructured, *unstructured.Unstructured) {
	b := sqlcgen.NetworkProviderBinding{Namespace: "tenant-one", ProviderName: "ani-vnic-one", BindingID: "binding-one", ProviderUid: "vnic-original"}
	target := biz.ProviderTarget{TenantID: "tenant-one", ResourceID: "vnic-one", Kind: "vnic", KnownIdentity: b.ProviderUid, RequestedIP: "10.42.1.42", RequestedMAC: "02:00:00:00:00:42"}
	parent := manualVNicObject(b, target, b.Namespace+"/subnet-one")
	parent.SetUID(types.UID(b.ProviderUid))
	parent.SetResourceVersion("7")
	parent.Object["status"] = map[string]any{"primaryIP": target.RequestedIP, "ips": map[string]any{b.ProviderName: target.RequestedIP}, "conditions": []any{map[string]any{"type": "Valid", "status": "True"}, map[string]any{"type": "Initialized", "status": "True"}, map[string]any{"type": "Ready", "status": "True"}}}
	child := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNicIP", "metadata": map[string]any{"name": b.ProviderName, "namespace": b.Namespace, "uid": "ip-original", "resourceVersion": "1", "labels": map[string]any{ownerLabel: "ani-network-service", tenantLabel: target.TenantID, resourceLabel: target.ResourceID, bindingLabel: b.BindingID}, "ownerReferences": []any{map[string]any{"apiVersion": "networking.kubercloud.com/v1", "kind": "VNic", "name": b.ProviderName, "uid": b.ProviderUid}}}, "spec": map[string]any{"subnet": b.Namespace + "/subnet-one", "vNic": b.ProviderName, "ipVersion": "IPv4", "ipAddress": target.RequestedIP, "dhcp": false}, "status": map[string]any{"vNic": b.ProviderName, "conditions": []any{map[string]any{"type": "Valid", "status": "True"}, map[string]any{"type": "Initialized", "status": "True"}}}}}
	return b, target, parent, child
}

func TestKCVNicRequestedAddressCreationContract(t *testing.T) {
	b, target, _, _ := requestedVNicAddressFixture()
	custom := manualVNicObject(b, target, "tenant-one/subnet-one")
	mac, _, _ := unstructured.NestedString(custom.Object, "spec", "macAddress")
	if mac != target.RequestedMAC {
		t.Fatal("requested MAC was not sent to KCN", mac)
	}
	if _, ok := custom.GetAnnotations()["networking.kubercloud.com/allocate_ip_with_dhcp"]; ok {
		t.Fatal("static request races KCN automatic same-name IP creation")
	}
	target.RequestedIP, target.RequestedMAC = "", ""
	automatic := manualVNicObject(b, target, "tenant-one/subnet-one")
	if automatic.GetAnnotations()["networking.kubercloud.com/allocate_ip_with_dhcp"] != "false" {
		t.Fatal("default allocation contract changed")
	}
	if _, exists, _ := unstructured.NestedString(automatic.Object, "spec", "macAddress"); exists {
		t.Fatal("default MAC allocation was replaced")
	}
}

func TestKCVNicRequestedIPCreateReplayAndResponseLoss(t *testing.T) {
	for _, alreadyCreated := range []bool{false, true} {
		name := "create"
		if alreadyCreated {
			name = "lost-response"
		}
		t.Run(name, func(t *testing.T) {
			b, target, parent, child := requestedVNicAddressFixture()
			objects := []runtime.Object{parent}
			if alreadyCreated {
				objects = append(objects, child)
			}
			client := fake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)
			creates, patches := 0, 0
			client.PrependReactor("create", "vnicips", func(action ktesting.Action) (bool, runtime.Object, error) {
				creates++
				o := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured)
				o.SetUID(child.GetUID())
				o.SetResourceVersion("1")
				return false, nil, nil
			})
			client.PrependReactor("patch", "vnics", func(action ktesting.Action) (bool, runtime.Object, error) {
				patches++
				var patch []map[string]any
				if err := json.Unmarshal(action.(ktesting.PatchAction).GetPatch(), &patch); err != nil {
					t.Fatal(err)
				}
				if len(patch) != 3 || patch[0]["op"] != "test" || patch[0]["path"] != "/metadata/uid" || patch[0]["value"] != b.ProviderUid || patch[1]["op"] != "test" || patch[1]["path"] != "/metadata/resourceVersion" || patch[1]["value"] != "7" {
					t.Fatal("child identity was recorded without original parent UID/RV fencing", patch)
				}
				return false, nil, nil
			})
			p := &KCProvider{client: client}
			observed, err := p.inspectVNic(context.Background(), parent, b, target)
			if err != nil || observed.Ready == nil || !*observed.Ready || !observed.NeedsUpdate || observed.Identity != b.ProviderUid {
				t.Fatal("partial creation cannot resume through pure observation", observed, err)
			}
			for _, a := range client.Actions() {
				if a.GetVerb() != "get" {
					t.Fatal("observation mutated provider state", a)
				}
			}
			parent, err = p.ensureRequestedVNicIP(context.Background(), parent, b, target)
			if err != nil || parent.GetAnnotations()[requestedVNicIPUIDAnnotation] != "ip-original" {
				t.Fatal("requested IP original identity was not persisted", parent, err)
			}
			if _, err = p.ensureRequestedVNicIP(context.Background(), parent, b, target); err != nil {
				t.Fatal("same identity replay", err)
			}
			wantCreates := 1
			if alreadyCreated {
				wantCreates = 0
			}
			if creates != wantCreates || patches != 1 {
				t.Fatal("replay duplicated IP creation or changed its original identity", creates, patches)
			}
			actual, err := client.Resource(kcManualVNicIPs).Namespace(b.Namespace).Get(context.Background(), b.ProviderName, metav1.GetOptions{})
			if err != nil || inspectRequestedVNicIP(actual, parent, b, target) != nil {
				t.Fatal("created IP does not carry exact requested address and original parent identity", actual, err)
			}
		})
	}
}

func TestKCVNicRequestedIPParentVersionConflictIsRetryable(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		b, target, parent, child := requestedVNicAddressFixture()
		current := parent.DeepCopy()
		current.SetResourceVersion("8")
		if replaced {
			current.SetUID("foreign-vnic")
		}
		client := fake.NewSimpleDynamicClient(runtime.NewScheme(), current, child)
		client.PrependReactor("patch", "vnics", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewInvalid(schema.GroupKind{Group: "networking.kubercloud.com", Kind: "VNic"}, b.ProviderName, field.ErrorList{field.Invalid(field.NewPath("metadata", "resourceVersion"), "7", "JSON patch test failed")})
		})
		p := &KCProvider{client: client}
		_, err := p.ensureRequestedVNicIP(context.Background(), parent, b, target)
		var failure *biz.ProviderError
		want := biz.ProviderTemporary
		if replaced {
			want = biz.ProviderConflict
		}
		if !errors.As(err, &failure) || failure.Kind != want {
			t.Fatalf("concurrent parent update incorrectly terminated address creation: replaced=%v error=%v", replaced, err)
		}
	}
}

func TestKCVNicStatusIsAuthoritativeWithoutChildReads(t *testing.T) {
	for _, name := range []string{"ready", "missing-address", "different-address", "valid-false", "ready-false", "unreported"} {
		t.Run(name, func(t *testing.T) {
			b, target, parent, _ := requestedVNicAddressFixture()
			parent.SetAnnotations(map[string]string{requestedVNicIPUIDAnnotation: "declared-child"})
			switch name {
			case "missing-address":
				unstructured.RemoveNestedField(parent.Object, "status", "primaryIP")
				unstructured.RemoveNestedField(parent.Object, "status", "ips")
			case "different-address":
				_ = unstructured.SetNestedField(parent.Object, "10.42.1.99", "status", "primaryIP")
				_ = unstructured.SetNestedField(parent.Object, "02:00:00:00:00:99", "spec", "macAddress")
			case "valid-false":
				_ = unstructured.SetNestedSlice(parent.Object, []any{map[string]any{"type": "Ready", "status": "True"}, map[string]any{"type": "Valid", "status": "False"}}, "status", "conditions")
			case "ready-false":
				_ = unstructured.SetNestedSlice(parent.Object, []any{map[string]any{"type": "Ready", "status": "False", "reason": "Waiting"}}, "status", "conditions")
			case "unreported":
				unstructured.RemoveNestedField(parent.Object, "status", "conditions")
			}
			client := fake.NewSimpleDynamicClient(runtime.NewScheme())
			p := &KCProvider{client: client}
			got, err := p.inspectVNic(context.Background(), parent, b, target)
			if err != nil || !got.Exists || got.NeedsUpdate || len(client.Actions()) != 0 {
				t.Fatal(got, err, client.Actions())
			}
			if name == "unreported" {
				if got.Ready != nil {
					t.Fatal(got)
				}
				return
			}
			if got.Ready == nil || *got.Ready != (name != "ready-false") {
				t.Fatal("Ready was reconstructed from child/address facts", got)
			}
		})
	}
}
