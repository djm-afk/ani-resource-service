package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

var jobResource = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
var uploadTokenResource = schema.GroupVersionResource{Group: "upload.cdi.kubevirt.io", Version: "v1beta1", Resource: "uploadtokenrequests"}

// ImageUploadConfig is the fixed deployment input for the upload lane. Nothing
// here may come from a request body.
type ImageUploadConfig struct {
	Enabled                       bool
	ExternalUploadOrigin          string
	ValidatorImage                string
	UploadDeadline                time.Duration
	MaxFileBytes, MaxSourceGB     int64
	StorageClass                  string
	ClusterID                     string
	TokenTTL                      time.Duration
	VerifierBackoffLimit          int32
	VerifierActiveDeadlineSeconds int64
}

// ImageNative extends the shared dynamic client with the CDI upload lane. It is
// a separate type so no image rule can reach into VM power or media code paths.
type ImageNative struct {
	client dynamic.Interface
	config ImageUploadConfig
}

func NewImageNative(c dynamic.Interface, cfg ImageUploadConfig) (*ImageNative, error) {
	if c == nil || cfg.ClusterID == "" || cfg.UploadDeadline <= 0 || cfg.TokenTTL <= 0 ||
		cfg.TokenTTL >= cfg.UploadDeadline || cfg.VerifierActiveDeadlineSeconds < 60 || cfg.VerifierBackoffLimit < 0 {
		return nil, biz.ErrImageInvalid
	}
	// The validator runs from a digest, never a mutable tag, and the upload
	// origin is HTTPS so a browser ticket cannot travel in clear text. The storage
	// class is only needed to provision an upload source, so it stays required
	// exactly where the lane is on: a deployment that never enabled uploads must
	// keep starting with the configuration it had before this feature existed.
	if cfg.Enabled && (cfg.StorageClass == "" || cfg.ValidatorImage == "" || !imageDigestReference(cfg.ValidatorImage) ||
		!strings.HasPrefix(cfg.ExternalUploadOrigin, "https://")) {
		return nil, biz.ErrImageInvalid
	}
	return &ImageNative{client: c, config: cfg}, nil
}

func imageDigestReference(image string) bool {
	parts := strings.Split(image, "@")
	return len(parts) == 2 && parts[0] != "" && strings.HasPrefix(parts[1], "sha256:") && len(parts[1]) == 71
}

func imageLabels(i biz.Image) map[string]string {
	return map[string]string{"ani.io/tenant-id": i.TenantID, "vm.ani.io/image-id": i.ID, "vm.ani.io/managed-by": biz.Owner}
}

// templateLabels copies the labels into the JSON type an unstructured object may hold.
// A map[string]string inside spec.template.metadata.labels survives a real API call but
// makes the client-side fake panic on deep copy, so no test could reach this Job.
func templateLabels(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func imageName(i biz.Image) string { return "vmi-src-" + strings.ReplaceAll(i.ID, "-", "") }
func verifyName(i biz.Image) string {
	return "vmi-verify-" + strings.ReplaceAll(i.ID, "-", "")
}
func tokenName(sessionID string) string { return "vmi-tok-" + strings.ReplaceAll(sessionID, "-", "") }

// matchesImage proves an existing object belongs to exactly this image record.
// Name alone never proves identity, so an adopted object needs all three marks.
func matchesImage(o *unstructured.Unstructured, i biz.Image) bool {
	labels := o.GetLabels()
	return labels["vm.ani.io/image-id"] == i.ID && labels["ani.io/tenant-id"] == i.TenantID &&
		labels["vm.ani.io/managed-by"] == biz.Owner && o.GetNamespace() == i.Native.Namespace
}

func ownerOf(o *unstructured.Unstructured, kind, uid string) bool {
	api := map[string]string{"DataVolume": "cdi.kubevirt.io/v1beta1", "PersistentVolumeClaim": "v1", "Job": "batch/v1"}[kind]
	for _, r := range o.GetOwnerReferences() {
		if uid != "" && r.APIVersion == api && r.Kind == kind && string(r.UID) == uid && r.Controller != nil && *r.Controller {
			return true
		}
	}
	return false
}

func (n *ImageNative) EnsureImageSource(ctx context.Context, i biz.Image) (biz.Image, error) {
	name := imageName(i)
	if i.Native.DVName != "" && i.Native.DVName != name || i.Native.Namespace == "" {
		return i, biz.ErrImageIdentity
	}
	current, e := n.client.Resource(dvResource).Namespace(i.Native.Namespace).Get(ctx, name, metav1.GetOptions{})
	if e == nil {
		if !matchesImage(current, i) || (i.Native.DVUID != "" && i.Native.DVUID != string(current.GetUID())) {
			return i, biz.ErrOwnership
		}
		if i.Native.DVUID == "" {
			i.Native.DVUID = string(current.GetUID())
		}
		i.Native.DVName = name
		return i, nil
	}
	if !apierrors.IsNotFound(e) {
		return i, e
	}
	if i.Native.DVUID != "" {
		return i, biz.ErrOwnership
	}
	mode := biz.SourceVolumeMode(i.Kind)
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cdi.kubevirt.io/v1beta1", "kind": "DataVolume",
		"metadata": map[string]any{"name": name, "namespace": i.Native.Namespace,
			// 66 honours HonorWaitForFirstConsumer globally, so an upload source
			// that waits for a consumer never binds and the upload never starts.
			"annotations": map[string]any{"cdi.kubevirt.io/storage.bind.immediate.requested": "true"}},
		"spec": map[string]any{"source": map[string]any{"upload": map[string]any{}}, "contentType": "kubevirt",
			"storage": map[string]any{"storageClassName": n.config.StorageClass, "volumeMode": mode,
				"accessModes": []any{"ReadWriteOnce"},
				"resources":   map[string]any{"requests": map[string]any{"storage": strconv.FormatInt(i.SourceCapacityGB, 10) + "Gi"}}}}}}
	object.SetLabels(imageLabels(i))
	created, e := n.client.Resource(dvResource).Namespace(i.Native.Namespace).Create(ctx, object, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(e) {
		return n.EnsureImageSource(ctx, i)
	}
	if e != nil {
		return i, e
	}
	i.Native.DVName, i.Native.DVUID = name, string(created.GetUID())
	i.Native.SourceVolumeMode, i.Native.SourceContentType = mode, "kubevirt"
	return i, nil
}

