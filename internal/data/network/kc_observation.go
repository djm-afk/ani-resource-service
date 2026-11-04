package data

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

type observedObjectKey struct {
	GVR       schema.GroupVersionResource
	Namespace string
	Name      string
}

// KCObservation maps ordinary object events to already registered Res work.
// Workers read their own targets directly; this component never applies state.
type KCObservation struct {
	provider       *KCProvider
	client         dynamic.Interface
	requestTimeout time.Duration
	informers      []cache.SharedIndexInformer
	queue          workqueue.TypedRateLimitingInterface[observedObjectKey]
	mu             sync.Mutex
	cancel         context.CancelFunc
	done           chan struct{}
	started        bool
	watchErrors    atomic.Int64
	notifications  atomic.Int64
	retries        atomic.Int64
	pgMetrics      map[string]float64
}

func observationResources() []schema.GroupVersionResource {
	return []schema.GroupVersionResource{
		kcVPCs, kcSubnets,
		{Group: kcVPCs.Group, Version: "v1", Resource: "vnics"},
		{Group: kcVPCs.Group, Version: "v1", Resource: "vnicips"},
		kcEIPs, kcSnats, kcEgressGateways, kcVlans, kcSecurityGroups,
		pods, kcNodes,
		lbEnvoyProxies, lbGateways, lbBackends, lbRoutes, lbTCPRoutes,
		lbUDPRoutes, lbPolicies, lbClientPolicies, lbPatchPolicies,
	}
}

func (p *KCProvider) EnableObservation(requestTimeout time.Duration) (*KCObservation, error) {
	if requestTimeout <= 0 || p.observation != nil || p.watchClient == nil {
		return nil, fmt.Errorf("invalid observation lifecycle or request timeout")
	}
	observer := &KCObservation{
		provider: p, client: p.watchClient, requestTimeout: requestTimeout,
		queue: workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[observedObjectKey]()),
		done:  make(chan struct{}),
	}
	for _, gvr := range observationResources() {
		resource := observer.client.Resource(gvr)
		informer := cache.NewSharedIndexInformer(&cache.ListWatch{
			ListWithContextFunc: func(ctx context.Context, options metav1.ListOptions) (runtime.Object, error) {
				return resource.List(ctx, options)
			},
			WatchFuncWithContext: func(ctx context.Context, options metav1.ListOptions) (watch.Interface, error) {
				return resource.Watch(ctx, options)
			},
		}, &unstructured.Unstructured{}, 0, cache.Indexers{})
		_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(object any) { observer.changed(gvr, nil, object) },
			UpdateFunc: func(old, current any) { observer.changed(gvr, old, current) },
			DeleteFunc: func(object any) { observer.changed(gvr, object, nil) },
		})
		if err != nil {
			observer.queue.ShutDown()
			return nil, err
		}
		if err = informer.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
			observer.watchErrors.Add(1)
			slog.Warn("network object watch failed", "resource", gvr.String(), "error", err)
		}); err != nil {
			observer.queue.ShutDown()
			return nil, err
		}
		observer.informers = append(observer.informers, informer)
	}
	p.observation = observer
	return observer, nil
}

func unwrapObject(value any) *unstructured.Unstructured {
	switch deleted := value.(type) {
	case cache.DeletedFinalStateUnknown:
		value = deleted.Obj
	case *cache.DeletedFinalStateUnknown:
		value = deleted.Obj
	}
	object, _ := value.(*unstructured.Unstructured)
	return object
}

func (o *KCObservation) changed(gvr schema.GroupVersionResource, old, current any) {
	before, after := unwrapObject(old), unwrapObject(current)
	if before != nil && after != nil && before.GetResourceVersion() != "" && before.GetResourceVersion() == after.GetResourceVersion() {
		return
	}
	for _, object := range []*unstructured.Unstructured{before, after} {
		if object != nil && object.GetName() != "" {
			o.queue.Add(observedObjectKey{GVR: gvr, Namespace: object.GetNamespace(), Name: object.GetName()})
		}
	}
}

func (o *KCObservation) Start(parent context.Context) error {
	o.mu.Lock()
	if o.started {
		o.mu.Unlock()
		return fmt.Errorf("observer already started")
	}
	o.started = true
	ctx, cancel := context.WithCancel(parent)
	o.cancel = cancel
	o.mu.Unlock()
	var group sync.WaitGroup
	defer func() {
		cancel()
		o.queue.ShutDown()
		group.Wait()
		close(o.done)
	}()
	// Restore only persisted Res tasks, including objects deleted while the
	// process was stopped. No Kubernetes inventory or status reset is needed.
	wakeCtx, wakeCancel := context.WithTimeout(ctx, o.requestTimeout)
	err := o.provider.repository.WakeManagedTasks(wakeCtx)
	wakeCancel()
	if err != nil {
		return err
	}
	for _, informer := range o.informers {
		group.Add(1)
		go func() {
			defer group.Done()
			informer.Run(ctx.Done())
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		for o.processNextNotification(ctx) {
		}
	}()
	group.Add(1)
	go func() {
		defer group.Done()
		o.collectTelemetry(ctx)
	}()
	<-ctx.Done()
	return nil
}

func (o *KCObservation) processNextNotification(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	key, shutdown := o.queue.Get()
	if shutdown {
		return false
	}
	defer o.queue.Done(key)
	request, cancel := context.WithTimeout(ctx, o.requestTimeout)
	err := o.provider.repository.NotifyObservedObject(request, key.GVR, key.Namespace, key.Name)
	cancel()
	if err != nil {
		o.retries.Add(1)
		o.queue.AddRateLimited(key)
		return true
	}
	o.notifications.Add(1)
	o.queue.Forget(key)
	return true
}

func (o *KCObservation) Stop(ctx context.Context) error {
	o.mu.Lock()
	cancel, started := o.cancel, o.started
	o.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if !started {
		return nil
	}
	select {
	case <-o.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *KCObservation) collectTelemetry(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			probe, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			metrics := o.provider.repository.observationTelemetry(probe)
			cancel()
			o.mu.Lock()
			o.pgMetrics = metrics
			o.mu.Unlock()
			timer.Reset(30 * time.Second)
		}
	}
}

func (o *KCObservation) Snapshot() map[string]float64 {
	metrics := o.provider.io.snapshot()
	metrics["pending_keys"] = float64(o.queue.Len())
	metrics["watch_errors_total"] = float64(o.watchErrors.Load())
	metrics["notifications_total"] = float64(o.notifications.Load())
	metrics["notification_retries_total"] = float64(o.retries.Load())
	synced := true
	for _, informer := range o.informers {
		synced = synced && informer.HasSynced()
	}
	metrics["source_synced"] = 0
	if synced {
		metrics["source_synced"] = 1
	}
	o.mu.Lock()
	for key, value := range o.pgMetrics {
		metrics[key] = value
	}
	o.mu.Unlock()
	return metrics
}
