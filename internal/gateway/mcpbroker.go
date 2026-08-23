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

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/mcp"
)

// The MCP broker: POST /v1/mcp/{toolProvider} on the mTLS listener, under
// the same dual-mode auth as the LLM proxy paths. It authenticates the
// workload, enforces the grant chain at call time, injects the tool server
// credential, wraps session ids, and relays. See
// docs/src/gateways/tool-plane.md (The Broker).

// errNoRedirects marks a refused upstream redirect: the broker never follows
// them, closing the confused-deputy path a compromised server could open.
var errNoRedirects = errors.New("mcp broker does not follow redirects")

// mcpAllowedMethod is the v0.4.0 method allowlist. The broker governs the
// tool surface; JSON-RPC surfaces the grant chain has no vocabulary for
// (resources, prompts, sampling) are denied rather than silently proxied.
// Widening is additive and tracked for a future milestone.
func mcpAllowedMethod(method string) bool {
	switch method {
	case "initialize", "ping", "tools/list", "tools/call":
		return true
	}
	return strings.HasPrefix(method, "notifications/")
}

// toolFilter is a caller's effective tool set: grant narrowing intersected
// with the provider's declared catalog. A nil allow map permits every tool.
type toolFilter struct {
	allow map[string]bool
}

func (f *toolFilter) permits(name string) bool {
	return f.allow == nil || f.allow[name]
}

// newToolFilter intersects an optional grant narrowing with an optional
// declared catalog. Either being empty means it does not constrain.
func newToolFilter(grantTools []string, catalog []kaalmv1beta1.ToolProviderTool) *toolFilter {
	catalogSet := map[string]bool{}
	for _, t := range catalog {
		catalogSet[t.ID] = true
	}
	switch {
	case len(grantTools) == 0 && len(catalogSet) == 0:
		return &toolFilter{}
	case len(grantTools) == 0:
		return &toolFilter{allow: catalogSet}
	default:
		allow := map[string]bool{}
		for _, t := range grantTools {
			if len(catalogSet) == 0 || catalogSet[t] {
				allow[t] = true
			}
		}
		return &toolFilter{allow: allow}
	}
}