// CreateUploadToken returns the ticket body plus its object identity. The token
// is never persisted; only the session and token identities are recorded.
func (n *ImageNative) CreateUploadToken(ctx context.Context, i biz.Image, sessionID string, ttl time.Duration) (biz.Image, string, time.Time, error) {
	if i.Native.DVUID == "" || !i.Uploadable() {
		return i, "", time.Time{}, biz.ErrImageNotUploadable
	}
	name := tokenName(sessionID)
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "upload.cdi.kubevirt.io/v1beta1", "kind": "UploadTokenRequest",
		"metadata": map[string]any{"name": name, "namespace": i.Native.Namespace},
		"spec":     map[string]any{"pvcName": imageName(i)}}}
	object.SetLabels(imageLabels(i))
	created, e := n.client.Resource(uploadTokenResource).Namespace(i.Native.Namespace).Create(ctx, object, metav1.CreateOptions{})
	if e != nil && !apierrors.IsAlreadyExists(e) {
		return i, "", time.Time{}, e
	}
	if apierrors.IsAlreadyExists(e) {
		created, e = n.client.Resource(uploadTokenResource).Namespace(i.Native.Namespace).Get(ctx, name, metav1.GetOptions{})
		if e != nil || !matchesImage(created, i) {
			return i, "", time.Time{}, biz.ErrOwnership
		}
	}
	token, _, _ := unstructured.NestedString(created.Object, "status", "token")
	if token == "" {
		return i, "", time.Time{}, biz.ErrImageDependency
	}
	i.Native.TokenName, i.Native.TokenUID, i.Native.SessionID = name, string(created.GetUID()), sessionID
	// The ticket object is single-use and must not linger after the response.
	if e = n.client.Resource(uploadTokenResource).Namespace(i.Native.Namespace).Delete(ctx, name, metav1.DeleteOptions{}); e != nil && !apierrors.IsNotFound(e) {
		return i, "", time.Time{}, e
	}
	expiry := time.Now().UTC().Add(ttl)
	return i, token, expiry, nil
}

// UploadURL is built only from the fixed configured origin. A caller-supplied
// origin is never accepted, so a ticket cannot be redirected.
func (n *ImageNative) UploadURL() string {
	return strings.TrimRight(n.config.ExternalUploadOrigin, "/") + "/v1beta1/upload-async"
}

func (n *ImageNative) MaxFileBytes() int64 { return n.config.MaxFileBytes }

