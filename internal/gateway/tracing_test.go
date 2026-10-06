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
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// newTestTracing wires the Tracing type onto an in-memory exporter with
// synchronous export, so spans are inspectable the moment a request returns.
func newTestTracing(exp sdktrace.SpanExporter) *Tracing {
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	return &Tracing{tracer: tp.Tracer("test"), prop: propagation.TraceContext{}, tp: tp}
}

// End-to-end tracing: one channel message yields one trace whose spans connect
// across all three hops. The fake agent copies the delivery's traceparent
// onto its LLM call, exactly what the runtime does, so the chain is
// channel.receive > agent.deliver > llm.request > llm.forward.
func TestTracing_OneMessageOneConnectedTrace(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tr := newTestTracing(exp)

	lh := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	lh.seedRoute()
	lh.server.Tracing = tr
	agentC := agentCert(t, lh.ca)
	llmClient := lh.client(&agentC)

	uh := newUserHarness(t, func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequest(http.MethodPost, lh.url("/v1/chat/completions"),
			strings.NewReader(`{"model":"prov/m1"}`))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("traceparent", r.Header.Get("Traceparent"))
		if ts := r.Header.Get("Tracestate"); ts != "" {
			req.Header.Set("tracestate", ts)
		}
		resp, err := llmClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":"done"}`))
	})
	uh.seedChannel("sync")
	uh.server.Tracing = tr

	resp := uh.post(t, "/channels/team-a/support", "hook-token", []byte(`{"content":"hi"}`))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sync delivery = %d", resp.StatusCode)
	}

	byName := map[string]tracetest.SpanStub{}
	for _, s := range exp.GetSpans() {
		byName[s.Name] = s
	}
	for _, name := range []string{"channel.receive", "agent.deliver", "llm.request", "llm.forward"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("span %s missing; got %v", name, spanNames(exp))
		}
	}
	traceID := byName["channel.receive"].SpanContext.TraceID()
	for name, s := range byName {
		if s.SpanContext.TraceID() != traceID {
			t.Errorf("span %s is on trace %s, want %s (one message, one trace)", name, s.SpanContext.TraceID(), traceID)
		}
	}
	assertChild(t, byName, "agent.deliver", "channel.receive")
	assertChild(t, byName, "llm.request", "agent.deliver")
	assertChild(t, byName, "llm.forward", "llm.request")
}

// Tool broker spans parent onto whatever context the caller propagated, so a
// tool call made while handling a message lands in the message's trace.
func TestTracing_ToolCallSpansParentOntoCallerContext(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tr := newTestTracing(exp)

	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":7,"result":{"content":[]}}`))
	})
	h.seedToolRoute()
	h.server.Tracing = tr
	agentC := agentCert(t, h.ca)

	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	resp := postJSON(t, h.client(&agentC), h.url("/v1/mcp/search"), mcpCall("web_search"),
		map[string]string{"traceparent": parent})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tool call = %d", resp.StatusCode)
	}

	byName := map[string]tracetest.SpanStub{}
	for _, s := range exp.GetSpans() {
		byName[s.Name] = s
	}
	call, ok := byName["tool.call"]
	if !ok {
		t.Fatalf("tool.call span missing; got %v", spanNames(exp))
	}
	if got := call.SpanContext.TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("tool.call trace = %s, want the propagated one", got)
	}
	if got := call.Parent.SpanID().String(); got != "00f067aa0ba902b7" {
		t.Errorf("tool.call parent = %s, want the caller's span", got)
	}
	assertChild(t, byName, "tool.forward", "tool.call")
}

// A relay failure marks the tool.call span with its error type, as a
// denial does; a stream cut at the cap is otherwise a 200.
func TestTracing_ToolCallRelayFailureMarksSpan(t *testing.T) {
	toolsList := map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"}
	cases := []struct {
		name     string
		upstream http.HandlerFunc
		body     map[string]any
		code     codes.Code
		desc     string
	}{
		{"buffered response too large", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":7,"result":{"blob":%q}}`, strings.Repeat("y", 4096))
		}, mcpCall("web_search"), codes.Error, errResponseTooLarge},
		{"buffered response unreadable", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "100")
			_, _ = fmt.Fprint(w, `{"jsonrpc"`)
		}, mcpCall("web_search"), codes.Error, errToolUnavailable},
		{"tools/list too large", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"web_search","description":%q}]}}`,
				strings.Repeat("d", 4096))
		}, toolsList, codes.Error, errResponseTooLarge},
		{"tools/list unparseable", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, "not json")
		}, toolsList, codes.Error, errToolUnavailable},
		{"stream passes the cap", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n"+
				"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"blob\":\""+
				strings.Repeat("z", 4096)+"\"}}\n\n")
		}, mcpCall("web_search"), codes.Error, errResponseTooLarge},
		{"stream breaks partway", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Content-Length", "4096")
			_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
		}, mcpCall("web_search"), codes.Error, errToolUnavailable},
		{"upstream 5xx denied", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}, mcpCall("web_search"), codes.Error, errToolUnavailable},
		{"success", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"result":{"content":[]}}`)
		}, mcpCall("web_search"), codes.Unset, ""},
		{"upstream 4xx relayed", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}, mcpCall("web_search"), codes.Unset, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exp := tracetest.NewInMemoryExporter()
			h := newHarness(t, c.upstream)
			h.seedToolRoute()
			h.server.Tracing = newTestTracing(exp)
			h.server.Config.MCPMaxBodyBytes = 1024
			cert := agentCert(t, h.ca)
			resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), c.body, nil)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			var call *tracetest.SpanStub
			for _, s := range exp.GetSpans() {
				if s.Name == "tool.call" {
					call = &s
				}
			}
			if call == nil {
				t.Fatalf("tool.call span missing; got %v", spanNames(exp))
			}
			if call.Status.Code != c.code || call.Status.Description != c.desc {
				t.Errorf("tool.call status = (%v, %q), want (%v, %q)",
					call.Status.Code, call.Status.Description, c.code, c.desc)
			}
		})
	}
}

