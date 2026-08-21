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
	"net/http"
	"strings"
	"testing"

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

// The #98 proof: one channel message yields one trace whose spans connect
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
