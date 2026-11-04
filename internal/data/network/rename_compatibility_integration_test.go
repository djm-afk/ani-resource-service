package data_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	networkv1 "github.com/zhangzhe-ctrl/ani-resource-service/api/network/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network"
	controlled "github.com/zhangzhe-ctrl/ani-resource-service/tests/net05a/provider"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
)

// Fixed pre-rename generated client and published schema-15/v2-reader server,
// with separate executables and descriptor registries. The earliest supported
// rollback is 586f861; schema-14 4e7ef74 rejects migration 15 and cannot resume
// controller_managed cleanup. Keep that older schema guard intact.
// This controlled PG/HTTP check does not replace real Kubernetes R4 evidence.
func TestResourceOldClientSchemaCompatibleSameDatabaseRollback(t *testing.T) {
	if os.Getenv("NETWORK_TEST_ADMIN_DSN") == "" {
		t.Skip("requires scripts/integration isolated PostgreSQL")
	}
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	oldRoot := os.Getenv("ANI_RESOURCE_BASELINE_ROOT")
	if oldRoot == "" {
		oldRoot = t.TempDir()
		archive := filepath.Join(t.TempDir(), "baseline.tar")
		command := exec.Command("git", "archive", "--output", archive, "66f787bd30134141726c596612501a83cf75bdb7")
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("fixed baseline archive: %v %s", err, output)
		}
		if output, err := exec.Command("tar", "-xf", archive, "-C", oldRoot).CombinedOutput(); err != nil {
			t.Fatalf("baseline extraction: %v %s", err, output)
		}
	}
	rollbackRoot := os.Getenv("ANI_RESOURCE_ROLLBACK_ROOT")
	if rollbackRoot == "" {
		rollbackRoot = t.TempDir()
		archive := filepath.Join(t.TempDir(), "rollback-baseline.tar")
		command := exec.Command("git", "archive", "--output", archive, "586f8619576b44117195fc7fffa99bbc66a6d108")
		command.Dir = root
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("fixed schema-compatible server archive: %v %s", err, output)
		}
		if output, err := exec.Command("tar", "-xf", archive, "-C", rollbackRoot).CombinedOutput(); err != nil {
			t.Fatalf("schema-compatible server extraction: %v %s", err, output)
		}
	}
	// Guard the published schema-15/v2-reader composition even when a supplied archive avoids
	// accessing Git history on an isolated remote test host.
	serverSource, err := os.ReadFile(filepath.Join(rollbackRoot, "cmd/ani-resource-service/app.go"))
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(serverSource)) != "25abef2cdfa4e3cc3e94f42851e5fbe52743c8aaf33906419948e2066b4a61a5" {
		t.Fatalf("fixed 586f861 schema-15/v2-reader server composition guard failed: %v", err)
	}
	build := func(dir, target, name string) string {
		binary := filepath.Join(t.TempDir(), name)
		command := exec.Command("go", "build", "-race", "-trimpath", "-o", binary, target)
		command.Dir = dir
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v %s", name, err, output)
		}
		return binary
	}
	oldBinary := build(rollbackRoot, "./cmd/ani-resource-service", "baseline")
	oldClient := buildOldClientTransportFixture(t, oldRoot)
	candidate := build(root, "./cmd/ani-resource-service", "candidate")
	migrate := func(binary string, owner *pgxpool.Pool, role string) {
		command := exec.Command(binary, "-migrate")
		command.Env = append(os.Environ(), "ANI_NETWORK_MIGRATION_DSN="+owner.Config().ConnString(), "ANI_NETWORK_RUNTIME_ROLE="+role)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("migration: %v %s", err, output)
		}
	}
	var originalSchema string
	fixture := testenv.NewDatabase(t, func(owner *pgxpool.Pool, role string) {
		migrate(oldBinary, owner, role)
		if err := owner.QueryRow(context.Background(), "SELECT jsonb_agg(to_jsonb(v) ORDER BY version)::text FROM network_schema_version v").Scan(&originalSchema); err != nil {
			t.Fatal(err)
		}
	})
	checkSchema := func() {
		var current string
		if err := fixture.Owner.QueryRow(context.Background(), "SELECT jsonb_agg(to_jsonb(v) ORDER BY version)::text FROM network_schema_version v").Scan(&current); err != nil {
			t.Fatal(err)
		}
		if current != originalSchema {
			t.Fatal("migration versions, checksums or applied timestamps changed")
		}
	}
	checkSchema()
	protocol := controlled.New()
	renewFacts := seedProcessVPCPlatformFacts(t, protocol)
	config := processVPCPresetConfig(t, root, "10.42.0.0/16", "10.43.0.0/16", "10.44.0.0/16")
	// These independently seeded platform objects predate tenant activity.
	// Cleanup must preserve them while deleting all tenant-owned resources.
	platformObjects := map[string]string{}
	protocol.Backend.Mu.Lock()
	for key, object := range protocol.Backend.Objects {
		m := object["metadata"].(map[string]any)
		content, _ := json.Marshal(map[string]any{"uid": m["uid"], "labels": m["labels"], "spec": object["spec"]})
		platformObjects[key] = string(content)
	}
	protocol.Backend.Mu.Unlock()
	var armed, captured atomic.Bool
	reached, release := make(chan struct{}), make(chan struct{})
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		renewFacts(r)
		if armed.Load() && r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/vpcs") && captured.CompareAndSwap(false, true) {
			recorder := httptest.NewRecorder()
			protocol.ServeHTTP(recorder, r)
			close(reached)
			<-release
			for k, v := range recorder.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			return
		}
		protocol.ServeHTTP(w, r)
	}))
	defer host.Close()
	defer close(release)
	kubeconfig, key := testenv.Kubeconfig(t, host.URL), testenv.SigningKey()
	grpcAddress, adminAddress := freeAddress(t), freeAddress(t)
	var activeProcess *networkProcess
	start := func(binary string) *networkProcess {
		options := []string{"subnet", "config=" + config, "grpc-address=" + grpcAddress, "admin-address=" + adminAddress}
		activeProcess = startNetworkProcess(t, root, binary, fixture.RuntimeDSN, kubeconfig, key, options...)
		return activeProcess
	}
	tenant := uuid.NewString()
	callWithTenant := func(scopeTenant, method string, request map[string]any, want string) map[string]any {
		requestID := uuid.NewString()
		const actor = "governance:user:7"
		delegated := make(map[string]any, len(request)+1)
		for k, v := range request {
			delegated[k] = v
		}
		if method == "CreateVPC" || method == "CreateSubnet" {
			if _, supplied := delegated["attribution"]; !supplied {
				delegated["attribution"] = map[string]any{"actor": actor, "direct_caller": "ani-governance", "correlation_id": requestID}
			}
		}
		body, _ := json.Marshal(delegated)
		command := exec.Command(oldClient, "call", "-target", grpcAddress, "-service", "NetworkService", "-method", method)
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "ANI_TEST_OLD_CLIENT_") {
				command.Env = append(command.Env, entry)
			}
		}
		command.Env = append(command.Env, "ANI_TEST_OLD_CLIENT_TENANT="+scopeTenant, "ANI_TEST_OLD_CLIENT_ACTOR="+actor, "ANI_TEST_OLD_CLIENT_REQUEST_ID="+requestID)
		if activeProcess.serviceTLS == nil || activeProcess.legacyPlaintext {
			t.Fatal("both released and candidate old-client fixtures require mTLS credentials")
		}
		command.Env = append(command.Env, "ANI_TEST_OLD_CLIENT_MODE=mtls", "ANI_TEST_OLD_CLIENT_CA="+activeProcess.serviceTLS.CAFile,
			"ANI_TEST_OLD_CLIENT_CERT="+activeProcess.serviceTLS.ClientCertFile, "ANI_TEST_OLD_CLIENT_KEY="+activeProcess.serviceTLS.ClientKeyFile)
		command.Stdin = bytes.NewReader(body)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		var result struct {
			Code     string         `json:"code"`
			Response map[string]any `json:"response"`
		}
		if json.Unmarshal(output, &result) != nil || result.Code != want || (want == "OK" && err != nil) {
			t.Fatalf("old client %s: %v %s %s", method, err, output, stderr.String())
		}
		return result.Response
	}
	call := func(method string, request map[string]any, want string) map[string]any {
		return callWithTenant(tenant, method, request, want)
	}
	create := func(name, cidr string) map[string]any {
		return map[string]any{"tenant_id": tenant, "name": name, "cidr": cidr, "idempotency_key": "rename-" + name}
	}
	a, b := create("old-a", "10.42.0.0/16"), create("old-b", "10.43.0.0/16")
	first := start(oldBinary)
	receiptA := call("CreateVPC", a, "OK")
	receiptB := call("CreateVPC", b, "OK")
	id := func(r map[string]any) string { return r["vpc"].(map[string]any)["id"].(string) }
	client, closeClient := first.client(t, tenant)
	waitRPCState(t, client, tenant, id(receiptA), networkv1.ResourceState_RESOURCE_STATE_AVAILABLE)
	waitRPCState(t, client, tenant, id(receiptB), networkv1.ResourceState_RESOURCE_STATE_AVAILABLE)
	closeClient()
	page := call("ListVPCs", map[string]any{"tenant_id": tenant, "limit": 1}, "OK")
	cursor := page["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("baseline cursor missing")
	}
	pageRequest := map[string]any{"tenant_id": tenant, "limit": 1, "cursor": cursor}
	pageTwo := call("ListVPCs", pageRequest, "OK")
	identities := func() map[string]string {
		protocol.Backend.Mu.Lock()
		defer protocol.Backend.Mu.Unlock()
		result := map[string]string{}
		for k, o := range protocol.Backend.Objects {
			m := o["metadata"].(map[string]any)
			content, _ := json.Marshal(map[string]any{"uid": m["uid"], "labels": m["labels"], "spec": o["spec"]})
			result[k] = string(content)
		}
		return result
	}
	originalObjects := identities()
	first.stop(t)
	second := start(candidate)
	if !reflect.DeepEqual(receiptA, call("CreateVPC", a, "OK")) {
		t.Fatal("old receipt changed on candidate")
	}
	resumedPage := call("ListVPCs", pageRequest, "OK")
	if resumedPage["items"].([]any)[0].(map[string]any)["id"] != pageTwo["items"].([]any)[0].(map[string]any)["id"] {
		t.Fatal("old cursor changed")
	}
	otherTenant := uuid.NewString()
	callWithTenant(otherTenant, "GetVPC", map[string]any{"tenant_id": otherTenant, "vpc_id": id(receiptA)}, "NotFound")
	call("GetVPC", map[string]any{"tenant_id": otherTenant, "vpc_id": id(receiptA)}, "PermissionDenied")
	call("CreateVPC", map[string]any{"tenant_id": tenant, "name": "untrusted-attribution", "cidr": "10.45.0.0/16", "idempotency_key": "untrusted-attribution",
		"attribution": map[string]any{"actor": "governance:user:99", "direct_caller": "ani-governance", "correlation_id": uuid.NewString()}}, "PermissionDenied")
	if !reflect.DeepEqual(originalObjects, identities()) {
		t.Fatal("candidate replaced original Provider identity/spec")
	}
	armed.Store(true)
	c := create("candidate-c", "10.44.0.0/16")
	receiptC := call("CreateVPC", c, "OK")
	select {
	case <-reached:
	case <-time.After(8 * time.Second):
		t.Fatal("candidate did not reach persisted Provider-success boundary")
	}
	second.kill(t)
	release <- struct{}{}
	migrate(oldBinary, fixture.Owner, fixture.RuntimeRole)
	checkSchema()
	third := start(oldBinary)
	client, closeClient = third.client(t, tenant)
	waitRPCState(t, client, tenant, id(receiptC), networkv1.ResourceState_RESOURCE_STATE_AVAILABLE)
	closeClient()
	if !reflect.DeepEqual(receiptC, call("CreateVPC", c, "OK")) {
		t.Fatal("rollback lost candidate receipt")
	}
	protocol.Backend.Mu.Lock()
	if protocol.Backend.Creates["vpcs"] != 3 || protocol.Backend.Creates["namespaces"] != 1 {
		t.Errorf("duplicate Provider creates: %v", protocol.Backend.Creates)
	}
	protocol.Backend.Mu.Unlock()
	third.stop(t)
	fourth := start(candidate)
	for _, r := range []map[string]any{receiptA, receiptB, receiptC} {
		call("DeleteVPC", map[string]any{"tenant_id": tenant, "vpc_id": id(r)}, "OK")
	}
	client, closeClient = fourth.client(t, tenant)
	for _, r := range []map[string]any{receiptA, receiptB, receiptC} {
		waitRPCState(t, client, tenant, id(r), networkv1.ResourceState_RESOURCE_STATE_DELETED)
	}
	closeClient()
	fourth.stop(t)
	checkSchema()
	remaining := identities()
	for name, content := range platformObjects {
		if remaining[name] != content {
			t.Fatalf("API cleanup changed an external platform identity/spec: %s", name)
		}
	}
	for name := range remaining {
		if _, platform := platformObjects[name]; !platform && !strings.HasPrefix(name, "namespaces/") {
			t.Fatalf("Provider resource remains after API cleanup: %s", name)
		}
	}
	t.Log("fixed 66f787 generated client with test-only transport/context overlay: published schema-15/v2-reader 586f861 mTLS -> candidate mTLS -> 586f861 rollback with pending work -> candidate mTLS; same schema-15 DB, endpoint, config, cursor key; identities, receipts, migration rows/checksums/timestamps preserved; API cleanup complete; older incompatible server rollback not asserted")

	// Candidate outbound adapter talks to an independent old generated owner.
	ownerDir, err := os.MkdirTemp("", "rename-owner-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(ownerDir)
	registry, socket := filepath.Join(ownerDir, "registry.json"), filepath.Join(ownerDir, "owner.sock")
	attachment := biz.Attachment{TenantID: tenant, InstanceID: "instance", SubmissionID: uuid.NewString(), Generation: 1, ID: uuid.NewString(), FinalizationID: uuid.NewString()}
	facts := []map[string]any{{"protocol_version": 1, "tenant_id": tenant, "instance_id": attachment.InstanceID, "submission_id": attachment.SubmissionID, "generation": 1, "attachment_id": attachment.ID, "cluster_id": "test-cluster", "namespace": "tenant-" + tenant, "state": "SUBMISSION_STATE_CLOSED", "finalization_id": attachment.FinalizationID, "pod_uids": []string{"old-pod"}, "controller_uids": []string{"old-controller"}, "closed_at": "2026-09-21T00:00:00Z"}}
	body, _ := json.Marshal(facts)
	if err := os.WriteFile(registry, body, 0600); err != nil {
		t.Fatal(err)
	}
	owner := exec.Command(oldClient, "owner", "-socket", socket, "-registry", registry)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Process.Signal(os.Interrupt); _ = owner.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old owner socket not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	consumer, err := data.NewInstanceConsumer("unix://" + socket)
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	submission, err := consumer.GetSubmission(ctx, attachment)
	// The old wire may carry controller history. Only current management
	// fields remain part of the Network consumer contract.
	if err != nil || submission.State != "closed" || submission.FinalizationID != attachment.FinalizationID || submission.TenantID != tenant || submission.ClosedAt == nil || !reflect.DeepEqual(submission.PodUIDs, []string{"old-pod"}) {
		t.Fatalf("old reverse consumer changed: %+v %v", submission, err)
	}
	t.Log("candidate outbound adapter -> independent old generated consumer: pass")
}