// authorizeToolRoute enforces the tenancy chain for one brokered call and
// returns the caller's effective tool filter. It mirrors authorizeRoute:
// namespace gate first, then the workload grant and class allowlist for
// callers that carry a workload identity. Gateway-only callers reduce to
// allowedNamespaces with the full server set, per the tool plane chapter.
func (s *Server) authorizeToolRoute(
	ctx context.Context, c *caller, tp *kaalmv1beta1.ToolProvider,
) (*toolFilter, *routeDenial) {
	if !namespaceGlobAllowed(c.Namespace, tp.Spec.AllowedNamespaces) {
		return nil, &routeDenial{http.StatusForbidden, errAccessDenied,
			fmt.Sprintf("namespace %q is not allowed to use tool provider %q", c.Namespace, tp.Name)}
	}
	if c.Workload == nil {
		return newToolFilter(nil, tp.Spec.Tools), nil
	}

	var grants []kaalmv1beta1.AgentToolGrant
	var className string
	switch c.Workload.Kind {
	case KindAgent:
		agent, ok := s.Store.AgentByName(ctx, c.Namespace, c.Workload.Name)
		if !ok {
			return nil, &routeDenial{http.StatusForbidden, errAccessDenied, "workload not found"}
		}
		grants, className = agent.Spec.Tools, agent.Spec.AgentClassRef.Name
	case KindAgentTask:
		task, ok := s.Store.TaskByName(ctx, c.Namespace, c.Workload.Name)
		if !ok {
			return nil, &routeDenial{http.StatusForbidden, errAccessDenied, "workload not found"}
		}
		grants, className = task.Spec.Tools, task.Spec.AgentClassRef.Name
	default:
		return nil, &routeDenial{http.StatusForbidden, errAccessDenied, "unrecognized workload kind"}
	}

	var grant *kaalmv1beta1.AgentToolGrant
	for i := range grants {
		if grants[i].ProviderRef.Name == tp.Name {
			grant = &grants[i]
			break
		}
	}
	if grant == nil {
		return nil, &routeDenial{http.StatusForbidden, errAccessDenied,
			fmt.Sprintf("workload has no tool grant for provider %q", tp.Name)}
	}

	class, ok := s.Store.ClassByName(ctx, className)
	if !ok {
		return nil, &routeDenial{http.StatusForbidden, errAccessDenied,
			fmt.Sprintf("AgentClass %q not found", className)}
	}
	allowed := false
	for _, ref := range class.Spec.AllowedToolProviders {
		if ref.Name == tp.Name {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, &routeDenial{http.StatusForbidden, errAccessDenied,
			fmt.Sprintf("tool provider %q is not in AgentClass %q allowedToolProviders", tp.Name, className)}
	}
	return newToolFilter(grant.Tools, tp.Spec.Tools), nil
}

// mcpRequest is the slice of a JSON-RPC request the broker reads to govern.
type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// mcpResult is the single funnel every terminal broker outcome passes
// through. It emits the per-call audit record (one info-level structured log
// line, never bodies) and the broker metrics. tp is nil when the call died
// before the ToolProvider resolved; forwarded marks calls that reached the
// upstream and gates the duration histogram, which observes real upstream
// latency rather than microsecond-scale local denials. See
// docs/src/gateways/tool-plane.md (Audit and Metering).
func (s *Server) mcpResult(
	c *caller, tp *kaalmv1beta1.ToolProvider, provider, method, tool string,
	status int, errType, detail string, start time.Time, reqBytes, respBytes int64, forwarded bool,
) {
	seconds := time.Since(start).Seconds()
	attrs := []any{"namespace", c.Namespace, "provider", provider,
		"method", method, "tool", tool, "status", status, "error_type", errType,
		"duration_seconds", seconds, "request_bytes", reqBytes, "response_bytes", respBytes}
	if c.Workload != nil {
		attrs = append(attrs, "workload", c.Workload.Name, "workload_kind", string(c.Workload.Kind))
	}
	// The denial reason (for example which gate refused, or that a session id
	// belongs to another caller) so same-typed denials stay distinguishable.
	if detail != "" {
		attrs = append(attrs, "detail", detail)
	}
	slog.Info("mcp call", attrs...)

	statusLabel := errType
	if statusLabel == "" {
		if status >= 200 && status < 300 {
			statusLabel = "ok"
		} else {
			statusLabel = "upstream_error"
		}
	}
	toolLabel := boundedToolLabel(tp, tool)
	s.Metrics.ToolCall(provider, c.Namespace, toolLabel, statusLabel)
	if forwarded {
		s.Metrics.ToolCallDuration(provider, toolLabel, seconds)
	}
}

// boundedToolLabel bounds the metric's tool label by the DECLARED catalog:
// with spec.tools present, cataloged ids pass verbatim and anything else
// collapses to "uncataloged"; without one, every tool collapses, because
// wire-supplied names are unbounded and a compromised server could inflate
// the label set at will. The audit record carries the real name regardless.
func boundedToolLabel(tp *kaalmv1beta1.ToolProvider, tool string) string {
	if tool == "" {
		return ""
	}
	if tp != nil {
		for _, t := range tp.Spec.Tools {
			if t.ID == tool {
				return tool
			}
		}
	}
	return "uncataloged"
}

// handleMCPBroker serves POST /v1/mcp/{toolProvider}.
func (s *Server) handleMCPBroker(w http.ResponseWriter, r *http.Request) {
	c := callerFrom(r.Context())
	providerName := strings.TrimPrefix(r.URL.Path, "/v1/mcp/")

	// Outcome state the deny closure funnels into mcpResult: tp stays nil
	// until resolved, reqBytes zero until the body is read, and forwarded
	// flips once the call reaches the upstream.
	start := time.Now()
	var tp *kaalmv1beta1.ToolProvider
	var reqBytes int64
	forwarded := false

	// tctx becomes the tool.call span context once the route is authorized;
	// the deny closure captures the variable, so late denials carry the
	// error status onto the span (early ones hit the noop span, harmlessly).
	tctx := r.Context()

	deny := func(status int, errType, message string, retryAfter int, method, tool string) {
		spanError(tctx, errType)
		s.mcpResult(c, tp, providerName, method, tool, status, errType, message, start, reqBytes, 0, forwarded)
		writeError(w, status, errorBody{Type: errType, Message: message,
			Provider: providerName, Retryable: retryAfter > 0 || status == http.StatusServiceUnavailable}, retryAfter)
	}

	if r.Method != http.MethodPost {
		deny(http.StatusMethodNotAllowed, errInvalidRequest, "MCP broker accepts POST only", 0, "", "")
		return
	}
	if providerName == "" || strings.Contains(providerName, "/") {
		deny(http.StatusBadRequest, errInvalidRequest, "path must be /v1/mcp/{toolProvider}", 0, "", "")
		return
	}
	resolved, ok := s.Store.ToolProviderByName(r.Context(), providerName)
	if !ok {
		deny(http.StatusBadRequest, errInvalidRequest,
			fmt.Sprintf("unknown tool provider %q", providerName), 0, "", "")
		return
	}
	tp = resolved

	filter, denial := s.authorizeToolRoute(r.Context(), c, tp)
	if denial != nil {
		deny(denial.status, denial.errType, denial.message, 0, "", "")
		return
	}

	if !s.RateLimiter.AllowTool(tp, c.Namespace) {
		deny(http.StatusTooManyRequests, errRateLimited,
			fmt.Sprintf("rate limit exceeded for namespace %s on tool provider %s", c.Namespace, tp.Name), 1, "", "")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.mcpMaxBodyBytes()))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			deny(http.StatusRequestEntityTooLarge, errRequestTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", s.mcpMaxBodyBytes()), 0, "", "")
			return
		}
		deny(http.StatusBadRequest, errInvalidRequest, "reading request body: "+err.Error(), 0, "", "")
		return
	}
	reqBytes = int64(len(body))
	bodyLog("mcp request", body)
	if trimmed := bytes.TrimLeft(body, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == '[' {
		deny(http.StatusBadRequest, errInvalidRequest, "JSON-RPC batch requests are not supported", 0, "", "")
		return
	}
	var msg mcpRequest
	if err := json.Unmarshal(body, &msg); err != nil {
		deny(http.StatusBadRequest, errInvalidRequest, "request body is not a JSON-RPC message", 0, "", "")
		return
	}

	if !mcpAllowedMethod(msg.Method) {
		deny(http.StatusForbidden, errToolDenied,
			fmt.Sprintf("method %q is not brokered in this version; the tool plane governs the tool surface", msg.Method),
			0, msg.Method, "")
		return
	}

	toolName := ""
	if msg.Method == "tools/call" {
		var params struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(msg.Params, &params)
		toolName = params.Name
		if !filter.permits(toolName) {
			deny(http.StatusForbidden, errToolDenied,
				fmt.Sprintf("tool %q is not granted to this workload", toolName), 0, msg.Method, toolName)
			return
		}
	}

	// tool.call parents onto whatever context the caller propagated; its
	// tool.forward child below covers the upstream half.
	var endSpan func(error)
	tctx, endSpan = s.Tracing.Start(s.Tracing.Extract(tctx, r.Header), "tool.call",
		trace.SpanKindServer,
		attribute.String("kaalm.provider", tp.Name),
		attribute.String("kaalm.namespace", c.Namespace),
		attribute.String("kaalm.workload", workloadKey(c)),
		attribute.String("kaalm.method", msg.Method),
		attribute.String("kaalm.tool", toolName))
	defer endSpan(nil)

	// Session ownership: never forward a wrapped id, never accept one bound
	// to a different caller.
	identity := callerIdentity(c)
	upstreamSession := ""
	if wrapped := r.Header.Get("Mcp-Session-Id"); wrapped != "" {
		raw, ok := unwrapSessionID(s.Config.SessionKey, wrapped, identity)
		if !ok {
			deny(http.StatusForbidden, errAccessDenied,
				"session id is not owned by this caller", 0, msg.Method, toolName)
			return
		}
		upstreamSession = raw
	}

	credential, err := s.Store.ToolCredential(r.Context(), tp)
	if err != nil {
		slog.Warn("mcp credential unavailable", "provider", tp.Name, "error", err)
		deny(http.StatusServiceUnavailable, errToolUnavailable,
			"tool provider credential is unavailable", 0, msg.Method, toolName)
		return
	}

	ctx, cancel := context.WithTimeout(tctx, s.mcpUpstreamTimeout())
	defer cancel()
	fctx, endForward := s.Tracing.Start(ctx, "tool.forward", trace.SpanKindClient,
		attribute.String("kaalm.provider", tp.Name))
	upReq, err := http.NewRequestWithContext(fctx, http.MethodPost, tp.Spec.Endpoint, bytes.NewReader(body))
	if err != nil {
		endForward(err)
		deny(http.StatusBadRequest, errInvalidRequest, "building upstream request: "+err.Error(), 0, msg.Method, toolName)
		return
	}
	copyMCPHeaders(upReq, r, upstreamSession, credential)

	forwarded = true
	resp, err := s.mcpHTTPClient().Do(upReq)
	endForward(err)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			deny(http.StatusGatewayTimeout, errToolTimeout,
				fmt.Sprintf("tool provider %q did not answer within the upstream timeout", tp.Name), 0, msg.Method, toolName)
		default:
			deny(http.StatusServiceUnavailable, errToolUnavailable,
				fmt.Sprintf("tool provider %q is unreachable: %v", tp.Name, err), 1, msg.Method, toolName)
		}
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// The gateway's credential being rejected is an operator problem, not
	// the caller's: surface unavailability, and let the ToolProvider health
	// probe flip the resource's conditions independently.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		slog.Warn("tool server rejected the gateway credential", "provider", tp.Name, "status", resp.StatusCode)
		deny(http.StatusServiceUnavailable, errToolUnavailable,
			fmt.Sprintf("tool provider %q rejected the gateway credential", tp.Name), 0, msg.Method, toolName)
		return
	}
	if resp.StatusCode >= 500 {
		deny(http.StatusServiceUnavailable, errToolUnavailable,
			fmt.Sprintf("tool provider %q returned HTTP %d", tp.Name, resp.StatusCode), 1, msg.Method, toolName)
		return
	}

	// Wrap any upstream session id before headers flush, on every remaining
	// path (including relayed 4xx, where MCP session semantics live).
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		w.Header().Set("Mcp-Session-Id", wrapSessionID(s.Config.SessionKey, sid, identity))
	}

	var respBytes int64
	var relayStatus int
	var relayErrType string
	switch {
	case msg.Method == "tools/list" && resp.StatusCode < 300:
		respBytes, relayStatus, relayErrType = s.relayFilteredToolsList(w, resp, msg, filter, providerName)
	case strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream"):
		respBytes = relayMCPStream(w, r, resp, s.mcpMaxBodyBytes())
		relayStatus = resp.StatusCode
	default:
		respBytes, relayStatus, relayErrType = relayMCPBuffered(w, resp, s.mcpMaxBodyBytes())
	}
	s.mcpResult(c, tp, providerName, msg.Method, toolName, relayStatus, relayErrType, "",
		start, reqBytes, respBytes, forwarded)
}

