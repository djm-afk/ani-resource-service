package biz

import "testing"

func TestReleasedAttachmentIsTerminalWithoutConsumerOrProviderAudit(t *testing.T) {
	a, _ := managedSnapshotFixture()
	a.State = Released
	consumer := &managedConsumer{}
	reader := &managedAddressReader{}
	got := runCurrentAttachment(t, a, consumer, reader)
	if got.State != Released || consumer.calls != 0 || reader.calls != 0 || got.Observed {
		t.Fatal(got, consumer.calls, reader.calls)
	}
}
