package instancev1

import (
	"encoding/json"
	acc "github.com/zhangzhe-ctrl/ani-accelerator-service/api/gen/go/accelerator/v1"
	"google.golang.org/protobuf/proto"
	"testing"
)

func TestCurrentCanonicalInputProjectionDoesNotFollowDescriptor(t *testing.T) {
	raw, err := CanonicalBusinessPayload(&CreateInstanceData{Name: "example"})
	if err != nil {
		t.Fatal(err)
	}
	const expected = `{"annotations":{},"env":[],"image_id":"","image_scope":"","labels":{},"name":"example","network":null,"preset_id":"","scheduling":null,"volume_mounts":[],"workspace_mount_path":"/workspace"}`
	if string(raw) != expected {
		t.Fatalf("current input bytes changed: %s", raw)
	}
}
func TestCommandCoversConfigurationAndEnvOrder(t *testing.T) {
	spec := currentBusiness()

	spec.WorkspaceMountPath = "/work"
	spec.Labels = map[string]string{"team": "ml"}
	spec.Env = []*InstanceEnvVar{{Name: "A", Value: " a "}, {Name: "B", Value: "$(A)"}}
	c := &CommandContext{TenantId: "tenant", ResourceId: "instance", OperationId: "operation"}
	want, err := CommandDigest(c, spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*CreateSpec){func(s *CreateSpec) { s.Env[0], s.Env[1] = s.Env[1], s.Env[0] }, func(s *CreateSpec) { s.Env[0].Value = "a" }, func(s *CreateSpec) { s.WorkspaceMountPath = "/other" }, func(s *CreateSpec) { s.Labels["team"] = "ops" }} {
		other := proto.Clone(spec).(*CreateSpec)
		change(other)
		got, err := CommandDigest(c, other)
		if err != nil || got == want {
			t.Fatal("configuration not bound", err)
		}
	}

}

func TestCurrentNestedCanonicalFieldProjection(t *testing.T) {
	spec := currentBusiness()
	spec.GpuExecution = &acc.GpuExecution{}
	spec.ComputeType = "gpu"
	spec.GpuBusinessSpec = &acc.GpuBusinessSpec{Mode: acc.SupplyMode_SHARED_FIXED, ProfileVersion: 2}
	spec.GpuSelection = &GpuPresetReference{Mode: acc.SupplyMode_WHOLE_EXCLUSIVE, ProfileVersion: 3}
	raw, err := CanonicalBusinessPayload(spec)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	golden := map[string]string{
		"gpu_execution":     `{"execution_digest":"","node_selector":{},"pod_annotations":{},"queue_name":"","resources":{},"runtime_class_name":"","scheduler_name":""}`,
		"gpu_business_spec": `{"adapter_id":"","allocation_type":"ALLOCATION_TYPE_UNSPECIFIED","compute_units_per_device":0,"core_limit_percent":0,"devices_per_replica":0,"memory_mib_per_device":"0","mode":"SHARED_FIXED","model_key":"","profile_id":"","profile_name":"","profile_version":"2","resource_key":"","spec_digest":""}`,
		"gpu_selection":     `{"adapter_id":"","allocation_type":"ALLOCATION_TYPE_UNSPECIFIED","cluster_id":"","compute_units_per_device":0,"core_limit_percent":0,"devices_per_instance":0,"memory_mib_per_device":"0","mode":"WHOLE_EXCLUSIVE","model_key":"","pool_id":"","profile_id":"","profile_version":"3","resource_key":"","spec_digest":""}`,
		"scheduling":        `{"pod_placement":"NONE"}`,
	}
	for field, want := range golden {
		if string(fields[field]) != want {
			t.Fatalf("current %s bytes changed: %s", field, fields[field])
		}
	}
}
