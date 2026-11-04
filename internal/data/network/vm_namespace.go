package data

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

const vmNetworkTemplate = "kcn-single-plugin-v1"
const vmManagedNADName = "ani-vm-primary-v1"
const vmManagementLabel = "network.ani.io/vm-network-id"
const vmTemplateLabel = "network.ani.io/vm-template"

var vmServiceAccounts = schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}

// VMAutoNetworkConfig is trusted deployment configuration, never tenant input.
// The NAD is shared by the tenant's Subnets; the actual Subnet reference always
// comes from the tenant-scoped, persisted KCN binding selected for the VM.
type VMAutoNetworkConfig struct {
	ServiceAccount string
	ServerSocket   string
}

func (p *KCProvider) ConfigureVMAutoNetwork(config VMAutoNetworkConfig) error {
	if config.ServiceAccount == "" {
		config.ServiceAccount = "ani-vm-workload"
	}
	if config.ServerSocket == "" {
		config.ServerSocket = "/run/openvswitch/kc-networking-daemon.sock"
	}
	if len(validation.IsDNS1123Subdomain(config.ServiceAccount)) != 0 || !filepath.IsAbs(config.ServerSocket) || filepath.Clean(config.ServerSocket) != config.ServerSocket || strings.ContainsAny(config.ServerSocket, "\r\n\x00") {
		return fmt.Errorf("invalid automatic VM network configuration")
	}
	p.vmAutoNetwork = &config
	return nil
}

func automaticVMNADConfig(config VMAutoNetworkConfig) string {
	raw, _ := json.Marshal(map[string]string{"cniVersion": "0.3.1", "name": "kc-networking", "type": "kc-networking", "server_socket": config.ServerSocket})
	return string(raw)
}

// VMNetworkIntent is read-only trusted input until a VM owner accepts it. The
// raw NAD is then stored in that owner's internal snapshot, outside canonical.
type VMNetworkIntent struct {
	ManagedVMNAD
	NADConfig, ServiceAccount string
}

func vmNetworkFromRecord(s biz.ResolvedNetworkSelection, record sqlcgen.NetworkVmNamespace) VMNetworkIntent {
	return VMNetworkIntent{ManagedVMNAD: ManagedVMNAD{TenantID: s.TenantID, VPCID: s.VPCID, SubnetID: s.SubnetID, ClusterID: s.ClusterID, Namespace: s.Namespace, NamespaceUID: record.NamespaceUid, NADName: record.NadName, NADUID: record.NadUid, NADConfigDigest: record.NadConfigDigest, KCNSubnetRef: s.NativeNamespace + "/" + s.NativeName}, NADConfig: record.NadConfig, ServiceAccount: record.ServiceAccount}
}

func vmIntentMatches(s biz.ResolvedNetworkSelection, intent VMNetworkIntent, expected *biz.VMNetworkExpectation, serviceAccount string) error {
	digest, err := VMNADConfigDigest(intent.NADConfig)
	if err != nil || validateVMSinglePluginNAD(intent.NADConfig) != nil || digest != intent.NADConfigDigest || intent.TenantID != s.TenantID || intent.VPCID != s.VPCID || intent.SubnetID != s.SubnetID || intent.ClusterID != s.ClusterID || intent.Namespace != s.Namespace || intent.NamespaceUID == "" || (s.NamespaceUID != "" && intent.NamespaceUID != s.NamespaceUID) || intent.NADName == "" || intent.ServiceAccount == "" || intent.KCNSubnetRef != s.NativeNamespace+"/"+s.NativeName {
		return biz.Fail(biz.AttachmentConflict, "frozen VM network intent differs")
	}
	if expected != nil && (expected.NADName != intent.NADName || expected.NADConfigDigest != intent.NADConfigDigest || (expected.NADUID != "" && expected.NADUID != intent.NADUID)) {
		return biz.Fail(biz.AttachmentConflict, "frozen VM NAD differs")
	}
	if serviceAccount != "" && serviceAccount != intent.ServiceAccount {
		return biz.Fail(biz.AttachmentConflict, "frozen VM ServiceAccount differs")
	}
	return nil
}

