package vm

import (
	"context"
	"errors"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	ktesting "k8s.io/client-go/testing"
	"sync/atomic"
	"testing"
	"time"
)

func TestVMSharedStateDeletedVMIResourceVersionAllowsReplacement(t *testing.T) {
	ctx := context.Background()
	o, v, client, _ := sharedStateFixture(t)
	events := watch.NewRaceFreeFake()
	client.PrependWatchReactor("virtualmachineinstances", func(ktesting.Action) (bool, watch.Interface, error) {
		return true, events, nil
	})
	if err := o.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if err := o.Stop(stop); err != nil {
			t.Error(err)
		}
	})
	first := waitState(t, o, v, "Running")
	if first.VMI == nil || first.Launcher == nil {
		t.Fatal("fixture has no original runtime", first)
	}
	original, err := client.Resource(vmiResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	launcher, err := client.Resource(podResource).Namespace(v.Spec.Namespace).Get(ctx, first.Launcher.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The API's DELETED object has the deletion event's RV, not the RV of
	// the earlier object retained by the informer. Deliver that external
	// boundary event through the actual shared informer, with no callback or
	// observation substitution.
	deleted := original.DeepCopy()
	deleted.SetResourceVersion("opaque-deletion-revision")
	if err := client.Tracker().Delete(vmiResource, v.Spec.Namespace, v.Native.Name); err != nil {
		t.Fatal(err)
	}
	events.Delete(deleted)
	if err := client.Resource(podResource).Namespace(v.Spec.Namespace).Delete(ctx, launcher.GetName(), metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	stopped := waitState(t, o, v, "Stopped")
	if !stopped.Stopped || stopped.VMI != nil || stopped.Launcher != nil {
		t.Fatal("same-UID deletion with a changed RV did not remove the old runtime", stopped)
	}
	replacement := original.DeepCopy()
	replacement.SetUID("replacement-vmi-uid")
	replacement.SetResourceVersion("opaque-replacement-revision")
	if err := client.Tracker().Create(vmiResource, replacement, v.Spec.Namespace); err != nil {
		t.Fatal(err)
	}
	events.Add(replacement)
	newLauncher := launcher.DeepCopy()
	newLauncher.SetName("virt-launcher-replacement")
	newLauncher.SetUID("replacement-launcher-uid")
	newLauncher.SetResourceVersion("opaque-replacement-launcher")
	labels := newLauncher.GetLabels()
	labels["kubevirt.io/created-by"] = string(replacement.GetUID())
	newLauncher.SetLabels(labels)
	owners := newLauncher.GetOwnerReferences()
	owners[0].UID = replacement.GetUID()
	newLauncher.SetOwnerReferences(owners)
	if _, err := client.Resource(podResource).Namespace(v.Spec.Namespace).Create(ctx, newLauncher, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		x, err := o.ObserveState(ctx, v)
		if err == nil && x.Running && x.VMI != nil && x.VMI.UID == string(replacement.GetUID()) && x.Launcher != nil && x.Launcher.UID == string(newLauncher.GetUID()) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for range 3 {
		x, err := o.ObserveState(ctx, v)
		if err != nil || !x.Running || x.VMI == nil || x.VMI.UID != string(replacement.GetUID()) || x.Launcher == nil || x.Launcher.UID != string(newLauncher.GetUID()) || time.Since(x.ObservedAt) > time.Second {
			t.Fatal("replacement runtime did not recover through Watch", x, err)
		}
	}
	_, err = o.ObserveState(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	// A delayed old-UID callback cannot remove the same-name replacement,
	// change its runtime, or its launcher binding.
	o.changed(o.resources[1], deleted, nil)
	after, err := o.ObserveState(ctx, v)
	if err != nil || !after.Running || after.VMI == nil || after.VMI.UID != string(replacement.GetUID()) {
		t.Fatal("old-UID deletion changed the replacement runtime", after, err)
	}
}

func TestVMSharedStateReconnectReportsOutageAndRecovers(t *testing.T) {
	o, v, client, _ := sharedStateFixture(t)
	var failed atomic.Bool
	events := watch.NewRaceFreeFake()
	client.PrependWatchReactor("virtualmachines", func(ktesting.Action) (bool, watch.Interface, error) {
		if failed.Load() {
			return true, nil, errors.New("connection unavailable")
		}
		return true, events, nil
	})
	primeSharedState(t, o, v)
	failed.Store(true)
	events.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := o.ObserveState(context.Background(), v); errors.Is(err, biz.ErrObservationUnavailable) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if x, err := o.ObserveState(context.Background(), v); !errors.Is(err, biz.ErrObservationUnavailable) || x.Running {
		t.Fatal("outage hidden", x, err)
	}
	// A new standard Watch (and the reflector's ordinary relist when needed)
	// restores availability. There is no independent recovery certificate.
	client.PrependWatchReactor("virtualmachines", func(ktesting.Action) (bool, watch.Interface, error) { return true, watch.NewRaceFreeFake(), nil })
	waitState(t, o, v, "Running")
}

func TestVMSharedStateAmbiguousPodsPreservePowerButNotLauncher(t *testing.T) {
	o, v, client, _ := sharedStateFixture(t)
	pod, _ := client.Resource(podResource).Namespace(v.Spec.Namespace).Get(context.Background(), "virt-launcher", metav1.GetOptions{})
	other := pod.DeepCopy()
	other.SetName("second-launcher")
	other.SetUID("second-uid")
	_, err := client.Resource(podResource).Namespace(v.Spec.Namespace).Create(context.Background(), other, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	primeSharedState(t, o, v)
	x, err := o.ObserveState(context.Background(), v)
	if err != nil || !x.Running || x.Launcher != nil || !x.RelatedPodsPresent || x.Stopped {
		t.Fatal(x, err)
	}
}
