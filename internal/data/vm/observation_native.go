package vm

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/flowcontrol"
)

const runtimeRelations = "runtime-relations"

type stateCache struct {
	gvr       schema.GroupVersionResource
	index     cache.Indexer
	collected time.Time
	valid     bool
}

// StateObservation is independent from lifecycle clients and their budgets.
// Watch provides prompt changes; three shared, bounded LISTs periodically renew
// actual evidence and recover missing events. VM reads never contact Kubernetes.
type StateObservation struct {
	client        dynamic.Interface
	notifier      biz.ObservationNotifier
	resources     []*stateCache
	informers     []cache.SharedIndexInformer
	registrations []cache.ResourceEventHandlerRegistration
	mu            sync.Mutex
	pending       map[string]biz.ObjectRef
	overflow      bool
	cancel        context.CancelFunc
	started       bool
	done          chan struct{}
	auditInterval time.Duration
	capacity      int
	retryAfter    time.Time
	changesAt     map[string]time.Time
}

func NewStateObservation(config *rest.Config, notifier biz.ObservationNotifier) (*StateObservation, error) {
	if config == nil || notifier == nil {
		return nil, biz.ErrInvalid
	}
	cfg := rest.CopyConfig(config)
	// Never inherit a control client's limiter. Watch reconnects and shared
	// audits must not consume Start/Stop/Delete request tokens.
	cfg.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(2, 4)
	cfg.Timeout = 0 // individual LISTs are bounded; watches own their lifetime
	cfg.UserAgent = "ani-resource-vm-state-observation"
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return newStateObservation(client, notifier, 30*time.Second), nil
}

func newStateObservation(client dynamic.Interface, notifier biz.ObservationNotifier, auditInterval time.Duration) *StateObservation {
	o := &StateObservation{client: client, notifier: notifier, pending: map[string]biz.ObjectRef{}, changesAt: map[string]time.Time{}, done: make(chan struct{}), auditInterval: auditInterval, capacity: 4096}
	for _, gvr := range []schema.GroupVersionResource{vmResource, vmiResource, podResource} {
		r := &stateCache{gvr: gvr, index: newStateIndex()}
		o.resources = append(o.resources, r)
		resource := client.Resource(gvr)
		listWatch := &cache.ListWatch{
			ListWithContextFunc: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
				read, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				return resource.List(read, opts)
			},
			WatchFuncWithContext: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
				return resource.Watch(ctx, opts)
			},
		}
		informer := cache.NewSharedIndexInformer(cache.ToListWatcherWithWatchListSemantics(listWatch, client), &unstructured.Unstructured{}, 0, cache.Indexers{})
		registration, _ := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(value any) { o.changed(r, nil, value) },
			UpdateFunc: func(old, value any) { o.changed(r, old, value) },
			DeleteFunc: func(value any) { o.changed(r, value, nil) },
		})
		_ = informer.SetWatchErrorHandler(func(_ *cache.Reflector, _ error) {
			o.mu.Lock()
			r.valid = false
			o.overflow = true
			o.mu.Unlock()
		})
		o.informers = append(o.informers, informer)
		o.registrations = append(o.registrations, registration)
	}
	return o
}

func newStateIndex() cache.Indexer {
	return cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{runtimeRelations: func(value any) ([]string, error) {
		u, _ := value.(*unstructured.Unstructured)
		if u == nil {
			return nil, nil
		}
		keys := []string{}
		if id := u.GetLabels()["vm.ani.io/vm-id"]; id != "" {
			keys = append(keys, u.GetNamespace()+"/vm-id/"+id)
		}
		if uid := u.GetLabels()["kubevirt.io/created-by"]; uid != "" {
			keys = append(keys, u.GetNamespace()+"/vmi-uid/"+uid)
		}
		for _, owner := range u.GetOwnerReferences() {
			if owner.APIVersion == "kubevirt.io/v1" && owner.Kind == "VirtualMachineInstance" {
				keys = append(keys, u.GetNamespace()+"/vmi-uid/"+string(owner.UID), u.GetNamespace()+"/vmi-name/"+owner.Name)
			}
		}
		return keys, nil
	}})
}