// ObserveImageSource follows the real CDI ownership chain: the upload DV, the
// target PVC it produces, and the populator objects CDI creates for it.
func (n *ImageNative) ObserveImageSource(ctx context.Context, i biz.Image) (biz.Image, error) {
	dv, e := n.client.Resource(dvResource).Namespace(i.Native.Namespace).Get(ctx, i.Native.DVName, metav1.GetOptions{})
	if e != nil {
		if apierrors.IsNotFound(e) {
			return i, biz.ErrImageIdentity
		}
		return i, e
	}
	if !matchesImage(dv, i) || i.Native.DVUID != string(dv.GetUID()) {
		return i, biz.ErrOwnership
	}
	phase, _, _ := unstructured.NestedString(dv.Object, "status", "phase")
	pvc, e := n.client.Resource(pvcResource).Namespace(i.Native.Namespace).Get(ctx, imageName(i), metav1.GetOptions{})
	if e != nil {
		if apierrors.IsNotFound(e) {
			return i, biz.ErrDependency
		}
		return i, e
	}
	if !ownerOf(pvc, "DataVolume", i.Native.DVUID) && !matchesImage(pvc, i) {
		return i, biz.ErrOwnership
	}
	mode, _, _ := unstructured.NestedString(pvc.Object, "spec", "volumeMode")
	if mode == "" {
		mode = "Filesystem"
	}
	if mode != i.Native.SourceVolumeMode {
		return i, biz.ErrImageIdentity
	}
	i.Native.PVCName = pvc.GetName()
	i.Native.PVCUID = string(pvc.GetUID())
	requested, _, _ := unstructured.NestedString(pvc.Object, "spec", "resources", "requests", "storage")
	if requested != "" {
		if quantity, parseErr := resource.ParseQuantity(requested); parseErr == nil {
			i.SourceRequestedBytes = quantity.Value()
		}
	}
	if capacity, _, _ := unstructured.NestedString(pvc.Object, "status", "capacity", "storage"); capacity != "" {
		if quantity, parseErr := resource.ParseQuantity(capacity); parseErr == nil {
			i.ObservedSourceCapacityBytes = quantity.Value()
		}
	}
	// CDI reports the upload server's own failure conditions; a DV stuck in
	// Pending is not a rejection, but Failed is terminal and needs no polling.
	conditions, _, _ := unstructured.NestedSlice(dv.Object, "status", "conditions")
	for _, value := range conditions {
		item, _ := value.(map[string]any)
		status, _ := item["status"].(string)
		reason, _ := item["reason"].(string)
		if status != "True" {
			continue
		}
		if reason == "UploadFailed" || reason == "ValidationError" || reason == "ImporterError" || reason == "SchedulingError" {
			i.Reason = "UPLOAD_REJECTED"
		}
	}
	if phase == "Failed" {
		i.Reason = "UPLOAD_REJECTED"
	}
	if phase == "Succeeded" {
		i.Reason = ""
	}
	i.Native.AnnotationResourceVersion = pvc.GetResourceVersion()
	// CDI creates a populator PVC (prime-<pvc-uid>) plus a scratch volume for an
	// upload source. They are registered from what the cluster reports, never
	// assumed, because they are what cleanup is actually allowed to remove.
	kind, _, _ := unstructured.NestedString(pvc.Object, "spec", "dataSourceRef", "kind")
	if kind == "VolumeUploadSource" {
		prime := "prime-" + string(pvc.GetUID())
		object, e := n.client.Resource(pvcResource).Namespace(i.Native.Namespace).Get(ctx, prime, metav1.GetOptions{})
		if e == nil {
			if !containsRef(i.Cleanup, "PersistentVolumeClaim", prime) {
				i.Cleanup = append(i.Cleanup, biz.ObjectRef{Kind: "PersistentVolumeClaim", Namespace: i.Native.Namespace, Name: prime, UID: string(object.GetUID())})
			}
		} else if !apierrors.IsNotFound(e) {
			return i, e
		}
	}
	i.Native.DVPhase = phase
	return i, nil
}

func (n *ImageNative) ObserveImageSourceIdentity(ctx context.Context, i biz.Image) (biz.Image, error) {
	if i.Native.PVCName == "" || i.Native.PVCUID == "" {
		return i, biz.ErrImageIdentity
	}
	pvc, e := n.client.Resource(pvcResource).Namespace(i.Native.Namespace).Get(ctx, i.Native.PVCName, metav1.GetOptions{})
	if e != nil {
		if apierrors.IsNotFound(e) {
			return i, biz.ErrImageIdentity
		}
		return i, e
	}
	if string(pvc.GetUID()) != i.Native.PVCUID || !matchesImage(pvc, i) {
		return i, biz.ErrOwnership
	}
	if pvc.GetDeletionTimestamp() != nil {
		return i, biz.ErrImageIdentity
	}
	phase, _, _ := unstructured.NestedString(pvc.Object, "status", "phase")
	if phase != "Bound" {
		return i, biz.ErrDependency
	}
	mode, _, _ := unstructured.NestedString(pvc.Object, "spec", "volumeMode")
	if mode == "" {
		mode = "Filesystem"
	}
	if mode != i.Native.SourceVolumeMode {
		return i, biz.ErrImageIdentity
	}
	if class, _, _ := unstructured.NestedString(pvc.Object, "spec", "storageClassName"); class != n.config.StorageClass {
		return i, biz.ErrImageIdentity
	}
	if capacity, _, _ := unstructured.NestedString(pvc.Object, "status", "capacity", "storage"); capacity != "" {
		if quantity, e := resource.ParseQuantity(capacity); e == nil {
			i.ObservedSourceCapacityBytes = quantity.Value()
		}
	}
	if i.SourceRequestedBytes < 1 || i.ObservedSourceCapacityBytes < i.SourceRequestedBytes {
		return i, biz.ErrImageIdentity
	}
	i.Native.AnnotationResourceVersion = pvc.GetResourceVersion()
	return i, nil
}

