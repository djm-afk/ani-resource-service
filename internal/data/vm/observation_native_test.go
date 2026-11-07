package vm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

type stateNotifications struct {
	mu      sync.Mutex
	roots   []biz.ObjectRef
	rescans int
	err     error
}

func (n *stateNotifications) NotifyObservation(_ context.Context, ref biz.ObjectRef) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.roots = append(n.roots, ref)
	return n.err
}
func (n *stateNotifications) NotifyAllObservations(context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.rescans++
	return n.err
}

func sharedStateFixture(t *testing.T) (*StateObservation, biz.VM, *fake.FakeDynamicClient, *stateNotifications) {
	t.Helper()
	_, v, original := inputNativeFixture(t, "linux")
	v.Native.UID = "registered-root"
	root := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine", "spec": map[string]any{"runStrategy": "Manual"}}}
	root.SetName(v.Native.Name)
	root.SetNamespace(v.Spec.Namespace)
	root.SetUID(types.UID(v.Native.UID))
	root.SetLabels(labelVM(v))
	root.SetResourceVersion("10")
	vmi := manualTerminalVMI(v, "Running")
	vmi.SetResourceVersion("11")
	pod := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Pod", "status": map[string]any{"phase": "Running"}}}
	pod.SetName("virt-launcher")
	pod.SetNamespace(v.Spec.Namespace)
	pod.SetUID("launcher-uid")
	pod.SetLabels(map[string]string{"kubevirt.io/created-by": string(vmi.GetUID()), "vm.ani.io/vm-id": v.ID})
	yes := true
	pod.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Name: vmi.GetName(), UID: vmi.GetUID(), Controller: &yes}})
	_ = original // actual source fixture only contains the three observed resources
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{vmResource: "VirtualMachineList", vmiResource: "VirtualMachineInstanceList", podResource: "PodList"}, root, vmi, pod)
	notifier := &stateNotifications{}
	return newStateObservation(client, notifier), v, client, notifier
}

func waitState(t *testing.T, o *StateObservation, v biz.VM, state string) biz.Observation {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		x, err := o.ObserveState(context.Background(), v)
		if err == nil && x.State == state {
			return x
		}
		time.Sleep(10 * time.Millisecond)
	}
	x, err := o.ObserveState(context.Background(), v)
	t.Fatalf("shared state never became %s: %+v %v", state, x, err)
	return x
}

func primeSharedState(t *testing.T, o *StateObservation, v biz.VM) {
	t.Helper()
	if err := o.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitState(t, o, v, "Running")
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := o.Stop(stop); err != nil {
			t.Error(err)
		}
	})
}

