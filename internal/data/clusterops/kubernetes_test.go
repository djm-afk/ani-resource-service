package clusterops

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/clusterops"
	"io"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func errorReason(err error) string {
	var e *biz.Error
	if errors.As(err, &e) {
		return e.Reason
	}
	if err == nil {
		return ""
	}
	return err.Error()
}
func output(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func discoveryResponse(w http.ResponseWriter, r *http.Request) bool {
	for _, prefix := range []string{"/api/", "/apis/"} {
		if !strings.HasPrefix(r.URL.Path, prefix) {
			continue
		}
		path := strings.TrimPrefix(r.URL.Path, prefix)
		parts := strings.Split(path, "/")
		group, version := "", path
		if prefix == "/apis/" {
			if len(parts) != 2 {
				return false
			}
			group, version = parts[0], parts[1]
		} else if len(parts) != 1 {
			return false
		}
		resources := []metav1.APIResource{}
		for _, d := range builtins {
			if d.Type.Group == group && d.Type.Version == version {
				resources = append(resources, metav1.APIResource{Name: d.Type.Resource, Kind: d.Kind, Namespaced: d.Namespaced, Verbs: metav1.Verbs{"list", "get", "create", "update", "delete"}})
			}
		}
		output(w, metav1.APIResourceList{GroupVersion: path, APIResources: resources})
		return true
	}
	return false
}
func nativeTest(t *testing.T, handler http.HandlerFunc) (*Kubernetes, *rest.Config) {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if discoveryResponse(w, r) {
			return
		}
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	cfg := &rest.Config{Host: s.URL, ContentConfig: rest.ContentConfig{ContentType: "application/json", AcceptContentTypes: "application/json"}}
	k, e := NewKubernetes(cfg)
	if e != nil {
		t.Fatal(e)
	}
	return k, cfg
}
func configRef() biz.ResourceRef {
	return biz.ResourceRef{ClusterID: "cluster", Type: biz.ResourceTypeRef{Version: "v1", Resource: "configmaps"}, Namespace: "platform", Name: "x"}
}
func decode(t *testing.T, s string) *unstructured.Unstructured {
	t.Helper()
	o, e := decodeManifestStrict(s)
	if e != nil {
		t.Fatal(e)
	}
	return o
}
func object() map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x", "namespace": "platform", "uid": "uid", "resourceVersion": "rv", "creationTimestamp": "2026-01-01T00:00:00Z", "generation": int64(4)}, "data": map[string]any{"x": "old"}, "status": map[string]any{"state": "native"}}
}
func TestStrictManifestDuplicateKeysSingleObjectAndInt64(t *testing.T) {
	for _, s := range []string{
		"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x, name: private-sentinel}",
		"apiVersion: v1\nkind: ConfigMap\ndata:\n  nested: {x: one, x: private-sentinel}",
		"{\"apiVersion\":\"v1\",\"data\":{\"nested\":{\"n\":1,\"n\":9007199254740993}}}",
		"{\"x\":1,\"x\":2}", "{} {}", "{}\n{}", "kind: ConfigMap\n...\nkind: Secret", "kind: ConfigMap\n---\nkind: Secret", "[]", "null", "kind: List\nitems: []", "",
	} {
		if _, e := decodeManifestStrict(s); errorReason(e) != "INVALID_ARGUMENT" || strings.Contains(e.Error(), "sentinel") {
			t.Fatalf("unsafe parse: %q %v", s, e)
		}
	}
	for _, s := range []string{"# leading\n---\napiVersion: test.io/v1\nkind: Widget\nmetadata: {name: x}\nspec: {count: 9007199254740993, Count: 2}\n# trailing\n", "{\"apiVersion\":\"test.io/v1\",\"kind\":\"Widget\",\"metadata\":{\"name\":\"x\"},\"spec\":{\"count\":9007199254740993,\"Count\":2}}"} {
		obj := decode(t, s)
		n, ok, e := unstructured.NestedInt64(obj.Object, "spec", "count")
		if e != nil || !ok || n != 9007199254740993 {
			t.Fatal(obj, e)
		}
		r, e := project(biz.ResourceRef{}, obj)
		if e != nil {
			t.Fatal(e)
		}
		round := decode(t, r.ManifestYAML)
		n, _, _ = unstructured.NestedInt64(round.Object, "spec", "count")
		if n != 9007199254740993 {
			t.Fatal("integer precision lost", r.ManifestYAML)
		}
	}
}
func TestManifestTargetSystemFieldsAndSecretProjection(t *testing.T) {
	r := configRef()
	d := builtins[10]
	for _, s := range []string{"apiVersion: v1\nkind: ConfigMap\nmetadata: {name: other}", "apiVersion: v1\nkind: Secret\nmetadata: {name: x}", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x, namespace: tenant}", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x, generateName: prefix}"} {
		if e := matchManifest(decode(t, s), r, d); e == nil {
			t.Fatal(s)
		}
	}
	o := decode(t, "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}")
	if e := matchManifest(o, r, d); e != nil || o.GetNamespace() != "platform" {
		t.Fatal(o, e)
	}
	for _, s := range []string{"metadata: {uid: x}", "metadata: {resourceVersion: x}", "metadata: {managedFields: []}", "status: {}"} {
		if e := checkCreateFields(decode(t, "apiVersion: v1\nkind: ConfigMap\n"+s)); errorReason(e) != "SYSTEM_FIELD_READ_ONLY" {
			t.Fatal(e)
		}
	}
	secret := decode(t, "apiVersion: v1\nkind: Secret\nmetadata: {name: x, annotations: {last-applied: private-sentinel}}\ndata: {b: cHJpdmF0ZS1zZW50aW5lbA==}\nstringData: {a: private-sentinel}")
	sr := configRef()
	sr.Type.Resource = "secrets"
	out, e := project(sr, secret)
	if e != nil || !reflect.DeepEqual(out.SecretKeys, []string{"a", "b"}) || strings.Contains(string(out.DisplayJSON), "private-sentinel") || strings.Contains(string(out.DisplayJSON), "annotations") || strings.Contains(string(out.DisplayJSON), "cHJpdmF0ZS") {
		t.Fatal(out, e)
	}
	if !strings.Contains(string(out.JSON), "private-sentinel") || !strings.Contains(out.ManifestYAML, "private-sentinel") {
		t.Fatal("explicit data lost")
	}
	if _, present := secret.Object["data"]; !present {
		t.Fatal("projection mutated original")
	}
}
func TestNativeUpdatePreservesStatusAndVersionsWithoutForce(t *testing.T) {
	var puts atomic.Int32
	conflict := false
	k, _ := nativeTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			output(w, object())
			return
		}
		if r.Method != "PUT" {
			t.Error(r.Method)
		}
		puts.Add(1)
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		if !reflect.DeepEqual(got["status"], object()["status"]) {
			t.Error("status was removed or changed", got)
		}
		meta := got["metadata"].(map[string]any)
		if meta["uid"] != "uid" || meta["resourceVersion"] != "rv" {
			t.Error(meta)
		}
		if r.URL.Query().Get("fieldValidation") != "Strict" || r.URL.Query().Get("fieldManager") != fieldManager || r.URL.Query().Get("dryRun") != "All" {
			t.Error(r.URL)
		}
		if conflict {
			w.WriteHeader(409)
			output(w, metav1.Status{Status: "Failure", Reason: metav1.StatusReasonConflict, Code: 409, Message: "private-sentinel"})
			return
		}
		output(w, object())
	})
	cmd := biz.UpdateCommand{Ref: configRef(), ManifestYAML: "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\ndata: {x: new}", ExpectedUID: "uid", ExpectedResourceVersion: "rv", DryRun: true}
	if _, e := k.UpdateResource(context.Background(), cmd); e != nil {
		t.Fatal(e)
	}
	for _, suffix := range []string{"\nstatus: {state: edited}", "\nmetadata: {name: x, uid: other}"} {
		c := cmd
		c.ManifestYAML += suffix
		if _, e := k.UpdateResource(context.Background(), c); e == nil {
			t.Fatal("system change accepted")
		}
	}
	for _, uid := range []bool{true, false} {
		c := cmd
		want := "RESOURCE_VERSION_CONFLICT"
		c.ExpectedResourceVersion = "old"
		if uid {
			c.ExpectedUID = "old"
			want = "RESOURCE_UID_CHANGED"
		}
		if _, e := k.UpdateResource(context.Background(), c); errorReason(e) != want {
			t.Fatal(e)
		}
	}
	if puts.Load() != 1 {
		t.Fatal("invalid writes sent", puts.Load())
	}
	conflict = true
	if _, e := k.UpdateResource(context.Background(), cmd); errorReason(e) != "RESOURCE_VERSION_CONFLICT" || strings.Contains(e.Error(), "sentinel") {
		t.Fatal(e)
	}
	if puts.Load() != 2 {
		t.Fatal("write retried")
	}
}
func TestNativeCreateDeleteAndSafeError(t *testing.T) {
	var calls atomic.Int32
	k, _ := nativeTest(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.Method {
		case "POST":
			if r.URL.Query().Get("fieldValidation") != "Strict" {
				t.Error(r.URL)
			}
			w.WriteHeader(422)
			output(w, metav1.Status{Status: "Failure", Reason: metav1.StatusReasonInvalid, Code: 422, Message: "manifest private-sentinel", Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{{Field: "data.x", Message: "private-sentinel"}}}})
		case "DELETE":
			var d metav1.DeleteOptions
			_ = json.NewDecoder(r.Body).Decode(&d)
			if d.Preconditions == nil || d.Preconditions.UID == nil || string(*d.Preconditions.UID) != "old-uid" || d.GracePeriodSeconds != nil || d.PropagationPolicy != nil {
				t.Error(d)
			}
			w.WriteHeader(409)
			output(w, metav1.Status{Reason: metav1.StatusReasonConflict, Code: 409, Message: "private-sentinel"})
		default:
			t.Error("unexpected", r.Method)
		}
	})
	c := biz.CreateCommand{Ref: configRef(), ManifestYAML: "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\ndata: {x: private-sentinel}"}
	if _, e := k.CreateResource(context.Background(), c); errorReason(e) != "KUBERNETES_INVALID" || strings.Contains(e.Error(), "sentinel") {
		t.Fatal(e)
	}
	if _, e := k.DeleteResource(context.Background(), configRef(), "old-uid"); errorReason(e) != "RESOURCE_UID_CHANGED" {
		t.Fatal(e)
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected retry", calls.Load())
	}
}
func TestNativeSelectorsPaginationAndExpired(t *testing.T) {
	k, _ := nativeTest(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("limit") != "17" || q.Get("continue") != "opaque" || q.Get("labelSelector") != "app=x" || !strings.Contains(q.Get("fieldSelector"), "metadata.name=pod") || !strings.Contains(q.Get("fieldSelector"), "spec.nodeName=node1") {
			t.Error(r.URL)
		}
		w.WriteHeader(410)
		output(w, metav1.Status{Reason: metav1.StatusReasonExpired, Code: 410, Message: "expired"})
	})
	q := biz.ListQuery{Type: biz.ResourceTypeRef{Version: "v1", Resource: "pods"}, AllNamespaces: true, PageSize: 17, ContinueToken: "opaque", Name: "pod", LabelSelector: "app=x", FieldSelector: "spec.nodeName=node1,status.phase=Running"}
	if _, e := k.ListResources(context.Background(), q); errorReason(e) != "LIST_EXPIRED" {
		t.Fatal(e)
	}
	q.FieldSelector = "metadata.name=other"
	if _, e := k.ListResources(context.Background(), q); errorReason(e) != "INVALID_ARGUMENT" {
		t.Fatal(e)
	}
	q.FieldSelector = "metadata.name!=pod"
	if _, e := k.ListResources(context.Background(), q); errorReason(e) != "INVALID_ARGUMENT" {
		t.Fatal(e)
	}
}
func TestRestartMinimalPatchAndUnsupportedStrategies(t *testing.T) {
	for _, strategy := range []string{"rolling", "paused", "OnDelete", "partition"} {
		t.Run(strategy, func(t *testing.T) {
			patches := 0
			k, _ := nativeTest(t, func(w http.ResponseWriter, r *http.Request) {
				o := object()
				o["apiVersion"] = "apps/v1"
				o["kind"] = "StatefulSet"
				spec := map[string]any{"template": map[string]any{"metadata": map[string]any{"annotations": map[string]any{"keep": "value"}}}}
				if strategy == "paused" {
					spec["paused"] = true
				}
				if strategy == "OnDelete" {
					spec["updateStrategy"] = map[string]any{"type": "OnDelete"}
				}
				if strategy == "partition" {
					spec["updateStrategy"] = map[string]any{"rollingUpdate": map[string]any{"partition": int64(1)}}
				}
				o["spec"] = spec
				if r.Method == "PATCH" {
					patches++
					var patch map[string]any
					_ = json.NewDecoder(r.Body).Decode(&patch)
					if len(patch) != 2 || patch["metadata"].(map[string]any)["resourceVersion"] != "rv" || !strings.Contains(fmt.Sprint(patch), "restartedAt") || strings.Contains(fmt.Sprint(patch), "updateStrategy") {
						t.Error(patch)
					}
				}
				output(w, o)
			})
			r := configRef()
			r.Type = biz.ResourceTypeRef{Group: "apps", Version: "v1", Resource: "statefulsets"}
			if strategy == "paused" {
				r.Type.Resource = "deployments"
			}
			_, e := k.RestartWorkload(context.Background(), biz.RestartCommand{Ref: r, ExpectedUID: "uid", ExpectedResourceVersion: "rv"})
			if strategy == "rolling" {
				if e != nil || patches != 1 {
					t.Fatal(e, patches)
				}
			} else if errorReason(e) != "RESTART_STRATEGY_UNSUPPORTED" || patches != 0 {
				t.Fatal(e, patches)
			}
		})
	}
}
func TestScaleZeroUsesNativeScaleWithConditions(t *testing.T) {
	for _, resource := range []string{"deployments", "statefulsets"} {
		t.Run(resource, func(t *testing.T) {
			k, _ := nativeTest(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/"+resource+"/x/scale") {
					t.Error(r.URL)
				}
				scale := map[string]any{"apiVersion": "autoscaling/v1", "kind": "Scale", "metadata": map[string]any{"uid": "uid", "resourceVersion": "rv", "name": "x"}, "spec": map[string]any{"replicas": 0}, "status": map[string]any{"replicas": 3}}
				if r.Method == "PUT" {
					var got map[string]any
					_ = json.NewDecoder(r.Body).Decode(&got)
					// Native Scale's int32 zero is allowed to use omitempty on the wire.
					replicas := got["spec"].(map[string]any)["replicas"]
					if (replicas != nil && replicas != float64(0)) || got["metadata"].(map[string]any)["resourceVersion"] != "rv" || r.URL.Query().Get("dryRun") != "All" {
						t.Error(got, r.URL)
					}
				}
				output(w, scale)
			})
			zero := int32(0)
			r := configRef()
			r.Type = biz.ResourceTypeRef{Group: "apps", Version: "v1", Resource: resource}
			out, e := k.ScaleWorkload(context.Background(), biz.ScaleCommand{Ref: r, ExpectedUID: "uid", ExpectedResourceVersion: "rv", Replicas: &zero, DryRun: true})
			if e != nil || out.DesiredReplicas != 0 || out.ObservedReplicas != 3 {
				t.Fatal(out, e)
			}
		})
	}
}

