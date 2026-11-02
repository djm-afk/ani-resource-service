package tenantnamespace

import (
	"encoding/json"
	"os"
	"testing"
)

func TestPublicTenantNamespaceVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/tenant-namespace-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		InputCases []struct {
			ID, Input, ExpectedNamespace string
			Valid                        bool
		}
		LiveBaseline struct {
			TenantID, Name, UID, Phase string
			Labels                     map[string]string
		}
		LiveCases []struct {
			ID        string
			APIStatus int `json:"api_status"`
			Valid     bool
			FrozenUID string `json:"frozen_uid"`
			Overrides map[string]json.RawMessage
		}
	}
	// The public vector uses snake_case input field names.
	var contract map[string]json.RawMessage
	if err = json.Unmarshal(raw, &contract); err != nil {
		t.Fatal(err)
	}
	var inputs []struct {
		ID, Input         string
		Valid             bool
		ExpectedNamespace string `json:"expected_namespace"`
	}
	json.Unmarshal(contract["input_cases"], &inputs)
	for _, c := range inputs {
		t.Run(c.ID, func(t *testing.T) {
			name, err := Name(c.Input)
			if (err == nil) != c.Valid || (c.Valid && name != c.ExpectedNamespace) {
				t.Fatalf("name=%q error=%v", name, err)
			}
		})
	}
	json.Unmarshal(contract["live_baseline"], &vectors.LiveBaseline)
	var tenant string
	var baseline map[string]json.RawMessage
	json.Unmarshal(contract["live_baseline"], &baseline)
	json.Unmarshal(baseline["tenant_id"], &tenant)
	json.Unmarshal(contract["live_cases"], &vectors.LiveCases)
	for _, c := range vectors.LiveCases {
		if c.APIStatus != 0 {
			continue
		}
		t.Run(c.ID, func(t *testing.T) {
			o := Object{Name: vectors.LiveBaseline.Name, UID: vectors.LiveBaseline.UID, Phase: vectors.LiveBaseline.Phase, Labels: vectors.LiveBaseline.Labels}
			for key, v := range c.Overrides {
				switch key {
				case "name":
					json.Unmarshal(v, &o.Name)
				case "uid":
					json.Unmarshal(v, &o.UID)
				case "phase":
					json.Unmarshal(v, &o.Phase)
				case "labels":
					json.Unmarshal(v, &o.Labels)
				case "deletion_timestamp":
					o.Deleting = string(v) != "null"
				}
			}
			if err := Validate(tenant, o, c.FrozenUID, true); (err == nil) != c.Valid {
				t.Fatalf("validation=%v valid=%v", err, c.Valid)
			}
		})
	}
}
