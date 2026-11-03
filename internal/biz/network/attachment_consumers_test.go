package biz

import (
	"context"
	"strings"
	"testing"
	"time"
)

type consumerRepositoryFake struct {
	rows      []AttachmentConsumerRow
	total     int64
	err       error
	tenant    string
	filter    AttachmentConsumerFilter
	freshness time.Duration
	calls     int
	prepared  PrepareAttachment
}

func (f *consumerRepositoryFake) PrepareAttachment(_ context.Context, v PrepareAttachment, _ time.Duration) (Attachment, error) {
	f.prepared = v
	return Attachment{ID: "att_" + strings.Repeat("a", 32), TenantID: v.TenantID, InstanceID: v.InstanceID, ConsumerType: v.ConsumerType, State: Reserved}, nil
}
func (f *consumerRepositoryFake) GetAttachment(context.Context, string, string) (Attachment, error) {
	return Attachment{}, nil
}
func (f *consumerRepositoryFake) ConfirmAttachment(context.Context, ConfirmAttachment) (Attachment, error) {
	return Attachment{}, nil
}
func (f *consumerRepositoryFake) ReleaseAttachment(context.Context, ReleaseAttachment) (Attachment, error) {
	return Attachment{}, nil
}
func (f *consumerRepositoryFake) ListAttachmentConsumers(_ context.Context, tenant string, filter AttachmentConsumerFilter, freshness time.Duration) ([]AttachmentConsumerRow, int64, error) {
	f.calls++
	f.tenant, f.filter, f.freshness = tenant, filter, freshness
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.rows, f.total, nil
}

// consumerDirectoryFake records each batch so a page cannot turn into one read
// per row, and can be told to resolve nothing at all.
type consumerDirectoryFake struct {
	values map[string]ConsumerInfo
	calls  []string
	err    error
}

func (f *consumerDirectoryFake) Describe(_ context.Context, consumerType, tenant string, ids []string) (map[string]ConsumerInfo, error) {
	f.calls = append(f.calls, consumerType+"|"+tenant+"|"+strings.Join(ids, ","))
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]ConsumerInfo{}
	for _, id := range ids {
		if value, ok := f.values[consumerType+"/"+id]; ok {
			out[id] = value
		}
	}
	return out, nil
}

const (
	consumerTenant   = "11111111-1111-4111-8111-111111111111"
	consumerVPC      = "vpc_" + "0123456789abcdef0123456789abcdef"
	consumerSubnet   = "subnet_" + "0123456789abcdef0123456789abcdef"
	containerOne     = "3f0c9a2e-7d1b-4c58-a6e0-f1b2c3d4e5f6"
	containerTwo     = "4f0c9a2e-7d1b-4c58-a6e0-f1b2c3d4e5f7"
	virtualMachineID = "5f0c9a2e-7d1b-4c58-a6e0-f1b2c3d4e5f8"
)

func newConsumerQuery(repository AttachmentRepository, directory ConsumerDirectory) *Attachments {
	a := NewAttachments(repository, time.Minute)
	if directory != nil {
		a.UseConsumerDirectory(directory)
	}
	return a
}

func TestListInstancesRejectsInvalidScopeBeforeStorage(t *testing.T) {
	repository := &consumerRepositoryFake{}
	a := newConsumerQuery(repository, nil)
	cases := []struct {
		name  string
		input ListAttachmentConsumers
		want  Reason
	}{
		{"no scope", ListAttachmentConsumers{TenantID: consumerTenant}, InvalidArgument},
		{"bad tenant", ListAttachmentConsumers{TenantID: "tenant-1", VPCID: consumerVPC}, TenantRequired},
		{"short vpc", ListAttachmentConsumers{TenantID: consumerTenant, VPCID: "vpc_1"}, InvalidArgument},
		{"uppercase subnet", ListAttachmentConsumers{TenantID: consumerTenant, SubnetID: "subnet_" + strings.Repeat("A", 32)}, InvalidArgument},
		{"oversized page", ListAttachmentConsumers{TenantID: consumerTenant, VPCID: consumerVPC, PageSize: pointer(uint32(101))}, InvalidArgument},
		{"zero page", ListAttachmentConsumers{TenantID: consumerTenant, VPCID: consumerVPC, Page: pointer(uint32(0))}, InvalidArgument},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := a.ListInstances(context.Background(), c.input); ReasonOf(err) != c.want {
				t.Fatalf("want %s, got %v", c.want, err)
			}
		})
	}
	if repository.calls != 0 {
		t.Fatalf("invalid scopes reached storage %d times", repository.calls)
	}
}

