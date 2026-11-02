package data

import (
	"io"
	"net/http"
	"strings"
	"sync"
)

type providerIO struct {
	mu     sync.Mutex
	values map[string]float64
}

func (p *providerIO) add(key string, n float64) { p.mu.Lock(); defer p.mu.Unlock(); p.values[key] += n }
func (p *providerIO) snapshot() map[string]float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]float64{}
	for k, v := range p.values {
		out[k] = v
	}
	return out
}

type measuredTransport struct {
	base  http.RoundTripper
	stats *providerIO
}

func (t measuredTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resource, collection := providerRequestResource(r.URL.Path)
	verb := r.Method
	switch verb {
	case "GET", "POST", "DELETE", "PATCH":
	default:
		verb = "OTHER"
	}
	if r.URL.Query().Get("watch") == "true" {
		verb = "WATCH"
	} else if verb == "GET" && collection {
		verb = "LIST"
	}
	key := verb + "_" + resource
	t.stats.add("requests_"+key+"_total", 1)
	response, err := t.base.RoundTrip(r)
	if err != nil {
		t.stats.add("errors_"+key+"_total", 1)
		return response, err
	}
	if response.StatusCode >= 400 {
		t.stats.add("errors_"+key+"_total", 1)
	}
	response.Body = measuredBody{response.Body, t.stats, key}
	return response, nil
}

// Resource names occupy a fixed position in Kubernetes REST paths. Namespace
// and object names may themselves match a resource name; neither is a label.
func providerRequestResource(path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	index := -1
	if len(parts) >= 3 && parts[0] == "api" {
		index = 2
	} else if len(parts) >= 4 && parts[0] == "apis" {
		index = 3
	}
	if index < 0 {
		return "other", false
	}
	if parts[index] == "namespaces" && len(parts) > index+2 && parts[index+2] != "status" && parts[index+2] != "finalize" {
		index += 2
	}
	resource := "other"
	switch parts[index] {
	case "vpcs", "subnets", "pods", "vnics", "vnicips", "eips", "namespaces", "snats", "nats", "eipgateways", "vlannetworks", "nodes", "configmaps", "services", "servicecidrs", "serviceaccounts":
		resource = parts[index]
	case "network-attachment-definitions":
		resource = "network_attachment_definitions"
	}
	return resource, len(parts) == index+1
}

type measuredBody struct {
	io.ReadCloser
	stats *providerIO
	key   string
}

func (b measuredBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.stats.add("bytes_"+b.key+"_total", float64(n))
	return n, err
}
