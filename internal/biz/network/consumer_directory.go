package biz

import "context"

// Consumer types are product identities, not attachment protocols. A row keeps
// an empty type only when it predates this column; it is reported as unknown.
const (
	ConsumerContainerInstance = "container_instance"
	ConsumerVirtualMachine    = "virtual_machine"
	ConsumerSandbox           = "sandbox"
	ConsumerUnknown           = "unknown"
)

// ConsumerInfo is the resource-side identity behind one attachment row. State is
// the raw value of the owning domain; display normalization stays in the query.
type ConsumerInfo struct {
	ID, Name, State string
}

// ConsumerReader resolves a batch of IDs of one consumer type. Tenant scope is
// part of the contract: a reader never returns another tenant's rows, and an ID
// with no record stays absent from the map rather than becoming an error.
type ConsumerReader interface {
	DescribeConsumers(ctx context.Context, tenant string, ids []string) (map[string]ConsumerInfo, error)
}

// ConsumerDirectory routes a batch read to the domain that owns the records.
type ConsumerDirectory interface {
	Describe(ctx context.Context, consumerType, tenant string, ids []string) (map[string]ConsumerInfo, error)
}

// consumerDirectory composes per-domain readers. An absent reader means that
// module is not composed into this process, so its rows resolve to unknown
// instead of failing the whole page or forcing a guess from another table.
type consumerDirectory struct {
	container      ConsumerReader
	virtualMachine ConsumerReader
}

func NewConsumerDirectory(container, virtualMachine ConsumerReader) ConsumerDirectory {
	return &consumerDirectory{container: container, virtualMachine: virtualMachine}
}

func (d *consumerDirectory) Describe(ctx context.Context, consumerType, tenant string, ids []string) (map[string]ConsumerInfo, error) {
	var reader ConsumerReader
	switch consumerType {
	case ConsumerContainerInstance:
		reader = d.container
	case ConsumerVirtualMachine:
		reader = d.virtualMachine
	default:
		return map[string]ConsumerInfo{}, nil
	}
	if reader == nil || len(ids) == 0 {
		return map[string]ConsumerInfo{}, nil
	}
	return reader.DescribeConsumers(ctx, tenant, ids)
}