func TestListInstancesUsesStablePageWindowAndScopePredicate(t *testing.T) {
	repository := &consumerRepositoryFake{total: 7}
	a := newConsumerQuery(repository, nil)
	if _, err := a.ListInstances(context.Background(), ListAttachmentConsumers{TenantID: consumerTenant, VPCID: consumerVPC, Page: pointer(uint32(3)), PageSize: pointer(uint32(25))}); err != nil {
		t.Fatal(err)
	}
	if repository.filter.Limit != 25 || repository.filter.Offset != 50 || repository.filter.VPCID != consumerVPC || repository.filter.SubnetID != "" {
		t.Fatalf("filter = %+v", repository.filter)
	}
	if repository.tenant != consumerTenant || repository.freshness != time.Minute {
		t.Fatalf("tenant = %q freshness = %v", repository.tenant, repository.freshness)
	}
}

func TestListInstancesDefaultsToFirstPage(t *testing.T) {
	repository := &consumerRepositoryFake{}
	a := newConsumerQuery(repository, nil)
	if _, err := a.ListInstances(context.Background(), ListAttachmentConsumers{TenantID: consumerTenant, SubnetID: consumerSubnet}); err != nil {
		t.Fatal(err)
	}
	if repository.filter.Limit != 20 || repository.filter.Offset != 0 {
		t.Fatalf("filter = %+v", repository.filter)
	}
}

