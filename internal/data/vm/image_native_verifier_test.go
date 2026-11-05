package vm

import (
	"strings"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

var testVerifierImage = "10.10.1.66:30003/ani-gpu-platform/ani-vm-image-verify@sha256:" + "e" + strings.Repeat("0", 63)

func verifierRecord() biz.Image {
	i := biz.Image{TenantID: "64c5c35a-0000-4000-8000-000000000001", ID: "2b1f0c2e-6d3a-4f57-9a10-3f7f5c1d8a11"}
	i.Native = biz.ImageNative{Namespace: "tenant-a", PVCName: "vmi-src-x", PVCUID: "pvc-uid", DVUID: "dv-uid",
		SourceVolumeMode: "Block"}
	return i
}

func verifierJobClient(t *testing.T, i biz.Image, image, uid string) *fake.FakeDynamicClient {
	t.Helper()
	c := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{jobResource: "JobList"})
	if uid == "" {
		// The API server assigns the identity the record freezes; the fake tracker does not.
		c.PrependReactor("create", "jobs", func(action ktesting.Action) (bool, runtime.Object, error) {
			created := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
			created.SetUID(types.UID("1b0a5e0c-0000-4000-8000-000000000009"))
			if e := c.Tracker().Add(created); e != nil {
				return true, nil, e
			}
			return true, created, nil
		})
		return c
	}
	job := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": verifyName(i), "namespace": i.Native.Namespace, "uid": uid},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{"name": "verify", "image": image}}}}}}}
	job.SetLabels(imageLabels(i))
	if e := c.Tracker().Add(job); e != nil {
		t.Fatal(e)
	}
	return c
}

// TestEnsureImageVerificationFreezesTheVerifierImage is the #2 guard: the created Job's
// image digest has to be recorded on the record, because the success path compares the
// result against Native.VerifierImage. Leaving it empty turned a verification that
// genuinely passed into SOURCE_IDENTITY_CHANGED plus source cleanup.
func TestEnsureImageVerificationFreezesTheVerifierImage(t *testing.T) {
	i := verifierRecord()
	n := &ImageNative{client: verifierJobClient(t, i, "", ""), config: ImageUploadConfig{ValidatorImage: testVerifierImage}}

	out, e := n.EnsureImageVerification(t.Context(), i)
	if e != nil {
		t.Fatalf("verification job refused: %v", e)
	}
	if out.Native.JobUID == "" {
		t.Fatal("job identity not registered")
	}
	if out.Native.VerifierImage != testVerifierImage {
		t.Fatalf("verifier image not frozen on the record: %q", out.Native.VerifierImage)
	}
}

// TestEnsureImageVerificationAdoptsTheExistingJobDigest covers the other half of the same
// guard: a Job taken over from an earlier attempt freezes the digest it actually runs.
func TestEnsureImageVerificationAdoptsTheExistingJobDigest(t *testing.T) {
	i := verifierRecord()
	n := &ImageNative{client: verifierJobClient(t, i, testVerifierImage, "1b0a5e0c-0000-4000-8000-000000000009"),
		config: ImageUploadConfig{ValidatorImage: testVerifierImage}}

	out, e := n.EnsureImageVerification(t.Context(), i)
	if e != nil {
		t.Fatalf("adopting the existing verification job: %v", e)
	}
	if out.Native.JobUID != "1b0a5e0c-0000-4000-8000-000000000009" || out.Native.VerifierImage != testVerifierImage {
		t.Fatalf("adopted job not frozen: uid=%q image=%q", out.Native.JobUID, out.Native.VerifierImage)
	}
	// The same record may not silently move to another digest.
	i.Native.VerifierImage = "registry/ani-vm-image-verify@sha256:" + strings.Repeat("f", 64)
	if _, e = n.EnsureImageVerification(t.Context(), i); e != biz.ErrImageIdentity {
		t.Fatalf("a verifier digest swap was accepted as %v, want the identity error", e)
	}
}

// TestNewImageNativeValidatesOnlyWhatUploadNeeds is the #6 guard: a deployment that
// leaves MediaStorageClass empty and keeps the image lane disabled must still start,
// because that field was optional before this feature existed.
func TestNewImageNativeValidatesOnlyWhatUploadNeeds(t *testing.T) {
	c := fake.NewSimpleDynamicClient(runtime.NewScheme())
	if _, e := NewImageNative(c, ImageUploadConfig{Enabled: false, ClusterID: "cluster-a",
		UploadDeadline: 24 * time.Hour, TokenTTL: 5 * time.Minute, VerifierActiveDeadlineSeconds: 900}); e != nil {
		t.Fatalf("disabled image lane rejected an otherwise valid configuration: %v", e)
	}
	// Enabled upload still has to be strict about everything it actually uses.
	if _, e := NewImageNative(c, ImageUploadConfig{Enabled: true, ClusterID: "cluster-a",
		UploadDeadline: 24 * time.Hour, TokenTTL: 5 * time.Minute, VerifierActiveDeadlineSeconds: 900}); e == nil {
		t.Fatal("enabled image lane accepted a configuration without a storage class")
	}
}