// relayFilteredToolsList buffers a tools/list response (either encoding),
// filters the tool set to the caller's grant, and replies as plain JSON: the
// model never sees a tool it cannot call. It returns the outcome triple the
// caller funnels into mcpResult.
func (s *Server) relayFilteredToolsList(
	w http.ResponseWriter, resp *http.Response, msg mcpRequest, filter *toolFilter, providerName string,
) (respBytes int64, status int, errType string) {
	parsed, err := mcp.ParseResponse(resp.Header.Get("Content-Type"),
		io.LimitReader(resp.Body, s.mcpMaxBodyBytes()), msg.ID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errorBody{Type: errToolUnavailable,
			Message: "tool provider returned an unparseable tools/list response", Provider: providerName, Retryable: true}, 0)
		return 0, http.StatusServiceUnavailable, errToolUnavailable
	}
	if parsed.Error == nil && parsed.Result != nil {
		var result map[string]json.RawMessage
		if err := json.Unmarshal(parsed.Result, &result); err == nil {
			var tools []json.RawMessage
			_ = json.Unmarshal(result["tools"], &tools)
			kept := make([]json.RawMessage, 0, len(tools))
			for _, raw := range tools {
				var t struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(raw, &t) == nil && filter.permits(t.Name) {
					kept = append(kept, raw)
				}
			}
			keptRaw, _ := json.Marshal(kept)
			result["tools"] = keptRaw
			newResult, _ := json.Marshal(result)
			parsed.Result = newResult
		}
	}
	encoded, err := json.Marshal(parsed)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errorBody{Type: errInternalUnavailable,
			Message: "re-encoding tools/list response", Provider: providerName, Retryable: true}, 0)
		return 0, http.StatusInternalServerError, errInternalUnavailable
	}
	bodyLog("mcp response", encoded)
	w.Header().Set("Content-Type", "application/json")
	n, _ := w.Write(encoded)
	return int64(n), http.StatusOK, ""
}