// LookupVMNetworkIntent resolves trusted raw configuration without writing a
// Namespace registration, NAD, ServiceAccount, BNI, Attachment or preparation row.
// A missing automatic preparation row is a valid initial state.
func (p *KCProvider) LookupVMNetworkIntent(ctx context.Context, s biz.ResolvedNetworkSelection, expected *biz.VMNetworkExpectation, serviceAccount string) (VMNetworkIntent, error) {
	identity, err := p.repository.ResolveTenantNamespace(ctx, s.TenantID)
	if err != nil {
		return VMNetworkIntent{}, err
	}
	if identity.ClusterID != s.ClusterID || identity.Name != s.Namespace || (s.NamespaceUID != "" && identity.UID != s.NamespaceUID) {
		return VMNetworkIntent{}, biz.Fail(biz.AttachmentConflict, "VM namespace identity changed")
	}
	s.NamespaceUID = identity.UID
	legacy, hasLegacy := p.vmNetworks[vmNetworkPolicyKey(s.TenantID, s.SubnetID, s.Namespace)]
	useLegacy := hasLegacy && expected != nil && expected.NADUID != "" && expected.NADName == legacy.NADName && expected.NADUID == legacy.NADUID && expected.NADConfigDigest == legacy.NADConfigDigest
	record, err := p.repository.readQueries(ctx).GetVMNamespace(ctx, sqlcgen.GetVMNamespaceParams{TenantID: s.TenantID, ClusterID: s.ClusterID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return VMNetworkIntent{}, databaseFailure(err)
	}
	useLegacy = useLegacy || (errors.Is(err, pgx.ErrNoRows) && hasLegacy)
	var intent VMNetworkIntent
	if useLegacy {
		intent = VMNetworkIntent{ManagedVMNAD: legacy, ServiceAccount: serviceAccount}
		if intent.ServiceAccount == "" && p.vmAutoNetwork != nil {
			intent.ServiceAccount = p.vmAutoNetwork.ServiceAccount
		}
		nad, getErr := p.client.Resource(vmNADs).Namespace(s.Namespace).Get(ctx, legacy.NADName, metav1.GetOptions{})
		if getErr != nil {
			return VMNetworkIntent{}, readFailure(getErr)
		}
		if nad.GetAPIVersion() != "k8s.cni.cncf.io/v1" || nad.GetKind() != "NetworkAttachmentDefinition" || string(nad.GetUID()) != legacy.NADUID || nad.GetDeletionTimestamp() != nil {
			return VMNetworkIntent{}, biz.Fail(biz.AttachmentConflict, "static VM NAD identity changed")
		}
		intent.NADConfig, _, _ = unstructured.NestedString(nad.Object, "spec", "config")
	} else if err == nil {
		if record.Namespace != s.Namespace || record.NamespaceUid != s.NamespaceUID || record.TemplateID != vmNetworkTemplate {
			return VMNetworkIntent{}, biz.Fail(biz.AttachmentConflict, "managed VM namespace identity changed")
		}
		intent = vmNetworkFromRecord(s, record)
	} else {
		if p.vmAutoNetwork == nil {
			return VMNetworkIntent{}, biz.Fail(biz.DependencyUnavailable, "automatic VM network configuration unavailable")
		}
		raw := automaticVMNADConfig(*p.vmAutoNetwork)
		digest, digestErr := VMNADConfigDigest(raw)
		if digestErr != nil {
			return VMNetworkIntent{}, digestErr
		}
		intent = VMNetworkIntent{ManagedVMNAD: ManagedVMNAD{TenantID: s.TenantID, VPCID: s.VPCID, SubnetID: s.SubnetID, ClusterID: s.ClusterID, Namespace: s.Namespace, NamespaceUID: s.NamespaceUID, NADName: vmManagedNADName, NADConfigDigest: digest, KCNSubnetRef: s.NativeNamespace + "/" + s.NativeName}, NADConfig: raw, ServiceAccount: p.vmAutoNetwork.ServiceAccount}
	}
	if err := vmIntentMatches(s, intent, expected, serviceAccount); err != nil {
		return VMNetworkIntent{}, err
	}
	return intent, nil
}

// PrepareTenantVMNetwork consumes a frozen owner intent. Namespace registration
// and the platform BNI declaration precede this helper in the owner adapter.
func (p *KCProvider) PrepareTenantVMNetwork(ctx context.Context, s biz.ResolvedNetworkSelection, intent VMNetworkIntent) (out ManagedVMNAD, result error) {
	if err := vmIntentMatches(s, intent, nil, intent.ServiceAccount); err != nil {
		return out, err
	}
	if err := p.verifyTenantNamespace(ctx, sqlcgen.NetworkProviderBinding{TenantID: s.TenantID, ClusterID: s.ClusterID, Namespace: s.Namespace}); err != nil {
		return out, err
	}
	legacy, hasLegacy := p.vmNetworks[vmNetworkPolicyKey(s.TenantID, s.SubnetID, s.Namespace)]
	if hasLegacy && intent.NADUID != "" && legacy.NADName == intent.NADName && legacy.NADUID == intent.NADUID && legacy.NADConfigDigest == intent.NADConfigDigest {
		// An accepted static mapping stays a read-only original-object path.
		if err := p.ValidateTenantVMNetwork(ctx, s, intent); err != nil {
			return out, err
		}
		return intent.ManagedVMNAD, nil
	}
	if intent.NADUID != "" {
		original, err := p.repository.readQueries(ctx).GetVMNamespace(ctx, sqlcgen.GetVMNamespaceParams{TenantID: s.TenantID, ClusterID: s.ClusterID})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return out, biz.Fail(biz.AttachmentConflict, "original VM network preparation or static mapping is missing")
			}
			return out, databaseFailure(err)
		}
		if original.NadName != intent.NADName || original.NadUid != intent.NADUID || original.NadConfigDigest != intent.NADConfigDigest {
			return out, biz.Fail(biz.AttachmentConflict, "original VM network preparation differs")
		}
	}
	if err := p.repository.queries.ReserveVMNamespace(ctx, sqlcgen.ReserveVMNamespaceParams{TenantID: s.TenantID, ClusterID: s.ClusterID, Namespace: s.Namespace, NamespaceUid: intent.NamespaceUID, ManagementID: uuid.NewString(), NadName: intent.NADName, NadConfig: intent.NADConfig, NadConfigDigest: intent.NADConfigDigest, ServiceAccount: intent.ServiceAccount}); err != nil {
		return out, databaseFailure(err)
	}
	record, err := p.repository.queries.GetVMNamespace(ctx, sqlcgen.GetVMNamespaceParams{TenantID: s.TenantID, ClusterID: s.ClusterID})
	if err != nil {
		return out, databaseFailure(err)
	}
	failureScope := record
	defer func() {
		if result != nil {
			_ = p.repository.queries.FailVMNamespacePreparation(ctx, sqlcgen.FailVMNamespacePreparationParams{TenantID: failureScope.TenantID, ClusterID: failureScope.ClusterID, ManagementID: failureScope.ManagementID, Reason: "VM_NETWORK_PREPARATION_FAILED"})
		}
	}()
	if record.Namespace != s.Namespace || record.NamespaceUid != intent.NamespaceUID || record.TemplateID != vmNetworkTemplate || record.NadName != intent.NADName || record.NadConfigDigest != intent.NADConfigDigest || record.ServiceAccount != intent.ServiceAccount || (intent.NADUID != "" && record.NadUid != intent.NADUID) {
		return out, biz.Fail(biz.AttachmentConflict, "managed VM namespace or frozen intent changed")
	}
	nad, err := p.ensureVMNetworkObject(ctx, record, vmNADs, "NetworkAttachmentDefinition", record.NadName, record.NadUid)
	if err != nil {
		return out, err
	}
	record, err = p.repository.queries.SaveVMNamespaceObjectIdentity(ctx, sqlcgen.SaveVMNamespaceObjectIdentityParams{TenantID: record.TenantID, ClusterID: record.ClusterID, ManagementID: record.ManagementID, ObjectKind: "nad", ObjectUid: string(nad.GetUID())})
	if err != nil {
		return out, databaseFailure(err)
	}
	sa, err := p.ensureVMNetworkObject(ctx, record, vmServiceAccounts, "ServiceAccount", record.ServiceAccount, record.ServiceAccountUid)
	if err != nil {
		return out, err
	}
	record, err = p.repository.queries.SaveVMNamespaceObjectIdentity(ctx, sqlcgen.SaveVMNamespaceObjectIdentityParams{TenantID: record.TenantID, ClusterID: record.ClusterID, ManagementID: record.ManagementID, ObjectKind: "service_account", ObjectUid: string(sa.GetUID())})
	if err != nil {
		return out, databaseFailure(err)
	}
	return vmNetworkFromRecord(s, record).ManagedVMNAD, nil
}

