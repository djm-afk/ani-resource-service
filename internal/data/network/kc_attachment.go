package data

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const attachmentLabel = "network.ani.io/attachment-id"
const instanceLabel = "network.ani.io/instance-id"
const submissionLabel = "network.ani.io/submission-id"
const generationLabel = "network.ani.io/generation"
const subnetAnnotation = "networking.kubercloud.com/subnet"
const primaryVNicAnnotation = "networking.kubercloud.com/vnic"

var pods = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

// These listing helpers are used by explicit management/list operations only.
// Attachment state and deletion never use a collection to prove absence.
func (p *KCProvider) listAll(ctx context.Context, resource schema.GroupVersionResource) ([]unstructured.Unstructured, error) {
	return p.listScoped(ctx, resource, "", "")
}

func (p *KCProvider) listScoped(ctx context.Context, resource schema.GroupVersionResource, namespace, selector string) ([]unstructured.Unstructured, error) {
	var values []unstructured.Unstructured
	next := ""
	seen := map[string]bool{}
	for {
		page, err := p.client.Resource(resource).Namespace(namespace).List(ctx, metav1.ListOptions{Limit: 500, Continue: next, LabelSelector: selector})
		if err != nil {
			return nil, readFailure(err)
		}
		values = append(values, page.Items...)
		next = page.GetContinue()
		if next == "" {
			return values, nil
		}
		if seen[next] {
			return nil, readFailure(fmt.Errorf("Kubernetes LIST repeated a continuation token"))
		}
		seen[next] = true
	}
}

func kcRef(value, namespace string) string {
	if value != "" && !strings.Contains(value, "/") {
		return namespace + "/" + value
	}
	return value
}

func currentVNicReference(a biz.Attachment) (string, string) {
	if namespace, name, qualified := strings.Cut(a.CurrentVNicName, "/"); qualified {
		return namespace, name
	}
	return a.Namespace, a.CurrentVNicName
}

func attachmentAddresses(a biz.Attachment) biz.AttachmentAddressObservation {
	return biz.AttachmentAddressObservation{
		CurrentVNicName: a.CurrentVNicName, CurrentVNicUID: a.CurrentVNicUID,
		CurrentMACAddress: a.CurrentMACAddress, CurrentIPs: append([]string{}, a.CurrentIPs...),
	}
}

func (p *KCProvider) ObserveAttachmentAddresses(ctx context.Context, a biz.Attachment, consumer biz.ConsumerSubmission) (biz.AttachmentAddressObservation, error) {
	previous := attachmentAddresses(a)
	if a.State != biz.Attached || a.PodName == "" || consumer.State != "open" || consumer.DesiredRunState == "stopped" || consumer.DesiredRunState == "deleted" {
		return biz.AttachmentAddressObservation{CurrentIPs: []string{}}, nil
	}
	pod, err := p.client.Resource(pods).Namespace(a.Namespace).Get(ctx, a.PodName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return biz.AttachmentAddressObservation{CurrentIPs: []string{}}, nil
	}
	if err != nil {
		return previous, readFailure(err)
	}
	if a.PodUID != "" && string(pod.GetUID()) != a.PodUID {
		// The registered consumer's source has gone. Do not borrow addresses
		// from a replacement Pod that happens to reuse the same name.
		return biz.AttachmentAddressObservation{CurrentIPs: []string{}}, nil
	}
	value := biz.AttachmentAddressObservation{CurrentVNicName: pod.GetAnnotations()[primaryVNicAnnotation], CurrentIPs: []string{}}
	if a.WorkloadMode == "vm_managed" {
		var networks []struct {
			Default bool     `json:"default"`
			IPs     []string `json:"ips"`
			MAC     string   `json:"mac"`
		}
		if raw := pod.GetAnnotations()["k8s.v1.cni.cncf.io/network-status"]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &networks); err != nil {
				return previous, readFailure(err)
			}
		}
		for _, network := range networks {
			if network.Default {
				value.CurrentIPs = append(value.CurrentIPs, network.IPs...)
				value.CurrentMACAddress = network.MAC
				break
			}
		}
	} else {
		ips, _, _ := unstructured.NestedSlice(pod.Object, "status", "podIPs")
		for _, raw := range ips {
			if item, ok := raw.(map[string]any); ok {
				if ip, ok := item["ip"].(string); ok && ip != "" {
					value.CurrentIPs = append(value.CurrentIPs, ip)
				}
			}
		}
		if len(value.CurrentIPs) == 0 {
			if ip, _, _ := unstructured.NestedString(pod.Object, "status", "podIP"); ip != "" {
				value.CurrentIPs = append(value.CurrentIPs, ip)
			}
		}
	}
	if namespace, name, qualified := strings.Cut(value.CurrentVNicName, "/"); qualified && namespace == a.Namespace {
		value.CurrentVNicName = name
	}
	if value.CurrentVNicName == "" || (value.CurrentMACAddress != "" && len(value.CurrentIPs) != 0) {
		return value, nil
	}
	current := a
	current.CurrentVNicName = value.CurrentVNicName
	namespace, name := currentVNicReference(current)
	vnic, err := p.client.Resource(kcManualVNics).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return value, nil
	}
	if err != nil {
		// Preserve only fields whose unchanged source could not be read. New
		// values already supplied by this Pod remain ordinary display data.
		if value.CurrentVNicName == previous.CurrentVNicName {
			value.CurrentVNicUID = previous.CurrentVNicUID
			if value.CurrentMACAddress == "" {
				value.CurrentMACAddress = previous.CurrentMACAddress
			}
			if len(value.CurrentIPs) == 0 {
				value.CurrentIPs = previous.CurrentIPs
			}
		}
		return value, readFailure(err)
	}
	facts := vnicFacts(vnic)
	value.CurrentVNicUID = string(vnic.GetUID())
	if value.CurrentMACAddress == "" {
		value.CurrentMACAddress = facts.MACAddress
	}
	if len(value.CurrentIPs) == 0 {
		value.CurrentIPs = facts.IPs
	}
	return value, nil
}
