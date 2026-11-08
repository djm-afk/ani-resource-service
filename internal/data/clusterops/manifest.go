package clusterops

import (
	"bytes"
	"encoding/json"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/clusterops"
	docyaml "go.yaml.in/yaml/v3"
	"io"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"reflect"
	"sigs.k8s.io/yaml"
	"sort"
	"strings"
)

func decodeManifestStrict(manifest string) (*unstructured.Unstructured, error) {
	invalid := biz.NewError("INVALID_ARGUMENT")
	trimmed := strings.TrimSpace(manifest)
	if trimmed == "" {
		return nil, invalid
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		dec := json.NewDecoder(strings.NewReader(trimmed))
		var raw json.RawMessage
		if dec.Decode(&raw) != nil || dec.Decode(new(json.RawMessage)) != io.EOF {
			return nil, invalid
		}
	}
	// Parse document boundaries into a syntax node, never a map: duplicate
	// keys and numeric tokens stay intact for YAMLToJSONStrict below.
	docs := docyaml.NewDecoder(strings.NewReader(manifest))
	var node docyaml.Node
	if err := docs.Decode(&node); err != nil || len(node.Content) != 1 || node.Content[0].Kind != docyaml.MappingNode {
		return nil, invalid
	}
	if err := docs.Decode(new(docyaml.Node)); err != io.EOF {
		return nil, invalid
	}
	data, err := yaml.YAMLToJSONStrict([]byte(manifest))
	if err != nil {
		return nil, invalid
	}
	obj := new(unstructured.Unstructured)
	if err = obj.UnmarshalJSON(data); err != nil || obj.Object == nil || obj.GetKind() == "List" {
		return nil, invalid
	}
	return obj, nil
}
func matchManifest(obj *unstructured.Unstructured, ref biz.ResourceRef, d biz.ResourceDescriptor) error {
	version := ref.Type.Version
	if ref.Type.Group != "" {
		version = ref.Type.Group + "/" + version
	}
	if obj.GetAPIVersion() != version || obj.GetKind() != d.Kind || obj.GetName() != ref.Name || obj.GetGenerateName() != "" {
		return biz.NewError("INVALID_ARGUMENT")
	}
	ns := obj.GetNamespace()
	if ns != "" && ns != ref.Namespace {
		return biz.NewError("INVALID_ARGUMENT")
	}
	if !d.Namespaced && ns != "" {
		return biz.NewError("INVALID_ARGUMENT")
	}
	if d.Namespaced {
		obj.SetNamespace(ref.Namespace)
	}
	return nil
}

var systemMetadata = []string{"uid", "resourceVersion", "creationTimestamp", "generation", "deletionTimestamp", "deletionGracePeriodSeconds", "managedFields", "selfLink"}

func checkCreateFields(obj *unstructured.Unstructured) error {
	if _, ok := obj.Object["status"]; ok {
		return biz.NewError("SYSTEM_FIELD_READ_ONLY")
	}
	for _, key := range systemMetadata {
		if _, found, _ := unstructured.NestedFieldNoCopy(obj.Object, "metadata", key); found {
			return biz.NewError("SYSTEM_FIELD_READ_ONLY")
		}
	}
	return nil
}
func preserveSystemFields(obj, current *unstructured.Unstructured) error {
	paths := [][]string{{"status"}}
	for _, key := range systemMetadata {
		paths = append(paths, []string{"metadata", key})
	}
	for _, path := range paths {
		next, present, err := unstructured.NestedFieldNoCopy(obj.Object, path...)
		if err != nil {
			return biz.NewError("INVALID_ARGUMENT")
		}
		old, exists, err := unstructured.NestedFieldNoCopy(current.Object, path...)
		if err != nil {
			return biz.NewError("INTERNAL_ERROR")
		}
		if present && (!exists || !reflect.DeepEqual(next, old)) {
			return biz.NewError("SYSTEM_FIELD_READ_ONLY")
		}
		if exists {
			if err := unstructured.SetNestedField(obj.Object, old, path...); err != nil {
				return biz.NewError("INTERNAL_ERROR")
			}
		}
	}
	return nil
}
func project(ref biz.ResourceRef, obj *unstructured.Unstructured) (biz.RawResource, error) {
	r := biz.RawResource{Ref: ref, UID: string(obj.GetUID()), ResourceVersion: obj.GetResourceVersion()}
	full := obj.DeepCopy()
	unstructured.RemoveNestedField(full.Object, "metadata", "managedFields")
	var err error
	r.JSON, err = full.MarshalJSON()
	if err != nil {
		return r, biz.NewError("INTERNAL_ERROR")
	}
	display := full.DeepCopy()
	if ref.Type.Group == "" && ref.Type.Resource == "secrets" {
		keys := map[string]struct{}{}
		for _, field := range []string{"data", "stringData"} {
			values, _, _ := unstructured.NestedMap(display.Object, field)
			for key := range values {
				keys[key] = struct{}{}
			}
			delete(display.Object, field)
		}
		for key := range keys {
			r.SecretKeys = append(r.SecretKeys, key)
		}
		sort.Strings(r.SecretKeys)
		unstructured.RemoveNestedField(display.Object, "metadata", "annotations")
	}
	r.DisplayJSON, err = display.MarshalJSON()
	if err != nil {
		return r, biz.NewError("INTERNAL_ERROR")
	}
	edit := full.DeepCopy()
	delete(edit.Object, "status")
	for _, key := range systemMetadata {
		unstructured.RemoveNestedField(edit.Object, "metadata", key)
	}
	editJSON, err := edit.MarshalJSON()
	if err != nil {
		return r, biz.NewError("INTERNAL_ERROR")
	}
	y, err := yaml.JSONToYAML(editJSON)
	if err != nil {
		return r, biz.NewError("INTERNAL_ERROR")
	}
	r.ManifestYAML = string(bytes.TrimSpace(y)) + "\n"
	return r, nil
}
