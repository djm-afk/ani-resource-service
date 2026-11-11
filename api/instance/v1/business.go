package instancev1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	acc "github.com/zhangzhe-ctrl/ani-accelerator-service/api/gen/go/accelerator/v1"
	"regexp"
	"strconv"
	"strings"

	"github.com/zhangzhe-ctrl/ani-accelerator-service/pkg/gpuprotocol"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// CanonicalBusinessPayload is shared by Gov and Res. Trusted attachments and
// command identities are separate messages and never enter the public hash.
func CanonicalBusinessPayload(m proto.Message) ([]byte, error) {
	if m == nil || !m.ProtoReflect().IsValid() || hasUnknown(m.ProtoReflect()) {
		return nil, ErrInvalidBusiness
	}
	if _, e := (protojson.MarshalOptions{}).Marshal(m); e != nil {
		return nil, e
	}
	var value map[string]any
	switch v := m.(type) {
	case *CreateInstanceData:
		if e := ValidateInstanceConfiguration(v); e != nil {
			return nil, e
		}
		value = map[string]any{"name": v.Name, "image_id": v.ImageId, "image_scope": v.ImageScope, "preset_id": v.PresetId, "scheduling": wireJSON(v.Scheduling)}
		workspace := "/workspace"
		if v.WorkspaceMountPath != nil {
			workspace = *v.WorkspaceMountPath
		}
		value["workspace_mount_path"] = workspace
		mounts := []any{}
		for _, x := range sortedInputMounts(v.VolumeMounts) {
			mounts = append(mounts, wireJSON(x))
		}
		value["volume_mounts"] = mounts
		value["network"] = wireJSON(v.Network)
		value["labels"] = nonNilMap(v.Labels)
		value["annotations"] = nonNilMap(v.Annotations)
		value["env"] = wireEnv(v.Env)
		if len(v.SecurityGroupIds) > 0 {
			value["security_group_ids"] = v.SecurityGroupIds
		}

	case *CreateSpec:
		if e := validateSpecConfiguration(v); e != nil {
			return nil, e
		}
		value = map[string]any{"name": v.Name, "image_id": v.ImageId, "image_scope": v.ImageScope, "preset_id": v.PresetId, "image_reference": v.ImageReference, "cpu": v.Cpu, "memory": v.Memory, "cluster_id": v.ClusterId, "namespace": v.Namespace, "service_account": v.ServiceAccount, "storage_class": v.StorageClass, "workspace_size": v.WorkspaceSize, "shell": v.Shell, "uid": strconv.FormatInt(v.Uid, 10), "gid": strconv.FormatInt(v.Gid, 10), "instance_kind": v.InstanceKind, "compute_type": v.ComputeType, "scheduling": wireJSON(v.Scheduling), "preset_name": v.PresetName, "preset_code": v.PresetCode, "gpu_execution": nil, "gpu_display": nil, "gpu_selection": nil}
		switch v.ComputeType {
		case "cpu":
			if v.GpuExecution != nil || v.GpuBusinessSpec != nil || v.GpuSelection != nil {
				return nil, ErrInvalidBusiness
			}
		case "gpu":
			delete(value, "gpu_display")
			value["gpu_execution"] = wireJSON(v.GpuExecution)
			value["gpu_business_spec"] = wireJSON(v.GpuBusinessSpec)
			value["gpu_selection"] = wireJSON(v.GpuSelection)
		default:
			return nil, ErrInvalidBusiness
		}

		value["workspace_mount_path"] = v.WorkspaceMountPath
		mounts := []any{}
		for _, x := range sortedFrozenMounts(v.VolumeMounts) {
			mounts = append(mounts, wireJSON(x))
		}
		value["volume_mounts"] = mounts
		value["network"] = wireJSON(v.Network)
		value["labels"] = nonNilMap(v.Labels)
		value["annotations"] = nonNilMap(v.Annotations)
		value["env"] = wireEnv(v.Env)
		if len(v.SecurityGroupIds) > 0 {
			value["security_group_ids"] = v.SecurityGroupIds
		}

	default:
		return nil, errors.New("unsupported instance business message")
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if e := enc.Encode(value); e != nil {
		return nil, e
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), nil
}

// wireJSON is a closed field projection, including nested defaults. Adding
// descriptor fields must not rewrite an accepted current payload.
func wireJSON(m proto.Message) any {
	if m == nil || !m.ProtoReflect().IsValid() {
		return nil
	}
	switch v := m.(type) {
	case *SchedulingOptions:
		out := map[string]any{}
		if v.PodPlacement != nil {
			out["pod_placement"] = fixedEnum(int32(*v.PodPlacement), map[int32]string{0: "POD_PLACEMENT_UNSPECIFIED", 1: "NONE", 2: "COLOCATE_PREFERRED", 3: "SPREAD_PREFERRED"})
		}
		if v.PriorityClassName != nil {
			out["priority_class_name"] = *v.PriorityClassName
		}
		return out
	case *acc.GpuExecution:
		return map[string]any{"scheduler_name": v.SchedulerName, "queue_name": v.QueueName, "runtime_class_name": v.RuntimeClassName, "node_selector": nonNilMap(v.NodeSelector), "pod_annotations": nonNilMap(v.PodAnnotations), "resources": nonNilMap(v.Resources), "execution_digest": v.ExecutionDigest}
	case *acc.GpuBusinessSpec:
		return map[string]any{"profile_id": v.ProfileId, "profile_version": strconv.FormatUint(v.ProfileVersion, 10), "profile_name": v.ProfileName, "model_key": v.ModelKey, "mode": supplyModeJSON(int32(v.Mode)), "allocation_type": allocationJSON(int32(v.AllocationType)), "devices_per_replica": v.DevicesPerReplica, "memory_mib_per_device": strconv.FormatInt(v.MemoryMibPerDevice, 10), "core_limit_percent": v.CoreLimitPercent, "compute_units_per_device": v.ComputeUnitsPerDevice, "resource_key": v.ResourceKey, "adapter_id": v.AdapterId, "spec_digest": v.SpecDigest}
	case *GpuPresetReference:
		return map[string]any{"profile_id": v.ProfileId, "profile_version": strconv.FormatUint(v.ProfileVersion, 10), "spec_digest": v.SpecDigest, "devices_per_instance": v.DevicesPerInstance, "cluster_id": v.ClusterId, "pool_id": v.PoolId, "mode": supplyModeJSON(int32(v.Mode)), "model_key": v.ModelKey, "memory_mib_per_device": strconv.FormatInt(v.MemoryMibPerDevice, 10), "core_limit_percent": v.CoreLimitPercent, "allocation_type": allocationJSON(int32(v.AllocationType)), "resource_key": v.ResourceKey, "compute_units_per_device": v.ComputeUnitsPerDevice, "adapter_id": v.AdapterId}
	case *VolumeMount:
		return map[string]any{"pvc_name": v.PvcName, "mount_path": v.MountPath, "read_only": v.ReadOnly}
	case *FrozenVolumeMount:
		return map[string]any{"cluster_id": v.ClusterId, "namespace": v.Namespace, "pvc_name": v.PvcName, "pvc_uid": v.PvcUid, "mount_path": v.MountPath, "read_only": v.ReadOnly, "ownership": v.Ownership}
	case *NetworkSelection:
		return map[string]any{"vpc_id": v.VpcId, "subnet_id": v.SubnetId}
	case *InstanceEnvVar:
		return map[string]any{"name": v.Name, "value": v.Value}
	default:
		panic("unsupported immutable instance field projection")
	}
}
func fixedEnum(number int32, names map[int32]string) any {
	if name, ok := names[number]; ok {
		return name
	}
	return number
}
func supplyModeJSON(number int32) any {
	return fixedEnum(number, map[int32]string{int32(acc.SupplyMode_SUPPLY_MODE_UNSPECIFIED): "SUPPLY_MODE_UNSPECIFIED", int32(acc.SupplyMode_WHOLE_EXCLUSIVE): "WHOLE_EXCLUSIVE", int32(acc.SupplyMode_SHARED_FIXED): "SHARED_FIXED"})
}
func allocationJSON(number int32) any {
	return fixedEnum(number, map[int32]string{0: "ALLOCATION_TYPE_UNSPECIFIED", 1: "PHYSICAL", 2: "DYNAMIC_SHARED", 3: "FIXED_PARTITION"})
}
func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
func wireEnv(env []*InstanceEnvVar) []any {
	out := []any{}
	for _, v := range env {
		out = append(out, wireJSON(v))
	}
	return out
}
func BusinessPayloadDigest(m proto.Message) (string, error) {
	raw, e := CanonicalBusinessPayload(m)
	if e != nil {
		return "", e
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
func DecodeBusinessPayload(raw []byte) (*CreateSpec, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	var kind string
	if err := json.Unmarshal(fields["compute_type"], &kind); err != nil {
		return nil, ErrInvalidBusiness
	}
	switch kind {
	case "cpu":
		if _, present := fields["gpu_business_spec"]; present {
			return nil, ErrInvalidBusiness
		}
		for _, key := range []string{"gpu_display", "gpu_execution", "gpu_selection"} {
			value, present := fields[key]
			if !present || !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, ErrInvalidBusiness
			}
		}
		delete(fields, "gpu_display")
	case "gpu":
		if _, present := fields["gpu_display"]; present {
			return nil, ErrInvalidBusiness
		}
	default:
		return nil, ErrInvalidBusiness
	}
	b := new(CreateSpec)
	descriptor := b.ProtoReflect().Descriptor().Fields()
	for name := range fields {
		if descriptor.ByName(protoreflect.Name(name)) == nil && descriptor.ByJSONName(name) == nil {
			return nil, ErrInvalidBusiness
		}
	}
	current, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	if err = protojson.Unmarshal(current, b); err != nil {
		return nil, err
	}
	if err = ValidateCreateSpec(b, b.GpuExecution != nil); err != nil {
		return nil, err
	}
	canonical, err := CanonicalBusinessPayload(b)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, canonical) {
		return nil, ErrInvalidBusiness
	}
	return b, nil
}

var ErrInvalidBusiness = errors.New("INVALID_INSTANCE_REQUEST")
var ErrInvalidScheduling = errors.New("INVALID_SCHEDULING_OPTIONS")
var ErrSchedulingConflict = errors.New("SCHEDULING_FIELD_CONFLICT")

var runtimeClassLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidateGPURuntimeClassName checks the managed owner RuntimeClass name.
func ValidateGPURuntimeClassName(name string) error {
	if len(name) == 0 || len(name) > 253 {
		return ErrInvalidBusiness
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) > 63 || !runtimeClassLabel.MatchString(label) {
			return ErrInvalidBusiness
		}
	}
	return nil
}

