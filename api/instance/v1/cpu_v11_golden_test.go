package instancev1

import (
	"bytes"
	"strings"
	"testing"
)

// Values captured from main 20f33f0 before this GPU contract change.
func TestCPUOriginalPayloadAndCommandRemainReplayable(t *testing.T) {
	s := currentBusiness()
	raw, err := CanonicalBusinessPayload(s)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := BusinessPayloadDigest(s)
	if err != nil || digest != "ef4b7f3e44fd32717a9a9f781401f1534fafcc8ffea3ec870dcd862c3867d019" {
		t.Fatal("CPU bytes/digest changed", digest, err)
	}
	command, err := CommandDigest(&CommandContext{TenantId: "tenant", ResourceId: "resource", OperationId: "operation"}, s)
	if err != nil || command != "e365aae2f12413cae5eec3ebf0c7a282d4bb395fe242bc2f99ec0cbdf5a53807" {
		t.Fatal("CPU command identity changed", command, err)
	}
	restored, err := DecodeBusinessPayload(raw)
	if err != nil {
		t.Fatal("original CPU payload rejected", err)
	}
	again, err := CanonicalBusinessPayload(restored)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatal("original CPU payload changed after replay", err)
	}
	for _, key := range []string{"gpu_display", "gpu_execution", "gpu_selection"} {
		invalid := bytes.Replace(raw, []byte(`"`+key+`":null`), []byte(`"`+key+`":{}`), 1)
		if _, err = DecodeBusinessPayload(invalid); err == nil {
			t.Fatal("CPU carried a GPU object", key)
		}
	}
	for _, value := range []string{"null", "{}"} {
		invalid := []byte(strings.TrimSuffix(string(raw), "}") + `,"gpu_business_spec":` + value + "}")
		if _, err = DecodeBusinessPayload(invalid); err == nil {
			t.Fatal("CPU accepted the new GPU key")
		}
	}
}
