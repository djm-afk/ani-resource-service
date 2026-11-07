package vm

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
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
	gvr        schema.GroupVersionResource
	index      cache.Indexer
	valid      bool // current standard List/Watch connection, not an expiring proof
	connection uint64
}

// StateObservation uses standard informer List/Watch, independently of lifecycle
// request budgets. It never audits a second full copy of the native state.
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
	capacity      int
	retryAfter    time.Time
	synced        bool
}

func NewStateObservation(config *rest.Config, notifier biz.ObservationNotifier) (*StateObservation, error) {
	if config == nil || notifier == nil {
		return nil, biz.ErrInvalid
	}
	cfg := rest.CopyConfig(config)
	// Never inherit a control client's limiter. Watch reconnects
	// must not consume Start/Stop/Delete request tokens.
	cfg.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(2, 4)
	cfg.Timeout = 0 // individual LISTs are bounded; watches own their lifetime
	cfg.UserAgent = "ani-resource-vm-state-observation"
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return newStateObservation(client, notifier), nil
}

func newStateObservation(client dynamic.Interface, notifier biz.ObservationNotifier) *StateObservation {
	o := &StateObservation{client: client, notifier: notifier, pending: map[string]biz.ObjectRef{}, done: make(chan struct{}), capacity: 4096}
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
				w, err := resource.Watch(ctx, opts)
				o.mu.Lock()
				r.connection++
				connection := r.connection
				r.valid = err == nil
				o.overflow = true
				o.mu.Unlock()
				if err != nil {
					return nil, err
				}
				return trackStateWatch(w, func() {
					o.mu.Lock()
					defer o.mu.Unlock()
					if r.connection == connection {
						r.valid = false
						o.overflow = true
					}
				}), nil
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
			// Reflector owns retries. Reads report known source unavailability
			// until its standard List/Watch reconnects.
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
	object := after
	if object == nil {
		object = before
	}
	if object == nil {
		return
	}
	current, exists, _ := r.index.GetByKey(object.GetNamespace() + "/" + object.GetName())
	if exists {
		u := current.(*unstructured.Unstructured)
		// Handlers run in informer order. Also fence delayed/duplicate
		// callbacks against the version they replace; resourceVersions are
		// opaque and must never be numerically ordered here.
		if before == nil || before.GetUID() != u.GetUID() {
			return
		}
		// A DELETED event carries its deletion revision, which need not
		// match the last cached revision. UID still fences replacements;
		// only an Update supplies the previous object version to match.
		if after != nil && before.GetResourceVersion() != "" && before.GetResourceVersion() != u.GetResourceVersion() {
			return
		}
		if after != nil && before.GetResourceVersion() == "" && u.GetResourceVersion() != "" {
			return
		}
	} else if after == nil {
		return
	}
	meaningful := !reflect.DeepEqual(stateProjection(before), stateProjection(after))
	if meaningful {
		for _, u := range []*unstructured.Unstructured{before, after} {
			if u == nil {
				continue
			}
			for _, ref := range o.eventRoots(u) {
				o.enqueue(ref)
			}
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
		o.mu.Lock()
		o.synced = true
		o.mu.Unlock()
		o.mu.Lock()
		o.overflow = true
		o.mu.Unlock()
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

// stateProjection contains the identity, relation and runtime fields actually
// consumed below. RVs, annotations, managedFields and unrelated labels cannot
// turn a normal metadata update into a runtime contradiction.
func stateProjection(u *unstructured.Unstructured) any {
	if u == nil {
		return nil
	}
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	if u.GetKind() == "Pod" && (u.GetLabels()["kubevirt.io"] == "hotplug-disk" || phase == "Succeeded" || phase == "Failed") {
		return nil
	}
	labelKeys := []string{}
	if u.GetKind() == "VirtualMachine" || u.GetKind() == "Pod" {
		labelKeys = append(labelKeys, "ani.io/tenant-id", "vm.ani.io/vm-id", "vm.ani.io/create-digest")
	}
	if u.GetKind() == "Pod" {
		labelKeys = append(labelKeys, "kubevirt.io/created-by", "kubevirt.io")
	}
	labels := map[string]string{}
	for _, key := range labelKeys {
		if value, ok := u.GetLabels()[key]; ok {
			labels[key] = value
		}
	}
	owners := []string{}
	for _, owner := range u.GetOwnerReferences() {
		if owner.APIVersion != "kubevirt.io/v1" || !((u.GetKind() == "VirtualMachineInstance" && owner.Kind == "VirtualMachine") || (u.GetKind() == "Pod" && owner.Kind == "VirtualMachineInstance")) {
			continue
		}
		controller := owner.Controller != nil && *owner.Controller
		encoded, _ := json.Marshal([]any{owner.APIVersion, owner.Kind, owner.Name, string(owner.UID), controller})
		owners = append(owners, string(encoded))
	}
	sort.Strings(owners)
	var requests []any
	if u.GetKind() == "VirtualMachine" {
		requests, _, _ = unstructured.NestedSlice(u.Object, "status", "stateChangeRequests")
		phase = ""
	}
	return []any{objectRef(u), labels, owners, phase, requests}
}

func runtimeObjects(indices []cache.Indexer, namespace, name, id string) (*unstructured.Unstructured, []*unstructured.Unstructured) {
	var instance *unstructured.Unstructured
	if item, exists, _ := indices[1].GetByKey(namespace + "/" + name); exists {
		instance = item.(*unstructured.Unstructured)
	}
	keys := []string{namespace + "/vmi-name/" + name}
	if id != "" {
		keys = append(keys, namespace+"/vm-id/"+id)
	}
	if instance != nil {
		keys = append(keys, namespace+"/vmi-uid/"+string(instance.GetUID()))
	}
	pods := []*unstructured.Unstructured{}
	seen := map[string]bool{}
	for _, key := range keys {
		items, _ := indices[2].ByIndex(runtimeRelations, key)
		for _, item := range items {
			pod := item.(*unstructured.Unstructured)
			id := pod.GetNamespace() + "/" + pod.GetName()
			if !seen[id] {
				pods = append(pods, pod)
				seen[id] = true
			}
		}
	}
	return instance, pods
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

// Preparation uses this read client's budget, isolated from lifecycle clients.
// The existing observation job owns retries; no storage watch/controller is added.
func (o *StateObservation) ObserveDiskPreparation(ctx context.Context, v biz.VM) ([]biz.Disk, error) {
	return NewNativeWithClients(o.client, nil).ObserveDiskPreparation(ctx, v)
}

func (o *StateObservation) ObserveState(ctx context.Context, v biz.VM) (biz.Observation, error) {
	unknown := biz.Observation{State: "Unknown"}
	if err := ctx.Err(); err != nil {
		return unknown, err
	}
	o.mu.Lock()
	if !o.synced {
		o.mu.Unlock()
		return unknown, biz.ErrObservationUnavailable
	}
	key := v.Spec.Namespace + "/" + v.Native.Name
	root, exists, _ := o.resources[0].index.GetByKey(key)
	if !exists {
		o.mu.Unlock()
		return unknown, biz.ErrUnknown
	}
	indices := []cache.Indexer{o.resources[0].index, o.resources[1].index, o.resources[2].index}
	instance, pods := runtimeObjects(indices, v.Spec.Namespace, v.Native.Name, v.ID)
	x, err := runtimeObservation(v, root.(*unstructured.Unstructured), instance, pods)
	for _, r := range o.resources {
		if !r.valid {
			o.mu.Unlock()
			return unknown, biz.ErrObservationUnavailable
		}
	}
	o.mu.Unlock()
	if err != nil {
		return x, err
	}
	x.ObservedAt = time.Now().UTC()
	return x, nil
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
	if err := observeRuntimePods(v, &x, pods); err != nil {
		return x, err
	}
	x.Stopped = instance == nil && !x.RelatedPodsPresent && len(x.StateChangeRequests) == 0
	if x.Stopped {
		x.State = "Stopped"
	}
	return x, nil
}

// Only consumers requiring a launcher use Launcher. Runtime exit is tracked
// independently, so an ambiguous or helper Pod cannot masquerade as absence.
func observeRuntimePods(v biz.VM, x *biz.Observation, pods []*unstructured.Unstructured) error {
	launchers := 0
	for _, pod := range pods {
		phase, _, _ := unstructured.NestedString(pod.Object, "status", "phase")
		if phase == "Succeeded" || phase == "Failed" {
			continue
		}
		if pod.GetNamespace() != v.Spec.Namespace {
			return biz.ErrOwnership
		}
		current := x.VMI != nil && (pod.GetLabels()["kubevirt.io/created-by"] == x.VMI.UID || owned(pod, "VirtualMachineInstance", x.VMI.UID))
		if current {
			if !owned(pod, "VirtualMachineInstance", x.VMI.UID) {
				return biz.ErrOwnership
			}
		} else if !matches(pod, v) {
			return biz.ErrOwnership
		}
		x.RelatedPodsPresent = true
		if current && pod.GetLabels()["kubevirt.io"] != "hotplug-disk" {
			launchers++
			ref := objectRef(pod)
			x.Launcher = &ref
		}
	}
	if launchers != 1 {
		x.Launcher = nil
	}
	return nil
}

// trackingWatch reports stream closure immediately, including a clean EOF.
// Reflector still owns all relists, resourceVersion handling and reconnects.
type trackingWatch struct {
	watch.Interface
	results chan watch.Event
	done    chan struct{}
	once    sync.Once
}

func (w *trackingWatch) ResultChan() <-chan watch.Event { return w.results }
func (w *trackingWatch) Stop() {
	w.once.Do(func() { close(w.done); w.Interface.Stop() })
}
func trackStateWatch(raw watch.Interface, disconnected func()) watch.Interface {
	w := &trackingWatch{Interface: raw, results: make(chan watch.Event), done: make(chan struct{})}
	go func() {
		defer close(w.results)
		defer disconnected()
		for {
			select {
			case <-w.done:
				return
			case event, ok := <-raw.ResultChan():
				if !ok {
					return
				}
				if event.Type == watch.Error {
					disconnected()
				}
				select {
				case w.results <- event:
				case <-w.done:
					return
				}
			}
		}
	}()
	return w
}

var _ biz.DiskPreparationSource = (*StateObservation)(nil)