// ValidateSchedulingOptions preserves optional presence. Effective options always
// contain a concrete placement; raw intent may omit the whole object.
func ValidateSchedulingOptions(s *SchedulingOptions, effective bool) error {
	if s == nil {
		if effective {
			return ErrInvalidScheduling
		}
		return nil
	}
	if len(s.ProtoReflect().GetUnknown()) != 0 || (s.PodPlacement == nil && s.PriorityClassName == nil) {
		return ErrInvalidScheduling
	}
	if effective && s.PodPlacement == nil {
		return ErrInvalidScheduling
	}
	if s.PodPlacement != nil && (*s.PodPlacement < SchedulingOptions_NONE || *s.PodPlacement > SchedulingOptions_SPREAD_PREFERRED) {
		return ErrInvalidScheduling
	}
	if s.PriorityClassName != nil && (strings.TrimSpace(*s.PriorityClassName) != *s.PriorityClassName || *s.PriorityClassName == "") {
		return ErrInvalidScheduling
	}
	return nil
}
func ResolveSchedulingOptions(raw, defaults *SchedulingOptions) (*SchedulingOptions, error) {
	if err := ValidateSchedulingOptions(raw, false); err != nil {
		return nil, err
	}
	if defaults != nil {
		if err := ValidateSchedulingOptions(defaults, false); err != nil {
			return nil, err
		}
	}
	out := &SchedulingOptions{PodPlacement: SchedulingOptions_NONE.Enum()}
	for _, s := range []*SchedulingOptions{defaults, raw} {
		if s == nil {
			continue
		}
		if s.PodPlacement != nil {
			out.PodPlacement = s.GetPodPlacement().Enum()
		}
		if s.PriorityClassName != nil {
			v := s.GetPriorityClassName()
			out.PriorityClassName = &v
		}
	}
	return out, nil
}
func ValidateSchedulingConsistency(raw, effective *SchedulingOptions) error {
	if err := ValidateSchedulingOptions(effective, true); err != nil {
		return err
	}
	if err := ValidateSchedulingOptions(raw, false); err != nil {
		return err
	}
	if raw == nil {
		return nil
	}
	if raw.PodPlacement != nil && raw.GetPodPlacement() != effective.GetPodPlacement() {
		return fmt.Errorf("%w: pod_placement", ErrSchedulingConflict)
	}
	if raw.PriorityClassName != nil && (effective.PriorityClassName == nil || raw.GetPriorityClassName() != effective.GetPriorityClassName()) {
		return fmt.Errorf("%w: priority_class_name", ErrSchedulingConflict)
	}
	return nil
}
func ValidateCreateSpec(s *CreateSpec, accepted bool) error {
	if s == nil || hasUnknown(s.ProtoReflect()) || s.InstanceKind != "container" || (s.ComputeType != "cpu" && s.ComputeType != "gpu") || s.Name == "" || s.ImageId == "" || (s.ImageScope != "tenant" && s.ImageScope != "platform") || s.PresetId == "" || s.Cpu == "" || s.Memory == "" || s.ClusterId == "" || s.Namespace == "" {
		return ErrInvalidBusiness
	}
	if err := validateSpecConfiguration(s); err != nil {
		return err
	}
	if err := ValidateSchedulingOptions(s.Scheduling, true); err != nil {
		return err
	}
	if s.ComputeType == "cpu" {
		if s.GpuExecution != nil || s.GpuBusinessSpec != nil || s.GpuSelection != nil {
			return ErrInvalidBusiness
		}
		return nil
	}
	if !accepted {
		if s.GpuSelection == nil || s.GpuExecution != nil || s.GpuBusinessSpec != nil || s.GpuSelection.ClusterId != s.ClusterId {
			return ErrInvalidBusiness
		}
		if err := gpuprotocol.ValidateGpuBusinessSpec(referenceBusinessSpec(s.GpuSelection)); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidBusiness, err)
		}
		return nil
	}
	if s.GpuSelection != nil || s.GpuExecution == nil || s.GpuBusinessSpec == nil {
		return ErrInvalidBusiness
	}
	if err := gpuprotocol.ValidateGpuExecutionForBusinessSpec(s.GpuExecution, s.GpuBusinessSpec); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidBusiness, err)
	}
	return nil
}