// EnsureImageVerification creates the fixed read-only validation Job. The pod
// template is built here and nowhere else: no user input reaches image, command
// or mount path, and the service account token is not projected at all.
func (n *ImageNative) EnsureImageVerification(ctx context.Context, i biz.Image) (biz.Image, error) {
	if i.Native.PVCUID == "" || i.Native.DVUID == "" {
		return i, biz.ErrImageDependency
	}
	name := verifyName(i)
	block := i.Native.SourceVolumeMode == "Block"
	limits := map[string]any{"cpu": "500m", "memory": "128Mi"}
	requests := map[string]any{"cpu": "50m", "memory": "32Mi"}
	container := map[string]any{
		"name": "verify", "image": n.config.ValidatorImage, "imagePullPolicy": "IfNotPresent",
		"args": []any{"--image-id", i.ID, "--kind", i.Kind, "--volume-mode", i.Native.SourceVolumeMode},
		"securityContext": map[string]any{"allowPrivilegeEscalation": false, "privileged": false,
			"runAsNonRoot": true, "runAsUser": int64(107), "runAsGroup": int64(107), "readOnlyRootFilesystem": true,
			"capabilities": map[string]any{"drop": []any{"ALL"}}},
		"resources":              map[string]any{"limits": limits, "requests": requests},
		"terminationMessagePath": "/dev/termination-log", "terminationMessagePolicy": "File"}
	if block {
		container["volumeDevices"] = []any{map[string]any{"name": "source", "devicePath": "/dev/source"}}
	} else {
		container["volumeMounts"] = []any{map[string]any{"name": "source", "mountPath": "/source", "readOnly": true}}
	}
	pod := map[string]any{"restartPolicy": "Never", "automountServiceAccountToken": false,
		"terminationGracePeriodSeconds": int64(0), "securityContext": map[string]any{"seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"containers": []any{container},
		"volumes":    []any{map[string]any{"name": "source", "persistentVolumeClaim": map[string]any{"claimName": i.Native.PVCName, "readOnly": true}}}}
	job := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]any{"name": name, "namespace": i.Native.Namespace},
		"spec": map[string]any{"backoffLimit": int64(n.config.VerifierBackoffLimit), "completionMode": "NonIndexed",
			"activeDeadlineSeconds": int64(n.config.VerifierActiveDeadlineSeconds), "ttlSecondsAfterFinished": nil,
			"template": map[string]any{"metadata": map[string]any{"labels": templateLabels(imageLabels(i))}, "spec": pod}}}}
	job.SetLabels(imageLabels(i))
	created, e := n.client.Resource(jobResource).Namespace(i.Native.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(e) {
		created, e = n.client.Resource(jobResource).Namespace(i.Native.Namespace).Get(ctx, name, metav1.GetOptions{})
		if e == nil && (!matchesImage(created, i) || i.Native.JobUID != "" && i.Native.JobUID != string(created.GetUID())) {
			return i, biz.ErrOwnership
		}
	}
	if e != nil {
		return i, e
	}
	// Freeze the digest the Job actually runs, not the one configured today: the
	// success path compares the verifying pod's image against this value, so leaving
	// it empty turned a verification that genuinely passed into
	// SOURCE_IDENTITY_CHANGED plus source cleanup.
	ran := verifierImage(created, "spec", "template", "spec")
	if ran == "" {
		return i, biz.ErrOwnership
	}
	if i.Native.VerifierImage != "" && i.Native.VerifierImage != ran {
		return i, biz.ErrImageIdentity
	}
	i.Native.VerifierImage = ran
	i.Native.JobName, i.Native.JobUID = name, string(created.GetUID())
	return i, nil
}

// verifierImage reads the digest of the verify container out of a Job or a Pod. The
// pod template sits at a different depth in each, so the caller names where the pod
// spec starts.
func verifierImage(o *unstructured.Unstructured, podSpec ...string) string {
	path := append(append([]string{}, podSpec...), "containers")
	containers, _, _ := unstructured.NestedSlice(o.Object, path...)
	for _, value := range containers {
		item, _ := value.(map[string]any)
		if name, _ := item["name"].(string); name != "verify" {
			continue
		}
		image, _ := item["image"].(string)
		return image
	}
	return ""
}

type verifierReport struct {
	Pass          bool   `json:"pass"`
	ImageID       string `json:"image_id"`
	Kind          string `json:"kind"`
	VolumeMode    string `json:"volume_mode"`
	Checksum      string `json:"checksum"`
	Bytes         int64  `json:"bytes"`
	DeclaredBytes int64  `json:"declared_bytes"`
	FSType        string `json:"fs_type"`
	ReadOnlyProof string `json:"read_only_proof"`
	Reason        string `json:"reason"`
}

// ObserveImageVerification reads one succeeded pod owned by the registered Job
// and re-checks every identity field before the result can be adopted.
func (n *ImageNative) ObserveImageVerification(ctx context.Context, i biz.Image) (biz.Image, biz.VerificationResult, error) {
	var out biz.VerificationResult
	if i.Native.JobName == "" || i.Native.JobUID == "" {
		return i, out, biz.ErrImageDependency
	}
	job, e := n.client.Resource(jobResource).Namespace(i.Native.Namespace).Get(ctx, i.Native.JobName, metav1.GetOptions{})
	if e != nil {
		if apierrors.IsNotFound(e) {
			return i, out, biz.ErrImageIdentity
		}
		return i, out, e
	}
	if !matchesImage(job, i) || string(job.GetUID()) != i.Native.JobUID {
		return i, out, biz.ErrOwnership
	}
	succeeded, _, _ := unstructured.NestedInt64(job.Object, "status", "succeeded")
	failed, _, _ := unstructured.NestedInt64(job.Object, "status", "failed")
	if succeeded < 1 {
		if failed > 0 {
			out.FailureReason = "SOURCE_VALIDATION_FAILED"
			out.Pass = false
			return i, out, nil
		}
		return i, out, biz.ErrDependency
	}
	pods, e := n.client.Resource(podResource).Namespace(i.Native.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "batch.kubernetes.io/job-name=" + i.Native.JobName})
	if e != nil {
		return i, out, e
	}
	var pod *unstructured.Unstructured
	for index := range pods.Items {
		candidate := &pods.Items[index]
		phase, _, _ := unstructured.NestedString(candidate.Object, "status", "phase")
		if phase != "Succeeded" {
			continue
		}
		if !ownerOfJob(candidate, i.Native.JobUID) {
			return i, out, biz.ErrOwnership
		}
		if pod != nil {
			return i, out, biz.ErrImageIdentity
		}
		pod = candidate
	}
	if pod == nil {
		return i, out, biz.ErrDependency
	}
	i.Native.PodUID = string(pod.GetUID())
	// The result carries the digest that actually ran, so the comparison below checks
	// the pod against the value frozen at creation instead of the current configuration.
	ran := verifierImage(pod, "spec")
	if ran == "" {
		return i, out, biz.ErrOwnership
	}
	report, e := readVerifierReport(pod)
	if e != nil {
		out.FailureReason = "SOURCE_VALIDATION_FAILED"
		return i, out, e
	}
	// The mounted PVC must still be the one this record registered.
	claim := ""
	mounts, _, _ := unstructured.NestedSlice(pod.Object, "spec", "volumes")
	for _, value := range mounts {
		item, _ := value.(map[string]any)
		if item["name"] != "source" {
			continue
		}
		claim = nestedString(item, "persistentVolumeClaim", "claimName")
	}
	if claim != i.Native.PVCName {
		out.FailureReason = "SOURCE_IDENTITY_CHANGED"
		return i, out, nil
	}
	out = biz.VerificationResult{Pass: report.Pass, Checksum: report.Checksum, Bytes: report.Bytes,
		VolumeMode: report.VolumeMode, FSType: report.FSType, ReadOnlyProof: report.ReadOnlyProof,
		JobUID: i.Native.JobUID, PodUID: i.Native.PodUID, VerifierImage: ran,
		ImageID: report.ImageID, PVCUID: i.Native.PVCUID, RequestedBytes: i.SourceRequestedBytes,
		ObservedBytes: i.ObservedSourceCapacityBytes, ObservedAt: time.Now().UTC(), FailureReason: report.Reason}
	if !report.Pass {
		if !biz.ValidImageReason(out.FailureReason) {
			out.FailureReason = "SOURCE_VALIDATION_FAILED"
		}
		out.Pass = false
	}
	if e = out.ValidatedFor(i); e != nil {
		return i, biz.VerificationResult{}, e
	}
	return i, out, nil
}

