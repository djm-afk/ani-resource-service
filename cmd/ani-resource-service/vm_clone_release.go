package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/vm"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var (
	vmCloneReleaseMode bool
	vmCloneTenant      string
	vmCloneTarget      string
	vmCloneReason      string
	vmCloneOperator    string
	vmCloneKind        string
	vmCloneApply       bool
)

func init() {
	flag.BoolVar(&vmCloneReleaseMode, "vm-clone-release", false, "release a stuck VM clone source reference (dry-run unless -vm-clone-apply)")
	flag.StringVar(&vmCloneTenant, "vm-clone-tenant", "", "tenant id of the clone target")
	flag.StringVar(&vmCloneTarget, "vm-clone-target", "", "target VM id of the clone")
	flag.StringVar(&vmCloneReason, "vm-clone-reason", "", "operator reason, required")
	flag.StringVar(&vmCloneOperator, "vm-clone-operator", "", "operator identity, required")
	flag.StringVar(&vmCloneKind, "vm-clone-kind", "ADMIN_RELEASED", "release kind: ADMIN_RELEASED, FAILED_NO_OUTPUT or GONE_NO_OUTPUT")
	flag.BoolVar(&vmCloneApply, "vm-clone-apply", false, "write the release; without it the command only prints the plan")
}

// cloneReleaseResult is the single JSON object printed to stdout.
type cloneReleaseResult struct {
	Applied  bool   `json:"applied"`
	TenantID string `json:"tenant_id"`
	TargetID string `json:"target_id"`
	SourceID string `json:"source_vm_id"`
	State    string `json:"state"`
	Kind     string `json:"release_kind"`
	Evidence string `json:"evidence"`
	Reason   string `json:"reason"`
}

func validateVMCloneReleaseModes() error {
	kinds := map[string]bool{"ADMIN_RELEASED": true, "FAILED_NO_OUTPUT": true, "GONE_NO_OUTPUT": true}
	if !kinds[vmCloneKind] {
		return fmt.Errorf("vm-clone-kind must be ADMIN_RELEASED, FAILED_NO_OUTPUT or GONE_NO_OUTPUT")
	}
	if !biz.UUID(vmCloneTenant) || !biz.UUID(vmCloneTarget) {
		return fmt.Errorf("vm-clone-tenant and vm-clone-target must be canonical UUIDs")
	}
	if strings.TrimSpace(vmCloneReason) == "" || strings.TrimSpace(vmCloneOperator) == "" {
		return fmt.Errorf("vm-clone-reason and vm-clone-operator are required")
	}
	return nil
}

