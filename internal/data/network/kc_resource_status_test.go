package data

import (
	"errors"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestResourcePrimaryReadyIsAuthoritativeWithoutSecondaryConditions(t *testing.T) {
	for _, state := range []string{"True", "False", "Unknown", "missing"} {
		t.Run(state, func(t *testing.T) {
			o, b, target, _ := subnetOptionsFixture(nil)
			o.SetGeneration(19)
			unstructured.RemoveNestedField(o.Object, "status", "observedGeneration")
			unstructured.RemoveNestedField(o.Object, "status", "boundResources")
			conditions := []any{map[string]any{"type": "Valid", "status": "False"}, map[string]any{"type": "Initialized", "status": "False"}}
			if state != "missing" {
				conditions = append(conditions, map[string]any{"type": "Ready", "status": state, "reason": "NativeReason"})
			}
			_ = unstructured.SetNestedSlice(o.Object, conditions, "status", "conditions")
			got, err := inspectResource(o, b, target)
			if err != nil || !got.Exists {
				t.Fatal(got, err)
			}
			if state == "True" || state == "False" {
				if got.Ready == nil || *got.Ready != (state == "True") || got.StatusReason != "NativeReason" {
					t.Fatal(got)
				}
			} else if got.Ready != nil {
				t.Fatal("unreported Ready became false", got)
			}
		})
	}
}

func TestResourceDeletionFinishesOriginalUIDWithoutAdoptingReplacement(t *testing.T) {
	o, b, target, _ := subnetOptionsFixture(nil)
	o.SetUID("replacement")
	o.SetLabels(nil)
	target.Deleting = true
	got, err := inspectResource(o, b, target)
	if err != nil || got.Exists {
		t.Fatal("replacement prolonged the original UID lifecycle", got, err)
	}
	target.Deleting = false
	_, err = inspectResource(o, b, target)
	var conflict *biz.ProviderError
	if !errors.As(err, &conflict) || conflict.Kind != biz.ProviderConflict {
		t.Fatal("replacement was adopted", err)
	}
	target.Deleting = true
	target.KnownIdentity, b.ProviderUid = "", ""
	if _, err = inspectResource(o, b, target); !errors.As(err, &conflict) {
		t.Fatal("unowned object without saved identity was deleted", err)
	}
}
