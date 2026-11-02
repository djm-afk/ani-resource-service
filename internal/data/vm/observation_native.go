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
	gvr       schema.GroupVersionResource
	index     cache.Indexer
	collected time.Time
	valid     bool
}

type stateProof struct {
	uid        string
	verifiedAt time.Time
	invalid    error
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
	proofs        map[string]stateProof
	recoveries    map[string]biz.ObservationRecovery
	recoveryRetry time.Time
	synced        bool
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
	o := &StateObservation{client: client, notifier: notifier, pending: map[string]biz.ObjectRef{}, changesAt: map[string]time.Time{}, proofs: map[string]stateProof{}, recoveries: map[string]biz.ObservationRecovery{}, done: make(chan struct{}), auditInterval: auditInterval, capacity: 4096}
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
			// A reconnect does not contradict the last real audit. Its proof
			// still expires after 60 seconds, even if events keep arriving.
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
	received := time.Now().UTC()
	for _, u := range []*unstructured.Unstructured{before, after} {
		if u == nil {
			continue
		}
		if !meaningful {
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
		o.mu.Lock()
		o.synced = true
		o.mu.Unlock()
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
	listed := make([]cache.Indexer, len(o.resources))
	for i, r := range o.resources {
		if ctx.Err() != nil {
			return
		}
		read, cancel := context.WithTimeout(ctx, 5*time.Second)
		list, err := o.client.Resource(r.gvr).List(read, metav1.ListOptions{})
		cancel()
		if err == nil && list != nil {
			listed[i] = newStateIndex()
			for j := range list.Items {
				_ = listed[i].Add(list.Items[j].DeepCopy())
			}
		}
		o.mu.Lock()
		// Informer callbacks are the only ordered fact source. In particular,
		// an older LIST response cannot overwrite a later Watch event, and an
		// already queued older Watch callback cannot roll back a newer LIST.
		// Compare each VM's runtime evidence only after all three LISTs. A
		// newer event can make one proof temporarily unverifiable, without
		// invalidating another VM or deleting a still-live earlier proof.
		r.valid = listed[i] != nil
		if r.valid {
			r.collected = started
		}
		o.mu.Unlock()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if listed[0] != nil && listed[1] != nil && listed[2] != nil {
		current := []cache.Indexer{o.resources[0].index, o.resources[1].index, o.resources[2].index}
		for _, item := range current[0].List() {
			root := item.(*unstructured.Unstructured)
			key := root.GetNamespace() + "/" + root.GetName()
			other, exists, _ := listed[0].GetByKey(key)
			if !exists || other.(*unstructured.Unstructured).GetUID() != root.GetUID() {
				// Missing roots and UID conflicts are positive contradictory
				// evidence, not an ordinary List/Watch timing difference.
				invalid := biz.ErrUnknown
				if exists {
					invalid = biz.ErrOwnership
				}
				proof := o.proofs[key]
				proof.invalid = invalid
				o.proofs[key] = proof
				continue
			}
			if !reflect.DeepEqual(runtimeProjection(current, root), runtimeProjection(listed, other.(*unstructured.Unstructured))) {
				continue
			}
			o.proofs[key] = stateProof{uid: string(root.GetUID()), verifiedAt: started}
			if changed := o.changesAt[key]; !changed.After(started) {
				delete(o.changesAt, key)
			}
			// Recovery is only a fresh proof hint, not an Observed write.
			// Durable registrations fence its UID and the subsequent read
			// still performs every tenant/owner/runtime validation.
			o.enqueueRecovery(key, biz.ObservationRecovery{Native: objectRef(root), ObservedAt: started})
		}
	}
	// Drop vanished roots rather than allowing retained proof maps to grow.
	for key := range o.proofs {
		if _, exists, _ := o.resources[0].index.GetByKey(key); !exists {
			delete(o.proofs, key)
			delete(o.changesAt, key)
			delete(o.recoveries, key)
		}
	}
	o.overflow = true
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

func runtimeProjection(indices []cache.Indexer, root *unstructured.Unstructured) any {
	instance, pods := runtimeObjects(indices, root.GetNamespace(), root.GetName(), root.GetLabels()["vm.ani.io/vm-id"])
	projected := map[string]any{}
	for _, pod := range pods {
		if evidence := stateProjection(pod); evidence != nil {
			projected[pod.GetNamespace()+"/"+pod.GetName()] = evidence
		}
	}
	return []any{stateProjection(root), stateProjection(instance), projected}
}

func (o *StateObservation) flush(ctx context.Context) {
	o.flushRecoveries(ctx)
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

func (o *StateObservation) enqueueRecovery(key string, recovery biz.ObservationRecovery) {
	if _, ok := o.recoveries[key]; !ok && len(o.recoveries) >= o.capacity {
		// The next shared audit retries this proof. Falling back to an
		// ordinary rescan must never bypass durable failure backoff.
		o.overflow = true
		return
	}
	o.recoveries[key] = recovery
}

func (o *StateObservation) flushRecoveries(ctx context.Context) {
	notifier, ok := o.notifier.(biz.ObservationRecoveryNotifier)
	if !ok {
		return
	}
	o.mu.Lock()
	if time.Now().Before(o.recoveryRetry) {
		o.mu.Unlock()
		return
	}
	batch := []biz.ObservationRecovery{}
	for key, recovery := range o.recoveries {
		proof := o.proofs[key]
		delete(o.recoveries, key)
		root, exists, _ := o.resources[0].index.GetByKey(key)
		if !exists || string(root.(*unstructured.Unstructured).GetUID()) != recovery.Native.UID || proof.invalid != nil || proof.uid != recovery.Native.UID || !proof.verifiedAt.Equal(recovery.ObservedAt) || time.Since(proof.verifiedAt) > 60*time.Second {
			continue
		}
		batch = append(batch, recovery)
		if len(batch) == 128 {
			break
		}
	}
	o.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	write, cancel := context.WithTimeout(ctx, time.Second)
	err := notifier.RecoverObservations(write, batch)
	cancel()
	if err != nil {
		o.mu.Lock()
		for _, recovery := range batch {
			key := recovery.Native.Namespace + "/" + recovery.Native.Name
			if queued, ok := o.recoveries[key]; !ok || queued.ObservedAt.Before(recovery.ObservedAt) {
				o.enqueueRecovery(key, recovery)
			}
		}
		o.recoveryRetry = time.Now().Add(5 * time.Second)
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
	proof, proven := o.proofs[key]
	collected := proof.verifiedAt
	if changed := o.changesAt[key]; changed.After(collected) {
		collected = changed
	}
	o.mu.Unlock()
	if err != nil {
		return x, err
	}
	if proof.invalid != nil {
		return unknown, proof.invalid
	}
	if !proven || proof.verifiedAt.IsZero() {
		return unknown, biz.ErrObservationUnavailable
	}
	if proof.uid != v.Native.UID {
		return unknown, biz.ErrOwnership
	}
	if time.Since(proof.verifiedAt) > 60*time.Second {
		return unknown, biz.ErrUnknown
	}
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
