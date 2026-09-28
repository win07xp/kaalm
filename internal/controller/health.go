/*
Copyright 2026 The Kaalm Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
)

// ProviderProbeResult classifies a provider liveness probe. Exactly one of
// Healthy, AuthFailed, or Err(!=nil) is the meaningful outcome. See the
// result handling in docs/src/controller/reconcilers.md (ModelProviderReconciler).
type ProviderProbeResult struct {
	// Healthy is true on a 2xx from the provider.
	Healthy bool
	// AuthFailed is true on a 401 or 403: the credential is invalid. For
	// google-vertex it is also set when the Secret holds no usable
	// service-account key or the token endpoint refuses it; Err then carries
	// the detail.
	AuthFailed bool
	// Err is a network error or a 5xx: transient, does not flip Ready.
	Err error
}

// ProviderHealthChecker probes an upstream LLM provider for liveness. It is an
// interface so reconcilers can be tested with a fake and never reach a real API.
type ProviderHealthChecker interface {
	Probe(ctx context.Context, provider *kaalmv1beta1.ModelProvider, credential string) ProviderProbeResult
}

// defaultHealthTimeout bounds a single liveness probe when the provider does not
// set healthCheck.timeoutSeconds.
const defaultHealthTimeout = 10 * time.Second

// HTTPProviderHealthChecker is the real checker. It issues a token-authenticated
// GET to the provider's model-list endpoint and classifies the response.
type HTTPProviderHealthChecker struct {
	// Client is the HTTP client. If nil, a client bounded by the provider's
	// healthCheck.timeoutSeconds (default 10s) is used per probe.
	Client *http.Client
}

// healthCheckTimeout returns the per-probe timeout: healthCheck.timeoutSeconds
// when set, otherwise defaultHealthTimeout.
func healthCheckTimeout(provider *kaalmv1beta1.ModelProvider) time.Duration {
	if hc := provider.Spec.HealthCheck; hc != nil && hc.TimeoutSeconds > 0 {
		return time.Duration(hc.TimeoutSeconds) * time.Second
	}
	return defaultHealthTimeout
}

// Probe implements ProviderHealthChecker.
func (h *HTTPProviderHealthChecker) Probe(
	ctx context.Context, provider *kaalmv1beta1.ModelProvider, credential string,
) ProviderProbeResult {
	timeout := healthCheckTimeout(provider)
	cl := h.Client
	if cl == nil {
		cl = &http.Client{
			Timeout: timeout,
			// Probes never follow redirects: Go's cross-host header stripping
			// does not cover x-api-key, and a redirecting endpoint is not a
			// healthy one anyway. A refusal classifies as a transient error.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("provider health probes do not follow redirects")
			},
		}
	}
	// Bound the request by the configured timeout via the context too, so an
	// injected Client (tests) and the default client both honor the field.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	base := strings.TrimSuffix(provider.Spec.Endpoint, "/")
	var req *http.Request
	var err error
	switch provider.Spec.Type {
	case "anthropic":
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
		if err != nil {
			return ProviderProbeResult{Err: err}
		}
		req.Header.Set("x-api-key", credential)
		req.Header.Set("anthropic-version", "2023-06-01")
	case "openai", "openai-compatible":
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
		if err != nil {
			return ProviderProbeResult{Err: err}
		}
		req.Header.Set("Authorization", "Bearer "+credential)
	case "google-vertex":
		var failed *ProviderProbeResult
		if req, failed = vertexProbeRequest(ctx, cl, provider.Spec.Endpoint, credential); failed != nil {
			return *failed
		}
	default:
		return ProviderProbeResult{Err: fmt.Errorf("unknown provider type %q", provider.Spec.Type)}
	}

	resp, err := cl.Do(req)
	if err != nil {
		return ProviderProbeResult{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return ProviderProbeResult{Healthy: true}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ProviderProbeResult{AuthFailed: true}
	default:
		return ProviderProbeResult{Err: fmt.Errorf("provider returned HTTP %d", resp.StatusCode)}
	}
}
