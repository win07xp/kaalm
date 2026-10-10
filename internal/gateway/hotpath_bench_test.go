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

// Benchmarks for the pure functions on the gateway's request paths, the
// ones CPU and allocation profiles under the perf baseline ranked.
// They need no cluster and are the regression guard the perf harness
// cannot be: run `make bench` before and after a change to one of these
// paths and compare with benchstat.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	kaalmv1beta1 "github.com/win07xp/kaalm/api/v1beta1"
	"github.com/win07xp/kaalm/internal/llmtranslate"
)

// openAIResponse is the shape a provider answers a chat completion with,
// sized like a short reply.
var openAIResponse = []byte(`{"id":"chatcmpl-123","object":"chat.completion","created":1757600000,` +
	`"model":"mock-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok from mock"},` +
	`"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`)

func BenchmarkExtractUsageOpenAI(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := (openaiAdapter{}).extractUsage(openAIResponse); !ok {
			b.Fatal("usage not found")
		}
	}
}

func BenchmarkCopyForwardedHeaders(b *testing.B) {
	src := http.Header{
		"Authorization":     {"Bearer token"},
		"Content-Type":      {"application/json"},
		"Accept":            {"application/json"},
		"User-Agent":        {"openai-python/1.0"},
		"Traceparent":       {"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"},
		"X-Request-Id":      {"req-1"},
		"Accept-Encoding":   {"gzip, deflate"},
		"Connection":        {"keep-alive"},
		"Content-Length":    {"512"},
		"X-Forwarded-Proto": {"https"},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst := make(http.Header, len(src))
		copyForwardedHeaders(dst, src)
	}
}

func BenchmarkParseWorkloadSAN(b *testing.B) {
	cert := certWithSANs("sup.team-a", "sup.team-a.svc", "sup.team-a.svc.cluster.local")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := ParseWorkloadSAN(cert); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBudgetLedgerAddAndEnforce(b *testing.B) {
	p := budgetProvider(kaalmv1beta1.ModelProviderBudgetPolicy{AtPercent: 100, Action: "block"})
	ledger := NewBudgetLedger()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ledger.Add(p, "team-a", "agent/sup", 0.000001)
		if d := ledger.Enforce(p, "team-a"); d.Action == kaalmv1beta1.BudgetActionBlock {
			b.Fatal("ceiling reached inside the benchmark")
		}
	}
}

func BenchmarkVerifyHMACHeader(b *testing.B) {
	secret := []byte("shh")
	body := []byte(`{"text":"hello","user":"u1"}`)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	expected := mac.Sum(nil)
	header := hex.EncodeToString(expected)
	cfg := &kaalmv1beta1.ChannelHMAC{Header: "X-Sig", Algorithm: "sha256"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !verifyHMACHeader(header, cfg, expected) {
			b.Fatal("signature rejected")
		}
	}
}

func BenchmarkSignCallback(b *testing.B) {
	auth := &kaalmv1beta1.ChannelAuth{Type: authTypeHMAC,
		HMAC: &kaalmv1beta1.ChannelHMAC{Header: "X-Sig", Algorithm: "sha256"}}
	body := []byte(`{"requestId":"req-1","content":"ok"}`)
	now := time.Unix(1757600000, 0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest(http.MethodPost, "https://receiver.example/cb", nil)
		signCallback(req, auth, "shh", "req-1", body, now)
	}
}

func BenchmarkDeliveryOutcome(b *testing.B) {
	errs := []error{
		fmt.Errorf("Post: %w", &dialStageError{stage: dialStageConnect, err: errors.New("i/o timeout")}),
		errors.New("agent returned 503"),
		errors.New("agent returned 200 with a malformed response envelope"),
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		deliveryOutcome(errs[i%len(errs)])
	}
}