// CommandDigest covers the immutable command identity and explicit business fields.
// The GPU execution has its own fixed-field digest; display remains audit data.
func CommandDigest(c *CommandContext, s *CreateSpec) (string, error) {
	if c == nil || s == nil || hasUnknown(c.ProtoReflect()) || hasUnknown(s.ProtoReflect()) {
		return "", ErrInvalidBusiness
	}
	if err := validateSpecConfiguration(s); err != nil {
		return "", err
	}
	execution := ""
	if s.GpuExecution != nil {
		execution = s.GpuExecution.ExecutionDigest
	}
	var scheduling any
	if p := s.Scheduling; p != nil {
		var placement, priority any
		if p.PodPlacement != nil {
			placement = fmt.Sprint(int32(p.GetPodPlacement()))
		}
		if p.PriorityClassName != nil {
			priority = p.GetPriorityClassName()
		}
		scheduling = map[string]any{"pod_placement": placement, "priority_class_name": priority}
	}
	value := map[string]any{"tenant_id": c.TenantId, "resource_id": c.ResourceId, "operation_id": c.OperationId, "name": s.Name, "image_id": s.ImageId, "image_scope": s.ImageScope, "preset_id": s.PresetId, "image_reference": s.ImageReference, "cpu": s.Cpu, "memory": s.Memory, "cluster_id": s.ClusterId, "namespace": s.Namespace, "service_account": s.ServiceAccount, "storage_class": s.StorageClass, "workspace_size": s.WorkspaceSize, "shell": s.Shell, "uid": fmt.Sprint(s.Uid), "gid": fmt.Sprint(s.Gid), "instance_kind": s.InstanceKind, "compute_type": s.ComputeType, "scheduling": scheduling, "preset_name": s.PresetName, "preset_code": s.PresetCode, "gpu_display": nil, "execution_digest": execution}
	switch s.ComputeType {
	case "cpu":
		if s.GpuBusinessSpec != nil || s.GpuExecution != nil || s.GpuSelection != nil {
			return "", ErrInvalidBusiness
		}
	case "gpu":
		delete(value, "gpu_display")
		value["gpu_business_spec"] = wireJSON(s.GpuBusinessSpec)
	default:
		return "", ErrInvalidBusiness
	}

	domain := "resource-create\n"
	value["workspace_mount_path"] = s.WorkspaceMountPath
	mounts := []any{}
	for _, m := range sortedFrozenMounts(s.VolumeMounts) {
		mounts = append(mounts, wireJSON(m))
	}
	value["volume_mounts"] = mounts
	value["network"] = wireJSON(s.Network)
	value["labels"] = nonNilMap(s.Labels)
	value["annotations"] = nonNilMap(s.Annotations)
	value["env"] = wireEnv(s.Env)

	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(domain), raw...))
	return hex.EncodeToString(sum[:]), nil
}
func DeleteCommandDigest(r *AcceptDeleteRequest) (string, error) {
	if r == nil || r.Context == nil || hasUnknown(r.ProtoReflect()) {
		return "", ErrInvalidBusiness
	}
	c := r.Context
	raw, err := json.Marshal(map[string]any{"tenant_id": c.TenantId, "resource_id": c.ResourceId, "operation_id": c.OperationId, "create_operation_id": r.CreateOperationId, "expected_version": fmt.Sprint(r.ExpectedVersion), "instance_kind": r.InstanceKind, "compute_type": r.ComputeType})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("resource-delete-v1\n"), raw...))
	return hex.EncodeToString(sum[:]), nil
}

