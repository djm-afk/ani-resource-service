package biz

import (
	"context"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	"strings"
)

// PullSecretCaller is the deployment-side authorization for one runtime
// identity. Allowlists are never accepted from the request body.
type PullSecretCaller struct {
	Subject            string
	ClusterIDs         []string
	Namespaces         []string
	OwnerLabels        []string
	RequireTenantLabel bool
}

// NamespaceClaim is the concrete target that must be verified before writing.
// Verify is false only for Resource's own instance path, which derives and
// validates its namespace in-process and has no cross-service assertion.
type NamespaceClaim struct {
	ClusterID, Namespace, NamespaceUID, TenantID string
	OwnerLabels                                  []string
	RequireTenantLabel                           bool
	Verify                                       bool
	StandardTenantNamespace                      bool
}

// NamespaceAuthority reads the real namespace to confirm name, UID and labels.
type NamespaceAuthority interface {
	VerifyNamespace(context.Context, NamespaceClaim) error
}

// UsePullSecretWriter installs the single writer, the namespace verifier and
// the per-caller authorization table. It is required for Ensure behavior. An
// empty table is valid for a deployment that only uses the instance path.
func (r *Runtime) UsePullSecretWriter(writer PullSecretWriter, namespaces NamespaceAuthority, callers []PullSecretCaller) error {
	if writer == nil || namespaces == nil {
		return Fail(InvalidArgument, "pull secret writer composition required")
	}
	table := make(map[string]PullSecretCaller, len(callers))
	for _, caller := range callers {
		if caller.Subject == "" || len(caller.ClusterIDs) == 0 || (caller.Subject != "ani-modeldev-service" && len(caller.Namespaces) == 0) {
			return Fail(InvalidArgument, "pull secret caller requires subject, cluster and namespace allowlist")
		}
		if caller.Subject == "ani-modeldev-service" && (len(caller.Namespaces) != 0 || len(caller.OwnerLabels) != 0 || caller.RequireTenantLabel) {
			return Fail(InvalidArgument, "ModelDev namespace and owner-label legacy configuration is forbidden")
		}
		if _, exists := table[caller.Subject]; exists {
			return Fail(InvalidArgument, "duplicate pull secret caller")
		}
		table[caller.Subject] = caller
	}
	r.writer, r.namespaces, r.callers = writer, namespaces, table
	return nil
}

// guardNamespace allows the standard platform namespace for both trusted
// workload owners. Inference's deployment allowlist remains for legacy shared
// namespaces and cannot authorize another tenant's standard namespace.
func (r *Runtime) guardNamespace(ctx context.Context, caller Caller, in EnsurePullSecret) (bool, error) {
	if r.namespaces == nil {
		return false, Fail(DependencyUnavailable, "namespace authority not configured")
	}
	claim := NamespaceClaim{ClusterID: in.ClusterID, Namespace: in.Namespace, NamespaceUID: in.NamespaceUID, TenantID: in.TenantID}
	if caller.Subject != instanceOwnerSubject {
		allow, ok := r.callers[caller.Subject]
		if !ok {
			return false, Fail(PermissionDenied, "runtime identity is not authorized to ensure pull secrets")
		}
		if !contains(allow.ClusterIDs, in.ClusterID) {
			return false, Fail(PermissionDenied, "pull secret target outside the caller allowlist")
		}
		name, nameErr := tenantnamespace.Name(in.TenantID)
		standard := nameErr == nil && name == in.Namespace
		if caller.Subject == "ani-modeldev-service" || (caller.Subject == "ani-inference-service" && standard) {
			if !standard || in.NamespaceUID == "" || !containsExact(allow.ClusterIDs, in.ClusterID) {
				return false, Fail(PermissionDenied, "standard tenant namespace and original UID required")
			}
			claim.StandardTenantNamespace = true
		} else if strings.HasPrefix(in.Namespace, "tenant-") || !contains(allow.Namespaces, in.Namespace) {
			return false, Fail(PermissionDenied, "pull secret target outside the caller allowlist")
		}
		claim.Verify = true
		claim.OwnerLabels = allow.OwnerLabels
		claim.RequireTenantLabel = allow.RequireTenantLabel
	}
	if err := r.namespaces.VerifyNamespace(ctx, claim); err != nil {
		return false, err
	}
	return claim.StandardTenantNamespace, nil
}

// instanceOwnerSubject is Resource's own instance manager. It writes through
// the same Ensure implementation but supplies no external namespace assertion.
const instanceOwnerSubject = "ani-resource-service"

func contains(values []string, want string) bool {
	want = strings.ToLower(want)
	for _, value := range values {
		if strings.ToLower(value) == want {
			return true
		}
	}
	return false
}

func containsExact(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
