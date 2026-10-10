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
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
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

// prepareBody runs a request body through the proxy's preparation for a,
// keeping its model.
func prepareBody(t *testing.T, a providerAdapter, body string) string {
	t.Helper()
	f, err := scanRequestFields([]byte(body))
	if err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	return string(rewriteRequestBody([]byte(body), f, f.model([]byte(body)), needsStreamOptions(a, f)))
}

func TestAnthropicAddsNoStreamOptions(t *testing.T) {
	body := `{"model":"m","stream":true}`
	if got := prepareBody(t, anthropicAdapter{}, body); got != body {
		t.Errorf("anthropic must not add stream_options: %s", got)
	}
	// Vertex adds nothing either.
	if got := prepareBody(t, vertexAdapter{}, body); got != body {
		t.Errorf("vertex must not change the body: %s", got)
	}
}

func TestOpenAIStreamOptions(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"non-streaming", `{"model":"m","stream":false}`, `{"model":"m","stream":false}`},
		{"streaming without stream_options", `{"model":"m","stream":true}`,
			`{"model":"m","stream":true,"stream_options":{"include_usage":true}}`},
		{"existing stream_options", `{"model":"m","stream":true,"stream_options":{"foo":"bar"}}`,
			`{"model":"m","stream":true,"stream_options":{"foo":"bar"}}`},
		{"null stream_options", `{"model":"m","stream":true,"stream_options":null}`,
			`{"model":"m","stream":true,"stream_options":null}`},
		{"stream as a string", `{"model":"m","stream":"true"}`, `{"model":"m","stream":"true"}`},
	} {
		if got := prepareBody(t, openaiAdapter{}, c.body); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
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

// referenceOpenAIStreamUsage is openaiAdapter.accumulateStreamUsage without
// its prefilter: every payload decoded.
func referenceOpenAIStreamUsage(data []byte, u *Usage) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return
	}
	var chunk struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil || chunk.Usage == nil {
		return
	}
	u.InputTokens = chunk.Usage.PromptTokens
	u.OutputTokens = chunk.Usage.CompletionTokens
}

// referenceAnthropicStreamUsage is anthropicAdapter.accumulateStreamUsage
// without its prefilter.
func referenceAnthropicStreamUsage(data []byte, u *Usage) {
	var evt struct {
		Type    string `json:"type"`
		Message struct {
			Usage struct {
				InputTokens int64 `json:"input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage struct {
			OutputTokens  int64           `json:"output_tokens"`
			ServerToolUse json.RawMessage `json:"server_tool_use"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &evt); err != nil {
		return
	}
	switch evt.Type {
	case "message_start":
		u.InputTokens = evt.Message.Usage.InputTokens
	case "message_delta":
		if evt.Usage.OutputTokens > 0 {
			u.OutputTokens = evt.Usage.OutputTokens
		}
		if tools := serverToolCounts(evt.Usage.ServerToolUse); tools != nil {
			u.ServerTools = tools
		}
	}
}

// streamUsageSeeds are payloads around the prefilters' edges.
var streamUsageSeeds = []string{
	`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3}}`,
	`{"choices":[{"delta":{"content":"usage"}}]}`,
	`{"choices":[{"delta":{"content":"hi"}}]}`,
	`{"USAGE":{"prompt_tokens":1,"completion_tokens":2}}`,
	"{\"u\xc5\xbfage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}",
	`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`,
	`{"usage":null}`,
	`[DONE]`,
	`{"type":"message_start","message":{"usage":{"input_tokens":7}}}`,
	`{"type":"message_delta","usage":{"output_tokens":9,"server_tool_use":{"web_search_requests":2}}}`,
	`{"type":"ping"}`,
	`{"type":"content_block_delta","delta":{"type":"text_delta","text":"message_start"}}`,
	`{"type":"message_start","message":{"usage":{"input_tokens":5}}}`,
	`{"TYPE":"message_delta","USAGE":{"OUTPUT_TOKENS":4}}`,
}

func fuzzStreamUsage(f *testing.F, got, want func([]byte, *Usage)) {
	for _, s := range streamUsageSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, prior := range []Usage{{}, {InputTokens: 3, OutputTokens: 4, ServerTools: map[string]int64{"x": 1}}} {
			g := prior
			w := prior
			g.ServerTools, w.ServerTools = maps.Clone(prior.ServerTools), maps.Clone(prior.ServerTools)
			got(data, &g)
			want(data, &w)
			if !reflect.DeepEqual(g, w) {
				t.Fatalf("payload %q from %+v: got %+v, want %+v", data, prior, g, w)
			}
		}
	})
}

// The prefilter never skips a payload whose usage the full decode reads.
func FuzzOpenAIStreamUsageMatchesFullDecode(f *testing.F) {
	fuzzStreamUsage(f, openaiAdapter{}.accumulateStreamUsage, referenceOpenAIStreamUsage)
}

func FuzzAnthropicStreamUsageMatchesFullDecode(f *testing.F) {
	fuzzStreamUsage(f, anthropicAdapter{}.accumulateStreamUsage, referenceAnthropicStreamUsage)
}