func runVMCloneRelease() error {
	if e := validateVMCloneReleaseModes(); e != nil {
		return e
	}
	raw, e := readImagePrivateFile(os.Getenv("ANI_VM_OWNER_DSN_FILE"))
	if e != nil {
		return e
	}
	defer clear(raw)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	repo, e := data.OpenPostgres(ctx, strings.TrimSpace(string(raw)))
	if e != nil {
		return e
	}
	defer repo.Close()
	ref, e := repo.CloneSourceReference(ctx, vmCloneTenant, vmCloneTarget)
	if e != nil {
		return e
	}
	if ref.State == "RELEASED" {
		return json.NewEncoder(os.Stdout).Encode(cloneReleaseResult{Applied: false, TenantID: ref.TenantID, TargetID: ref.TargetVMID, SourceID: ref.SourceVMID, State: ref.State, Kind: ref.ReleaseKind, Evidence: ref.ReleaseEvidence, Reason: "already released"})
	}
	target, targetErr := repo.Get(ctx, vmCloneTenant, vmCloneTarget)
	if targetErr != nil && !errors.Is(targetErr, biz.ErrNotFound) {
		return targetErr
	}
	if targetErr == nil && target.CloneProgress != nil && target.CloneProgress.RegistrationCompleted && !target.Closing {
		return fmt.Errorf("clone already registered and still alive; release is not allowed")
	}
	// A manual release must not leave the original create able to submit later.
	// Either the target row is gone, or the VM is closing because a Delete was
	// accepted, which fences every later create tick. A bare "clone absent" read
	// or an expired lease proves neither.
	if targetErr == nil && !target.Closing {
		return fmt.Errorf("clone target %s is still open; issue a delete for it first so the create cannot submit again", vmCloneTarget)
	}
	// Closing stops later create ticks but cannot cancel a clone submission that
	// was already sent. The worker lease is shorter than every native request
	// timeout, so waiting out an active lease means no request of that create is
	// still in flight. Checked before reading the clone so a request that is still
	// transmitting can never be authorized by an earlier absent observation.
	if active, e := repo.CloneCreateLeaseActive(ctx, vmCloneTenant, vmCloneTarget, ref.CreateOperationID); e != nil {
		return e
	} else if active {
		return fmt.Errorf("create operation %s still holds an active worker lease; retry after it finishes", ref.CreateOperationID)
	}
	namespace := ""
	if targetErr == nil {
		namespace = target.Spec.Namespace
	}
	if namespace == "" {
		source, e := repo.Get(ctx, vmCloneTenant, ref.SourceVMID)
		if e != nil {
			return fmt.Errorf("cannot determine clone namespace: %w", e)
		}
		namespace = source.Spec.Namespace
	}
	cloneName := biz.CloneName(ref.TargetVMID)
	if targetErr == nil && target.CloneProgress != nil && target.CloneProgress.Name != "" {
		cloneName = target.CloneProgress.Name
	}
	residual, e := clonePresence(ctx, namespace, cloneName)
	if e != nil {
		return e
	}
	evidence := ""
	if !residual.Exists {
		evidence = fmt.Sprintf("clone=%s absent", cloneName)
	} else if residual.Deleting && len(residual.Finalizers) == 0 {
		evidence = fmt.Sprintf("clone=%s deleting uid=%s", cloneName, residual.UID)
	} else {
		return fmt.Errorf("clone %s still exists and is not blocked only by finalizers", cloneName)
	}
	targetEvidence := "target absent"
	if targetErr == nil {
		if target.CloneProgress != nil && target.CloneProgress.PendingTargetRef != nil && target.CloneProgress.PendingTargetRef.UID != "" && target.CloneProgress.PendingTargetRef.UID != target.Native.UID {
			targetEvidence = fmt.Sprintf("target uid=%s pending uid=%s", target.Native.UID, target.CloneProgress.PendingTargetRef.UID)
		} else {
			targetEvidence = fmt.Sprintf("target uid=%s unregistered", target.Native.UID)
		}
	}
	evidence = evidence + "; " + targetEvidence
	result := cloneReleaseResult{Applied: false, TenantID: ref.TenantID, TargetID: ref.TargetVMID, SourceID: ref.SourceVMID, State: ref.State, Kind: vmCloneKind, Evidence: evidence, Reason: vmCloneReason}
	if !vmCloneApply {
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	if e = repo.ReleaseCloneSourceHELD(ctx, biz.CloneSourceRelease{TenantID: ref.TenantID, TargetVMID: ref.TargetVMID, Kind: vmCloneKind, By: vmCloneOperator, Reason: vmCloneReason, Evidence: evidence}); e != nil {
		return e
	}
	result.Applied = true
	result.State = "RELEASED"
	return json.NewEncoder(os.Stdout).Encode(result)
}

func clonePresence(ctx context.Context, namespace, name string) (data.ClonePresence, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return data.ClonePresence{}, fmt.Errorf("in-cluster Kubernetes configuration unavailable: %w", err)
	}
	config.Timeout = 10 * time.Second
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return data.ClonePresence{}, err
	}
	native, err := data.NewNative(client, config)
	if err != nil {
		return data.ClonePresence{}, err
	}
	return native.ClonePresence(ctx, namespace, name)
}