// Reserved fields must not disappear when gRPC messages enter the JSON digest.
func hasUnknown(m protoreflect.Message) bool {
	if len(m.GetUnknown()) != 0 {
		return true
	}
	unknown := false
	m.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.IsMap() {
			if field.MapValue().Kind() == protoreflect.MessageKind {
				value.Map().Range(func(_ protoreflect.MapKey, v protoreflect.Value) bool {
					unknown = hasUnknown(v.Message())
					return !unknown
				})
			}
		} else if field.IsList() {
			if field.Kind() == protoreflect.MessageKind {
				list := value.List()
				for k := 0; k < list.Len(); k++ {
					if hasUnknown(list.Get(k).Message()) {
						unknown = true
						break
					}
				}
			}
		} else if field.Kind() == protoreflect.MessageKind {
			unknown = hasUnknown(value.Message())
		}
		return !unknown
	})
	return unknown
}

func referenceBusinessSpec(g *GpuPresetReference) *acc.GpuBusinessSpec {
	if g == nil {
		return nil
	}
	return &acc.GpuBusinessSpec{ProfileId: g.ProfileId, ProfileVersion: g.ProfileVersion, SpecDigest: g.SpecDigest, ModelKey: g.ModelKey, Mode: g.Mode, AllocationType: g.AllocationType, DevicesPerReplica: g.DevicesPerInstance, MemoryMibPerDevice: g.MemoryMibPerDevice, CoreLimitPercent: g.CoreLimitPercent, ComputeUnitsPerDevice: g.ComputeUnitsPerDevice, ResourceKey: g.ResourceKey, AdapterId: g.AdapterId}
}