// ValidateTenantVMNetwork checks only the accepted network and the VM's own
// original namespace/NAD/ServiceAccount identities. It never consults current
// automatic configuration, Ready, preparation reason or catalog publication.
func (p *KCProvider) ValidateTenantVMNetwork(ctx context.Context, s biz.ResolvedNetworkSelection, intent VMNetworkIntent) error {
	if err := vmIntentMatches(s, intent, &biz.VMNetworkExpectation{NADName: intent.NADName, NADUID: intent.NADUID, NADConfigDigest: intent.NADConfigDigest}, intent.ServiceAccount); err != nil {
		return err
	}
	if intent.NADUID == "" {
		return biz.Fail(biz.AttachmentConflict, "accepted VM NAD identity is missing")
	}
	ns, err := p.tenantNamespace(ctx, sqlcgen.NetworkProviderBinding{TenantID: s.TenantID, ClusterID: s.ClusterID, Namespace: s.Namespace})
	if err != nil {
		return err
	}
	if string(ns.GetUID()) != intent.NamespaceUID {
		return biz.Fail(biz.AttachmentConflict, "VM namespace identity changed")
	}
	nad, err := p.client.Resource(vmNADs).Namespace(s.Namespace).Get(ctx, intent.NADName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return biz.Fail(biz.ProviderOwnership, "accepted VM NAD no longer exists")
		}
		return readFailure(err)
	}
	raw, _, _ := unstructured.NestedString(nad.Object, "spec", "config")
	digest, digestErr := VMNADConfigDigest(raw)
	if nad.GetAPIVersion() != "k8s.cni.cncf.io/v1" || nad.GetKind() != "NetworkAttachmentDefinition" || string(nad.GetUID()) != intent.NADUID || nad.GetDeletionTimestamp() != nil || digestErr != nil || digest != intent.NADConfigDigest {
		return biz.Fail(biz.AttachmentConflict, "accepted VM NAD identity or configuration changed")
	}
	record, err := p.repository.readQueries(ctx).GetVMNamespace(ctx, sqlcgen.GetVMNamespaceParams{TenantID: s.TenantID, ClusterID: s.ClusterID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return databaseFailure(err)
	}
	legacy, static := p.vmNetworks[vmNetworkPolicyKey(s.TenantID, s.SubnetID, s.Namespace)]
	static = static && intent.NADName == legacy.NADName && intent.NADUID == legacy.NADUID && intent.NADConfigDigest == legacy.NADConfigDigest
	if static {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) || record.Namespace != s.Namespace || record.NamespaceUid != intent.NamespaceUID || record.NadName != intent.NADName || record.NadUid != intent.NADUID || record.NadConfigDigest != intent.NADConfigDigest || record.ServiceAccount != intent.ServiceAccount || record.ServiceAccountUid == "" {
		return biz.Fail(biz.AttachmentConflict, "accepted VM preparation identity changed")
	}
	if err := verifyVMNetworkObject(nad, record, "NetworkAttachmentDefinition", record.NadName, record.NadUid); err != nil {
		return err
	}
	sa, err := p.client.Resource(vmServiceAccounts).Namespace(s.Namespace).Get(ctx, record.ServiceAccount, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return biz.Fail(biz.ProviderOwnership, "accepted VM ServiceAccount no longer exists")
		}
		return readFailure(err)
	}
	return verifyVMNetworkObject(sa, record, "ServiceAccount", record.ServiceAccount, record.ServiceAccountUid)
}