// BenchmarkLLMProxyMTLS runs the whole LLM proxy path in process: an mTLS
// caller, route authorization, the forwarded-header contract, the upstream
// round trip to a local provider, usage extraction, and spend accounting.
// TLS on both hops is included, so the number is an upper bound on the
// per-request cost the gateway phase measures without the cluster.
func BenchmarkLLMProxyMTLS(b *testing.B) {
	h := newHarness(b, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(openAIResponse)
	})
	h.seedRoute()
	cert := agentCert(b, h.ca)
	client := h.client(&cert)
	body := map[string]any{"model": "prov/m1", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := postJSON(b, client, h.url("/v1/chat/completions"), body, nil)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("status %d", resp.StatusCode)
		}
		<-h.upreqs
	}
}

// rateLimitedProxyHarness is the BenchmarkLLMProxyMTLS setup with request and
// token rate limits on the provider, set far above any rate a benchmark
// reaches, so every request runs the limiter (admission, and the token debit
// once usage settles) and none is refused.
func rateLimitedProxyHarness(b *testing.B) (*harness, *http.Client, map[string]any) {
	b.Helper()
	h := newHarness(b, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(openAIResponse)
	})
	h.seedRoute()
	h.store.providers["prov"].Spec.RateLimits = kaalmv1beta1.ModelProviderRateLimits{
		RequestsPerMinute: 2_000_000_000,
		TokensPerMinute:   2_000_000_000,
	}
	cert := agentCert(b, h.ca)
	body := map[string]any{"model": "prov/m1", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	return h, h.client(&cert), body
}

// BenchmarkLLMProxyMTLSRateLimited is BenchmarkLLMProxyMTLS with rate limits
// on: the difference between the two is the limiter's per-request cost.
func BenchmarkLLMProxyMTLSRateLimited(b *testing.B) {
	h, client, body := rateLimitedProxyHarness(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp := postJSON(b, client, h.url("/v1/chat/completions"), body, nil)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("status %d", resp.StatusCode)
		}
		<-h.upreqs
	}
}

// BenchmarkLLMProxyMTLSRateLimitedParallel runs the rate-limited proxy path
// from many callers at once on one (namespace, model) key, so the number
// includes contention on the limiter's lock.
func BenchmarkLLMProxyMTLSRateLimitedParallel(b *testing.B) {
	h, client, body := rateLimitedProxyHarness(b)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp := postJSON(b, client, h.url("/v1/chat/completions"), body, nil)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				b.Errorf("status %d", resp.StatusCode)
				return
			}
			<-h.upreqs
		}
	})
}

// countingWriter is an http.ResponseWriter and http.Flusher that discards
// what it is given and counts the Write and Flush calls, so a relay test or
// benchmark can see how many network writes a stream would cost.
type countingWriter struct {
	header  http.Header
	status  int
	bytes   int
	writes  int
	flushes int
}

func newCountingWriter() *countingWriter { return &countingWriter{header: http.Header{}} }

func (w *countingWriter) Header() http.Header { return w.header }
func (w *countingWriter) WriteHeader(code int) {
	w.status = code
}
func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++
	w.bytes += len(p)
	return len(p), nil
}
func (w *countingWriter) Flush() { w.flushes++ }

// chunkReader returns one canned chunk per Read call, the way a provider's
// stream reaches the gateway one network read at a time.
type chunkReader struct {
	chunks [][]byte
	reads  int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	for len(r.chunks) > 0 && len(r.chunks[0]) == 0 {
		r.chunks = r.chunks[1:]
	}
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	r.reads++
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	return n, nil
}

func (r *chunkReader) Close() error { return nil }

// sseResponse wraps chunks as a 200 text/event-stream response.
func sseResponse(chunks [][]byte) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       &chunkReader{chunks: chunks},
	}
}

// openAIStreamEvents is an OpenAI chat stream of n content chunks, the usage
// chunk, and [DONE], one event per element.
func openAIStreamEvents(n int) [][]byte {
	events := make([][]byte, 0, n+2)
	for i := 0; i < n; i++ {
		events = append(events, []byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1757600000,`+
			`"model":"m1","choices":[{"index":0,"delta":{"content":"tok "},"finish_reason":null}]}`+"\n\n"))
	}
	events = append(events,
		[]byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1757600000,"model":"m1",`+
			`"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":200,"total_tokens":212}}`+"\n\n"),
		[]byte("data: [DONE]\n\n"))
	return events
}

