package vm

import (
	"strings"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
)

// The count and page queries share one argument slice, so every placeholder the
// builder emits must exist in that slice regardless of which filters are set.
func TestVMListWhereKeepsPlaceholdersAndArgumentsInSync(t *testing.T) {
	tenant := "a0000000-0000-4000-8000-000000000001"
	cases := []struct {
		name   string
		filter biz.ListFilter
	}{
		{"default", biz.ListFilter{Page: 1, PageSize: 20}},
		{"state", biz.ListFilter{Page: 1, PageSize: 20, State: "running"}},
		{"keyword", biz.ListFilter{Page: 1, PageSize: 20, Keyword: "vm-linux"}},
		{"candidates", biz.ListFilter{SubnetID: "subnet-a", ClusterID: "cluster-a", Namespace: "tenant-a", BindableOnly: true}},
		{"both", biz.ListFilter{Page: 1, PageSize: 20, State: "deleted", Keyword: "vm"}},
	}
	for _, c := range cases {
		where, args := vmListWhere(tenant, c.filter)
		if len(args) != 7 || args[0] != tenant || args[1] != c.filter.State || args[2] != c.filter.Keyword || args[3] != c.filter.SubnetID || args[4] != c.filter.ClusterID || args[5] != c.filter.Namespace || args[6] != c.filter.BindableOnly {
			t.Fatalf("%s: arguments drifted: %#v", c.name, args)
		}
		for _, want := range []string{"tenant_id=$1", "$2::text", "$3::text", "id::text", "current_configuration->>'Name'", "'deleted'"} {
			if !strings.Contains(where, want) {
				t.Fatalf("%s: where is missing %q: %s", c.name, want, where)
			}
		}
		if strings.Contains(where, "$8") {
			t.Fatalf("%s: where must not use a page placeholder: %s", c.name, where)
		}
	}
}

// The display-state classifier must keep every stored state in exactly one group.
func TestVMDisplayStateCoversEveryStoredState(t *testing.T) {
	for _, want := range []string{"deleted", "deleting", "failed", "running", "stopped", "stopping", "starting", "provisioning"} {
		if !strings.Contains(vmDisplayState, "THEN '"+want+"'") {
			t.Fatalf("display state %q is not classified: %s", want, vmDisplayState)
		}
	}
	if !strings.Contains(vmDisplayState, "ELSE 'pending'") {
		t.Fatalf("unclassified rows must fall back to pending: %s", vmDisplayState)
	}
}
