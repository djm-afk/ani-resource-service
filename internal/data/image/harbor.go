package data

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
)

// HarborConfig is supplied only by the composition root, never by an RPC.
// RobotNamePrefix must match the deployed Harbor configuration; usernames still
// come from Harbor responses. The adapter never assumes the default prefix.
type HarborConfig struct {
	URL, Username, RobotNamePrefix string
	Password                       biz.Secret
	CAPEM                          []byte
	Timeout                        time.Duration
}
type Harbor struct {
	base                  *url.URL
	client                *http.Client
	username, robotPrefix string
	password              biz.Secret
}

const maxHarborBody = 4 << 20

func NewHarbor(c HarborConfig) (*Harbor, error) {
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || c.Username == "" || strings.ContainsAny(c.Username, ":\r\n") || len(c.Password) == 0 || !regexp.MustCompile(`^[A-Za-z0-9_$-]{1,64}$`).MatchString(c.RobotNamePrefix) {
		return nil, fmt.Errorf("invalid Image Harbor configuration")
	}
	if c.Timeout == 0 {
		c.Timeout = 10 * time.Second
	}
	if c.Timeout <= 0 || c.Timeout > 30*time.Second {
		return nil, fmt.Errorf("invalid Image Harbor timeout")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, fmt.Errorf("Harbor trust store unavailable")
	}
	if len(c.CAPEM) > 0 && !roots.AppendCertsFromPEM(c.CAPEM) {
		return nil, fmt.Errorf("invalid Image Harbor CA")
	}
	tr := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: c.Timeout, KeepAlive: 30 * time.Second}).DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, TLSHandshakeTimeout: c.Timeout, ResponseHeaderTimeout: c.Timeout, MaxResponseHeaderBytes: 64 << 10, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second}
	u.Path = ""
	return &Harbor{base: u, username: c.Username, password: append(biz.Secret(nil), c.Password...), robotPrefix: c.RobotNamePrefix, client: &http.Client{Transport: tr, Timeout: c.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (h *Harbor) Close() { h.client.CloseIdleConnections(); clear(h.password) }
func harborUnavailable() error {
	return biz.Fail(biz.DependencyUnavailable, "Image registry unavailable")
}

// Keep the HTTP result internal to this adapter. Domain reasons alone cannot
// distinguish a complete rejection response from an uncertain write outcome.
type harborResponseError struct {
	status int
	cause  error
}

func (e *harborResponseError) Error() string { return e.cause.Error() }
func (e *harborResponseError) Unwrap() error { return e.cause }

// Bodies, credentials, response headers and URL errors never enter error text.
func mapHarborError(status int) error {
	switch status {
	case 400:
		return biz.Fail(biz.InvalidArgument, "registry rejected request")
	case 401, 403:
		return biz.Fail(biz.PermissionDenied, "registry operation denied")
	case 404:
		return biz.Fail(biz.ImageNotFound, "registry object not found")
	case 409:
		return biz.Fail(biz.IdempotencyConflict, "registry object conflicts")
	default:
		return harborUnavailable()
	}
}

// Automatic retry is restricted to one GET retry for 429/502/503/504. Mutating
// requests are never replayed here: the persisted command owns recovery.
func (h *Harbor) doJSON(ctx context.Context, method, path string, in, out any) (http.Header, error) {
	if !strings.HasPrefix(path, "/api/v2.0/") || strings.HasPrefix(path, "//") {
		return nil, harborUnavailable()
	}
	var body []byte
	if in != nil {
		var err error
		body, err = json.Marshal(in)
		if err != nil {
			return nil, harborUnavailable()
		}
		defer clear(body)
	}
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, h.base.String()+path, bytes.NewReader(body))
		if err != nil {
			return nil, harborUnavailable()
		}
		req.SetBasicAuth(h.username, string(h.password))
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Resource-Name-In-Location", "false")
		res, err := h.client.Do(req)
		if err != nil {
			var timeout net.Error
			if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
				return nil, biz.Fail(biz.DeadlineExceeded, "registry request canceled or expired")
			}
			return nil, harborUnavailable()
		}
		data, readErr := io.ReadAll(io.LimitReader(res.Body, maxHarborBody+1))
		res.Body.Close()
		if readErr != nil || len(data) > maxHarborBody {
			clear(data)
			return nil, harborUnavailable()
		}
		if method == http.MethodGet && attempt == 0 && (res.StatusCode == 429 || res.StatusCode == 502 || res.StatusCode == 503 || res.StatusCode == 504) {
			clear(data)
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, biz.Fail(biz.DeadlineExceeded, "registry request canceled or expired")
			}
			continue
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			clear(data)
			return nil, &harborResponseError{status: res.StatusCode, cause: mapHarborError(res.StatusCode)}
		}
		if out != nil {
			if err = json.Unmarshal(data, out); err != nil {
				clear(data)
				return nil, harborUnavailable()
			}
		}
		clear(data)
		return res.Header, nil
	}
	return nil, harborUnavailable()
}

var _ biz.Registry = (*Harbor)(nil)