// anthropicStreamEvents is an Anthropic messages stream with n text deltas,
// one event (event line, data line, blank line) per element.
func anthropicStreamEvents(n int) [][]byte {
	events := make([][]byte, 0, n+6)
	events = append(events,
		[]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\","+
			"\"role\":\"assistant\",\"model\":\"m1\",\"content\":[],\"usage\":{\"input_tokens\":12,\"output_tokens\":1}}}\n\n"),
		[]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,"+
			"\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
	for i := 0; i < n; i++ {
		events = append(events, []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,"+
			"\"delta\":{\"type\":\"text_delta\",\"text\":\"tok \"}}\n\n"))
	}
	events = append(events,
		[]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"),
		[]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},"+
			"\"usage\":{\"output_tokens\":200}}\n\n"),
		[]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	return events
}

// openAIToolStreamEvents is an OpenAI chat stream that opens one tool call
// and streams its arguments in n chunks, then the finish chunk, the usage
// chunk, and [DONE], one event per element.
func openAIToolStreamEvents(n int) [][]byte {
	events := make([][]byte, 0, n+4)
	events = append(events, []byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1757600000,`+
		`"model":"m1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function",`+
		`"function":{"name":"lookup","arguments":""}}]},"finish_reason":null}]}`+"\n\n"))
	for i := 0; i < n; i++ {
		events = append(events, []byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1757600000,`+
			`"model":"m1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"k\":1}"}}]},`+
			`"finish_reason":null}]}`+"\n\n"))
	}
	return append(events,
		[]byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1757600000,"model":"m1",`+
			`"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n"),
		[]byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1757600000,"model":"m1",`+
			`"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":200,"total_tokens":212}}`+"\n\n"),
		[]byte("data: [DONE]\n\n"))
}

// anthropicToolStreamEvents is an Anthropic messages stream that opens one
// tool_use block and streams its input in n deltas, one event per element.
func anthropicToolStreamEvents(n int) [][]byte {
	events := make([][]byte, 0, n+5)
	events = append(events,
		[]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\","+
			"\"role\":\"assistant\",\"model\":\"m1\",\"content\":[],\"usage\":{\"input_tokens\":12,\"output_tokens\":1}}}\n\n"),
		[]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,"+
			"\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu_1\",\"name\":\"lookup\",\"input\":{}}}\n\n"))
	for i := 0; i < n; i++ {
		events = append(events, []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,"+
			"\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"k\\\":1}\"}}\n\n"))
	}
	return append(events,
		[]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"),
		[]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},"+
			"\"usage\":{\"output_tokens\":200}}\n\n"),
		[]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
}

// cloneChunks copies the chunk slices, which chunkReader consumes.
func cloneChunks(chunks [][]byte) [][]byte {
	out := make([][]byte, len(chunks))
	for i, c := range chunks {
		out[i] = append([]byte(nil), c...)
	}
	return out
}

// BenchmarkRelayStream relays a 200-event stream of text or tool-call
// arguments delivered one event per read, in each upstream format, copied or
// translated to the other format.
// writes/op and flushes/op count the writer calls one stream costs.
func BenchmarkRelayStream(b *testing.B) {
	h := newHarness(b, func(http.ResponseWriter, *http.Request) {})
	h.seedRoute()
	provider := h.store.providers["prov"]
	for _, c := range []struct {
		name       string
		upstream   llmtranslate.Format
		events     [][]byte
		translated bool
	}{
		{"openai/passthrough", llmtranslate.FormatOpenAI, openAIStreamEvents(200), false},
		{"openai/translated", llmtranslate.FormatOpenAI, openAIStreamEvents(200), true},
		{"anthropic/passthrough", llmtranslate.FormatAnthropic, anthropicStreamEvents(200), false},
		{"anthropic/translated", llmtranslate.FormatAnthropic, anthropicStreamEvents(200), true},
		{"openai-tools/passthrough", llmtranslate.FormatOpenAI, openAIToolStreamEvents(200), false},
		{"openai-tools/translated", llmtranslate.FormatOpenAI, openAIToolStreamEvents(200), true},
		{"anthropic-tools/passthrough", llmtranslate.FormatAnthropic, anthropicToolStreamEvents(200), false},
		{"anthropic-tools/translated", llmtranslate.FormatAnthropic, anthropicToolStreamEvents(200), true},
	} {
		b.Run(c.name, func(b *testing.B) {
			adapter, _ := adapterForProviderType(string(c.upstream))
			callerFormat := c.upstream
			if c.translated {
				callerFormat = llmtranslate.FormatAnthropic
				if c.upstream == llmtranslate.FormatAnthropic {
					callerFormat = llmtranslate.FormatOpenAI
				}
			}
			var writes, flushes int
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				resp := sseResponse(cloneChunks(c.events))
				var translator llmtranslate.Stream
				if c.translated {
					translator = llmtranslate.NewStream(c.upstream, callerFormat, "m1")
				}
				w := newCountingWriter()
				b.StartTimer()
				if out := h.server.relayStream(context.Background(), w, resp, adapter, translator, callerFormat,
					"team-a", "agent/sup", provider, "m1", nil, nil); out != outcomeOK {
					b.Fatalf("outcome %q", out)
				}
				writes += w.writes
				flushes += w.flushes
			}
			b.ReportMetric(float64(writes)/float64(b.N), "writes/op")
			b.ReportMetric(float64(flushes)/float64(b.N), "flushes/op")
		})
	}
}

// mcpStreamEvents is a broker SSE stream of n progress notifications and the
// final response, one event per element.
func mcpStreamEvents(n int) [][]byte {
	events := make([][]byte, 0, n+1)
	for i := 0; i < n; i++ {
		events = append(events, []byte(`data: {"jsonrpc":"2.0","method":"notifications/progress",`+
			`"params":{"progressToken":"t1","progress":1,"total":100,"message":"working on it"}}`+"\n\n"))
	}
	events = append(events, []byte(`data: {"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"done"}]}}`+"\n\n"))
	return events
}

