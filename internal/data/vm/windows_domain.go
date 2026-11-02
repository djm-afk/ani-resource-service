package vm

import (
	"context"
	"encoding/json"
	"reflect"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// Fixed KubeVirt v1.9 Windows defaults, selected only by the frozen OS family.
func configureWindowsDomain(v biz.VM, domain map[string]any) error {
	if v.Spec.Media.OSFamily != "windows" {
		return nil
	}
	if _, found, err := unstructured.NestedFieldNoCopy(domain, "clock", "timezone"); err != nil || found {
		return biz.ErrChanged
	}
	if offset, found, err := unstructured.NestedInt64(domain, "clock", "utc", "offsetSeconds"); err != nil || found && offset != 0 {
		return biz.ErrChanged
	}
	if passthrough, found, err := unstructured.NestedMap(domain, "features", "hypervPassthrough"); err != nil || found && passthrough["enabled"] != false {
		return biz.ErrChanged
	}
	required := map[string]any{
		"features": map[string]any{
			"acpi": map[string]any{"enabled": true},
			"apic": map[string]any{"enabled": true},
			"hyperv": map[string]any{
				"relaxed":   map[string]any{"enabled": true},
				"vapic":     map[string]any{"enabled": true},
				"spinlocks": map[string]any{"enabled": true, "spinlocks": int64(8191)},
			},
		},
		"clock": map[string]any{
			"utc": map[string]any{},
			"timer": map[string]any{
				"hpet":   map[string]any{"present": false},
				"pit":    map[string]any{"present": true, "tickPolicy": "delay"},
				"rtc":    map[string]any{"present": true, "tickPolicy": "catchup"},
				"hyperv": map[string]any{"present": true},
			},
		},
	}
	// Reject every explicit conflict before changing the caller's domain.
	desired := runtime.DeepCopyJSONValue(domain).(map[string]any)
	if err := fillWindowsDomain(desired, required); err != nil {
		return err
	}
	for _, key := range []string{"features", "clock"} {
		domain[key] = desired[key]
	}
	return nil
}

func fillWindowsDomain(domain, required map[string]any) error {
	for key, value := range required {
		old, found := domain[key]
		if !found {
			domain[key] = value
			continue
		}
		if fields, ok := value.(map[string]any); ok {
			current, ok := old.(map[string]any)
			if !ok {
				return biz.ErrChanged
			}
			if err := fillWindowsDomain(current, fields); err != nil {
				return err
			}
		} else if !reflect.DeepEqual(old, value) {
			return biz.ErrChanged
		}
	}
	return nil
}

// Only a normal Start may complete an old Windows template. EnsureVM's
// existing-object/observation branch never calls this migration.
func (n *Native) ensureWindowsDomain(ctx context.Context, v biz.VM) error {
	if v.Spec.Media.OSFamily != "windows" {
		return nil
	}
	if v.Closing {
		return biz.ErrBusy
	}
	if v.Native.UID == "" || v.Native.Namespace != v.Spec.Namespace {
		return biz.ErrOwnership
	}
	if err := n.namespace(ctx, v); err != nil {
		return err
	}
	root, err := n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Get(ctx, v.Native.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if !matches(root, v) || string(root.GetUID()) != v.Native.UID || root.GetDeletionTimestamp() != nil {
		return biz.ErrOwnership
	}
	domain, found, err := unstructured.NestedMap(root.Object, "spec", "template", "spec", "domain")
	if err != nil || !found {
		return biz.ErrChanged
	}
	inputs, _, err := unstructured.NestedSlice(domain, "devices", "inputs")
	if err != nil {
		return biz.ErrChanged
	}
	if _, _, err = withConsoleTablet(inputs); err != nil {
		return err
	}
	before := runtime.DeepCopyJSONValue(domain).(map[string]any)
	if err = configureWindowsDomain(v, domain); err != nil {
		return err
	}
	observation, err := n.observe(ctx, v, false)
	if err != nil {
		return err
	}
	if observation.VM.UID != v.Native.UID {
		return biz.ErrOwnership
	}
	if (!observation.Stopped && !observation.Terminal) || observation.Launcher != nil || len(observation.StateChangeRequests) != 0 {
		return biz.ErrBusy
	}
	patch := []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": v.Native.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": root.GetResourceVersion()},
	}
	for _, key := range []string{"features", "clock"} {
		if !reflect.DeepEqual(before[key], domain[key]) {
			patch = append(patch, map[string]any{"op": "add", "path": "/spec/template/spec/domain/" + key, "value": domain[key]})
		}
	}
	if len(patch) == 2 {
		return nil
	}
	if root.GetResourceVersion() == "" {
		return biz.ErrChanged
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	updated, err := n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Patch(ctx, v.Native.Name, types.JSONPatchType, body, metav1.PatchOptions{})
	if apierrors.IsConflict(err) || apierrors.IsInvalid(err) {
		return biz.ErrChanged
	}
	if err != nil {
		return err
	}
	if !matches(updated, v) || string(updated.GetUID()) != v.Native.UID {
		return biz.ErrOwnership
	}
	return nil
}