// PrepareVMNetwork is the existing private Subnet preparation entry point. The
// same owner helper is used by a first VM, without manufacturing Subnet work.
func (p *KCProvider) PrepareVMNetwork(ctx context.Context, target biz.ProviderTarget) error {
	if p.vmAutoNetwork == nil || target.Kind != "subnet" {
		return nil
	}
	binding, err := p.binding(ctx, target)
	if err != nil {
		return err
	}
	selected, err := p.repository.ResolveAcceptedNetworkSelection(ctx, biz.NetworkSelection{TenantID: binding.TenantID, VPCID: target.VPCID, SubnetID: target.ResourceID, ClusterID: binding.ClusterID, Namespace: binding.Namespace})
	if err != nil {
		return err
	}
	record, err := p.repository.readQueries(ctx).GetVMNamespace(ctx, sqlcgen.GetVMNamespaceParams{TenantID: selected.TenantID, ClusterID: selected.ClusterID})
	var intent VMNetworkIntent
	if err == nil {
		intent = vmNetworkFromRecord(selected, record)
	} else if errors.Is(err, pgx.ErrNoRows) {
		raw := automaticVMNADConfig(*p.vmAutoNetwork)
		digest, digestErr := VMNADConfigDigest(raw)
		if digestErr != nil {
			return digestErr
		}
		intent = VMNetworkIntent{ManagedVMNAD: ManagedVMNAD{TenantID: selected.TenantID, VPCID: selected.VPCID, SubnetID: selected.SubnetID, ClusterID: selected.ClusterID, Namespace: selected.Namespace, NamespaceUID: selected.NamespaceUID, NADName: vmManagedNADName, NADConfigDigest: digest, KCNSubnetRef: selected.NativeNamespace + "/" + selected.NativeName}, NADConfig: raw, ServiceAccount: p.vmAutoNetwork.ServiceAccount}
	} else {
		return databaseFailure(err)
	}
	_, err = p.PrepareTenantVMNetwork(ctx, selected, intent)
	return err
}

