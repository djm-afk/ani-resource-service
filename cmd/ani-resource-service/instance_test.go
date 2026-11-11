package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestInstanceRetiredSettingsRejectedAndQuotaMTLSRequired(t *testing.T) {
	cfg := instanceSettings{
		DatabaseDSNFile: "/managed/instance-runtime.dsn",
		QuotaAddress:    "governance.internal:9445",
		QuotaCAFile:     "/managed/quota-ca.pem",
		QuotaCertFile:   "/managed/resource-client.pem",
		QuotaKeyFile:    "/managed/resource-client.key",
		QuotaServerName: "ani-governance",
	}
	if err := validateInstanceSettings(cfg); err != nil {
		t.Fatalf("owner refund configuration without ACC release settings rejected: %v", err)
	}
	legacy := []byte(`{"ReleaseAddress":"unavailable.invalid:1","ReleaseCAFile":"missing-ca","ReleaseCertFile":"missing-cert","ReleaseKeyFile":"missing-key","ReleaseServerName":"*"}`)
	decoder := json.NewDecoder(bytes.NewReader(legacy))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err == nil {
		t.Fatal("retired release settings accepted")
	}

	for _, tc := range []struct {
		name   string
		change func(*instanceSettings)
	}{
		{"missing quota CA", func(c *instanceSettings) { c.QuotaCAFile = "" }},
		{"missing quota client identity", func(c *instanceSettings) { c.QuotaCertFile = "" }},
		{"missing quota private key", func(c *instanceSettings) { c.QuotaKeyFile = "" }},
		{"missing quota address", func(c *instanceSettings) { c.QuotaAddress = "" }},
		{"wildcard quota identity", func(c *instanceSettings) { c.QuotaServerName = "*.internal" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := cfg
			tc.change(&invalid)
			if err := validateInstanceSettings(invalid); err == nil {
				t.Fatal("required owner signing or Gov mTLS configuration accepted")
			}
		})
	}
}
