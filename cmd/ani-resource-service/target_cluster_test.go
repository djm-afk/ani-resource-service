package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
	"k8s.io/client-go/rest"
)

func TestResolveTargetClusterUsesNamespaceUID(t *testing.T) {
	const uid = "11111111-1111-4111-8111-111111111111"
	for _, tc := range []struct {
		name, configured, returned string
		status                     int
		reason                     string
	}{
		{"discover", "", uid, 200, ""},
		{"matches", uid, uid, 200, ""},
		{"drift", "22222222-2222-4222-8222-222222222222", uid, 200, "differs"},
		{"invalid", "", "name-is-not-identity", 200, "canonical UUID"},
		{"forbidden", uid, "", 403, "namespaces/kube-system"},
		{"unavailable", uid, "", 503, "namespaces/kube-system"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != "GET" || r.URL.Path != "/api/v1/namespaces/kube-system" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if tc.status == 200 {
					_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]string{"uid": tc.returned}})
				} else {
					_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Status", "status": "Failure", "code": tc.status, "reason": http.StatusText(tc.status)})
				}
			}))
			defer api.Close()
			cfg := &conf.Network{ClusterId: tc.configured, Kubeconfig: testenv.Kubeconfig(t, api.URL)}
			target, err := resolveTargetCluster(context.Background(), cfg)
			if tc.reason != "" {
				if err == nil || !strings.Contains(err.Error(), tc.reason) {
					t.Fatalf("error=%v", err)
				}
				if cfg.ClusterId != tc.configured {
					t.Fatal("failed identity replaced configuration")
				}
			} else if err != nil || cfg.ClusterId != uid || target.Host != api.URL {
				t.Fatalf("target=%v cluster=%q err=%v", target, cfg.ClusterId, err)
			}
			if requests.Load() != 1 {
				t.Fatalf("identity reads=%d", requests.Load())
			}
		})
	}
}

func TestOnlineCompositionsRejectTargetBeforeOtherDependencies(t *testing.T) {
	const actual = "11111111-1111-4111-8111-111111111111"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"uid":"` + actual + `"}}`))
	}))
	defer api.Close()
	for _, governance := range []bool{false, true} {
		bc := &conf.Bootstrap{Network: &conf.Network{Kubeconfig: testenv.Kubeconfig(t, api.URL), ClusterId: "22222222-2222-4222-8222-222222222222", DatabaseDsn: "must-not-open"}}
		var err error
		if governance {
			err = runGovernance(bc, newRuntimeLogger(io.Discard))
		} else {
			_, err = buildApp(bc, newRuntimeLogger(io.Discard))
		}
		if err == nil || !strings.Contains(err.Error(), "differs from target") {
			t.Fatalf("identity was not checked first: %v", err)
		}
	}
}

func TestResolvedTargetCopiesAndDisabledImage(t *testing.T) {
	original := &rest.Config{Host: "https://target", QPS: 7, Burst: 11}
	copy, err := resolvedKubernetesConfig(nil, original)
	if err != nil {
		t.Fatal(err)
	}
	copy.QPS = 99
	if original.QPS != 7 {
		t.Fatal("client modified shared rate budget")
	}
	if cfg, err := resolvePullSecretTarget(context.Background(), &conf.Image{}, nil, nil); err != nil || cfg != nil {
		t.Fatal("disabled Image required a target", err)
	}
}

func TestImageEmptyKubeconfigKeepsServiceAccountCredentials(t *testing.T) {
	const uid = "11111111-1111-4111-8111-111111111111"
	for _, actual := range []string{uid, "22222222-2222-4222-8222-222222222222"} {
		t.Run(actual, func(t *testing.T) {
			var reads int
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads++
				if r.Header.Get("Authorization") != "Bearer image-service-account" {
					t.Errorf("wrong credentials: %q", r.Header.Get("Authorization"))
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]string{"uid": actual}})
			}))
			defer api.Close()
			image := &conf.Image{Enabled: true, PullSecretClusterId: uid}
			network := &conf.Network{ClusterId: uid, Kubeconfig: "explicit-network-config"}
			cfg, err := resolvePullSecretConnection(context.Background(), image, network, &rest.Config{Host: api.URL, BearerToken: "network-admin"}, func(path string) (*rest.Config, error) {
				if path != "" {
					t.Fatalf("Image selected path %q", path)
				}
				return &rest.Config{Host: api.URL, BearerToken: "image-service-account"}, nil
			})
			if reads != 1 {
				t.Fatalf("Image identity reads=%d", reads)
			}
			if actual != uid {
				if err == nil || !strings.Contains(err.Error(), "different kube-system UID") || cfg != nil {
					t.Fatalf("cfg=%v err=%v", cfg, err)
				}
			} else if err != nil || cfg.BearerToken != "image-service-account" {
				t.Fatalf("Image credentials lost: cfg=%v err=%v", cfg, err)
			}
		})
	}
}

func TestDefaultConfigEntryDiscoversTargetUID(t *testing.T) {
	const uid = "11111111-1111-4111-8111-111111111111"
	for _, configured := range []string{"", "22222222-2222-4222-8222-222222222222"} {
		t.Run(configured, func(t *testing.T) {
			var reads int
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reads++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Namespace","metadata":{"uid":"` + uid + `"}}`))
			}))
			defer api.Close()
			for _, key := range []string{"NETWORK_CLUSTER_ID", "ANI_NETWORK_CLUSTER_ID"} {
				old, existed := os.LookupEnv(key)
				_ = os.Unsetenv(key)
				t.Cleanup(func() {
					if existed {
						_ = os.Setenv(key, old)
					} else {
						_ = os.Unsetenv(key)
					}
				})
			}
			if configured != "" {
				t.Setenv("ANI_NETWORK_CLUSTER_ID", configured)
			}
			t.Setenv("ANI_NETWORK_KUBECONFIG", testenv.Kubeconfig(t, api.URL))
			t.Setenv("ANI_NETWORK_DATABASE_DSN", "://invalid-dsn")
			t.Setenv("ANI_NETWORK_MODE", "governance")
			old := flagconf
			flagconf = filepath.Join("..", "..", "configs")
			t.Cleanup(func() { flagconf = old })
			err := run(newRuntimeLogger(io.Discard))
			if reads != 1 {
				t.Fatalf("real -conf ./configs identity reads=%d error=%v", reads, err)
			}
			if err == nil {
				t.Fatal("invalid database unexpectedly accepted")
			}
			if configured == "" && strings.Contains(err.Error(), "differs from target") {
				t.Fatalf("default blocked UID discovery: %v", err)
			}
			if configured != "" && !strings.Contains(err.Error(), "differs from target") {
				t.Fatalf("explicit mismatch admitted: %v", err)
			}
		})
	}
}