func (p *Postgres) RetryVMNetworkPreparation(ctx context.Context, w biz.Work, delay time.Duration) error {
	// Finish already advanced the resource version once and released this lease.
	// Match that completed version; any later mutation or lease rejects the retry.
	result, err := p.pool.Exec(ctx, `UPDATE network_reconciliations q SET
 requested_generation=q.requested_generation+1,
 retry_not_before=greatest(q.retry_not_before,clock_timestamp()+$6::bigint*interval '1 microsecond'),
 next_run_at=greatest(q.retry_not_before,clock_timestamp()+$6::bigint*interval '1 microsecond')
 FROM network_provider_bindings b JOIN network_subnets s USING(tenant_id,subnet_id)
 WHERE q.tenant_id=$1 AND q.subnet_id=$2 AND b.tenant_id=q.tenant_id AND b.subnet_id=q.subnet_id
 AND b.binding_id=$3 AND s.version=($4::bigint+1) AND q.lease_epoch=$5 AND q.lease_owner IS NULL
 AND NOT q.retired AND s.state NOT IN ('deleting','deleted')`, w.Resource.TenantID, w.Resource.ID, w.BindingID, w.Resource.Version, w.Epoch, delay.Microseconds())
	if err != nil {
		return databaseFailure(err)
	}
	if result.RowsAffected() != 1 {
		return biz.ErrLeaseLost
	}
	return nil
}

func (p *KCProvider) ensureVMNetworkObject(ctx context.Context, record sqlcgen.NetworkVmNamespace, gvr schema.GroupVersionResource, kind, name, uid string) (*unstructured.Unstructured, error) {
	resource := p.client.Resource(gvr).Namespace(record.Namespace)
	value, err := resource.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && uid == "" {
		object := &unstructured.Unstructured{Object: map[string]any{"apiVersion": gvr.GroupVersion().String(), "kind": kind,
			"metadata": map[string]any{"namespace": record.Namespace, "name": name, "labels": map[string]any{ownerLabel: "ani-network-service", tenantLabel: record.TenantID, vmManagementLabel: record.ManagementID, vmTemplateLabel: record.TemplateID}}}}
		if kind == "NetworkAttachmentDefinition" {
			object.Object["spec"] = map[string]any{"config": record.NadConfig}
		} else {
			object.Object["automountServiceAccountToken"] = false
		}
		value, err = resource.Create(ctx, object, metav1.CreateOptions{FieldManager: "ani-network-service"})
		if apierrors.IsAlreadyExists(err) {
			value, err = resource.Get(ctx, name, metav1.GetOptions{})
		}
	}
	if err != nil {
		if apierrors.IsNotFound(err) && uid != "" {
			return nil, biz.Fail(biz.ProviderOwnership, "managed VM network original object no longer exists")
		}
		return nil, readFailure(err)
	}
	if err = verifyVMNetworkObject(value, record, kind, name, uid); err != nil {
		return nil, err
	}
	return value, nil
}

