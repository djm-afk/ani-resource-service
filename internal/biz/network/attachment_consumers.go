package biz

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
)

// ListAttachmentConsumers asks which compute consumers hold a network slot in
// one VPC or one subnet. Exactly one scope is required; both are accepted only
// to assert that the subnet belongs to the VPC.
type ListAttachmentConsumers struct {
	TenantID, VPCID, SubnetID string
	Page, PageSize            *uint32
}

type AttachmentAddress struct {
	NICName, Address string
}

// AttachmentConsumerRow is one active attachment as Network owns it. Addresses
// and staleness come from Network's own persisted observation, so the row never
// depends on a live workload read.
type AttachmentConsumerRow struct {
	Addresses                         []AttachmentAddress
	AttachmentID, VPCID, SubnetID     string
	SubnetName, Slot, ConsumerType    string
	ConsumerID, State, Reason, PodUID string
	TenantID                          string
	ObservationStale, ProtocolBlocked bool
	ObservedAt, CreatedAt             time.Time
	HasObservation                    bool
}

type AttachmentConsumerFilter struct {
	VPCID, SubnetID string
	Limit           int32
	Offset          int64
}

type AttachmentConsumer struct {
	Addresses                          []AttachmentAddress
	AttachmentID, SubnetID, SubnetName string
	Slot, ConsumerType, ConsumerID     string
	TenantID                           string
	ConsumerName, ConsumerState        string
	AttachmentState, AttachmentReason  string
	ObservedAt                         *time.Time
	ObservationStale, ProtocolBlocked  bool
}

type AttachmentConsumerPage struct {
	Items []AttachmentConsumer
	Total int64
}

// UseConsumerDirectory attaches the batch readers that resolve consumer names
// and run states. Without it the query still returns every attachment row, but
// every unresolved consumer is reported as unknown instead of guessed.
func (a *Attachments) UseConsumerDirectory(d ConsumerDirectory) { a.consumers = d }

// ListInstances returns the consumers attached to one network scope.
//
// A row is kept even when its resource record is gone: that is exactly the
// state a deleting or release-failed attachment is in, and hiding it would make
// the network look cleaner than it is.
func (a *Attachments) ListInstances(ctx context.Context, r ListAttachmentConsumers) (AttachmentConsumerPage, error) {
	tenant, err := ParseTenant(r.TenantID)
	if err != nil {
		return AttachmentConsumerPage{}, err
	}
	if r.VPCID == "" && r.SubnetID == "" {
		return AttachmentConsumerPage{}, Fail(InvalidArgument, "vpc_id or subnet_id is required")
	}
	if (r.VPCID != "" && !validVPCID(r.VPCID)) || (r.SubnetID != "" && !validSubnetID(r.SubnetID)) {
		return AttachmentConsumerPage{}, Fail(InvalidArgument, "invalid network scope identity")
	}
	window, err := ResolvePage(r.Page, r.PageSize)
	if err != nil {
		return AttachmentConsumerPage{}, err
	}
	rows, total, err := a.repository.ListAttachmentConsumers(ctx, tenant, AttachmentConsumerFilter{VPCID: r.VPCID, SubnetID: r.SubnetID, Limit: window.Limit, Offset: window.Offset}, a.freshness)
	if err != nil {
		return AttachmentConsumerPage{}, err
	}
	page := AttachmentConsumerPage{Total: total, Items: make([]AttachmentConsumer, 0, len(rows))}
	if len(rows) == 0 {
		return page, nil
	}
	resolved, err := a.resolveConsumers(ctx, tenant, rows)
	if err != nil {
		return AttachmentConsumerPage{}, err
	}
	for _, row := range rows {
		value := AttachmentConsumer{
			Addresses: row.Addresses, AttachmentID: row.AttachmentID, SubnetID: row.SubnetID, SubnetName: row.SubnetName,
			Slot: row.Slot, ConsumerID: canonicalConsumerID(row.ConsumerID), ConsumerType: row.ConsumerType,
			TenantID: row.TenantID, ConsumerState: ConsumerUnknown, AttachmentState: row.State, AttachmentReason: row.Reason,
			ObservationStale: row.ObservationStale, ProtocolBlocked: row.ProtocolBlocked,
		}
		if value.ConsumerType == "" {
			value.ConsumerType = ConsumerUnknown
		}
		if value.Addresses == nil {
			value.Addresses = []AttachmentAddress{}
		}
		if row.HasObservation {
			observed := row.ObservedAt
			value.ObservedAt = &observed
		}
		if info, ok := resolved[consumerKey(value.ConsumerType, value.ConsumerID)]; ok {
			value.ConsumerName = info.Name
			value.ConsumerState = consumerDisplayState(info.State)
		}
		page.Items = append(page.Items, value)
	}
	return page, nil
}

// resolveConsumers reads each consumer type once for the whole page. An unknown
// type has no owning domain, so it is never looked up in a table that merely
// happens to contain a matching primary key.
func (a *Attachments) resolveConsumers(ctx context.Context, tenant string, rows []AttachmentConsumerRow) (map[string]ConsumerInfo, error) {
	byType := map[string][]string{}
	queued := map[string]bool{}
	for _, row := range rows {
		if row.ConsumerType != ConsumerContainerInstance && row.ConsumerType != ConsumerVirtualMachine {
			continue
		}
		id := canonicalConsumerID(row.ConsumerID)
		if _, err := uuid.Parse(id); err != nil {
			// Both resource tables key consumers by UUID. An identity that is
			// not one cannot be resolved, so it stays unknown rather than
			// matching a row that merely looks similar.
			continue
		}
		key := consumerKey(row.ConsumerType, id)
		if queued[key] {
			continue
		}
		queued[key] = true
		byType[row.ConsumerType] = append(byType[row.ConsumerType], id)
	}
	out := map[string]ConsumerInfo{}
	if a.consumers == nil {
		return out, nil
	}
	for consumerType, ids := range byType {
		sort.Strings(ids)
		values, err := a.consumers.Describe(ctx, consumerType, tenant, ids)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			// Only an exact identity match is trusted; a reader that returns a
			// foreign or malformed ID cannot rename or restate another row.
			if info, ok := values[id]; ok && info.ID == id {
				out[consumerKey(consumerType, id)] = info
			}
		}
	}
	return out, nil
}

func consumerKey(consumerType, id string) string { return consumerType + "/" + id }

// canonicalConsumerID renders stored text in the standard UUID form both
// resource tables use, and leaves any other identity untouched.
func canonicalConsumerID(value string) string {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil {
		return value
	}
	return id.String()
}

// consumerDisplayState maps either resource vocabulary onto the display states
// this query publishes. Both sides share one table so the same word cannot come
// to mean two things, and an unmapped or absent value stays unknown.
func consumerDisplayState(raw string) string {
	switch raw {
	case "running":
		return "running"
	case "stopped", "succeeded":
		return "stopped"
	case "pending", "starting", "provisioning":
		return "provisioning"
	case "deleting", "deleted", "stopping":
		return "deleting"
	case "failed":
		return "failed"
	default:
		return ConsumerUnknown
	}
}
