package biz

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"testing"
)

func TestSubnetCreationOptionsNormalizeAndFingerprint(t *testing.T) {
	r := CreateSubnet{TenantID: "7a7750cf-73b0-49c5-a3b1-4dba42689401", VPCID: "vpc_12345678901234567890123456789012", Name: "options", CIDR: "10.42.1.0/24", IdempotencyKey: "options"}
	legacy, err := NewSubnetIntent(r)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(struct {
		Version                                 int
		Name, CIDR, Description, VPCID, Gateway string
	}{1, legacy.Name, legacy.CIDR, legacy.Description, legacy.VPCID, legacy.Gateway})
	h := sha256.Sum256(b)
	if legacy.Fingerprint() != hex.EncodeToString(h[:]) {
		t.Fatal("default options changed previously accepted fingerprints")
	}
	r.SubnetOptions = SubnetOptions{ExcludeIPs: []string{"10.42.1.20", " 10.42.1.10 .. 10.42.1.19 ", "10.42.1.15"}, EnableDHCP: true, DNSServers: []string{" 10.96.0.10 ", "1.1.1.1", "10.96.0.10"}}
	custom, err := NewSubnetIntent(r)
	if err != nil || !slices.Equal(custom.ExcludeIPs, []string{"10.42.1.10..10.42.1.20"}) || !slices.Equal(custom.DNSServers, []string{"10.96.0.10", "1.1.1.1"}) {
		t.Fatal(custom, err)
	}
	r.ExcludeIPs = []string{"10.42.1.10..10.42.1.20"}
	r.DNSServers = []string{"10.96.0.10", "1.1.1.1"}
	equivalent, err := NewSubnetIntent(r)
	if err != nil || equivalent.Fingerprint() != custom.Fingerprint() {
		t.Fatal("equivalent exclusion sets changed intent", err)
	}
	r.DNSServers = []string{"1.1.1.1", "10.96.0.10"}
	reordered, _ := NewSubnetIntent(r)
	if reordered.Fingerprint() == custom.Fingerprint() {
		t.Fatal("DNS preference order was lost")
	}
	r.EnableDHCP = false
	stored, err := NewSubnetIntent(r)
	if err != nil || len(stored.DNSServers) != 2 {
		t.Fatal("DNS cannot be prepared while DHCP is disabled", err)
	}
}

func TestSubnetCreationOptionsRejectInvalidAddresses(t *testing.T) {
	for _, options := range []SubnetOptions{
		{ExcludeIPs: []string{"10.42.2.10"}}, {ExcludeIPs: []string{"10.42.1.20..10.42.1.10"}}, {ExcludeIPs: []string{"10.42.1.10..10.42.2.10"}}, {ExcludeIPs: []string{""}}, {ExcludeIPs: []string{"::1"}},
		{DNSServers: []string{"bad"}}, {DNSServers: []string{"::1"}}, {DNSServers: []string{"0.0.0.0"}}, {DNSServers: []string{"224.0.0.1"}},
	} {
		_, err := NewSubnetIntent(CreateSubnet{TenantID: "7a7750cf-73b0-49c5-a3b1-4dba42689401", VPCID: "vpc_12345678901234567890123456789012", Name: "options", CIDR: "10.42.1.0/24", IdempotencyKey: "options", SubnetOptions: options})
		if ReasonOf(err) != InvalidArgument {
			t.Fatalf("invalid options accepted: %+v error=%v", options, err)
		}
	}
}
