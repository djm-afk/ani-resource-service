package data

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type metricsRoundTripper func(*http.Request) (*http.Response, error)

func (f metricsRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProviderRequestMetricsUseResourcePathNotNamespaceOrObjectName(t *testing.T) {
	for _, tc := range []struct {
		path string
		key  string
	}{
		{"/api/v1/namespaces/tenant-a", "GET_namespaces"},
		{"/api/v1/namespaces/nodes", "GET_namespaces"},
		{"/api/v1/namespaces/tenant-a/status", "GET_namespaces"},
		{"/api/v1/namespaces/tenant-a/serviceaccounts/ani-vm-workload", "GET_serviceaccounts"},
		{"/apis/k8s.cni.cncf.io/v1/namespaces/tenant-a/network-attachment-definitions/ani-vm-primary-v1", "GET_network_attachment_definitions"},
		{"/api/v1/namespaces/tenant-a/pods/pods", "GET_pods"},
		{"/api/v1/namespaces/tenant-a/pods/pod-a/log", "GET_pods"},
		{"/apis/networking.kubercloud.com/v1/vnics", "LIST_vnics"},
		{"/apis/networking.kubercloud.com/v1/vnics?watch=true", "WATCH_vnics"},
		{"/apis/apps/v1/namespaces/tenant-a/deployments/nodes", "GET_other"},
	} {
		t.Run(tc.key+tc.path, func(t *testing.T) {
			stats := &providerIO{values: map[string]float64{}}
			transport := measuredTransport{stats: stats, base: metricsRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("abc"))}, nil
			})}
			req, err := http.NewRequest(http.MethodGet, "https://cluster.invalid"+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := transport.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = io.Copy(io.Discard, response.Body); err != nil {
				t.Fatal(err)
			}
			if err = response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			got := stats.snapshot()
			if len(got) != 3 || got["requests_"+tc.key+"_total"] != 1 || got["errors_"+tc.key+"_total"] != 1 || got["bytes_"+tc.key+"_total"] != 3 {
				t.Fatalf("request/error/bytes attribution differs: want %s, got %v", tc.key, got)
			}
		})
	}
}
