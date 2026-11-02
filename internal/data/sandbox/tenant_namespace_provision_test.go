package sandbox

import (
	"context"
	"errors"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/tenantnamespace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

type namespaceJournal struct {
	biz.TenantSpaceJournal
	uid, pending string
	writes       int
}

func (j *namespaceJournal) PinTenantSpaceNamespace(_ context.Context, _, _, uid string) (biz.TenantSpace, error) {
	if j.pending != "" {
		return biz.TenantSpace{}, biz.ErrConflict
	}
	if j.uid != "" && j.uid != uid {
		return biz.TenantSpace{}, biz.ErrOwnership
	}
	j.uid = uid
	return biz.TenantSpace{NamespaceUID: uid}, nil
}
func (j *namespaceJournal) BeginTenantSpaceMutation(_ context.Context, _, _, id, kind string) (biz.TenantSpaceMutation, error) {
	if j.pending != "" || j.uid != "" {
		return biz.TenantSpaceMutation{}, biz.ErrConflict
	}
	j.pending = id
	j.writes++
	return biz.TenantSpaceMutation{ID: id, Kind: kind, Dispatch: true}, nil
}
func (j *namespaceJournal) FinishTenantSpaceMutation(_ context.Context, _, _, id, outcome, receipt string) (biz.TenantSpaceMutation, error) {
	if j.pending != id {
		return biz.TenantSpaceMutation{}, biz.ErrConflict
	}
	j.pending = ""
	if outcome == "applied" {
		j.uid = receipt
	}
	return biz.TenantSpaceMutation{ID: id, Outcome: outcome, Receipt: receipt}, nil
}

func TestProvisionTenantNamespaceReusesOrCreatesStandardName(t *testing.T) {
	const operation = "22222222-2222-4222-8222-222222222222"
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "existing"}[existing], func(t *testing.T) {
			client := fake.NewSimpleDynamicClient(runtime.NewScheme())
			if existing {
				if err := client.Tracker().Add(ownedNamespace("owned-uid")); err != nil {
					t.Fatal(err)
				}
			}
			client.PrependReactor("create", "namespaces", func(action ktesting.Action) (bool, runtime.Object, error) {
				obj := action.(ktesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
				if obj.GetName() != "tenant-"+namespaceTenant || len(obj.GetName()) != 43 {
					t.Fatalf("namespace name: %q", obj.GetName())
				}
				obj.SetUID("created-uid")
				obj.Object["status"] = map[string]any{"phase": "Pending"}
				if err := client.Tracker().Create(namespaceGVR, obj, ""); err != nil {
					t.Fatal(err)
				}
				return true, obj, nil
			})
			j := &namespaceJournal{}
			n := TenantNamespace{Client: client, NamespacePrefix: tenantnamespace.Prefix}
			uid, err := n.ProvisionJournaled(context.Background(), namespaceTenant, operation, j)
			if existing {
				if err != nil || uid != "owned-uid" || j.writes != 0 {
					t.Fatalf("reuse: uid=%q writes=%d err=%v", uid, j.writes, err)
				}
				return
			}
			if !errors.Is(err, biz.ErrDependencyUnavailable) || j.uid != "created-uid" || j.pending != "" || j.writes != 1 {
				t.Fatalf("pending namespace must retain receipt: uid=%q journal=%+v err=%v", uid, j, err)
			}
			obj, err := client.Resource(namespaceGVR).Get(context.Background(), "tenant-"+namespaceTenant, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			obj.Object["status"] = map[string]any{"phase": "Active"}
			if err = client.Tracker().Update(namespaceGVR, obj, ""); err != nil {
				t.Fatal(err)
			}
			uid, err = n.ProvisionJournaled(context.Background(), namespaceTenant, operation, j)
			if err != nil || uid != "created-uid" || j.writes != 1 {
				t.Fatalf("retry: uid=%q journal=%+v err=%v", uid, j, err)
			}
		})
	}
}

func TestProvisionTenantNamespaceAlreadyExistsRaceAndUnknown(t *testing.T) {
	const operation = "22222222-2222-4222-8222-222222222222"
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "already-exists", true: "unknown"}[unknown], func(t *testing.T) {
			client := fake.NewSimpleDynamicClient(runtime.NewScheme())
			j := &namespaceJournal{}
			client.PrependReactor("create", "namespaces", func(_ ktesting.Action) (bool, runtime.Object, error) {
				if err := client.Tracker().Add(ownedNamespace("other-uid")); err != nil {
					t.Fatal(err)
				}
				if unknown {
					return true, nil, errors.New("response lost")
				}
				return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "namespaces"}, "tenant-"+namespaceTenant)
			})
			n := TenantNamespace{Client: client, NamespacePrefix: tenantnamespace.Prefix}
			uid, err := n.ProvisionJournaled(context.Background(), namespaceTenant, operation, j)
			if !unknown {
				if err != nil || uid != "other-uid" || j.pending != "" || j.writes != 1 {
					t.Fatalf("race: %q %+v %v", uid, j, err)
				}
				return
			}
			if err == nil || j.pending == "" {
				t.Fatalf("unknown write must remain fenced: %+v %v", j, err)
			}
			if _, err = n.ProvisionJournaled(context.Background(), namespaceTenant, operation, j); !errors.Is(err, biz.ErrConflict) {
				t.Fatalf("unknown replay: %v", err)
			}
			if j.writes != 1 {
				t.Fatalf("reissued create %d", j.writes)
			}
		})
	}
}
