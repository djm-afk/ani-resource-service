package biz

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// VirtualSwitch is platform metadata. Its empty binding is a valid persisted
// state; a KCN vCluster has no provider object or UID to invent here.
type VirtualSwitch struct {
	EgressMetadata
	ClusterID, GroupName, BindingID, BindingState, MembershipFingerprint string
	BindingVersion                                                       int64
	Nodes                                                                []VirtualSwitchNode
}

type VirtualSwitchNode struct {
	NodeName, NodeUID, State, Reason string

	Priority  int32
	Allocated bool
	// Patch intent and acknowledgement survive ambiguous Kubernetes responses.
	ResourceVersion, OriginalValue, OriginalOwner string
	OriginalPresent, Prepared, Applied, Unknown   bool
	DeleteFenced                                  bool
}

type VirtualSwitchIntent struct{ Name, IdempotencyKey string }
type VirtualSwitchFilter struct {
	Name, State    string
	Page, PageSize int32
}

type VirtualSwitchInventory struct {
	Inventory   PreparedDeviceInventory
	Members     []VirtualSwitchNode
	Fingerprint string
}
type VirtualSwitchInfrastructure interface {
	VirtualSwitchInventory(context.Context, string, VirtualSwitch, string) (VirtualSwitchInventory, error)
}
type VirtualSwitchRepository interface {
	CreateVirtualSwitch(context.Context, VirtualSwitchIntent, Attribution) (VirtualSwitch, error)
	GetVirtualSwitch(context.Context, string) (VirtualSwitch, error)
	ListVirtualSwitches(context.Context, VirtualSwitchFilter) ([]VirtualSwitch, int64, error)
	DeleteVirtualSwitch(context.Context, string, Attribution) (VirtualSwitch, error)
}

var virtualSwitchName = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9_-]{0,61}[a-zA-Z0-9])?$`)

func (e *Egress) switchRepository() (VirtualSwitchRepository, error) {
	r, ok := e.repository.(VirtualSwitchRepository)
	if !ok {
		return nil, Fail(DependencyUnavailable, "virtual switch persistence is unavailable")
	}
	return r, nil
}

func (e *Egress) CreateVirtualSwitch(ctx context.Context, i VirtualSwitchIntent) (VirtualSwitch, error) {
	a, err := e.authorization.Platform(ctx)
	if err != nil {
		return VirtualSwitch{}, err
	}
	if !virtualSwitchName.MatchString(i.Name) || strings.Contains(i.Name, ".") {
		return VirtualSwitch{}, Fail(InvalidArgument, "name must be a label name without dots (1..63 characters)")
	}
	if _, err = normalizeEgressName(i.Name, "", i.IdempotencyKey); err != nil {
		return VirtualSwitch{}, err
	}
	r, err := e.switchRepository()
	if err != nil {
		return VirtualSwitch{}, err
	}
	return r.CreateVirtualSwitch(ctx, i, a)
}

func (e *Egress) GetVirtualSwitch(ctx context.Context, id string) (VirtualSwitch, error) {
	if _, err := e.authorization.Platform(ctx); err != nil {
		return VirtualSwitch{}, err
	}
	if !validEgressID(id, "vsw") {
		return VirtualSwitch{}, Fail(ResourceNotFound, "virtual switch not found")
	}
	r, err := e.switchRepository()
	if err != nil {
		return VirtualSwitch{}, err
	}
	return r.GetVirtualSwitch(ctx, id)
}

func (e *Egress) ListVirtualSwitches(ctx context.Context, f VirtualSwitchFilter) ([]VirtualSwitch, int64, error) {
	if _, err := e.authorization.Platform(ctx); err != nil {
		return nil, 0, err
	}
	if f.Page == 0 {
		f.Page = 1
	}
	if f.PageSize == 0 {
		f.PageSize = 20
	}
	if f.Page < 1 || f.PageSize < 1 || f.PageSize > 100 {
		return nil, 0, Fail(InvalidArgument, "page must be positive and page_size must be 1..100")
	}
	if _, err := normalizeList(f.Name, f.State, 1); err != nil {
		return nil, 0, err
	}
	r, err := e.switchRepository()
	if err != nil {
		return nil, 0, err
	}
	return r.ListVirtualSwitches(ctx, f)
}

func (e *Egress) DeleteVirtualSwitch(ctx context.Context, id string) (VirtualSwitch, error) {
	a, err := e.authorization.Platform(ctx)
	if err != nil {
		return VirtualSwitch{}, err
	}
	if !validEgressID(id, "vsw") {
		return VirtualSwitch{}, Fail(ResourceNotFound, "virtual switch not found")
	}
	r, err := e.switchRepository()
	if err != nil {
		return VirtualSwitch{}, err
	}
	return r.DeleteVirtualSwitch(ctx, id, a)
}

// VirtualSwitchWork owns a separate lease: concurrent VLANs never patch the
// shared group under unrelated per-VLAN leases.
type VirtualSwitchWork struct {
	Switch                   VirtualSwitch
	Owner                    string
	Epoch, ClaimedGeneration int64
	Now                      time.Time
}

type VirtualSwitchWorkRepository interface {
	ClaimVirtualSwitch(context.Context, string, time.Duration) (VirtualSwitchWork, bool, error)
	SaveVirtualSwitchNode(context.Context, VirtualSwitchWork, int, VirtualSwitchNode) error
	FinishVirtualSwitch(context.Context, VirtualSwitchWork, VirtualSwitchProgress, time.Duration) error
}

// Read/patch operate on the frozen Res membership and use the original Node
// identity plus metadata owner CAS for each patch.
type VirtualSwitchGroupProvider interface {
	InspectVirtualSwitchNode(context.Context, VirtualSwitch, VirtualSwitchNode) (VirtualSwitchNode, error)
	PatchVirtualSwitchNode(context.Context, VirtualSwitch, VirtualSwitchNode, bool) (VirtualSwitchNode, error)
}
