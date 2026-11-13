package data

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
)

const sentinelSecret = "Aa0-secret-sentinel-should-never-appear"

func testHarbor(t *testing.T, handler http.HandlerFunc) *Harbor {
	t.Helper()
	s := httptest.NewTLSServer(handler)
	t.Cleanup(s.Close)
	h, err := NewHarbor(HarborConfig{URL: s.URL, Username: "image-admin", Password: biz.Secret(sentinelSecret), RobotNamePrefix: "custom$", CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}
func TestHarborTransportBoundary(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 429, 500, 502, 503, 504} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			h := testHarbor(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				u, p, ok := r.BasicAuth()
				if !ok || u != "image-admin" || p != sentinelSecret {
					t.Error("credentials not sent to fixed origin")
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, sentinelSecret)
			})
			_, err := h.GetProjectByID(context.Background(), 1)
			if err == nil || strings.Contains(fmt.Sprintf("%+v", err), sentinelSecret) {
				t.Fatal("unsafe error")
			}
			expected := int32(1)
			if status == 429 || status == 502 || status == 503 || status == 504 {
				expected = 2
			}
			if calls.Load() != expected {
				t.Fatal("wrong read retry budget")
			}
			calls.Store(0)
			_, _ = h.CreatePrivateProject(context.Background(), "t-alpha")
			if calls.Load() != 1 {
				t.Fatal("write was automatically retried")
			}
		})
	}
	t.Run("redirect", func(t *testing.T) {
		var leaked atomic.Int32
		other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
		defer other.Close()
		h := testHarbor(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
		})
		_, err := h.GetProjectByID(context.Background(), 1)
		if err == nil || leaked.Load() != 0 {
			t.Fatal("redirect followed")
		}
	})
	t.Run("untrusted TLS", func(t *testing.T) {
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted upstream reached") }))
		defer s.Close()
		h, err := NewHarbor(HarborConfig{URL: s.URL, Username: "admin", Password: biz.Secret(sentinelSecret), RobotNamePrefix: "custom$"})
		if err != nil {
			t.Fatal(err)
		}
		defer h.Close()
		_, err = h.GetProjectByID(context.Background(), 1)
		if err == nil || strings.Contains(err.Error(), sentinelSecret) {
			t.Fatal("TLS failure unsafe")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		h := testHarbor(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		_, err := h.GetProjectByID(ctx, 1)
		if biz.ReasonOf(err) != biz.DeadlineExceeded {
			t.Fatal(err)
		}
	})
	t.Run("client timeout without parent deadline", func(t *testing.T) {
		h := testHarbor(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		h.client.Timeout = 30 * time.Millisecond
		_, err := h.GetProjectByID(context.Background(), 1)
		if biz.ReasonOf(err) != biz.DeadlineExceeded {
			t.Fatal(err)
		}
	})
	t.Run("body limit", func(t *testing.T) {
		h := testHarbor(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, strings.Repeat("x", maxHarborBody+1))
		})
		_, err := h.GetProjectByID(context.Background(), 1)
		if err == nil {
			t.Fatal("oversize response accepted")
		}
	})
	for _, u := range []string{"http://registry.invalid", "https://user:secret@registry.invalid", "https://registry.invalid/path", "https://registry.invalid?host=elsewhere"} {
		if _, err := NewHarbor(HarborConfig{URL: u, Username: "admin", Password: biz.Secret(sentinelSecret), RobotNamePrefix: "custom$"}); err == nil {
			t.Fatal("unsafe config accepted")
		}
	}
}
func TestHarborPrivateProjectAndLocation(t *testing.T) {
	var posts atomic.Int32
	h := testHarbor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "POST":
			posts.Add(1)
			var input map[string]any
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			metadata := input["metadata"].(map[string]any)
			if metadata["public"] != "false" || len(input) != 2 || r.Header.Get("X-Resource-Name-In-Location") != "false" {
				t.Error("unexpected project contract")
			}
			w.Header().Set("Location", "/api/v2.0/projects/17")
			w.WriteHeader(201)
		default:
			_, _ = io.WriteString(w, `{"project_id":17,"name":"t-alpha","metadata":{"public":"false"}}`)
		}
	})
	p, err := h.CreatePrivateProject(context.Background(), "t-alpha")
	if err != nil || p.ID != 17 || !p.Private || posts.Load() != 1 {
		t.Fatal(p, err)
	}
	for _, fn := range []func() (biz.Project, error){func() (biz.Project, error) { return h.GetProjectByID(context.Background(), 17) }, func() (biz.Project, error) { return h.FindProjectByName(context.Background(), "t-alpha") }} {
		p, err = fn()
		if err != nil || !p.Private || p.Name != "t-alpha" {
			t.Fatal(p, err)
		}
	}
	for _, location := range []string{"", "/api/v2.0/projects/t-alpha", "https://evil.invalid/api/v2.0/projects/17", "/api/v2.0/projects/0"} {
		h := testHarbor(t, func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Location", location); w.WriteHeader(201) })
		_, err := h.CreatePrivateProject(context.Background(), "t-alpha")
		if biz.ReasonOf(err) != biz.SpaceOwnershipUnconfirmed {
			t.Fatal("lost Location must not adopt by name", err)
		}
	}
}
func TestHarborRobotOwnershipAndWireContract(t *testing.T) {
	perms, err := biz.RobotPermissions("t-alpha", "platform", "publisher", biz.TenantImages)
	if err != nil {
		t.Fatal(err)
	}
	request := biz.RobotRequest{Name: "i-test-g1", Description: "ani-image:v1:installation:space:publisher:1:command", DurationDays: 30, Permissions: perms}
	hp, err := robotPermissions(perms)
	if err != nil {
		t.Fatal(err)
	}
	state := harborRobot{ID: 12, Name: "custom$" + request.Name, Description: request.Description, Level: "system", Duration: 30, Editable: true, ExpiresAt: time.Now().Add(30 * 24 * time.Hour).Unix(), Permissions: hp}
	var patches, puts atomic.Int32
	h := testHarbor(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "POST":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["secret"]; ok {
				t.Error("create supplied unsupported stable secret")
			}
			if body["level"] != "system" || body["name"] != request.Name {
				t.Error("wrong create identity")
			}
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 12, "name": state.Name, "expires_at": state.ExpiresAt, "secret": sentinelSecret})
		case "PATCH":
			patches.Add(1)
			if r.URL.Path != "/api/v2.0/robots/12" {
				t.Error("wrong RefreshSec path")
			}
			var body struct {
				Secret string `json:"secret"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Secret != sentinelSecret {
				t.Error("stable secret missing")
			}
			_, _ = io.WriteString(w, `{"secret":""}`)
		case "PUT":
			puts.Add(1)
			var input harborRobot
			_ = json.NewDecoder(r.Body).Decode(&input)
			if input.Name != state.Name || input.Duration != state.Duration || input.Level != "system" {
				t.Error("update changed identity or duration")
			}
			state.Disable = input.Disable
		default:
			if r.URL.Path == "/api/v2.0/robots" {
				if !strings.Contains(r.URL.Query().Get("q"), state.Name) {
					t.Error("lookup not exact configured name")
				}
				_ = json.NewEncoder(w).Encode([]harborRobot{state})
			} else {
				_ = json.NewEncoder(w).Encode(state)
			}
		}
	})
	created, err := h.CreateRobot(context.Background(), request)
	if err != nil || created.Username != state.Name || strings.Contains(fmt.Sprint(created), sentinelSecret) {
		t.Fatal("unsafe creation response", err)
	}
	owned, err := h.FindOwnedRobot(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.SetRobotSecret(context.Background(), owned, biz.Secret(sentinelSecret)); err != nil {
		t.Fatal("empty response secret is valid", err)
	}
	if err = h.SetRobotDisabled(context.Background(), owned, true); err != nil {
		t.Fatal(err)
	}
	if patches.Load() != 1 || puts.Load() != 1 {
		t.Fatal("unexpected mutation count")
	}
	state.Description = "foreign"
	if _, err = h.FindOwnedRobot(context.Background(), request); err == nil {
		t.Fatal("foreign identity adopted")
	}
	if err = h.SetRobotSecret(context.Background(), owned, biz.Secret(sentinelSecret)); err == nil {
		t.Fatal("foreign identity mutated")
	}
	if err = h.SetRobotDisabled(context.Background(), owned, false); err == nil {
		t.Fatal("foreign identity mutated")
	}
	if patches.Load() != 1 || puts.Load() != 1 {
		t.Fatal("ownership failure emitted writes")
	}
	state.Description = request.Description
	state.Permissions = append(state.Permissions, harborPermission{Kind: "system", Namespace: "*", Access: []harborAccess{{"robot", "create", "allow"}}})
	if _, err = h.GetRobot(context.Background(), 12); err == nil {
		t.Fatal("system privileges accepted")
	}
}
func TestHarborArtifactDigestAndPlatforms(t *testing.T) {
	digest := func(ch string) string { return "sha256:" + strings.Repeat(ch, 64) }
	root, child := digest("a"), digest("b")
	cases := []struct {
		name   string
		mutate func(map[string]harborArtifact)
		ok     bool
	}{
		{"manifest", func(m map[string]harborArtifact) { m[root] = m[child]; a := m[root]; a.Digest = root; m[root] = a }, true},
		{"index", func(map[string]harborArtifact) {}, true},
		{"legacy empty config type", func(m map[string]harborArtifact) { a := m[child]; a.ArtifactType = ""; m[child] = a }, true},
		{"docker image config type", func(m map[string]harborArtifact) {
			a := m[child]
			a.ManifestMediaType = dockerManifest
			a.ArtifactType = "application/vnd.docker.container.image.v1+json"
			m[child] = a
		}, true},
		{"config type mismatch", func(m map[string]harborArtifact) {
			a := m[child]
			a.ArtifactType = "application/vnd.docker.container.image.v1+json"
			m[child] = a
		}, false},
		{"attestation ignored", func(m map[string]harborArtifact) {
			a := m[root]
			a.References = append(a.References, harborReference{Digest: digest("c"), Annotations: map[string]string{"vnd.docker.reference.type": "attestation-manifest"}})
			m[root] = a
		}, true},
		{"chart", func(m map[string]harborArtifact) { a := m[child]; a.Type = "CHART"; m[child] = a }, false},
		{"artifact", func(m map[string]harborArtifact) { a := m[child]; a.ArtifactType = "application/example"; m[child] = a }, false},
		{"unknown", func(m map[string]harborArtifact) { a := m[child]; a.Extra.OS = "unknown"; m[child] = a }, false},
		{"platform mismatch", func(m map[string]harborArtifact) { a := m[child]; a.Extra.Architecture = "arm64"; m[child] = a }, false},
		{"descriptor limit", func(m map[string]harborArtifact) {
			a := m[root]
			for len(a.References) < 33 {
				a.References = append(a.References, a.References[0])
			}
			m[root] = a
		}, false},
		{"cycle", func(m map[string]harborArtifact) { a := m[root]; a.References[0].Digest = root; m[root] = a }, false},
		{"depth", func(m map[string]harborArtifact) {
			a := m[root]
			b := a
			b.Digest = child
			b.References = []harborReference{{Digest: digest("c")}}
			m[child] = b
			c := b
			c.Digest = digest("c")
			c.References = []harborReference{{Digest: digest("d")}}
			m[c.Digest] = c
			d := c
			d.Digest = digest("d")
			m[d.Digest] = d
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			leaf := harborArtifact{Digest: child, Type: "IMAGE", ManifestMediaType: ociManifest, ArtifactType: "application/vnd.oci.image.config.v1+json", RepositoryName: "t-alpha/repo", Extra: harborPlatform{OS: "linux", Architecture: "amd64"}}
			index := harborArtifact{Digest: root, Type: "IMAGE", ManifestMediaType: ociIndex, RepositoryName: "t-alpha/repo", References: []harborReference{{Digest: child, Platform: leaf.Extra}}}
			objects := map[string]harborArtifact{root: index, child: leaf}
			tc.mutate(objects)
			var tagReads, digestReads atomic.Int32
			h := testHarbor(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/api/v2.0/projects/t-alpha/repositories/repo/artifacts/") {
					t.Error("unexpected artifact endpoint (must not download layers)")
					w.WriteHeader(400)
					return
				}
				key := strings.TrimPrefix(r.URL.Path, "/api/v2.0/projects/t-alpha/repositories/repo/artifacts/")
				if key == "latest" {
					tagReads.Add(1)
					_ = json.NewEncoder(w).Encode(harborArtifact{Digest: root, RepositoryName: "t-alpha/repo", Extra: harborPlatform{OS: "fake", Architecture: "fake"}})
					return
				}
				digestReads.Add(1)
				obj, ok := objects[key]
				if !ok {
					t.Error("unexpected artifact read")
					w.WriteHeader(404)
					return
				}
				_ = json.NewEncoder(w).Encode(obj)
			})
			result, err := h.ResolveArtifact(context.Background(), "t-alpha", "repo", "latest")
			if tc.ok {
				if err != nil || result.Digest != root || len(result.Platforms) != 1 || result.Platforms[0].OS != "linux" {
					t.Fatal(result, err)
				}
			} else if err == nil {
				t.Fatal("unsupported artifact accepted")
			}
			if tagReads.Load() != 1 || digestReads.Load() < 1 {
				t.Fatal("tag not reread by digest")
			}
		})
	}
}
