package vm

import (
	"context"
	"encoding/json"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

const consoleTabletName = "console-tablet"

func consoleTablet() map[string]any {
	return map[string]any{"name": consoleTabletName, "type": "tablet", "bus": "usb"}
}

func withConsoleTablet(inputs []any) ([]any, bool, error) {
	matched, managed := false, false
	for _, value := range inputs {
		input, ok := value.(map[string]any)
		if !ok {
			return nil, false, biz.ErrChanged
		}
		compatible := input["type"] == "tablet" && input["bus"] == "usb"
		if input["name"] == consoleTabletName {
			if managed || !compatible {
				return nil, false, biz.ErrOwnership
			}
			managed = true
		}
		matched = matched || compatible
	}
	if matched {
		return inputs, false, nil
	}
	return append(append([]any(nil), inputs...), consoleTablet()), true, nil
}

// Existing templates gain an absolute pointer only through a normal Start after
// fresh observation proves no live VMI, launcher, or outstanding native request.
func (n *Native) ensureConsoleTablet(ctx context.Context, v biz.VM) error {
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
	requests, _, err := unstructured.NestedSlice(root.Object, "status", "stateChangeRequests")
	if err != nil {
		return biz.ErrChanged
	}
	if len(requests) != 0 {
		return biz.ErrBusy
	}
	inputs, _, err := unstructured.NestedSlice(root.Object, "spec", "template", "spec", "domain", "devices", "inputs")
	if err != nil {
		return biz.ErrChanged
	}
	desired, changed, err := withConsoleTablet(inputs)
	if err != nil {
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
	if !changed {
		return nil
	}
	if root.GetResourceVersion() == "" {
		return biz.ErrChanged
	}
	patch := []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": v.Native.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": root.GetResourceVersion()},
		{"op": "add", "path": "/spec/template/spec/domain/devices/inputs", "value": desired},
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	updated, err := n.client.Resource(vmResource).Namespace(v.Spec.Namespace).Patch(ctx, v.Native.Name, types.JSONPatchType, body, metav1.PatchOptions{})
	if err != nil {
		return err
	}
	if !matches(updated, v) || string(updated.GetUID()) != v.Native.UID {
		return biz.ErrOwnership
	}
	return nil
}
