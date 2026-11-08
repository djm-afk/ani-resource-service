package clusterops

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type sourceFunc func(context.Context, string) ([]string, error)

func (f sourceFunc) ListRegisteredNamespaceNames(ctx context.Context, c string) ([]string, error) {
	return f(ctx, c)
}
func source(names ...string) NamespaceSource {
	return sourceFunc(func(context.Context, string) ([]string, error) { return names, nil })
}
func admin() context.Context {
	return WithCaller(context.Background(), Caller{Actor: "governance:user:7", DirectCaller: "ani-governance", RequestID: "request", Platform: true})
}
func reason(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

type kubeStub struct {
	KubernetesPort
	descriptor ResourceDescriptor
	pages      func(context.Context, ListQuery) (ResourcePage, error)
	crd        CRDReadInfo
	crdErr     error
	crdReads   int
	deleted    *DeleteResult
	opened     OpenedPodLog
	writes     int
}

func (k *kubeStub) ResolveResourceType(_ context.Context, _ string, t ResourceTypeRef) (ResourceDescriptor, error) {
	d := k.descriptor
	d.Type = t
	if t.Resource == "namespaces" {
		d.Namespaced = false
	}
	return d, nil
}
func (k *kubeStub) ListResources(ctx context.Context, q ListQuery) (ResourcePage, error) {
	if k.pages != nil {
		return k.pages(ctx, q)
	}
	return ResourcePage{Raw: []RawResource{{Ref: ResourceRef{ClusterID: q.ClusterID, Type: q.Type, Namespace: q.Namespace, Name: "x"}, DisplayJSON: []byte("{}")}}}, nil
}
func (k *kubeStub) GetResource(_ context.Context, r ResourceRef) (RawResource, error) {
	return RawResource{Ref: r, DisplayJSON: []byte("{}")}, nil
}
func (k *kubeStub) CreateResource(_ context.Context, c CreateCommand) (RawResource, error) {
	k.writes++
	return RawResource{Ref: c.Ref}, nil
}
func (k *kubeStub) UpdateResource(_ context.Context, c UpdateCommand) (RawResource, error) {
	k.writes++
	return RawResource{Ref: c.Ref}, nil
}
func (k *kubeStub) DeleteResource(context.Context, ResourceRef, string) (DeleteResult, error) {
	k.writes++
	if k.deleted != nil {
		return *k.deleted, nil
	}
	return DeleteResult{DeletionRequested: true}, nil
}
func (k *kubeStub) ScaleWorkload(_ context.Context, c ScaleCommand) (ScaleResult, error) {
	k.writes++
	return ScaleResult{Ref: c.Ref}, nil
}
func (k *kubeStub) RestartWorkload(_ context.Context, c RestartCommand) (RawResource, error) {
	k.writes++
	return RawResource{Ref: c.Ref}, nil
}
func (k *kubeStub) GetCRDReadInfo(context.Context, ResourceRef) (CRDReadInfo, error) {
	k.crdReads++
	return k.crd, k.crdErr
}
func (k *kubeStub) OpenPodLogs(context.Context, PodLogQuery) (OpenedPodLog, error) {
	return k.opened, nil
}
func newCase(t *testing.T, k *kubeStub, sources ...NamespaceSource) *UseCase {
	t.Helper()
	if sources == nil {
		sources = []NamespaceSource{source(), source()}
	}
	u, err := NewUseCase("cluster", k, NewNamespacePolicy(sources...))
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func mutable() ResourceDescriptor {
	return ResourceDescriptor{Namespaced: true, Actions: []string{"list", "get", "create", "update", "delete", "scale", "restart"}}
}
func ref(ns string) ResourceRef {
	return ResourceRef{ClusterID: "cluster", Type: ResourceTypeRef{Group: "apps", Version: "v1", Resource: "deployments"}, Namespace: ns, Name: "x"}
}
func TestAllWritesProtectBothRegistrationsAndNamespaceBody(t *testing.T) {
	for _, ns := range []string{"network-only", "sandbox-only", "both"} {
		for _, dry := range []bool{false, true} {
			k := &kubeStub{descriptor: mutable()}
			u := newCase(t, k, source("network-only", "both"), source("sandbox-only", "both"))
			r := ref(ns)
			replicas := int32(0)
			actions := []func() error{
				func() error { _, e := u.CreateResource(admin(), CreateCommand{Ref: r, DryRun: dry}); return e },
				func() error {
					_, e := u.UpdateResource(admin(), UpdateCommand{Ref: r, ExpectedUID: "u", ExpectedResourceVersion: "v", DryRun: dry})
					return e
				},
				func() error { _, e := u.DeleteResource(admin(), DeleteCommand{Ref: r, ExpectedUID: "u"}); return e },
				func() error {
					_, e := u.ScaleWorkload(admin(), ScaleCommand{Ref: r, ExpectedUID: "u", ExpectedResourceVersion: "v", Replicas: &replicas, DryRun: dry})
					return e
				},
				func() error {
					_, e := u.RestartWorkload(admin(), RestartCommand{Ref: r, ExpectedUID: "u", ExpectedResourceVersion: "v", DryRun: dry})
					return e
				},
			}
			for i, f := range actions {
				if got := reason(f()); got != "TENANT_NAMESPACE_READ_ONLY" {
					t.Fatalf("%s action %d: %s", ns, i, got)
				}
			}
			r = ResourceRef{ClusterID: "cluster", Type: ResourceTypeRef{Version: "v1", Resource: "namespaces"}, Name: ns}
			for _, f := range actions[:3] {
				if got := reason(f()); got != "TENANT_NAMESPACE_READ_ONLY" {
					t.Fatal(got)
				}
			}
			if k.writes != 0 {
				t.Fatal("protected write reached Kubernetes")
			}
		}
	}
}
func TestPolicyFailureReadHintsRecoveryAndClusterIndependence(t *testing.T) {
	fail := true
	dependency := sourceFunc(func(context.Context, string) ([]string, error) {
		if fail {
			return nil, errors.New("private DSN")
		}
		return []string{"tenant"}, nil
	})
	k := &kubeStub{descriptor: mutable()}
	u := newCase(t, k, source(), dependency)
	doc, err := u.GetResource(admin(), GetQuery{Ref: ref("platform")})
	if err != nil || !doc.Resource.ReadOnly || doc.Resource.ReadOnlyReason != "NAMESPACE_POLICY_UNAVAILABLE" {
		t.Fatalf("%+v %v", doc, err)
	}
	if _, err = u.CreateResource(admin(), CreateCommand{Ref: ref("platform")}); reason(err) != "NAMESPACE_POLICY_UNAVAILABLE" {
		t.Fatal(err)
	}
	fail = false
	doc, err = u.GetResource(admin(), GetQuery{Ref: ref("platform")})
	if err != nil || doc.Resource.ReadOnly {
		t.Fatal(doc, err)
	}
	doc, err = u.GetResource(admin(), GetQuery{Ref: ref("tenant")})
	if err != nil || doc.Resource.ReadOnlyReason != "TENANT_NAMESPACE_READ_ONLY" {
		t.Fatal(doc, err)
	}
	fail = true
	k.descriptor.Namespaced = false
	r := ref("")
	r.Type.Resource = "clusterroles"
	r.Type.Group = "rbac.authorization.k8s.io"
	r.Name = "system:fixture"
	if _, err = u.CreateResource(admin(), CreateCommand{Ref: r}); err != nil {
		t.Fatal("unrelated cluster write depends on registrations", err)
	}
}
func TestRegistrationSharedBudgetAndParentCancellation(t *testing.T) {
	var deadline time.Time
	first := sourceFunc(func(ctx context.Context, _ string) ([]string, error) {
		deadline, _ = ctx.Deadline()
		return []string{"a"}, nil
	})
	second := sourceFunc(func(ctx context.Context, _ string) ([]string, error) {
		d, _ := ctx.Deadline()
		if d != deadline || time.Until(d) > NamespaceTimeout {
			t.Error("sources did not share 2s budget")
		}
		return []string{"b"}, nil
	})
	p := NewNamespacePolicy(first, second)
	names, err := p.ProtectedNamespaces(admin(), "cluster")
	if err != nil || len(names) != 2 {
		t.Fatal(names, err)
	}
	block := sourceFunc(func(ctx context.Context, _ string) ([]string, error) { <-ctx.Done(); return nil, ctx.Err() })
	start := time.Now()
	_, err = NewNamespacePolicy(block, second).ProtectedNamespaces(admin(), "cluster")
	if reason(err) != "NAMESPACE_POLICY_UNAVAILABLE" || time.Since(start) > 3*time.Second {
		t.Fatal(err, time.Since(start))
	}
	ctx, cancel := context.WithTimeout(admin(), 15*time.Millisecond)
	defer cancel()
	_, err = NewNamespacePolicy(block, second).ProtectedNamespaces(ctx, "cluster")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	for _, p := range []*NamespacePolicy{nil, NewNamespacePolicy(source()), NewNamespacePolicy(source(), nil), NewNamespacePolicy(source(""), source())} {
		if _, err := p.ProtectedNamespaces(admin(), "cluster"); reason(err) != "NAMESPACE_POLICY_UNAVAILABLE" {
			t.Fatal(err)
		}
	}
}
func TestReadCancellationAndInvalidAddressing(t *testing.T) {
	block := sourceFunc(func(ctx context.Context, _ string) ([]string, error) { <-ctx.Done(); return nil, ctx.Err() })
	k := &kubeStub{descriptor: mutable()}
	u := newCase(t, k, block, source())
	ctx, cancel := context.WithCancel(admin())
	cancel()
	if _, err := u.GetResource(ctx, GetQuery{Ref: ref("platform")}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, r := range []ResourceRef{{}, {ClusterID: "other", Type: ref("platform").Type, Name: "x", Namespace: "platform"}, {ClusterID: "cluster", Type: ResourceTypeRef{Version: "v1", Resource: "pods/log"}, Name: "x", Namespace: "platform"}} {
		if _, err := u.GetResource(admin(), GetQuery{Ref: r}); err == nil {
			t.Fatal("invalid target allowed", r)
		}
	}
	if _, err := u.ListClusters(context.Background()); reason(err) != "PLATFORM_SCOPE_REQUIRED" {
		t.Fatal(err)
	}
	if _, err := u.UpdateResource(context.Background(), UpdateCommand{}); reason(err) != "PLATFORM_SCOPE_REQUIRED" {
		t.Fatal(err)
	}
}
func TestCRDPreflightPagesAndDeadlineNeverDelete(t *testing.T) {
	r := ResourceRef{ClusterID: "cluster", Type: ResourceTypeRef{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}, Name: "widgets.test.io"}
	for _, mode := range []string{"protected", "page-failure", "canceled", "success", "cluster"} {
		t.Run(mode, func(t *testing.T) {
			k := &kubeStub{descriptor: mutable(), crd: CRDReadInfo{UID: "u", Scope: "Namespaced", Group: "test.io", Plural: "widgets", Versions: []CRDVersion{{Name: "v1", Served: true}, {Name: "v2", Served: true, Storage: true}}}}
			k.descriptor.Namespaced = false
			ctx, cancel := context.WithCancel(admin())
			defer cancel()
			calls := 0
			k.pages = func(c context.Context, q ListQuery) (ResourcePage, error) {
				calls++
				if q.PageSize != 200 || !q.AllNamespaces || q.Type.Version != "v2" {
					t.Error(q)
				}
				if calls == 1 {
					return ResourcePage{NextContinueToken: "next"}, nil
				}
				if mode == "page-failure" {
					return ResourcePage{}, errors.New("conversion unavailable")
				}
				if mode == "canceled" {
					cancel()
					return ResourcePage{}, nil
				}
				ns := "platform"
				if mode == "protected" {
					ns = "tenant"
				}
				return ResourcePage{Raw: []RawResource{{Ref: ResourceRef{Namespace: ns}}}}, nil
			}
			if mode == "cluster" {
				k.crd.Scope = "Cluster"
			}
			u := newCase(t, k, source("tenant"), source())
			_, err := u.DeleteResource(ctx, DeleteCommand{Ref: r, ExpectedUID: "u"})
			wantSuccess := mode == "success" || mode == "cluster"
			if (err == nil) != wantSuccess || k.writes != map[bool]int{true: 1, false: 0}[wantSuccess] {
				t.Fatal(err, k.writes)
			}
			if mode == "protected" && reason(err) != "CRD_HAS_PROTECTED_NAMESPACE_INSTANCES" {
				t.Fatal(err)
			}
		})
	}
}

func TestCRDDeletionAbsenceIsOnlyFromInitialGet(t *testing.T) {
	ref := ResourceRef{ClusterID: "cluster", Type: ResourceTypeRef{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}, Name: "widgets.test.io"}
	missing := NewError("RESOURCE_NOT_FOUND")
	for _, tc := range []struct {
		name        string
		getErr      error
		failPage    int
		pageErr     error
		deleteGone  bool
		wantErr     error
		wantAbsent  bool
		wantPages   int
		wantWrites  int
		expectedUID string
	}{
		{name: "initial GET missing", getErr: missing, wantAbsent: true},
		{name: "initial GET forbidden", getErr: NewError("KUBERNETES_FORBIDDEN"), wantErr: NewError("KUBERNETES_FORBIDDEN")},
		{name: "UID changed", expectedUID: "other", wantErr: NewError("RESOURCE_UID_CHANGED")},
		{name: "first LIST missing", failPage: 1, pageErr: missing, wantErr: missing, wantPages: 1},
		{name: "later LIST missing", failPage: 2, pageErr: missing, wantErr: missing, wantPages: 2},
		{name: "later conversion failure", failPage: 2, pageErr: NewError("KUBERNETES_UNAVAILABLE"), wantErr: NewError("KUBERNETES_UNAVAILABLE"), wantPages: 2},
		{name: "native DELETE disappeared", deleteGone: true, wantErr: missing, wantPages: 2, wantWrites: 1},
		{name: "complete preflight", wantPages: 2, wantWrites: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := &kubeStub{descriptor: mutable(), crdErr: tc.getErr, crd: CRDReadInfo{UID: "u", Scope: "Namespaced", Group: "test.io", Plural: "widgets", Versions: []CRDVersion{{Name: "v1", Served: true, Storage: true}}}}
			k.descriptor.Namespaced = false
			pages := 0
			k.pages = func(_ context.Context, q ListQuery) (ResourcePage, error) {
				pages++
				if tc.failPage == pages {
					return ResourcePage{}, tc.pageErr
				}
				if pages == 1 {
					return ResourcePage{NextContinueToken: "next"}, nil
				}
				if q.ContinueToken != "next" {
					t.Fatalf("preflight did not preserve cursor: %+v", q)
				}
				return ResourcePage{}, nil
			}
			if tc.deleteGone {
				k.deleted = &DeleteResult{AlreadyAbsent: true}
			}
			uid := tc.expectedUID
			if uid == "" {
				uid = "u"
			}
			result, err := newCase(t, k).DeleteResource(admin(), DeleteCommand{Ref: ref, ExpectedUID: uid})
			if reason(err) != reason(tc.wantErr) || result.AlreadyAbsent != tc.wantAbsent || pages != tc.wantPages || k.writes != tc.wantWrites || k.crdReads != 1 {
				t.Fatalf("result=%+v err=%v pages=%d deletes=%d initial reads=%d", result, err, pages, k.writes, k.crdReads)
			}
			if err == nil && !tc.wantAbsent && !result.DeletionRequested {
				t.Fatalf("completed deletion was not reported as accepted: %+v", result)
			}
			if err != nil && (result.AlreadyAbsent || result.DeletionRequested) {
				t.Fatalf("preflight failure was reported as success: %+v", result)
			}
		})
	}
	// The special CRD preflight rule must not change ordinary native deletion.
	k := &kubeStub{descriptor: mutable(), deleted: &DeleteResult{AlreadyAbsent: true}}
	ordinary := ResourceRef{ClusterID: "cluster", Type: ResourceTypeRef{Version: "v1", Resource: "configmaps"}, Namespace: "platform", Name: "gone"}
	result, err := newCase(t, k).DeleteResource(admin(), DeleteCommand{Ref: ordinary, ExpectedUID: "u"})
	if err != nil || !result.AlreadyAbsent || k.crdReads != 0 || k.writes != 1 {
		t.Fatalf("ordinary absence changed: %+v %v", result, err)
	}
}

type trackedReader struct {
	io.Reader
	closed chan struct{}
	once   sync.Once
}

func (r *trackedReader) Close() error { r.once.Do(func() { close(r.closed) }); return nil }
func logQuery() PodLogQuery {
	return PodLogQuery{ClusterID: "cluster", Namespace: "tenant", PodName: "pod"}
}
func TestLogsFramesLongLineAndReadError(t *testing.T) {
	text := strings.Repeat("x", 100000) + "\n"
	r := &trackedReader{Reader: strings.NewReader(text), closed: make(chan struct{})}
	k := &kubeStub{opened: OpenedPodLog{Reader: r, PodUID: "uid", Container: "main"}}
	u := newCase(t, k)
	var frames []LogFrame
	err := u.StreamPodLogs(admin(), logQuery(), func(f LogFrame) error { frames = append(frames, f); return nil })
	if err != nil || frames[0].Type != LogReady || frames[len(frames)-1].Reason != "EOF" {
		t.Fatal(err, frames)
	}
	var received strings.Builder
	for _, f := range frames {
		if f.Type == LogData {
			if len(f.Data) > 16384 {
				t.Fatal("oversized frame")
			}
			received.Write(f.Data)
		}
	}
	if received.String() != text {
		t.Fatal("long line truncated")
	}
	select {
	case <-r.closed:
	default:
		t.Fatal("reader not closed")
	}
	r = &trackedReader{Reader: errorReader{}, closed: make(chan struct{})}
	k.opened.Reader = r
	frames = nil
	err = u.StreamPodLogs(admin(), logQuery(), func(f LogFrame) error { frames = append(frames, f); return nil })
	if reason(err) != "LOG_READ_FAILED" || len(frames) != 2 || frames[1].Type != LogError || strings.Contains(frames[1].Message, "sentinel") {
		t.Fatal(err, frames)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("private sentinel") }
func TestLogSendTimeoutReleasesReaderSlotAndLateSender(t *testing.T) {
	for _, blockedType := range []LogFrameType{LogReady, LogData, LogEnd, LogError} {
		t.Run(string(rune('0'+blockedType)), func(t *testing.T) {
			r := &trackedReader{Reader: strings.NewReader("data"), closed: make(chan struct{})}
			if blockedType == LogError {
				r.Reader = errorReader{}
			}
			k := &kubeStub{opened: OpenedPodLog{Reader: r, PodUID: "u", Container: "c"}}
			u := newCase(t, k)
			u.logBudget = logBudget{80 * time.Millisecond, 25 * time.Millisecond, 15 * time.Millisecond}
			streamClosed := make(chan struct{})
			senderExited := make(chan struct{})
			var active, maxActive atomic.Int32
			err := u.StreamPodLogs(admin(), logQuery(), func(f LogFrame) error {
				n := active.Add(1)
				defer active.Add(-1)
				if n > maxActive.Load() {
					maxActive.Store(n)
				}
				if f.Type == blockedType {
					<-streamClosed
					close(senderExited)
					return errors.New("stream closed")
				}
				return nil
			})
			if reason(err) != "LOG_SEND_TIMEOUT" || len(u.logSlots) != 0 || maxActive.Load() != 1 {
				t.Fatal(err, len(u.logSlots), maxActive.Load())
			}
			select {
			case <-r.closed:
			default:
				t.Fatal("reader still open")
			}
			close(streamClosed)
			select {
			case <-senderExited:
			case <-time.After(time.Second):
				t.Fatal("sender failed to exit after underlying stream closed")
			}
		})
	}
}
func TestLogsWindowNormalEndAndCapacity(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	k := &kubeStub{opened: OpenedPodLog{Reader: r, PodUID: "u", Container: "c"}}
	u := newCase(t, k)
	u.logBudget = logBudget{20 * time.Millisecond, 20 * time.Millisecond, 20 * time.Millisecond}
	var last LogFrame
	if err := u.StreamPodLogs(admin(), logQuery(), func(f LogFrame) error { last = f; return nil }); err != nil || last.Reason != "STREAM_LIMIT_REACHED" {
		t.Fatal(err, last)
	}
	for range cap(u.logSlots) {
		u.logSlots <- struct{}{}
	}
	if err := u.StreamPodLogs(admin(), logQuery(), func(LogFrame) error { return nil }); reason(err) != "RESOURCE_EXHAUSTED" {
		t.Fatal(err)
	}
}