type warningRecorder struct{ calls atomic.Int32 }

func (w *warningRecorder) HandleWarningHeaderWithContext(context.Context, int, string, string) {
	w.calls.Add(1)
}
func TestWarningsSuppressedOnAllClusterOpsClientsOnlyAndLogsNativeOptions(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Warning", "299 test \"private-warning-sentinel\"")
		if discoveryResponse(w, r) {
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/log"):
			q := r.URL.Query()
			if q.Get("container") != "main" || q.Get("follow") != "true" || q.Get("tailLines") != "0" || q.Has("sinceSeconds") {
				t.Error(r.URL)
			}
			w.Write([]byte("private-log-sentinel"))
		case strings.Contains(r.URL.Path, "/pods/"):
			output(w, &corev1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "platform", UID: "pod-uid"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}})
		case strings.HasSuffix(r.URL.Path, "/configmaps/x"):
			output(w, object())
		default:
			output(w, &corev1.NamespaceList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NamespaceList"}})
		}
	}))
	defer s.Close()
	recorder := new(warningRecorder)
	cfg := &rest.Config{Host: s.URL, Timeout: time.Second, WarningHandlerWithContext: recorder}
	k, e := NewKubernetes(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if cfg.Timeout != time.Second || cfg.WarningHandlerWithContext != recorder {
		t.Fatal("Owner config mutated")
	}
	owner, e := kubernetes.NewForConfig(cfg)
	if e != nil {
		t.Fatal(e)
	}
	_, e = owner.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	if e != nil {
		t.Fatal(e)
	}
	before := recorder.calls.Load()
	if _, e = k.ResolveResourceType(context.Background(), "cluster", configRef().Type); e != nil {
		t.Fatal(e)
	}
	if _, e = k.GetResource(context.Background(), configRef()); e != nil {
		t.Fatal(e)
	}
	zero := int64(0)
	opened, e := k.OpenPodLogs(context.Background(), biz.PodLogQuery{Namespace: "platform", PodName: "x", ExpectedPodUID: "pod-uid", Follow: true, TailLines: &zero})
	if e != nil {
		t.Fatal(e)
	}
	_, _ = io.Copy(io.Discard, opened.Reader)
	_ = opened.Reader.Close()
	if recorder.calls.Load() != before {
		t.Fatal("clusterops passed HTTP Warning to owner logger")
	}
	_, _ = owner.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{})
	if recorder.calls.Load() != before+1 {
		t.Fatal("Owner warning behavior changed")
	}
}
func TestOverviewPaginationAndParentDeadline(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			var namespaces atomic.Int32
			k, _ := nativeTest(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/version":
					output(w, map[string]any{"gitVersion": "v1.35.8"})
				case "/api/v1/nodes":
					if r.URL.Query().Get("continue") == "next" {
						if cancelled {
							<-r.Context().Done()
							return
						}
						output(w, map[string]any{"kind": "NodeList", "apiVersion": "v1", "items": []any{}})
						return
					}
					output(w, map[string]any{"kind": "NodeList", "apiVersion": "v1", "metadata": map[string]any{"continue": "next"}, "items": []any{map[string]any{"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}, "capacity": map[string]any{"cpu": "2", "memory": "1Gi", "vendor.io/gpu": "3"}, "allocatable": map[string]any{"cpu": "1500m"}}}}})
				case "/api/v1/namespaces":
					namespaces.Add(1)
					output(w, map[string]any{"kind": "NamespaceList", "apiVersion": "v1", "items": []any{map[string]any{}}})
				case "/api/v1/pods":
					output(w, map[string]any{"kind": "PodList", "apiVersion": "v1", "items": []any{map[string]any{"status": map[string]any{"phase": "Running"}}, map[string]any{}}})
				default:
					t.Error(r.URL)
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			out, e := k.GetClusterOverview(ctx, "cluster")
			if cancelled {
				if !errors.Is(e, context.DeadlineExceeded) || namespaces.Load() != 0 {
					t.Fatal(out, e)
				}
			} else if e != nil || out.NodesTotal != 1 || out.NodesReady != 1 || out.PodsTotal != 2 || out.PodPhaseCounts["Unknown"] != 1 || out.Capacity["vendor.io/gpu"] != "3" || out.Allocatable["cpu"] != "1500m" {
				t.Fatal(out, e)
			}
		})
	}
}

func TestTLSLogClientKeepsNativeTransportAndBodyContext(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/log") {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			time.Sleep(60 * time.Millisecond)
			_, _ = w.Write([]byte("tail"))
			return
		}
		output(w, &corev1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Name: "x", UID: "uid"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}})
	}))
	defer s.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
	cfg := &rest.Config{Host: s.URL, Timeout: 20 * time.Millisecond, TLSClientConfig: rest.TLSClientConfig{CAData: ca}}
	k, e := NewKubernetes(cfg)
	if e != nil {
		t.Fatal(e)
	}
	opened, e := k.OpenPodLogs(context.Background(), biz.PodLogQuery{Namespace: "platform", PodName: "x"})
	if e != nil {
		t.Fatal(e)
	}
	defer opened.Reader.Close()
	body, e := io.ReadAll(opened.Reader)
	if e != nil || string(body) != "tail" {
		t.Fatal("body canceled after response headers", e)
	}
	if cfg.Timeout != 20*time.Millisecond {
		t.Fatal("owner timeout mutated")
	}
}