// relayMCPBuffered copies a JSON response through, capped. It returns the
// outcome triple the caller funnels into mcpResult.
func relayMCPBuffered(w http.ResponseWriter, resp *http.Response, maxBytes int64) (respBytes int64, status int, errType string) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errorBody{Type: errToolUnavailable,
			Message: "reading tool provider response: " + err.Error(), Retryable: true}, 0)
		return 0, http.StatusServiceUnavailable, errToolUnavailable
	}
	if int64(len(body)) > maxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, errorBody{Type: errResponseTooLarge,
			Message: fmt.Sprintf("tool provider response exceeds %d bytes", maxBytes)}, 0)
		return 0, http.StatusRequestEntityTooLarge, errResponseTooLarge
	}
	bodyLog("mcp response", body)
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	n, _ := w.Write(body)
	return int64(n), resp.StatusCode, ""
}

// relayMCPStream forwards SSE events as they arrive, flushing per line,
// bounded by the response cap and the caller's disconnect. It returns the
// bytes relayed downstream.
func relayMCPStream(w http.ResponseWriter, r *http.Request, resp *http.Response, maxBytes int64) int64 {
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)

	var written int64
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, maxBytes))
	scanner.Buffer(make([]byte, 0, 64*1024), int(maxBytes))
	for scanner.Scan() {
		select {
		case <-r.Context().Done():
			return written
		default:
		}
		line := scanner.Bytes()
		bodyLog("mcp stream", line)
		n, err := w.Write(append(line, '\n'))
		written += int64(n)
		if err != nil {
			return written
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	return written
}

// copyMCPHeaders applies the forwarded-header contract: hop-by-hop and
// inbound auth material stripped, the tool credential injected, the raw
// upstream session id restored.
func copyMCPHeaders(upReq *http.Request, r *http.Request, upstreamSession, credential string) {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		upReq.Header.Set("Content-Type", ct)
	} else {
		upReq.Header.Set("Content-Type", "application/json")
	}
	if accept := r.Header.Get("Accept"); accept != "" {
		upReq.Header.Set("Accept", accept)
	} else {
		upReq.Header.Set("Accept", "application/json, text/event-stream")
	}
	if pv := r.Header.Get("MCP-Protocol-Version"); pv != "" {
		upReq.Header.Set("MCP-Protocol-Version", pv)
	}
	if upstreamSession != "" {
		upReq.Header.Set("Mcp-Session-Id", upstreamSession)
	}
	if credential != "" {
		upReq.Header.Set("Authorization", "Bearer "+credential)
	}
}

func (s *Server) mcpMaxBodyBytes() int64 {
	if s.Config.MCPMaxBodyBytes > 0 {
		return s.Config.MCPMaxBodyBytes
	}
	return s.Config.MaxBodyBytes
}