func stateObject(value any) *unstructured.Unstructured {
	if tombstone, ok := value.(cache.DeletedFinalStateUnknown); ok {
		value = tombstone.Obj
	}
	u, _ := value.(*unstructured.Unstructured)
	return u
}

func (o *StateObservation) changed(r *stateCache, old, value any) {
	before, after := stateObject(old), stateObject(value)
	if before != nil && after != nil && reflect.DeepEqual(before.Object, after.Object) {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	received := time.Now().UTC()
	for _, u := range []*unstructured.Unstructured{before, after} {
		if u == nil {
			continue
		}
		for _, ref := range o.eventRoots(u) {
			o.enqueue(ref)
			key := ref.Namespace + "/" + ref.Name
			if len(o.changesAt) >= o.capacity {
				clear(o.changesAt)
				o.overflow = true
			}
			o.changesAt[key] = received
		}
	}
	if before != nil {
		_ = r.index.Delete(before)
	}
	if after != nil {
		_ = r.index.Add(after.DeepCopy())
	}
}

// eventRoots maps hints to already registered roots. Durable notification does
// the tenant/native-name lookup and never creates a VM from an arbitrary event.
func (o *StateObservation) eventRoots(u *unstructured.Unstructured) []biz.ObjectRef {
	if u.GetKind() == "VirtualMachine" {
		return []biz.ObjectRef{objectRef(u)}
	}
	if u.GetKind() == "VirtualMachineInstance" {
		for _, r := range u.GetOwnerReferences() {
			if r.APIVersion == "kubevirt.io/v1" && r.Kind == "VirtualMachine" && r.Controller != nil && *r.Controller {
				return []biz.ObjectRef{{Kind: "VirtualMachine", Namespace: u.GetNamespace(), Name: r.Name, UID: string(r.UID)}}
			}
		}
	}
	refs := []biz.ObjectRef{}
	if id := u.GetLabels()["vm.ani.io/vm-id"]; id != "" {
		roots, _ := o.resources[0].index.ByIndex(runtimeRelations, u.GetNamespace()+"/vm-id/"+id)
		for _, root := range roots {
			refs = append(refs, objectRef(root.(*unstructured.Unstructured)))
		}
	}
	for _, owner := range u.GetOwnerReferences() {
		if owner.APIVersion == "kubevirt.io/v1" && owner.Kind == "VirtualMachineInstance" {
			refs = append(refs, biz.ObjectRef{Kind: "VirtualMachine", Namespace: u.GetNamespace(), Name: owner.Name})
		}
	}
	return refs
}

func (o *StateObservation) enqueue(ref biz.ObjectRef) {
	if ref.Namespace == "" || ref.Name == "" {
		return
	}
	key := ref.Namespace + "/" + ref.Name
	if _, ok := o.pending[key]; !ok && len(o.pending) >= o.capacity {
		o.overflow = true
		return
	}
	o.pending[key] = ref
}

// Start returns without waiting for any cache, RBAC or cluster connectivity.
func (o *StateObservation) Start(parent context.Context) error {
	o.mu.Lock()
	if o.started {
		o.mu.Unlock()
		return biz.ErrInvalid
	}
	o.started = true
	ctx, cancel := context.WithCancel(parent)
	o.cancel = cancel
	o.mu.Unlock()
	go o.run(ctx)
	return nil
}

func (o *StateObservation) run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, informer := range o.informers {
		wg.Add(1)
		go func() { defer wg.Done(); informer.Run(ctx.Done()) }()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		synced := []cache.InformerSynced{}
		for _, informer := range o.informers {
			synced = append(synced, informer.HasSynced)
		}
		for _, registration := range o.registrations {
			synced = append(synced, registration.HasSynced)
		}
		if !cache.WaitForCacheSync(ctx.Done(), synced...) {
			return
		}
		o.audit(ctx)
		ticker := time.NewTicker(o.auditInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				o.audit(ctx)
			}
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	defer func() { wg.Wait(); close(o.done) }()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			o.flush(ctx)
		}
	}
}