func verifyVMNetworkObject(value *unstructured.Unstructured, record sqlcgen.NetworkVmNamespace, kind, name, uid string) error {
	expectedAPI := "v1"
	if kind == "NetworkAttachmentDefinition" {
		expectedAPI = "k8s.cni.cncf.io/v1"
	}
	labels := value.GetLabels()
	if value.GetAPIVersion() != expectedAPI || value.GetKind() != kind || value.GetNamespace() != record.Namespace || value.GetName() != name || value.GetUID() == "" || value.GetDeletionTimestamp() != nil || (uid != "" && string(value.GetUID()) != uid) || labels[ownerLabel] != "ani-network-service" || labels[tenantLabel] != record.TenantID || labels[vmManagementLabel] != record.ManagementID || labels[vmTemplateLabel] != record.TemplateID {
		return biz.Fail(biz.AttachmentConflict, "managed VM network object ownership or identity changed")
	}
	if kind == "NetworkAttachmentDefinition" {
		raw, _, _ := unstructured.NestedString(value.Object, "spec", "config")
		digest, err := VMNADConfigDigest(raw)
		if err != nil || validateVMSinglePluginNAD(raw) != nil || digest != record.NadConfigDigest {
			return biz.Fail(biz.AttachmentConflict, "managed VM NAD template changed")
		}
	} else {
		automount, found, err := unstructured.NestedBool(value.Object, "automountServiceAccountToken")
		if err != nil || !found || automount {
			return biz.Fail(biz.AttachmentConflict, "managed VM ServiceAccount configuration changed")
		}
	}
	return nil
}

// LookupManagedVMNetwork preserves the read-only lookup entry point. A new VM
// can freeze an empty NAD UID while its own create operation prepares the NAD.
func (p *KCProvider) LookupManagedVMNetwork(ctx context.Context, tenant, vpc, subnet, cluster, namespace string) (ManagedVMNAD, error) {
	s, err := p.repository.ResolveNewNetworkSelection(ctx, biz.NetworkSelection{TenantID: tenant, VPCID: vpc, SubnetID: subnet, ClusterID: cluster, Namespace: namespace})
	if err != nil {
		return ManagedVMNAD{}, err
	}
	intent, err := p.LookupVMNetworkIntent(ctx, s, nil, "")
	return intent.ManagedVMNAD, err
}

// Runtime selection uses the accepted binding and original preparation record.
// requireRunnable controls the caller's own object checks, never current config
// or the preparation record's cached ready/reason fields.
func (p *KCProvider) lookupManagedVMNetwork(ctx context.Context, tenant, vpc, subnet, cluster, namespace string, requireRunnable bool, references ...biz.NetworkReference) (ManagedVMNAD, error) {
	selection := biz.NetworkSelection{TenantID: tenant, VPCID: vpc, SubnetID: subnet, ClusterID: cluster, Namespace: namespace}
	if len(references) > 0 {
		selection.Reference = references[0]
	}
	s, err := p.repository.ResolveAcceptedNetworkSelection(ctx, selection)
	if err != nil {
		return ManagedVMNAD{}, err
	}
	if err := p.verifyTenantNamespace(ctx, sqlcgen.NetworkProviderBinding{TenantID: tenant, ClusterID: cluster, Namespace: namespace}); err != nil {
		return ManagedVMNAD{}, err
	}
	record, err := p.repository.readQueries(ctx).GetVMNamespace(ctx, sqlcgen.GetVMNamespaceParams{TenantID: tenant, ClusterID: cluster})
	if errors.Is(err, pgx.ErrNoRows) {
		if legacy, ok := p.vmNetworks[vmNetworkPolicyKey(tenant, subnet, namespace)]; ok && legacy.ClusterID == cluster && legacy.VPCID == vpc && legacy.KCNSubnetRef == s.NativeNamespace+"/"+s.NativeName {
			return legacy, nil
		}
		return ManagedVMNAD{}, biz.Fail(biz.NetworkNotReady, "managed VM preparation has no original record")
	}
	if err != nil {
		return ManagedVMNAD{}, databaseFailure(err)
	}
	if record.Namespace != namespace || record.NamespaceUid == "" || (s.NamespaceUID != "" && record.NamespaceUid != s.NamespaceUID) || record.TemplateID != vmNetworkTemplate || record.NadUid == "" {
		return ManagedVMNAD{}, biz.Fail(biz.AttachmentConflict, "managed VM namespace identity changed")
	}
	intent := vmNetworkFromRecord(s, record)
	if err := vmIntentMatches(s, intent, nil, record.ServiceAccount); err != nil {
		return ManagedVMNAD{}, err
	}
	return intent.ManagedVMNAD, nil
}
