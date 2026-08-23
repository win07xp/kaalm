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
	"net/http"
	"time"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/mcp"
)

// ToolHealthChecker probes an upstream tool server for liveness. It is an
// interface so reconcilers can be tested with a fake and never reach a real
// server. Results reuse ProviderProbeResult; the classification contract is
// identical to the ModelProvider probe's.
type ToolHealthChecker interface {
	Probe(ctx context.Context, provider *kaalmv1beta1.ToolProvider, credential string) ProviderProbeResult
}

// MCPToolHealthChecker is the real checker. The probe speaks the protocol it
// governs: an MCP initialize handshake followed by tools/list
// (docs/src/gateways/tool-plane.md, The ToolProvider Resource).
type MCPToolHealthChecker struct {
	// Client is the HTTP client. If nil, a client bounded by the provider's
	// healthCheck.timeoutSeconds (default 10s) is used per probe.
	Client *http.Client
}

// Probe implements ToolHealthChecker.
func (h *MCPToolHealthChecker) Probe(
	ctx context.Context, provider *kaalmv1beta1.ToolProvider, credential string,
) ProviderProbeResult {
	timeout := defaultHealthTimeout
	if hc := provider.Spec.HealthCheck; hc != nil && hc.TimeoutSeconds > 0 {
		timeout = time.Duration(hc.TimeoutSeconds) * time.Second
	}
	cl := h.Client
	if cl == nil {
		cl = &http.Client{Timeout: timeout}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := &mcp.Client{Endpoint: provider.Spec.Endpoint, Credential: credential, HTTPClient: cl}
	session, err := client.Initialize(ctx)
	if err == nil {
		_, err = client.ListTools(ctx, session)
	}
	switch {
	case err == nil:
		return ProviderProbeResult{Healthy: true}
	case isAuthError(err):
		return ProviderProbeResult{AuthFailed: true}
	default:
		return ProviderProbeResult{Err: err}
	}
}

// isAuthError reports whether the server rejected the credential.
func isAuthError(err error) bool {
	var httpErr *mcp.HTTPError
	return errors.As(err, &httpErr) &&
		(httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden)
}