func TestListInstancesKeepsRowsAndNormalizesStates(t *testing.T) {
	observed := time.Date(2026, 11, 3, 7, 20, 11, 0, time.UTC)
	container := AttachmentConsumerRow{
		TenantID: consumerTenant, AttachmentID: "att_" + strings.Repeat("a", 32), VPCID: consumerVPC, SubnetID: consumerSubnet, SubnetName: "test",
		Slot: "primary", ConsumerType: ConsumerContainerInstance, ConsumerID: containerOne, State: "attached",
		ObservedAt: observed, CreatedAt: observed.Add(-time.Hour), HasObservation: true,
		Addresses: []AttachmentAddress{{NICName: "default", Address: "10.42.1.15"}},
	}
	vm := AttachmentConsumerRow{
		AttachmentID: "att_" + strings.Repeat("b", 32), VPCID: consumerVPC, SubnetID: consumerSubnet, SubnetName: "test",
		Slot: "primary", ConsumerType: ConsumerVirtualMachine, ConsumerID: virtualMachineID, State: "reserved",
	}
	legacy := AttachmentConsumerRow{
		AttachmentID: "att_" + strings.Repeat("c", 32), VPCID: consumerVPC, SubnetID: consumerSubnet, SubnetName: "old",
		Slot: "primary", ConsumerID: containerTwo, State: "attached", ObservationStale: true, ObservedAt: observed, HasObservation: true,
	}
	blocked := AttachmentConsumerRow{
		AttachmentID: "att_" + strings.Repeat("d", 32), VPCID: consumerVPC, SubnetID: consumerSubnet,
		Slot: "primary", ConsumerType: ConsumerContainerInstance, ConsumerID: containerOne, State: "released",
		Reason: "ATTACHMENT_PROTOCOL_VIOLATION", ProtocolBlocked: true,
	}
	gone := AttachmentConsumerRow{
		AttachmentID: "att_" + strings.Repeat("e", 32), VPCID: consumerVPC, SubnetID: consumerSubnet,
		Slot: "primary", ConsumerType: ConsumerVirtualMachine, ConsumerID: "6f0c9a2e-7d1b-4c58-a6e0-f1b2c3d4e5f9", State: "releasing", Reason: "CONSUMER_NOT_CLOSED",
	}
	repository := &consumerRepositoryFake{rows: []AttachmentConsumerRow{container, vm, legacy, blocked, gone}, total: 5}
	directory := &consumerDirectoryFake{values: map[string]ConsumerInfo{
		ConsumerContainerInstance + "/" + containerOne:  {ID: containerOne, Name: "demo-1", State: "running"},
		ConsumerVirtualMachine + "/" + virtualMachineID: {ID: virtualMachineID, Name: "vm-1", State: "starting"},
		ConsumerContainerInstance + "/" + containerTwo:  {ID: containerTwo, Name: "legacy-gateway", State: "running"},
	}}
	a := newConsumerQuery(repository, directory)
	page, err := a.ListInstances(context.Background(), ListAttachmentConsumers{TenantID: consumerTenant, VPCID: consumerVPC})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 5 || len(page.Items) != 5 {
		t.Fatalf("total = %d items = %d", page.Total, len(page.Items))
	}
	first := page.Items[0]
	if first.TenantID != consumerTenant || first.ConsumerName != "demo-1" || first.ConsumerState != "running" || first.SubnetName != "test" || first.AttachmentState != "attached" || first.ObservationStale {
		t.Fatalf("attached row = %+v", first)
	}
	if len(first.Addresses) != 1 || first.Addresses[0].NICName != "default" || first.Addresses[0].Address != "10.42.1.15" {
		t.Fatalf("addresses = %+v", first.Addresses)
	}
	if first.ObservedAt == nil || !first.ObservedAt.Equal(observed) {
		t.Fatalf("observed_at = %v", first.ObservedAt)
	}
	if second := page.Items[1]; second.ConsumerState != "provisioning" || second.AttachmentState != "reserved" || len(second.Addresses) != 0 || second.ObservedAt != nil {
		t.Fatalf("starting VM row = %+v", second)
	}
	// A legacy row keeps its attachment but never borrows a type it did not record.
	if third := page.Items[2]; third.ConsumerType != ConsumerUnknown || third.ConsumerState != ConsumerUnknown || third.ConsumerName != "" || !third.ObservationStale {
		t.Fatalf("untyped legacy row = %+v", third)
	}
	if fourth := page.Items[3]; fourth.AttachmentState != "released" || fourth.AttachmentReason != "ATTACHMENT_PROTOCOL_VIOLATION" || !fourth.ProtocolBlocked {
		t.Fatalf("blocked release row = %+v", fourth)
	}
	// The deleted machine keeps its releasing row with unknown identity.
	if fifth := page.Items[4]; fifth.ConsumerState != ConsumerUnknown || fifth.ConsumerName != "" || fifth.AttachmentReason != "CONSUMER_NOT_CLOSED" {
		t.Fatalf("missing resource row = %+v", fifth)
	}
	if len(directory.calls) != 2 {
		t.Fatalf("expected one batch per consumer type, got %v", directory.calls)
	}
	containerCall, vmCall := "", ""
	for _, call := range directory.calls {
		switch {
		case strings.HasPrefix(call, ConsumerContainerInstance+"|"):
			containerCall = call
		case strings.HasPrefix(call, ConsumerVirtualMachine+"|"):
			vmCall = call
		}
	}
	// The untyped legacy row must not drag its identity into a container read.
	if want := ConsumerContainerInstance + "|" + consumerTenant + "|" + containerOne; containerCall != want {
		t.Fatalf("container batch = %q, want %q", containerCall, want)
	}
	if !strings.Contains(vmCall, virtualMachineID) || !strings.Contains(vmCall, "6f0c9a2e-7d1b-4c58-a6e0-f1b2c3d4e5f9") {
		t.Fatalf("virtual machine batch = %q", vmCall)
	}
}

func TestListInstancesResolvesContainerIDStoredWithoutHyphens(t *testing.T) {
	repository := &consumerRepositoryFake{rows: []AttachmentConsumerRow{{
		AttachmentID: "att_" + strings.Repeat("a", 32), ConsumerType: ConsumerContainerInstance,
		ConsumerID: "3f0c9a2e7d1b4c58a6e0f1b2c3d4e5f6", State: "attached",
	}}, total: 1}
	directory := &consumerDirectoryFake{values: map[string]ConsumerInfo{
		ConsumerContainerInstance + "/" + containerOne: {ID: containerOne, Name: "demo-2", State: "stopped"},
	}}
	page, err := newConsumerQuery(repository, directory).ListInstances(context.Background(), ListAttachmentConsumers{TenantID: consumerTenant, SubnetID: consumerSubnet})
	if err != nil {
		t.Fatal(err)
	}
	if page.Items[0].ConsumerID != containerOne || page.Items[0].ConsumerName != "demo-2" || page.Items[0].ConsumerState != "stopped" {
		t.Fatalf("item = %+v", page.Items[0])
	}
}