func ownerOfJob(pod *unstructured.Unstructured, jobUID string) bool {
	for _, r := range pod.GetOwnerReferences() {
		if r.Kind == "Job" && string(r.UID) == jobUID {
			return true
		}
	}
	return false
}

func nestedString(item map[string]any, path ...string) string {
	current := item
	for index, key := range path {
		if index == len(path)-1 {
			value, _ := current[key].(string)
			return value
		}
		next, _ := current[key].(map[string]any)
		if next == nil {
			return ""
		}
		current = next
	}
	return ""
}

func readVerifierReport(pod *unstructured.Unstructured) (verifierReport, error) {
	var report verifierReport
	statuses, _, _ := unstructured.NestedSlice(pod.Object, "status", "containerStatuses")
	for _, value := range statuses {
		item, _ := value.(map[string]any)
		if name, _ := item["name"].(string); name != "verify" {
			continue
		}
		state, _ := item["state"].(map[string]any)
		terminated, _ := state["terminated"].(map[string]any)
		code, _ := terminated["exitCode"].(int64)
		if code != 0 {
			return report, fmt.Errorf("verifier exited with code %d", code)
		}
		message, _ := terminated["message"].(string)
		if message == "" || len(message) > 4096 {
			return report, fmt.Errorf("verifier termination message is absent or oversized")
		}
		if e := json.Unmarshal([]byte(message), &report); e != nil {
			return report, fmt.Errorf("verifier termination message is not valid JSON")
		}
		return report, nil
	}
	return report, fmt.Errorf("verifier container status is missing")
}

// PublishSourceAnnotation writes the checksum annotation under a UID and
// resourceVersion guard, then reads it back. This is the same fence the manual
// import evidence used, so ValidateSource keeps working unchanged.
func (n *ImageNative) PublishSourceAnnotation(ctx context.Context, i biz.Image) error {
	pvc, e := n.client.Resource(pvcResource).Namespace(i.Native.Namespace).Get(ctx, i.Native.PVCName, metav1.GetOptions{})
	if e != nil {
		return e
	}
	if string(pvc.GetUID()) != i.Native.PVCUID || pvc.GetAnnotations()["vm.ani.io/source-sha256"] == i.SourceSHA256 && i.SourceSHA256 != "" {
		if string(pvc.GetUID()) != i.Native.PVCUID {
			return biz.ErrOwnership
		}
		return nil
	}
	patch, _ := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": pvc.GetUID()},
		{"op": "test", "path": "/metadata/resourceVersion", "value": pvc.GetResourceVersion()},
		{"op": "add", "path": "/metadata/annotations/vm.ani.io~1source-sha256", "value": i.SourceSHA256}})
	updated, e := n.client.Resource(pvcResource).Namespace(i.Native.Namespace).Patch(ctx, i.Native.PVCName, types.JSONPatchType, patch, metav1.PatchOptions{})
	if e != nil {
		if apierrors.IsConflict(e) {
			return biz.ErrImageVersion
		}
		return e
	}
	if updated.GetAnnotations()["vm.ani.io/source-sha256"] != i.SourceSHA256 {
		return biz.ErrImageIdentity
	}
	i.Native.AnnotationResourceVersion = updated.GetResourceVersion()
	return nil
}