func (o *StateObservation) audit(ctx context.Context) {
	started := time.Now().UTC()
	for _, r := range o.resources {
		if ctx.Err() != nil {
			return
		}
		collected := time.Now().UTC()
		read, cancel := context.WithTimeout(ctx, 5*time.Second)
		list, err := o.client.Resource(r.gvr).List(read, metav1.ListOptions{})
		cancel()
		o.mu.Lock()
		// Informer callbacks are the only ordered fact source. In particular,
		// an older LIST response cannot overwrite a later Watch event, and an
		// already queued older Watch callback cannot roll back a newer LIST.
		// A mismatch is unavailable evidence until a later consistent audit.
		r.valid = err == nil && stateCacheMatches(r.index, list)
		if r.valid {
			r.collected = collected
		}
		o.mu.Unlock()
	}
	o.mu.Lock()
	for key, at := range o.changesAt {
		if at.Before(started) {
			delete(o.changesAt, key)
		}
	}
	o.overflow = true
	o.mu.Unlock()
}

func stateCacheMatches(index cache.Indexer, list *unstructured.UnstructuredList) bool {
	if list == nil || len(index.ListKeys()) != len(list.Items) {
		return false
	}
	for i := range list.Items {
		item := &list.Items[i]
		current, exists, err := index.GetByKey(item.GetNamespace() + "/" + item.GetName())
		if err != nil || !exists || !reflect.DeepEqual(current.(*unstructured.Unstructured).Object, item.Object) {
			return false
		}
	}
	return true
}

func (o *StateObservation) flush(ctx context.Context) {
	o.mu.Lock()
	if time.Now().Before(o.retryAfter) {
		o.mu.Unlock()
		return
	}
	pending, overflow := o.pending, o.overflow
	o.pending = map[string]biz.ObjectRef{}
	o.overflow = false
	o.mu.Unlock()
	failed := false
	if !overflow && len(pending) <= 128 {
		for _, ref := range pending {
			if ctx.Err() != nil {
				return
			}
			write, cancel := context.WithTimeout(ctx, time.Second)
			err := o.notifier.NotifyObservation(write, ref)
			cancel()
			if err != nil {
				failed = true
				break
			}
		}
	}
	if overflow || failed || len(pending) > 128 {
		rescan, ok := o.notifier.(biz.ObservationRescanNotifier)
		if !ok {
			failed = true
		} else {
			write, cancel := context.WithTimeout(ctx, time.Second)
			failed = rescan.NotifyAllObservations(write) != nil
			cancel()
		}
	}
	if failed {
		o.mu.Lock()
		o.overflow = true
		o.retryAfter = time.Now().Add(5 * time.Second)
		o.mu.Unlock()
	}
}