func TestListInstancesWithoutDirectoryKeepsEveryRow(t *testing.T) {
	repository := &consumerRepositoryFake{rows: []AttachmentConsumerRow{{
		AttachmentID: "att_" + strings.Repeat("a", 32), ConsumerType: ConsumerVirtualMachine, ConsumerID: virtualMachineID, State: "attached",
	}}, total: 1}
	page, err := newConsumerQuery(repository, nil).ListInstances(context.Background(), ListAttachmentConsumers{TenantID: consumerTenant, VPCID: consumerVPC})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ConsumerState != ConsumerUnknown || page.Items[0].ConsumerType != ConsumerVirtualMachine {
		t.Fatalf("item = %+v", page.Items[0])
	}
}

func TestListInstancesPropagatesScopeFailure(t *testing.T) {
	repository := &consumerRepositoryFake{err: Fail(ResourceNotFound, "vpc not found")}
	if _, err := newConsumerQuery(repository, nil).ListInstances(context.Background(), ListAttachmentConsumers{TenantID: consumerTenant, VPCID: consumerVPC}); ReasonOf(err) != ResourceNotFound {
		t.Fatalf("err = %v", err)
	}
}

func TestListInstancesFailsClosedWhenConsumerReadFails(t *testing.T) {
	repository := &consumerRepositoryFake{rows: []AttachmentConsumerRow{{ConsumerType: ConsumerContainerInstance, ConsumerID: containerOne, State: "attached"}}, total: 1}
	directory := &consumerDirectoryFake{err: Fail(DependencyUnavailable, "instance database unavailable")}
	if _, err := newConsumerQuery(repository, directory).ListInstances(context.Background(), ListAttachmentConsumers{TenantID: consumerTenant, VPCID: consumerVPC}); ReasonOf(err) != DependencyUnavailable {
		t.Fatalf("err = %v", err)
	}
}

func TestConsumerDirectorySkipsUnknownType(t *testing.T) {
	reader := &consumerReaderFake{}
	directory := NewConsumerDirectory(reader, nil)
	if _, err := directory.Describe(context.Background(), ConsumerUnknown, consumerTenant, []string{containerOne}); err != nil {
		t.Fatal(err)
	}
	if len(reader.calls) != 0 {
		t.Fatalf("unknown type queried a resource table: %v", reader.calls)
	}
	if _, err := directory.Describe(context.Background(), ConsumerVirtualMachine, consumerTenant, []string{virtualMachineID}); err != nil {
		t.Fatalf("unconfigured module must resolve to unknown, got %v", err)
	}
	if got, err := directory.Describe(context.Background(), ConsumerContainerInstance, consumerTenant, []string{containerOne}); err != nil || len(got) != 0 {
		t.Fatalf("reader batch = %v err = %v", got, err)
	}
	if len(reader.calls) != 1 || reader.calls[0] != containerOne {
		t.Fatalf("reader calls = %v", reader.calls)
	}
}

// consumerReaderFake is one domain's batch reader, used to check the router.
type consumerReaderFake struct{ calls []string }

func (f *consumerReaderFake) DescribeConsumers(_ context.Context, _ string, ids []string) (map[string]ConsumerInfo, error) {
	f.calls = append(f.calls, strings.Join(ids, ","))
	return map[string]ConsumerInfo{}, nil
}

// ConsumerType must not change the intent digest, or every stored attachment
// would reject its own replay after the column ships.
func TestPrepareFingerprintIgnoresConsumerType(t *testing.T) {
	base := PrepareAttachment{TenantID: consumerTenant, InstanceID: "instance-1", SubnetID: consumerSubnet, Slot: "primary", RequestKey: "key-1", SubmissionID: "22222222-2222-4222-8222-222222222222", Generation: 1, ClusterID: consumerTenant, Namespace: "tenant-1", WorkloadMode: "controller_managed", ConsumerType: ConsumerContainerInstance}
	untyped := base
	untyped.ConsumerType = ""
	if base.Fingerprint() != untyped.Fingerprint() {
		t.Fatal("consumer type changed the attachment intent digest")
	}
	other := base
	other.SubnetID = "subnet_" + strings.Repeat("f", 32)
	if base.Fingerprint() == other.Fingerprint() {
		t.Fatal("subnet change did not alter the intent digest")
	}
}