// DeleteImageObjects removes exactly the registered chain, in dependency order:
// the validation Job, then the populator PVCs CDI created, then the target PVC,
// then the upload DataVolume. Nothing is removed by name pattern, and a name
// held by a different identity stops the pass instead of being overwritten.
// ObserveManagedSources is the cleanup proof. Every name here is either derived
// from the image id or embeds a UID this record already observed, so the probe
// finds objects a crashed worker created but never registered — and it can never
// point at another image's assets. Absence is only reported when every probe in
// the chain returns NotFound.
func (n *ImageNative) ObserveManagedSources(ctx context.Context, i biz.Image) ([]biz.ObjectRef, error) {
	if i.Native.Namespace == "" {
		return nil, biz.ErrImageIdentity
	}
	var found []biz.ObjectRef
	add := func(kind, namespace, name, uid string) {
		found = append(found, biz.ObjectRef{Kind: kind, Namespace: namespace, Name: name, UID: uid})
	}
	get := func(resource schema.GroupVersionResource, name string) (*unstructured.Unstructured, error) {
		object, e := n.client.Resource(resource).Namespace(i.Native.Namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(e) {
			return nil, nil
		}
		return object, e
	}
	if dv, e := get(dvResource, imageName(i)); e != nil {
		return nil, e
	} else if dv != nil {
		if !matchesImage(dv, i) {
			return nil, biz.ErrOwnership
		}
		add("DataVolume", i.Native.Namespace, dv.GetName(), string(dv.GetUID()))
	}
	if job, e := get(jobResource, verifyName(i)); e != nil {
		return nil, e
	} else if job != nil {
		if !matchesImage(job, i) {
			return nil, biz.ErrOwnership
		}
		add("Job", i.Native.Namespace, job.GetName(), string(job.GetUID()))
	}
	if i.Native.TokenName != "" {
		if token, e := get(uploadTokenResource, i.Native.TokenName); e != nil {
			return nil, e
		} else if token != nil {
			add("UploadTokenRequest", i.Native.Namespace, token.GetName(), string(token.GetUID()))
		}
	}
	// The PVC name equals the DV name; its UID is what scopes the CDI children,
	// so the children are probed only once the parent is actually present.
	pvcName := imageName(i)
	if i.Native.PVCName != "" {
		pvcName = i.Native.PVCName
	}
	pvc, e := get(pvcResource, pvcName)
	if e != nil {
		return nil, e
	}
	if pvc == nil {
		return found, nil
	}
	uid := string(pvc.GetUID())
	if i.Native.PVCUID != "" && i.Native.PVCUID != uid {
		return nil, biz.ErrOwnership
	}
	add("PersistentVolumeClaim", i.Native.Namespace, pvc.GetName(), uid)
	for _, child := range []string{"prime-" + uid, "prime-" + uid + "-scratch", pvcName + "-scratch"} {
		object, e := get(pvcResource, child)
		if e != nil {
			return nil, e
		}
		if object != nil {
			add("PersistentVolumeClaim", i.Native.Namespace, object.GetName(), string(object.GetUID()))
		}
	}
	return found, nil
}

func (n *ImageNative) DeleteImageObjects(ctx context.Context, i biz.Image) (biz.Image, bool, error) {
	if i.Native.Namespace == "" {
		return i, i.Native.DVName == "" && i.Native.PVCName == "" && len(i.Cleanup) == 0, nil
	}
	for _, item := range []struct {
		resource schema.GroupVersionResource
		name     string
		uid      string
	}{{jobResource, i.Native.JobName, i.Native.JobUID},
		{uploadTokenResource, i.Native.TokenName, i.Native.TokenUID}} {
		if item.name == "" {
			continue
		}
		e := n.client.Resource(item.resource).Namespace(i.Native.Namespace).Delete(ctx, item.name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: uidPointer(item.uid)}, PropagationPolicy: foreground()})
		if e != nil && !apierrors.IsNotFound(e) && !apierrors.IsConflict(e) {
			return i, false, e
		}
	}
	// The DataVolume goes first: while it exists its populator keeps recreating the
	// child PVCs, so draining the chain before it would chase objects CDI rebuilds
	// with fresh UIDs on every pass. It also owns the source PVC, so removing it is
	// the root of the native chain rather than one of its leaves.
	if i.Native.DVName != "" {
		dv, e := n.client.Resource(dvResource).Namespace(i.Native.Namespace).Get(ctx, i.Native.DVName, metav1.GetOptions{})
		if e == nil {
			if !matchesImage(dv, i) {
				return i, false, biz.ErrOwnership
			}
			e = n.client.Resource(dvResource).Namespace(i.Native.Namespace).Delete(ctx, i.Native.DVName, metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: uidPointer(i.Native.DVUID)}, PropagationPolicy: foreground()})
			if e != nil && !apierrors.IsNotFound(e) {
				return i, false, e
			}
		} else if !apierrors.IsNotFound(e) {
			return i, false, e
		}
	}
	// Populator and scratch PVCs are removed from the registered list head, so a
	// partial pass never loses track of what is still outstanding.
	// A failed upload server Pod can outlive its transfer and hold a PVC of this
	// chain through pvc-protection, which stalls the whole drain. Retire those Pods
	// first; the head alone is not enough, because the blocked PVC may sit deeper in
	// the chain (CDI owns the uploader with the populator PVC, not the source PVC).
	if cleared, e := n.retireFailedUploaders(ctx, i); e != nil {
		return i, false, e
	} else if cleared {
		return i, false, nil
	}
	if len(i.Cleanup) > 0 {
		ref := i.Cleanup[0]
		switch ref.Kind {
		case "PersistentVolumeClaim":
			object, e := n.client.Resource(pvcResource).Namespace(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(e) {
				i.Cleanup = i.Cleanup[1:]
				return i, false, nil
			}
			if e != nil {
				return i, false, e
			}
			if string(object.GetUID()) != ref.UID {
				// The volume names are derived from this record's own source PVC UID,
				// so a name match already proves the slot is ours; a changed UID is CDI
				// rebuilding its child, not a foreign object taking the name. Refresh it
				// and delete on the identity the cluster reports now.
				if !cdiChildName(i, ref.Name) {
					return i, false, biz.ErrOwnership
				}
				ref.UID = string(object.GetUID())
				i.Cleanup[0] = ref
			}
			if object.GetDeletionTimestamp() == nil {
				e = n.client.Resource(pvcResource).Namespace(ref.Namespace).Delete(ctx, ref.Name, metav1.DeleteOptions{
					Preconditions: &metav1.Preconditions{UID: uidPointer(ref.UID)}})
				if e != nil && !apierrors.IsNotFound(e) {
					return i, false, e
				}
			}
			return i, false, nil
		case "PersistentVolume":
			pv, e := n.client.Resource(pvResource).Get(ctx, ref.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(e) {
				i.Cleanup = i.Cleanup[1:]
				return i, false, nil
			}
			if e != nil {
				return i, false, e
			}
			if string(pv.GetUID()) != ref.UID {
				return i, false, biz.ErrOwnership
			}
			// A backend that is not Delete-reclaiming is reported, never removed
			// on a guess, and it blocks the tombstone from closing.
			if policy, _, _ := unstructured.NestedString(pv.Object, "spec", "persistentVolumeReclaimPolicy"); policy != "Delete" {
				return i, false, biz.ErrImageCleanup
			}
			return i, false, nil
		default:
			return i, false, biz.ErrOwnership
		}
	}
	pvcName := i.Native.PVCName
	if pvcName == "" {
		pvcName = i.Native.DVName
	}
	if pvcName != "" {
		current, e := n.client.Resource(pvcResource).Namespace(i.Native.Namespace).Get(ctx, pvcName, metav1.GetOptions{})
		if e != nil && !apierrors.IsNotFound(e) {
			return i, false, e
		}
		if e == nil {
			if i.Native.PVCUID != "" && string(current.GetUID()) != i.Native.PVCUID {
				return i, false, biz.ErrOwnership
			}
			if current.GetDeletionTimestamp() == nil {
				e = n.client.Resource(pvcResource).Namespace(i.Native.Namespace).Delete(ctx, pvcName, metav1.DeleteOptions{
					Preconditions: &metav1.Preconditions{UID: uidPointer(i.Native.PVCUID)}})
				if e != nil && !apierrors.IsNotFound(e) {
					return i, false, e
				}
			}
			if pvName, _, _ := unstructured.NestedString(current.Object, "spec", "volumeName"); pvName != "" && !containsRef(i.Cleanup, "PersistentVolume", pvName) {
				pv, e := n.client.Resource(pvResource).Get(ctx, pvName, metav1.GetOptions{})
				if e == nil {
					i.Cleanup = append(i.Cleanup, biz.ObjectRef{Kind: "PersistentVolume", Name: pvName, UID: string(pv.GetUID())})
				} else if !apierrors.IsNotFound(e) {
					return i, false, e
				}
			}
			return i, false, nil
		}
	}
	// The DataVolume must be gone too, otherwise CDI can still recreate objects
	// under a record that already reads as deleted.
	if i.Native.DVName != "" {
		dv, e := n.client.Resource(dvResource).Namespace(i.Native.Namespace).Get(ctx, i.Native.DVName, metav1.GetOptions{})
		if e == nil {
			if !matchesImage(dv, i) {
				return i, false, biz.ErrOwnership
			}
			e = n.client.Resource(dvResource).Namespace(i.Native.Namespace).Delete(ctx, i.Native.DVName, metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: uidPointer(i.Native.DVUID)}, PropagationPolicy: foreground()})
			if e != nil && !apierrors.IsNotFound(e) {
				return i, false, e
			}
			return i, false, nil
		}
		if !apierrors.IsNotFound(e) {
			return i, false, e
		}
	}
	return i, true, nil
}