// The pinned historical CLI predates the required mTLS transport. Adapt only
// that test driver's dialing/context through a temporary compiler overlay;
// its archived source, generated protobuf/descriptor, and old service stay intact.
func buildOldClientTransportFixture(t *testing.T, oldRoot string) string {
	t.Helper()
	// SHA-256 values from git show 66f787bd30134141726c596612501a83cf75bdb7:<path>.
	guards := map[string]string{
		"scripts/lb-api/main.go":       "df42be7110cfdd2a3e3bd29bf67b9de51100ecbf9fcd4692861dcf3e12b4c78e",
		"api/network/v1/network.pb.go": "540c62e3e83caf1507a0f2a8090791a357d142f2853a83a293e513079b301d35",
	}
	checkOriginal := func() {
		t.Helper()
		for path, want := range guards {
			body, err := os.ReadFile(filepath.Join(oldRoot, path))
			if err != nil || fmt.Sprintf("%x", sha256.Sum256(body)) != want {
				t.Fatalf("fixed old source guard failed for %s: %v", path, err)
			}
		}
	}
	checkOriginal()
	t.Cleanup(checkOriginal)
	original := filepath.Join(oldRoot, "scripts/lb-api/main.go")
	body, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)
	replace := func(before, after string) {
		t.Helper()
		if strings.Count(source, before) != 1 {
			t.Fatalf("fixed old transport hook changed: %q", before)
		}
		source = strings.Replace(source, before, after, 1)
	}
	replace("\"context\"", "\"context\"\n\t\"crypto/tls\"\n\t\"crypto/x509\"")
	replace("\"google.golang.org/grpc/codes\"", "\"google.golang.org/grpc/codes\"\n\t\"google.golang.org/grpc/credentials\"\n\t\"google.golang.org/grpc/metadata\"")
	replace("conn, err := grpc.NewClient(*target, grpc.WithTransportCredentials(insecure.NewCredentials()))", `transport, err := oldFixtureCredentials()
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(*target, grpc.WithTransportCredentials(transport), grpc.WithDisableRetry(), grpc.WithDisableServiceConfig())`)
	replace("context.WithTimeout(context.Background(), 15*time.Second)", "context.WithTimeout(oldFixtureContext(context.Background()), 15*time.Second)")
	source += `
// These credentials and delegated values belong only to the isolated test.
// The candidate verifies them through its unchanged production listener.
func oldFixtureCredentials() (credentials.TransportCredentials, error) {
	switch os.Getenv("ANI_TEST_OLD_CLIENT_MODE") {
	case "legacy":
		return insecure.NewCredentials(), nil
	case "mtls":
		ca, err := os.ReadFile(os.Getenv("ANI_TEST_OLD_CLIENT_CA"))
		if err != nil {
			return nil, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			return nil, errors.New("isolated old-client CA is invalid")
		}
		cert, err := tls.LoadX509KeyPair(os.Getenv("ANI_TEST_OLD_CLIENT_CERT"), os.Getenv("ANI_TEST_OLD_CLIENT_KEY"))
		if err != nil {
			return nil, err
		}
		for _, key := range []string{"ANI_TEST_OLD_CLIENT_TENANT", "ANI_TEST_OLD_CLIENT_ACTOR", "ANI_TEST_OLD_CLIENT_REQUEST_ID"} {
			if os.Getenv(key) == "" {
				return nil, errors.New("isolated old-client trusted scope is required")
			}
		}
		return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{cert}, ServerName: "ani-network-service"}), nil
	default:
		return nil, errors.New("isolated old-client transport mode must be explicit")
	}
}
func oldFixtureContext(ctx context.Context) context.Context {
	return metadata.NewOutgoingContext(ctx, metadata.Pairs(
		"x-ani-tenant-id", os.Getenv("ANI_TEST_OLD_CLIENT_TENANT"),
		"x-ani-actor", os.Getenv("ANI_TEST_OLD_CLIENT_ACTOR"),
		"x-ani-request-id", os.Getenv("ANI_TEST_OLD_CLIENT_REQUEST_ID")))
}
`
	dir := t.TempDir()
	replacement := filepath.Join(dir, "main.go")
	if err := os.WriteFile(replacement, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	overlayBody, err := json.Marshal(map[string]any{"Replace": map[string]string{original: replacement}})
	if err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(overlay, overlayBody, 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "old-client")
	command := exec.Command("go", "build", "-overlay", overlay, "-race", "-trimpath", "-o", binary, "./scripts/lb-api")
	command.Dir = oldRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fixed old protobuf client with test transport overlay: %v %s", err, output)
	}
	checkOriginal()
	return binary
}
