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

// ToolProbeResult is a liveness probe outcome plus the MCP revision the
// probe negotiated, recorded on the ToolProvider's status when healthy.
type ToolProbeResult struct {
	ProviderProbeResult
	// MCPRevision is the negotiated protocol revision: mcp.ModernRevision
	// when server/discover succeeded, the version the initialize handshake
	// returned otherwise. Empty unless the probe was healthy.
	MCPRevision string
}

// ToolHealthChecker probes an upstream tool server for liveness. It is an
// interface so reconcilers can be tested with a fake and never reach a real
// server. The embedded classification contract is identical to the
// ModelProvider probe's.
type ToolHealthChecker interface {
	Probe(ctx context.Context, provider *kaalmv1beta1.ToolProvider, credential string) ToolProbeResult
}

// MCPToolHealthChecker is the real checker. The probe speaks the protocol
// it governs, in whichever revision the server does: server/discover
// selects the stateless 2026-07-28 form and the probe completes with a
// _meta-versioned tools/list; any answer that is not a recognized modern
// JSON-RPC error selects the legacy form, the initialize handshake
// followed by tools/list (docs/src/gateways/tool-plane.md, Protocol
// Revisions).
type MCPToolHealthChecker struct {
	// Client is the HTTP client. If nil, a client bounded by the provider's
	// healthCheck.timeoutSeconds (default 10s) is used per probe.
	Client *http.Client
}

// Probe implements ToolHealthChecker.
func (h *MCPToolHealthChecker) Probe(
	ctx context.Context, provider *kaalmv1beta1.ToolProvider, credential string,
) ToolProbeResult {
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
	session, err := client.Discover(ctx)
	if errors.Is(err, mcp.ErrLegacyServer) {
		session, err = client.Initialize(ctx)
	}
	if err == nil {
		_, err = client.ListTools(ctx, session)
	}
	switch {
	case err == nil:
		return ToolProbeResult{ProviderProbeResult: ProviderProbeResult{Healthy: true},
			MCPRevision: session.ProtocolVersion}
	case isAuthError(err):
		return ToolProbeResult{ProviderProbeResult: ProviderProbeResult{AuthFailed: true}}
	default:
		return ToolProbeResult{ProviderProbeResult: ProviderProbeResult{Err: err}}
	}
}

// isAuthError reports whether the server rejected the credential.
func isAuthError(err error) bool {
	var httpErr *mcp.HTTPError
	return errors.As(err, &httpErr) &&
		(httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden)
}