// BenchmarkRelayMCPStream relays a 100-event tool stream delivered one event
// per read.
func BenchmarkRelayMCPStream(b *testing.B) {
	events := mcpStreamEvents(100)
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp/search", nil)
	var writes, flushes int
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		resp := sseResponse(cloneChunks(events))
		w := newCountingWriter()
		b.StartTimer()
		if _, errType, _ := relayMCPStream(w, req, resp, 4<<20, json.RawMessage("7"), "search"); errType != "" {
			b.Fatalf("errType %q", errType)
		}
		writes += w.writes
		flushes += w.flushes
	}
	b.ReportMetric(float64(writes)/float64(b.N), "writes/op")
	b.ReportMetric(float64(flushes)/float64(b.N), "flushes/op")
}

// BenchmarkRelayMCPBuffered relays a buffered tools/call answer.
func BenchmarkRelayMCPBuffered(b *testing.B) {
	body := []byte(`{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"` +
		strings.Repeat("result text ", 400) + `"}]}}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(bytes.NewReader(body))}
		if _, status, _, _ := relayMCPBuffered(context.Background(), newCountingWriter(), resp, 4<<20, "search"); status != http.StatusOK {
			b.Fatalf("status %d", status)
		}
	}
}

// toolsListCatalog is a tools/list answer of n tools of about 1 KB each.
func toolsListCatalog(n int) []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"jsonrpc":"2.0","id":3,"result":{"tools":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			buf.WriteByte(',')
		}
		fmt.Fprintf(&buf, `{"name":"tool_%d","description":"%s","inputSchema":{"type":"object",`+
			`"properties":{"query":{"type":"string","description":"%s"}},"required":["query"]}}`,
			i, strings.Repeat("Looks things up. ", 30), strings.Repeat("What to look up. ", 20))
	}
	buf.WriteString(`]}}`)
	return buf.Bytes()
}

// BenchmarkRelayFilteredToolsList relays an 80-tool catalog (about 80 KB) in
// each encoding, to a caller allowed every tool and to one allowed half.
func BenchmarkRelayFilteredToolsList(b *testing.B) {
	h := newHarness(b, func(http.ResponseWriter, *http.Request) {})
	h.server.Config.MCPMaxBodyBytes = 4 << 20
	catalog := toolsListCatalog(80)
	half := map[string]bool{}
	for i := 0; i < 80; i += 2 {
		half[fmt.Sprintf("tool_%d", i)] = true
	}
	msg := mcpRequest{JSONRPC: "2.0", ID: json.RawMessage(`3`), Method: "tools/list"}
	for _, enc := range []string{"json", "sse"} {
		body, ct := catalog, "application/json"
		if enc == "sse" {
			body, ct = []byte("data: "+string(catalog)+"\n\n"), "text/event-stream"
		}
		for _, f := range []struct {
			name   string
			filter *toolFilter
		}{{"all", &toolFilter{}}, {"filtered", &toolFilter{allow: half}}} {
			b.Run(enc+"/"+f.name, func(b *testing.B) {
				b.SetBytes(int64(len(body)))
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {ct}},
						Body: io.NopCloser(bytes.NewReader(body))}
					if _, status, _, _ := h.server.relayFilteredToolsList(context.Background(), newCountingWriter(),
						resp, msg, f.filter, "search"); status != http.StatusOK {
						b.Fatalf("status %d", status)
					}
				}
			})
		}
	}
}

// chatBody50KB is a chat completion request of about 50 KB.
func chatBody50KB(stream bool) map[string]any {
	messages := make([]any, 0, 50)
	for i := 0; i < 50; i++ {
		messages = append(messages, map[string]any{"role": "user", "content": strings.Repeat("context line. ", 70)})
	}
	body := map[string]any{"model": "prov/m1", "messages": messages, "temperature": 0.2, "max_tokens": 512}
	if stream {
		body["stream"] = true
	}
	return body
}

// BenchmarkLLMProxyBody50KB is BenchmarkLLMProxyMTLS with a 50 KB request
// body, buffered and streaming, so the request body preparation shows.
func BenchmarkLLMProxyBody50KB(b *testing.B) {
	for _, stream := range []bool{false, true} {
		name := "buffered"
		if stream {
			name = "stream"
		}
		b.Run(name, func(b *testing.B) {
			h := newHarness(b, func(w http.ResponseWriter, _ *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte(`data: {"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}` +
						"\n\ndata: [DONE]\n\n"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(openAIResponse)
			})
			h.seedRoute()
			cert := agentCert(b, h.ca)
			client := h.client(&cert)
			body := chatBody50KB(stream)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				resp := postJSON(b, client, h.url("/v1/chat/completions"), body, nil)
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					b.Fatalf("status %d", resp.StatusCode)
				}
				<-h.upreqs
			}
		})
	}
}

// streamDataPayloads returns the data payloads of a canned stream, as the
// relay hands them to accumulateStreamUsage.
func streamDataPayloads(events [][]byte) [][]byte {
	var out [][]byte
	for _, e := range events {
		for _, line := range bytes.Split(e, []byte("\n")) {
			if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				out = append(out, bytes.TrimSpace(data))
			}
		}
	}
	return out
}

// BenchmarkAccumulateStreamUsage reads usage from every data payload of a
// 200-event stream, in each format.
func BenchmarkAccumulateStreamUsage(b *testing.B) {
	for _, c := range []struct {
		name    string
		adapter providerAdapter
		events  [][]byte
	}{
		{"openai", openaiAdapter{}, openAIStreamEvents(200)},
		{"anthropic", anthropicAdapter{}, anthropicStreamEvents(200)},
	} {
		b.Run(c.name, func(b *testing.B) {
			payloads := streamDataPayloads(c.events)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var u Usage
				for _, p := range payloads {
					c.adapter.accumulateStreamUsage(p, &u)
				}
				if u.OutputTokens != 200 {
					b.Fatalf("usage %+v", u)
				}
			}
		})
	}
}