// spanNamed returns the exported span called name.
func spanNamed(t *testing.T, exp *tracetest.InMemoryExporter, name string) tracetest.SpanStub {
	t.Helper()
	for _, s := range exp.GetSpans() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("span %s missing; got %v", name, spanNames(exp))
	return tracetest.SpanStub{}
}

// An upstream 4xx the gateway relays unchanged leaves the llm.request
// server span unset, as tool.call does; the llm.forward client span carries
// the failure.
func TestTracing_LLMRelayedUpstream4xxLeavesRequestUnset(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	})
	h.seedRoute()
	h.server.Tracing = newTestTracing(exp)
	cert := agentCert(t, h.ca)
	resp := postJSON(t, h.client(&cert), h.url("/v1/chat/completions"), map[string]any{"model": "prov/m1"}, nil)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want the upstream 400 relayed", resp.StatusCode)
	}
	if req := spanNamed(t, exp, "llm.request"); req.Status.Code != codes.Unset {
		t.Errorf("llm.request status = (%v, %q), want Unset", req.Status.Code, req.Status.Description)
	}
	if fwd := spanNamed(t, exp, "llm.forward"); fwd.Status.Code != codes.Error {
		t.Errorf("llm.forward status = (%v, %q), want Error", fwd.Status.Code, fwd.Status.Description)
	}
}

// The tool.forward client span fails on any upstream answer of 400 or
// above, as llm.forward does.
func TestTracing_ToolForwardMarksUpstreamErrorStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   codes.Code
		desc   string
	}{
		{"404", http.StatusNotFound, codes.Error, "upstream_error"},
		{"502", http.StatusBadGateway, codes.Error, "upstream_error"},
		{"200", http.StatusOK, codes.Unset, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exp := tracetest.NewInMemoryExporter()
			h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":7,"result":{"content":[]}}`)
			})
			h.seedToolRoute()
			h.server.Tracing = newTestTracing(exp)
			cert := agentCert(t, h.ca)
			resp := postJSON(t, h.client(&cert), h.url("/v1/mcp/search"), mcpCall("web_search"), nil)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			fwd := spanNamed(t, exp, "tool.forward")
			if fwd.Status.Code != c.code || fwd.Status.Description != c.desc {
				t.Errorf("tool.forward status = (%v, %q), want (%v, %q)",
					fwd.Status.Code, fwd.Status.Description, c.code, c.desc)
			}
		})
	}
}

// A call whose caller disconnected mid-stream marks the tool.call span
// with client_closed.
func TestTracing_ToolCallCallerGoneMarksSpan(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	h := newAbandonHarness(t)
	h.server.Tracing = newTestTracing(exp)
	abandonStream(t, h)
	var call *tracetest.SpanStub
	waitFor(t, func() bool {
		for _, s := range exp.GetSpans() {
			if s.Name == "tool.call" {
				call = &s
				return true
			}
		}
		return false
	})
	if call.Status.Code != codes.Error || call.Status.Description != "client_closed" {
		t.Errorf("tool.call status = (%v, %q), want (Error, client_closed)", call.Status.Code, call.Status.Description)
	}
}

func assertChild(t *testing.T, byName map[string]tracetest.SpanStub, child, parent string) {
	t.Helper()
	if byName[child].Parent.SpanID() != byName[parent].SpanContext.SpanID() {
		t.Errorf("%s must be a child of %s (parent = %s)", child, parent, byName[child].Parent.SpanID())
	}
}

func spanNames(exp *tracetest.InMemoryExporter) []string {
	var names []string
	for _, s := range exp.GetSpans() {
		names = append(names, s.Name)
	}
	return names
}