// cdiChildName proves the object is a CDI child of this record's own source PVC:
// both populator PVCs embed the parent's UID, and the scratch PVC is the parent's
// name with a suffix, so the name alone can only match inside this record's slot.
func cdiChildName(i biz.Image, name string) bool {
	parent := i.Native.PVCName
	if parent == "" {
		parent = imageName(i)
	}
	if i.Native.PVCUID == "" {
		return false
	}
	return name == "prime-"+i.Native.PVCUID || name == "prime-"+i.Native.PVCUID+"-scratch" ||
		name == parent+"-scratch"
}

// retireFailedUploaders clears the finalizer deadlock that keeps an upload PVC stuck.
//
// CDI deletes the DataVolume with foreground propagation, and the API server stamps the
// same finalizer onto the upload server Pod. That Pod in turn is the workload holding
// its PVC, and pvc-protection refuses to release the PVC while any Pod mounts it, so the
// two objects wait on each other forever and the image never leaves DELETING.
//
// Deleting the Pod does not break the cycle: the delete is refused while it is still
// finalised. Removing foregroundDeletion from a finished Pod lets the garbage collector
// reclaim it; pvc-protection then clears on its own and the drain continues.
func (n *ImageNative) retireFailedUploaders(ctx context.Context, i biz.Image) (bool, error) {
	owned := make(map[string]bool, len(i.Cleanup))
	for _, ref := range i.Cleanup {
		if ref.Kind == "PersistentVolumeClaim" && ref.UID != "" {
			owned[ref.UID] = true
		}
	}
	if len(owned) == 0 {
		return false, nil
	}
	pods, e := n.client.Resource(podResource).Namespace(i.Native.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "cdi.kubevirt.io=cdi-upload-server"})
	if e != nil {
		return false, e
	}
	released := false
	for index := range pods.Items {
		pod := &pods.Items[index]
		owner := ""
		for _, r := range pod.GetOwnerReferences() {
			if r.Kind == "PersistentVolumeClaim" && r.Controller != nil && *r.Controller {
				owner = string(r.UID)
			}
		}
		if !owned[owner] {
			continue
		}
		phase, _, _ := unstructured.NestedString(pod.Object, "status", "phase")
		// A Running uploader is a live transfer; killing it would corrupt data instead
		// of draining the chain, and a Pod without the deadlock has nothing to release.
		if phase != "Failed" && phase != "Succeeded" {
			continue
		}
		if !containsString(pod.GetFinalizers(), "foregroundDeletion") {
			continue
		}
		// Merge patch replaces the whole list, so everything that is not ours has to be
		// sent back. A foreign finalizer is another controller's cleanup contract;
		// dropping it here would retire work this service cannot know about.
		var remaining []string
		for _, finalizer := range pod.GetFinalizers() {
			if finalizer != "foregroundDeletion" {
				remaining = append(remaining, finalizer)
			}
		}
		patch, e := json.Marshal(map[string]any{"metadata": map[string]any{"finalizers": remaining}})
		if e != nil {
			return false, e
		}
		if _, e := n.client.Resource(podResource).Namespace(i.Native.Namespace).Patch(ctx, pod.GetName(),
			types.MergePatchType, patch, metav1.PatchOptions{}); e != nil && !apierrors.IsNotFound(e) {
			return false, e
		}
		// Kept in production: an uploader that had to be unstuck explains a slow
		// delete, and the Pod name is the only handle an operator needs.
		slog.Info("released stuck upload pod", "image_id", i.ID, "pod", pod.GetName())
		released = true
	}
	return released, nil
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

