package sandboxadmission

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"go.yaml.in/yaml/v3"
)

type expression struct {
	Name       string `yaml:"name"`
	Expression string `yaml:"expression"`
}

type policyDocument struct {
	Kind     string
	Metadata struct{ Name string }
	Spec     struct {
		FailurePolicy     string       `yaml:"failurePolicy"`
		PolicyName        string       `yaml:"policyName"`
		ValidationActions []string     `yaml:"validationActions"`
		MatchConditions   []expression `yaml:"matchConditions"`
		Variables         []expression
		Validations       []expression
	}
}

func readPolicies(t *testing.T) map[string]policyDocument {
	t.Helper()
	file, err := os.Open("tenant-provisioning-admission.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	policies := map[string]policyDocument{}
	bindings := map[string]bool{}
	decoder := yaml.NewDecoder(file)
	for {
		var doc policyDocument
		if err := decoder.Decode(&doc); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if doc.Metadata.Name != "ani-sandbox-tenant-role" && doc.Metadata.Name != "ani-sandbox-tenant-binding" {
			continue
		}
		if doc.Kind == "ValidatingAdmissionPolicy" {
			if doc.Spec.FailurePolicy != "Fail" {
				t.Fatal("tenant policy must fail closed")
			}
			policies[doc.Metadata.Name] = doc
		} else if doc.Kind == "ValidatingAdmissionPolicyBinding" {
			bindings[doc.Spec.PolicyName] = len(doc.Spec.ValidationActions) == 1 && doc.Spec.ValidationActions[0] == "Deny"
		}
	}
	if len(policies) != 2 || !bindings["ani-sandbox-tenant-role"] || !bindings["ani-sandbox-tenant-binding"] {
		t.Fatal("both tenant policies and Deny bindings are required")
	}
	return policies
}

// Evaluate the expressions from the shipped manifest, including its actual
// variable declarations. Errors deny just as failurePolicy: Fail does.
func permits(t *testing.T, policy policyDocument, object, namespace map[string]any, actor, operation string) bool {
	t.Helper()
	env, err := cel.NewEnv(
		cel.Variable("object", cel.DynType),
		cel.Variable("oldObject", cel.DynType),
		cel.Variable("namespaceObject", cel.DynType),
		cel.Variable("request", cel.DynType),
		cel.Variable("variables", cel.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	var ns any
	nsName := "platform"
	if namespace != nil {
		ns = namespace
		nsName = namespace["metadata"].(map[string]any)["name"].(string)
	}
	variables := map[string]any{}
	activation := map[string]any{
		"object": object, "oldObject": object, "namespaceObject": ns,
		"request":   map[string]any{"namespace": nsName, "operation": operation, "userInfo": map[string]any{"username": actor}},
		"variables": variables,
	}
	evaluate := func(source string) (any, error) {
		ast, issues := env.Compile(source)
		if issues != nil && issues.Err() != nil {
			t.Fatalf("manifest CEL does not compile: %v", issues.Err())
		}
		program, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		value, _, err := program.Eval(activation)
		if err != nil {
			return nil, err
		}
		return value.Value(), nil
	}
	for _, condition := range policy.Spec.MatchConditions {
		value, err := evaluate(condition.Expression)
		if err != nil {
			return false
		}
		if value == false {
			return true
		}
		if value != true {
			return false
		}
	}
	for _, variable := range policy.Spec.Variables {
		value, err := evaluate(variable.Expression)
		if err != nil {
			return false
		}
		variables[variable.Name] = value
	}
	for _, validation := range policy.Spec.Validations {
		value, err := evaluate(validation.Expression)
		if err != nil || value != true {
			return false
		}
	}
	return true
}

func copyObject(t *testing.T, object map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPlatformRBACAndTenantIsolation(t *testing.T) {
	policies := readPolicies(t)
	const tenantID = "11111111-1111-4111-8111-111111111111"
	const uid = "22222222-2222-4222-8222-222222222222"
	const actor = "system:serviceaccount:ani-system:ani-resource-service"
	plain := map[string]any{"metadata": map[string]any{"name": "platform"}}
	tenant := map[string]any{"metadata": map[string]any{
		"name":        "tenant-" + tenantID,
		"labels":      map[string]any{"network.ani.io/managed-by": "ani-network-service", "network.ani.io/tenant-id": tenantID},
		"annotations": map[string]any{"sandbox.ani.io/namespace-uid": uid},
	}}
	role := map[string]any{
		"metadata": map[string]any{"name": "platform-reader"},
		"rules":    []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"pods"}, "verbs": []any{"get"}}},
	}
	binding := map[string]any{
		"metadata": map[string]any{"name": "platform-reader"},
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "platform-reader"},
		"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": "reader", "namespace": "platform"}},
	}
	pullRole := copyObject(t, role)
	pullRole["metadata"] = map[string]any{"name": "ani-sandbox-pull-secret", "annotations": map[string]any{"sandbox.ani.io/namespace-uid": uid}}
	pullRole["rules"] = []any{map[string]any{"apiGroups": []any{""}, "resources": []any{"secrets"}, "verbs": []any{"get", "update"}, "resourceNames": []any{"ani-sandbox-pull-" + tenantID}}}
	tenantBinding := copyObject(t, binding)
	tenantBinding["metadata"] = map[string]any{"name": "ani-sandbox-tenant", "annotations": map[string]any{"sandbox.ani.io/namespace-uid": uid}}
	tenantBinding["roleRef"] = map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "ani-sandbox-tenant"}
	tenantBinding["subjects"] = []any{map[string]any{"kind": "ServiceAccount", "name": "ani-resource-service", "namespace": "ani-system"}}
	pullBinding := copyObject(t, tenantBinding)
	pullBinding["metadata"].(map[string]any)["name"] = "ani-sandbox-pull-secret"
	pullBinding["roleRef"] = map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "ani-sandbox-pull-secret"}

	type testCase struct {
		name, policy string
		object, ns   map[string]any
		allow        bool
	}
	cases := []testCase{
		{"ordinary Role", "role", role, plain, true},
		{"ordinary RoleBinding", "binding", binding, plain, true},
		{"Sandbox pull Role", "role", pullRole, tenant, true},
		{"Sandbox tenant binding", "binding", tenantBinding, tenant, true},
		{"Sandbox pull binding", "binding", pullBinding, tenant, true},
		{"generic tenant Role", "role", role, tenant, false},
		{"generic tenant binding", "binding", binding, tenant, false},
		{"reserved Role outside tenant", "role", pullRole, plain, false},
		{"reserved binding outside tenant", "binding", tenantBinding, plain, false},
		{"missing namespace Role", "role", role, nil, false},
		{"missing namespace binding", "binding", binding, nil, false},
	}
	for _, kind := range []string{"role", "binding"} {
		var valid map[string]any
		if kind == "role" {
			valid = pullRole
		} else {
			valid = tenantBinding
		}
		for _, mode := range []string{"missing UID", "wrong UID", "owner ref", "finalizer"} {
			obj := copyObject(t, valid)
			meta := obj["metadata"].(map[string]any)
			switch mode {
			case "missing UID":
				delete(meta, "annotations")
			case "wrong UID":
				meta["annotations"] = map[string]any{"sandbox.ani.io/namespace-uid": "other"}
			case "owner ref":
				meta["ownerReferences"] = []any{map[string]any{"uid": "foreign"}}
			case "finalizer":
				meta["finalizers"] = []any{"foreign"}
			}
			cases = append(cases, testCase{mode + " " + kind, kind, obj, tenant, false})
		}
		for _, marker := range []string{"tenant ID only", "UID only", "managed-by only"} {
			ns := copyObject(t, plain)
			meta := ns["metadata"].(map[string]any)
			switch marker {
			case "tenant ID only":
				meta["labels"] = map[string]any{"network.ani.io/tenant-id": tenantID}
			case "UID only":
				meta["annotations"] = map[string]any{"sandbox.ani.io/namespace-uid": uid}
			case "managed-by only":
				meta["labels"] = map[string]any{"network.ani.io/managed-by": "ani-network-service"}
			}
			cases = append(cases, testCase{marker + " " + kind, kind, valid, ns, false})
		}
	}
	widened := copyObject(t, pullRole)
	widened["rules"].([]any)[0].(map[string]any)["verbs"] = []any{"get", "update", "list"}
	cases = append(cases, testCase{"expanded tenant Role", "role", widened, tenant, false})
	for _, mode := range []string{"foreign subject", "extra subject", "foreign roleRef", "reserved roleRef"} {
		obj := copyObject(t, tenantBinding)
		ns := tenant
		switch mode {
		case "foreign subject":
			obj["subjects"].([]any)[0].(map[string]any)["namespace"] = "another-tenant"
		case "extra subject":
			obj["subjects"] = append(obj["subjects"].([]any), map[string]any{"kind": "User", "name": "other"})
		case "foreign roleRef":
			obj["roleRef"].(map[string]any)["name"] = "cluster-admin"
		case "reserved roleRef":
			obj = copyObject(t, binding)
			obj["roleRef"].(map[string]any)["name"] = "ani-sandbox-tenant"
			ns = plain
		}
		cases = append(cases, testCase{mode, "binding", obj, ns, false})
	}
	for _, tc := range cases {
		for _, operation := range []string{"CREATE", "UPDATE"} {
			t.Run(strings.ReplaceAll(tc.name+"/"+operation, " ", "_"), func(t *testing.T) {
				if got := permits(t, policies["ani-sandbox-tenant-"+tc.policy], tc.object, tc.ns, actor, operation); got != tc.allow {
					t.Fatalf("allow=%v want=%v", got, tc.allow)
				}
			})
		}
	}
}
