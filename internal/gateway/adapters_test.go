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
	"testing"
)

func TestAdapterFormatNames(t *testing.T) {
	if (anthropicAdapter{}).formatName() != providerTypeAnthropic {
		t.Error("anthropic formatName wrong")
	}
	if (openaiAdapter{}).formatName() != "openai" {
		t.Error("openai formatName wrong")
	}
	if (vertexAdapter{}).formatName() != providerTypeVertex {
		t.Error("vertex formatName wrong")
	}
}

func TestAnthropicFixupIsNoOp(t *testing.T) {
	body := map[string]any{"stream": true}
	anthropicAdapter{}.fixupRequestBody(body)
	if _, ok := body["stream_options"]; ok {
		t.Error("anthropic must not inject stream_options")
	}
	// Vertex fixup is also a no-op.
	vBody := map[string]any{"stream": true}
	vertexAdapter{}.fixupRequestBody(vBody)
	if len(vBody) != 1 {
		t.Error("vertex fixup must not mutate the body")
	}
}

func TestOpenAIFixup(t *testing.T) {
	// Non-streaming: untouched.
	nonStream := map[string]any{"stream": false}
	openaiAdapter{}.fixupRequestBody(nonStream)
	if _, ok := nonStream["stream_options"]; ok {
		t.Error("non-streaming request must not get stream_options")
	}
	// Streaming without stream_options: injected.
	stream := map[string]any{"stream": true}
	openaiAdapter{}.fixupRequestBody(stream)
	opts, ok := stream["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Errorf("stream_options.include_usage not injected: %v", stream["stream_options"])
	}
	// Streaming with an existing stream_options: preserved.
	pre := map[string]any{"stream": true, "stream_options": map[string]any{"foo": "bar"}}
	openaiAdapter{}.fixupRequestBody(pre)
	if got := pre["stream_options"].(map[string]any); got["foo"] != "bar" {
		t.Error("existing stream_options must be preserved")
	}
}

func TestAdapterForProviderType(t *testing.T) {
	cases := map[string]string{
		providerTypeAnthropic:        providerTypeAnthropic,
		providerTypeOpenAI:           "openai",
		providerTypeOpenAICompatible: "openai",
		providerTypeVertex:           providerTypeVertex,
	}
	for ptype, want := range cases {
		a, ok := adapterForProviderType(ptype)
		if !ok || a.formatName() != want {
			t.Errorf("adapterForProviderType(%q) = %v ok=%v", ptype, a, ok)
		}
	}
	if _, ok := adapterForProviderType("mystery"); ok {
		t.Error("unknown provider type must not resolve")
	}
}

func TestExtractUsage_MalformedAndZero(t *testing.T) {
	adapters := []providerAdapter{anthropicAdapter{}, openaiAdapter{}, vertexAdapter{}}
	for _, a := range adapters {
		if _, ok := a.extractUsage([]byte("not json")); ok {
			t.Errorf("%s: malformed body must not yield usage", a.formatName())
		}
		if _, ok := a.extractUsage([]byte(`{}`)); ok {
			t.Errorf("%s: zero usage must not yield usage", a.formatName())
		}
	}
	// OpenAI-shaped zero usage.
	if _, ok := (openaiAdapter{}).extractUsage([]byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`)); ok {
		t.Error("openai zero usage must be false")
	}
}

// Recorded Anthropic response shape with a server-side tool: usage carries
// server_tool_use with per-tool "_requests" counters (web search docs).
func TestAnthropicExtractUsage_ServerToolUse(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":6039,"output_tokens":931,` +
		`"server_tool_use":{"web_search_requests":2}}}`)
	u, ok := anthropicAdapter{}.extractUsage(body)
	if !ok {
		t.Fatal("usage with server tools must extract")
	}
	if u.InputTokens != 6039 || u.OutputTokens != 931 {
		t.Errorf("tokens = %+v", u)
	}
	if u.ServerTools["web_search"] != 2 {
		t.Errorf("ServerTools = %v, want web_search:2", u.ServerTools)
	}

	// A malformed server_tool_use value loses only the tool counts, never
	// the tokens (the facets decode separately).
	body = []byte(`{"usage":{"input_tokens":5,"output_tokens":2,` +
		`"server_tool_use":{"web_search_requests":"three"}}}`)
	u, ok = anthropicAdapter{}.extractUsage(body)
	if !ok || u.InputTokens != 5 || u.OutputTokens != 2 {
		t.Fatalf("tokens must survive a malformed server_tool_use: %+v ok=%v", u, ok)
	}
	if u.ServerTools != nil {
		t.Errorf("malformed server_tool_use must yield nil, got %v", u.ServerTools)
	}
}

func TestServerToolCounts_Normalization(t *testing.T) {
	got := serverToolCounts([]byte(`{"web_search_requests":3,"code_execution_requests":1,"idle":0}`))
	if got["web_search"] != 3 || got["code_execution"] != 1 {
		t.Errorf("counts = %v", got)
	}
	if _, present := got["idle"]; present {
		t.Error("zero counts must be dropped")
	}
	if serverToolCounts(nil) != nil || serverToolCounts([]byte(`{}`)) != nil {
		t.Error("empty input must yield nil")
	}
}

// Streamed server_tool_use counts are cumulative on message_delta: the last
// snapshot wins, mirroring the output_tokens treatment.
func TestAnthropicStream_ServerToolUseCumulative(t *testing.T) {
	var u Usage
	a := anthropicAdapter{}
	a.accumulateStreamUsage([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":40}}}`), &u)
	a.accumulateStreamUsage([]byte(`{"type":"message_delta","usage":{"output_tokens":10,"server_tool_use":{"web_search_requests":1}}}`), &u)
	a.accumulateStreamUsage([]byte(`{"type":"message_delta","usage":{"output_tokens":25,"server_tool_use":{"web_search_requests":3}}}`), &u)
	if u.InputTokens != 40 || u.OutputTokens != 25 {
		t.Errorf("tokens = %+v", u)
	}
	if u.ServerTools["web_search"] != 3 {
		t.Errorf("ServerTools = %v, want cumulative web_search:3", u.ServerTools)
	}
}

// The OpenAI chat-completions format carries no usage-level server-tool
// counts on the paths the gateway proxies; the adapter extracts none.
func TestOpenAIExtractUsage_NoServerTools(t *testing.T) {
	u, ok := openaiAdapter{}.extractUsage([]byte(`{"usage":{"prompt_tokens":9,"completion_tokens":4}}`))
	if !ok || u.ServerTools != nil {
		t.Errorf("openai must extract tokens only: %+v ok=%v", u, ok)
	}
}

func TestAccumulateStreamUsage_Malformed(t *testing.T) {
	var u Usage
	// Malformed data is ignored for every adapter.
	anthropicAdapter{}.accumulateStreamUsage([]byte("nope"), &u)
	openaiAdapter{}.accumulateStreamUsage([]byte("nope"), &u)
	openaiAdapter{}.accumulateStreamUsage([]byte("[DONE]"), &u)
	vertexAdapter{}.accumulateStreamUsage([]byte("nope"), &u)
	if u.InputTokens != 0 || u.OutputTokens != 0 {
		t.Errorf("malformed stream data must not accumulate: %+v", u)
	}
	// Anthropic message_stop carries no usage.
	anthropicAdapter{}.accumulateStreamUsage([]byte(`{"type":"message_stop"}`), &u)
	if u.InputTokens != 0 {
		t.Error("message_stop must not change usage")
	}
}