func uidPointer(uid string) *types.UID {
	if uid == "" {
		return nil
	}
	value := types.UID(uid)
	return &value
}

func foreground() *metav1.DeletionPropagation {
	value := metav1.DeletePropagationForeground
	return &value
}

func containsRef(list []biz.ObjectRef, kind, name string) bool {
	for _, item := range list {
		if item.Kind == kind && item.Name == name {
			return true
		}
	}
	return false
}

// ObserveImageClone proves the consumer's own copy finished, is still pending, or is
// gone. Operation success, PVC binding alone, progress or a timeout are never
// sufficient evidence; absence is only evidence for a retiring consumer, which the
// caller decides.
func (n *ImageNative) ObserveImageClone(ctx context.Context, v biz.VM, i biz.Image, r biz.ImageReference) (biz.CloneState, error) {
	target := biz.ObjectRef{}
	if r.ConsumerKind == "SYSTEM_DISK" {
		for _, d := range v.Disks {
			if d.Role == "system" && d.ID == r.ConsumerID {
				target = d.DV
			}
		}
	} else {
		for _, copy := range v.MediaCopies {
			if copy.ID == r.ConsumerID {
				target = copy.DV
			}
		}
	}
	if target.Namespace == "" || target.Name == "" {
		return biz.ClonePending, nil
	}
	dv, e := n.client.Resource(dvResource).Namespace(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(e) {
		return biz.CloneAbsent, nil
	}
	if e != nil {
		return biz.ClonePending, e
	}
	if target.UID != "" && string(dv.GetUID()) != target.UID {
		return biz.ClonePending, biz.ErrOwnership
	}
	phase, _, _ := unstructured.NestedString(dv.Object, "status", "phase")
	if phase != "Succeeded" {
		return biz.ClonePending, nil
	}
	namespace, _, _ := unstructured.NestedString(dv.Object, "spec", "source", "pvc", "namespace")
	name, _, _ := unstructured.NestedString(dv.Object, "spec", "source", "pvc", "name")
	if namespace != i.Native.Namespace || name != i.Native.PVCName {
		return biz.ClonePending, biz.ErrImageIdentity
	}
	pvc, e := n.client.Resource(pvcResource).Namespace(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	if e != nil {
		if apierrors.IsNotFound(e) {
			return biz.ClonePending, nil
		}
		return biz.ClonePending, e
	}
	phase, _, _ = unstructured.NestedString(pvc.Object, "status", "phase")
	mode, _, _ := unstructured.NestedString(pvc.Object, "spec", "volumeMode")
	if phase != "Bound" || pvc.GetDeletionTimestamp() != nil {
		return biz.ClonePending, nil
	}
	if mode == "" {
		mode = "Filesystem"
	}
	if mode != biz.SourceVolumeMode(i.Kind) {
		return biz.ClonePending, biz.ErrImageIdentity
	}
	return biz.CloneCompleted, nil
}

var _ biz.ImageNativePort = (*ImageNative)(nil)
