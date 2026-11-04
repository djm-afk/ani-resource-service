package data

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"testing"
)

func TestObjectEventsKeepExactKeysAndCoalesceRepeats(t *testing.T) {
	observer := &KCObservation{queue: workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[observedObjectKey]())}
	defer observer.queue.ShutDown()
	old := &unstructured.Unstructured{}
	old.SetNamespace("tenant-a")
	old.SetName("vpc-a")
	old.SetResourceVersion("1")
	current := old.DeepCopy()
	current.SetResourceVersion("2")
	observer.changed(kcVPCs, old, current)
	observer.changed(kcVPCs, current, current)
	observer.changed(kcVPCs, cache.DeletedFinalStateUnknown{Obj: current}, nil)
	if observer.queue.Len() != 1 {
		t.Fatal("one object generated unrelated work", observer.queue.Len())
	}
	key, shutdown := observer.queue.Get()
	observer.queue.Done(key)
	observer.queue.Forget(key)
	if shutdown || key != (observedObjectKey{GVR: kcVPCs, Namespace: "tenant-a", Name: "vpc-a"}) {
		t.Fatal(key, shutdown)
	}
	other := current.DeepCopy()
	other.SetNamespace("tenant-b")
	observer.changed(kcVPCs, nil, other)
	key, shutdown = observer.queue.Get()
	observer.queue.Done(key)
	if shutdown || key.Namespace != "tenant-b" {
		t.Fatal("tenant keys were combined", key)
	}
}
