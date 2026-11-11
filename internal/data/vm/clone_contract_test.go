package vm

import (
	"slices"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func contractVM() biz.VM {
	return biz.VM{TenantID: "64c5c35a-f554-432c-b4bc-7b4f36f08420", ID: "21cf875f-781a-41db-81cf-7063f4f4cfc4", Spec: biz.Spec{Digest: "digest", Namespace: "tenant-a"}}
}

func contractRequest() biz.CloneRequest {
	return biz.CloneRequest{
		Name:         "vm-clone-21cf875f-781a-41db-81cf-7063f4f4cfc4",
		OperationID:  "b492e0ba-f34e-4143-a9dc-94427c2d3bec",
		TargetName:   "vm-21cf875f-781a-41db-81cf-7063f4f4cfc4",
		SMBiosSerial: "21cf875f-781a-41db-81cf-7063f4f4cfc4",
		Source:       biz.ObjectRef{Kind: "VirtualMachine", Namespace: "tenant-a", Name: "vm-source", UID: "source-uid"},
	}
}

// TestCloneObjectContract asserts the generated VirtualMachineClone: identity,
// both explicit typed references, randomize policy, no patches, and four filter
// groups that keep everything then drop exactly the 21 known source keys.
func TestCloneObjectContract(t *testing.T) {
	v, r := contractVM(), contractRequest()
	object := newCloneObject(v, r)
	if object.GetAPIVersion() != "clone.kubevirt.io/v1beta1" || object.GetKind() != "VirtualMachineClone" {
		t.Fatalf("unexpected object identity: %s %s", object.GetAPIVersion(), object.GetKind())
	}
	if object.GetName() != r.Name || object.GetNamespace() != v.Spec.Namespace {
		t.Fatalf("unexpected name/namespace: %s/%s", object.GetNamespace(), object.GetName())
	}
	if object.GetLabels()["vm.ani.io/vm-id"] != v.ID || object.GetAnnotations()[CloneCreateOperationAnnotation] != r.OperationID {
		t.Fatalf("target identity or operation anchor missing: %v %v", object.GetLabels(), object.GetAnnotations())
	}
	if _, found, _ := unstructured.NestedSlice(object.Object, "spec", "patches"); found {
		t.Fatal("clone must not carry spec.patches")
	}
	source, _, _ := unstructured.NestedStringMap(object.Object, "spec", "source")
	target, _, _ := unstructured.NestedStringMap(object.Object, "spec", "target")
	for _, ref := range []map[string]string{source, target} {
		if ref["apiGroup"] != "kubevirt.io" || ref["kind"] != "VirtualMachine" {
			t.Fatalf("typed reference is incomplete: %v", ref)
		}
	}
	if source["name"] != r.Source.Name || target["name"] != r.TargetName {
		t.Fatalf("typed reference names differ: %v %v", source, target)
	}
	if policy, _, _ := unstructured.NestedString(object.Object, "spec", "volumeNamePolicy"); policy != "RandomizeNames" {
		t.Fatalf("volumeNamePolicy = %q", policy)
	}
	if serial, _, _ := unstructured.NestedString(object.Object, "spec", "newSMBiosSerial"); serial != r.SMBiosSerial {
		t.Fatalf("newSMBiosSerial = %q", serial)
	}
	groups := [][]string{
		{"spec", "labelFilters"},
		{"spec", "annotationFilters"},
		{"spec", "template", "labelFilters"},
		{"spec", "template", "annotationFilters"},
	}
	// A06: the 21 dropped keys are compared literally, not by count or shape.
	expected := []string{
		"*",
		"!^vm[.]ani[.]io/vm-id$",
		"!^vm[.]ani[.]io/create-digest$",
		"!^vm[.]ani[.]io/clone-create-operation-id$",
		"!^network[.]ani[.]io/instance-id$",
		"!^network[.]ani[.]io/attachment-id$",
		"!^network[.]ani[.]io/submission-id$",
		"!^network[.]ani[.]io/generation$",
		"!^network[.]ani[.]io/interface-revision$",
		"!^networking[.]kubercloud[.]com/vnic$",
		"!^kubectl[.]kubernetes[.]io/last-applied-configuration$",
		"!^vm[.]ani[.]io/media-id$",
		"!^vm[.]ani[.]io/media-source-digest$",
		"!^vm[.]ani[.]io/initialization-intent$",
		"!^vm[.]ani[.]io/network-initialization$",
		"!^vm[.]ani[.]io/snapshot-id$",
		"!^vm[.]ani[.]io/snapshot-uid$",
		"!^vm[.]ani[.]io/source-vm-uid$",
		"!^vm[.]ani[.]io/source-sha256$",
		"!^verification[.]ani[.]io/run-id$",
		"!^verification[.]ani[.]io/preparation-method$",
		"!^verification[.]ani[.]io/origin-vm-uid$",
	}
	filters := make([]string, 0, len(cloneFilters))
	for _, value := range cloneFilters {
		text, ok := value.(string)
		if !ok {
			t.Fatalf("cloneFilters has a non-string entry: %v", value)
		}
		filters = append(filters, text)
	}
	if !slices.Equal(expected, filters) {
		t.Fatalf("cloneFilters differ from the contract: %v", filters)
	}
	for _, path := range groups {
		values, found, err := unstructured.NestedStringSlice(object.Object, path...)
		if err != nil || !found {
			t.Fatalf("%v missing: %v", path, err)
		}
		if !slices.Equal(expected, values) {
			t.Fatalf("%v differs from the contract: %v", path, values)
		}
	}
}

// TestProjectCloneOwnership rejects any same-name object that is not this
// operation's target.
func TestProjectCloneOwnership(t *testing.T) {
	v, r := contractVM(), contractRequest()
	object := newCloneObject(v, r)
	// A different target VM id shares nothing with this operation.
	other := contractVM()
	other.ID = "11111111-1111-4111-8111-111111111111"
	if _, err := (&Native{}).projectClone(nil, other, r, object); err == nil {
		t.Fatal("adopted a clone created for another target")
	}
	// A different operation cannot adopt the same object either.
	otherOp := r
	otherOp.OperationID = "22222222-2222-4222-8222-222222222222"
	if _, err := (&Native{}).projectClone(nil, v, otherOp, object); err == nil {
		t.Fatal("adopted a clone created by another operation")
	}
	// A different source name is also a mismatch.
	otherSource := r
	otherSource.Source.Name = "vm-other"
	if _, err := (&Native{}).projectClone(nil, v, otherSource, object); err == nil {
		t.Fatal("adopted a clone with another source")
	}
}