func TestPrepareValidatesConsumerTypeWithWorkloadMode(t *testing.T) {
	valid := PrepareAttachment{TenantID: consumerTenant, InstanceID: "instance-1", SubnetID: consumerSubnet, Slot: "primary", RequestKey: "key-1", SubmissionID: "22222222-2222-4222-8222-222222222222", Generation: 1, ClusterID: consumerTenant, Namespace: "tenant-1", WorkloadMode: "controller_managed", ConsumerType: ConsumerContainerInstance}
	repository := &consumerRepositoryFake{}
	a := NewAttachments(repository, time.Minute)
	if _, err := a.Prepare(context.Background(), valid); err != nil {
		t.Fatalf("controller managed prepare rejected: %v", err)
	}
	if repository.prepared.ConsumerType != ConsumerContainerInstance {
		t.Fatalf("repository received %q", repository.prepared.ConsumerType)
	}
	for name, mutate := range map[string]func(*PrepareAttachment){
		"missing type":         func(p *PrepareAttachment) { p.ConsumerType = "" },
		"virtual machine":      func(p *PrepareAttachment) { p.ConsumerType = ConsumerVirtualMachine },
		"sandbox":              func(p *PrepareAttachment) { p.ConsumerType = ConsumerSandbox },
		"invented type":        func(p *PrepareAttachment) { p.ConsumerType = "notebook" },
		"external single type": func(p *PrepareAttachment) { p.WorkloadMode = ""; p.ConsumerType = ConsumerContainerInstance },
	} {
		t.Run(name, func(t *testing.T) {
			r := valid
			mutate(&r)
			if _, err := a.Prepare(context.Background(), r); ReasonOf(err) != InvalidArgument {
				t.Fatalf("want InvalidArgument, got %v", err)
			}
		})
	}
	// The immutable v1 intent carries no product identity and must still work.
	external := valid
	external.WorkloadMode = ""
	external.ConsumerType = ""
	if _, err := a.Prepare(context.Background(), external); err != nil {
		t.Fatalf("external single_pod prepare rejected: %v", err)
	}
}

func TestValidConsumerIntentMatrix(t *testing.T) {
	cases := []struct {
		mode, consumer string
		ok             bool
	}{
		{"", "", true},
		{"", ConsumerContainerInstance, false},
		{"", ConsumerVirtualMachine, false},
		{"", ConsumerSandbox, false},
		{"controller_managed", ConsumerContainerInstance, true},
		{"controller_managed", ConsumerVirtualMachine, false},
		{"controller_managed", "", false},
		{"controller_managed", ConsumerSandbox, false},
		{"vm_managed", ConsumerVirtualMachine, true},
		{"vm_managed", ConsumerContainerInstance, false},
		{"vm_managed", "", false},
	}
	for _, c := range cases {
		if err := validConsumerIntent(c.mode, c.consumer); (err == nil) != c.ok {
			t.Fatalf("mode=%q consumer=%q want ok=%v got %v", c.mode, c.consumer, c.ok, err)
		}
	}
}

func TestConsumerDisplayStateCoversBothVocabularies(t *testing.T) {
	for raw, want := range map[string]string{
		"running": "running", "stopped": "stopped", "succeeded": "stopped",
		"pending": "provisioning", "starting": "provisioning", "provisioning": "provisioning",
		"deleting": "deleting", "deleted": "deleting", "stopping": "deleting",
		"failed": "failed", "": ConsumerUnknown, "scheduled": ConsumerUnknown, "RUNNING": ConsumerUnknown,
	} {
		if got := consumerDisplayState(raw); got != want {
			t.Fatalf("raw %q -> %q, want %q", raw, got, want)
		}
	}
}

func pointer[T any](value T) *T { return &value }