func TestVMSharedStateWatchTracksGuestShutdownWithoutPerVMReads(t *testing.T) {
	o, v, client, notifier := sharedStateFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := o.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := o.Stop(stop); err != nil {
			t.Error(err)
		}
	})
	first := waitState(t, o, v, "Running")
	if first.Launcher == nil || first.VMI == nil || first.ObservedAt.IsZero() {
		t.Fatal("runtime identity not observed", first)
	}
	// The initial shared source establishes exactly three Watch connections.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		watches := 0
		for _, a := range client.Actions() {
			if a.GetVerb() == "watch" {
				watches++
			}
		}
		if watches == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	before := len(client.Actions())
	for range 100 {
		if _, err := o.ObserveState(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(client.Actions()); got != before {
		t.Fatalf("per-VM reads made Kubernetes calls: %d -> %d", before, got)
	}
	vmi, _ := client.Resource(vmiResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	_ = unstructured.SetNestedField(vmi.Object, "Succeeded", "status", "phase")
	vmi.SetResourceVersion("12")
	if _, err := client.Resource(vmiResource).Namespace(v.Spec.Namespace).UpdateStatus(ctx, vmi, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	finished := waitState(t, o, v, "Stopped")
	if !finished.Terminal || finished.Stopped || !finished.ObservedAt.After(first.ObservedAt) {
		t.Fatal("terminal event lost its identity/real event time", finished)
	}
	if err := client.Resource(podResource).Namespace(v.Spec.Namespace).Delete(ctx, "virt-launcher", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		x, err := o.ObserveState(ctx, v)
		if err == nil && x.Launcher == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	finished, _ = o.ObserveState(ctx, v)
	if finished.Launcher != nil || finished.Stopped {
		t.Fatal("ended VMI presence was confused with complete runtime absence", finished)
	}
	time.Sleep(120 * time.Millisecond)
	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.roots) == 0 && notifier.rescans == 0 {
		t.Fatal("watch did not bridge to durable observation")
	}
	for _, a := range client.Actions() {
		switch a.GetResource().Resource {
		case "virtualmachines", "virtualmachineinstances", "pods":
		default:
			t.Fatal("state source scanned unrelated resources", a)
		}
	}
}

func TestVMSharedStateHealthyCacheNeedsNoExpiringProof(t *testing.T) {
	o, v, client, _ := sharedStateFixture(t)
	primeSharedState(t, o, v)
	v.ObservedAt = time.Now().Add(-2 * time.Minute)
	before := len(client.Actions())
	for range 100 {
		x, err := o.ObserveState(context.Background(), v)
		if err != nil || !x.Running || time.Since(x.ObservedAt) > time.Second {
			t.Fatal(x, err)
		}
	}
	if len(client.Actions()) != before {
		t.Fatal("cache reads made extra audit requests")
	}
	o.mu.Lock()
	o.resources[0].valid = false
	o.mu.Unlock()
	if x, err := o.ObserveState(context.Background(), v); !errors.Is(err, biz.ErrObservationUnavailable) || x.State != "Unknown" {
		t.Fatal("known disconnected source renewed cached facts", x, err)
	}
}

func TestVMSharedStatePreservesIdentityAndBoundsNotificationQueue(t *testing.T) {
	o, v, client, notifier := sharedStateFixture(t)
	primeSharedState(t, o, v)
	notifier.mu.Lock()
	notifier.roots = nil
	notifier.rescans = 0
	notifier.mu.Unlock()
	root, _ := client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(context.Background(), v.Native.Name, metav1.GetOptions{})
	before := root.DeepCopy()
	root.SetUID("replacement-root")
	o.changed(o.resources[0], before, root)
	if _, err := o.ObserveState(context.Background(), v); !errors.Is(err, biz.ErrOwnership) {
		t.Fatal("same-name replacement adopted", err)
	}
	o.mu.Lock()
	for i := range o.capacity + 10 {
		o.enqueue(biz.ObjectRef{Kind: "VirtualMachine", Namespace: "tenant", Name: fmt.Sprint(i)})
	}
	if len(o.pending) > o.capacity || !o.overflow {
		t.Fatal("notification queue is not bounded")
	}
	o.mu.Unlock()
	o.flush(context.Background())
	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if notifier.rescans != 1 || len(notifier.roots) != 0 {
		t.Fatal("overflow did not use one durable shared rescan", notifier)
	}
}

func TestVMSharedStateStartupAndShutdownDoNotWaitForRBAC(t *testing.T) {
	o, v, client, _ := sharedStateFixture(t)
	client.PrependReactor("list", "*", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(vmResource.GroupResource(), "", errors.New("denied"))
	})
	started := time.Now()
	if err := o.Start(context.Background()); err != nil || time.Since(started) > 100*time.Millisecond {
		t.Fatal("observation blocked service startup", err)
	}
	if _, err := o.ObserveState(context.Background(), v); !errors.Is(err, biz.ErrObservationUnavailable) {
		t.Fatal("unsynced source returned facts", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := o.Stop(ctx); err != nil {
		t.Fatal("failed source blocked shutdown", err)
	}
}

func TestVMSharedStateDelayedCallbackCannotOverwriteNewWatchEvent(t *testing.T) {
	o, v, client, _ := sharedStateFixture(t)
	primeSharedState(t, o, v)
	old, _ := client.Resource(vmiResource).Namespace(v.Spec.Namespace).Get(context.Background(), v.Native.Name, metav1.GetOptions{})
	newer := old.DeepCopy()
	newer.SetResourceVersion("later-opaque-revision")
	_ = unstructured.SetNestedField(newer.Object, "Succeeded", "status", "phase")
	o.changed(o.resources[1], old, newer)
	queued := old.DeepCopy()
	queued.SetResourceVersion("queued-revision")
	o.changed(o.resources[1], old, queued)
	if x, err := o.ObserveState(context.Background(), v); err != nil || !x.Terminal || x.State != "Stopped" {
		t.Fatal(x, err)
	}
}

func TestVMSharedStateNotifierFailureHasBoundedRetry(t *testing.T) {
	o, _, _, notifier := sharedStateFixture(t)
	notifier.err = errors.New("database unavailable")
	o.enqueue(biz.ObjectRef{Kind: "VirtualMachine", Namespace: "tenant", Name: "vm"})
	o.flush(context.Background())
	for range 100 {
		o.flush(context.Background())
	}
	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.roots) != 1 || notifier.rescans != 1 || !o.overflow || time.Until(o.retryAfter) < 4*time.Second {
		t.Fatal("notifier failure became a hot retry loop", notifier, o.retryAfter)
	}
}

func TestVMSharedStateSeparatesLauncherFromHotplugHelper(t *testing.T) {
	o, v, client, _ := sharedStateFixture(t)
	pod, _ := client.Resource(podResource).Namespace(v.Spec.Namespace).Get(context.Background(), "virt-launcher", metav1.GetOptions{})
	helper := pod.DeepCopy()
	helper.SetName("hotplug-disk")
	helper.SetUID("helper-uid")
	labels := helper.GetLabels()
	labels["kubevirt.io"] = "hotplug-disk"
	helper.SetLabels(labels)
	if _, err := client.Resource(podResource).Namespace(v.Spec.Namespace).Create(context.Background(), helper, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	primeSharedState(t, o, v)
	x, err := o.ObserveState(context.Background(), v)
	if err != nil || !x.Running || x.Launcher == nil || x.Launcher.Name != "virt-launcher" {
		t.Fatal("hotplug helper replaced the launcher identity", x, err)
	}
}