func (o *StateObservation) Stop(ctx context.Context) error {
	o.mu.Lock()
	cancel, started := o.cancel, o.started
	o.mu.Unlock()
	if !started {
		return nil
	}
	cancel()
	select {
	case <-o.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *StateObservation) ObserveState(ctx context.Context, v biz.VM) (biz.Observation, error) {
	unknown := biz.Observation{State: "Unknown"}
	if err := ctx.Err(); err != nil {
		return unknown, err
	}
	o.mu.Lock()
	collected := time.Time{}
	for _, r := range o.resources {
		if !r.valid || r.collected.IsZero() || time.Since(r.collected) > 60*time.Second {
			o.mu.Unlock()
			return unknown, biz.ErrUnknown
		}
		if collected.IsZero() || r.collected.Before(collected) {
			collected = r.collected
		}
	}
	if changed := o.changesAt[v.Spec.Namespace+"/"+v.Native.Name]; changed.After(collected) {
		collected = changed
	}
	root, exists, _ := o.resources[0].index.GetByKey(v.Spec.Namespace + "/" + v.Native.Name)
	if !exists {
		o.mu.Unlock()
		return unknown, biz.ErrUnknown
	}
	vmi, hasVMI, _ := o.resources[1].index.GetByKey(v.Spec.Namespace + "/" + v.Native.Name)
	keys := []string{v.Spec.Namespace + "/vm-id/" + v.ID, v.Spec.Namespace + "/vmi-name/" + v.Native.Name}
	if hasVMI {
		keys = append(keys, v.Spec.Namespace+"/vmi-uid/"+string(vmi.(*unstructured.Unstructured).GetUID()))
	}
	pods := []*unstructured.Unstructured{}
	seen := map[string]bool{}
	for _, key := range keys {
		items, _ := o.resources[2].index.ByIndex(runtimeRelations, key)
		for _, item := range items {
			pod := item.(*unstructured.Unstructured)
			id := pod.GetNamespace() + "/" + pod.GetName()
			if !seen[id] {
				pods = append(pods, pod)
				seen[id] = true
			}
		}
	}
	o.mu.Unlock()
	var instance *unstructured.Unstructured
	if hasVMI {
		instance = vmi.(*unstructured.Unstructured)
	}
	x, err := runtimeObservation(v, root.(*unstructured.Unstructured), instance, pods)
	x.ObservedAt = collected
	return x, err
}

func nativePowerState(phase string) string {
	switch phase {
	case "Succeeded":
		return "Stopped"
	case "Scheduling", "Scheduled":
		return "Starting"
	case "Running", "Pending", "Failed":
		return phase
	default:
		return "Unknown"
	}
}

func runtimeObservation(v biz.VM, root, instance *unstructured.Unstructured, pods []*unstructured.Unstructured) (biz.Observation, error) {
	x := biz.Observation{State: "Unknown"}
	if root == nil || !matches(root, v) || v.Native.UID == "" || string(root.GetUID()) != v.Native.UID {
		return x, biz.ErrOwnership
	}
	x.VM = objectRef(root)
	requests, _, _ := unstructured.NestedSlice(root.Object, "status", "stateChangeRequests")
	for _, request := range requests {
		b, err := json.Marshal(request)
		if err != nil {
			return x, err
		}
		x.StateChangeRequests = append(x.StateChangeRequests, string(b))
	}
	if instance != nil {
		if !owned(instance, "VirtualMachine", x.VM.UID) {
			return x, biz.ErrOwnership
		}
		ref := objectRef(instance)
		x.VMI = &ref
		phase, _, _ := unstructured.NestedString(instance.Object, "status", "phase")
		x.State = nativePowerState(phase)
		x.Running = phase == "Running"
		x.Terminal = phase == "Succeeded" || phase == "Failed"
	}
	for _, pod := range pods {
		// Hotplug helpers are owned by the VMI but are not its runtime
		// launcher. They cannot make a single running VM appear ambiguous.
		if pod.GetLabels()["kubevirt.io"] == "hotplug-disk" {
			continue
		}
		phase, _, _ := unstructured.NestedString(pod.Object, "status", "phase")
		if phase == "Succeeded" || phase == "Failed" {
			continue
		}
		if pod.GetNamespace() != v.Spec.Namespace {
			return x, biz.ErrOwnership
		}
		current := x.VMI != nil && (pod.GetLabels()["kubevirt.io/created-by"] == x.VMI.UID || owned(pod, "VirtualMachineInstance", x.VMI.UID))
		if current {
			if !owned(pod, "VirtualMachineInstance", x.VMI.UID) {
				return x, biz.ErrOwnership
			}
		} else if !matches(pod, v) {
			return x, biz.ErrOwnership
		}
		if x.Launcher != nil {
			return x, biz.ErrDependency
		}
		ref := objectRef(pod)
		x.Launcher = &ref
	}
	x.Stopped = instance == nil && x.Launcher == nil && len(x.StateChangeRequests) == 0
	if x.Stopped {
		x.State = "Stopped"
	}
	return x, nil
}
