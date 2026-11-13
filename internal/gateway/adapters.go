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
	"net/http"
	"strings"
)

// Usage is the token spend extracted from a provider response, plus any
// provider-side tool calls its usage surface reported.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	// ServerTools counts provider-side tool calls by normalized tool id
	// (for example "web_search"). Nil when the response reported none.
	ServerTools map[string]int64
	// InputEstimated and OutputEstimated mark a count as the adapter's
	// estimate, filled where the provider reported none (see withEstimate).
	InputEstimated, OutputEstimated bool
}

// isZero reports whether nothing was extracted. Usage carries a map, so
// struct equality is unavailable; "did we see usage" checks go through here.
func (u Usage) isZero() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && len(u.ServerTools) == 0
}

// estimated reports whether any count is the adapter's estimate.
func (u Usage) estimated() bool { return u.InputEstimated || u.OutputEstimated }

// estimatedPart is the usage holding only the estimated counts, for pricing
// the estimated share of a call.
func (u Usage) estimatedPart() Usage {
	var p Usage
	if u.InputEstimated {
		p.InputTokens, p.InputEstimated = u.InputTokens, true
	}
	if u.OutputEstimated {
		p.OutputTokens, p.OutputEstimated = u.OutputTokens, true
	}
	return p
}

// streamEstimator keeps one stream's running token estimate, for the parts
// of its usage the provider does not report: a stream the caller leaves
// before its usage event, one the provider breaks, or one that finishes
// with no usage. It returns tokens; costOf prices them.
type streamEstimator interface {
	// event reads one SSE data payload of the upstream's own stream,
	// trimmed, before any translation.
	event(data []byte)
	// estimateInput returns the input estimate. It may scan the whole
	// request, so it is called only when the provider reported no input.
	estimateInput() int64
	// estimateOutput returns the running output estimate.
	estimateOutput() int64
}

// withEstimate fills the parts of reported the provider did not report with
// est's estimate, input and output separately, and marks each part it
// fills. It asks est for an input estimate only when input is missing. A count of zero counts as not reported, and a part whose estimate
// is zero stays zero and unmarked. A nil est returns reported unchanged.
func withEstimate(reported Usage, est streamEstimator) Usage {
	if est == nil {
		return reported
	}
	if reported.InputTokens == 0 {
		if input := est.estimateInput(); input > 0 {
			reported.InputTokens, reported.InputEstimated = input, true
		}
	}
	if reported.OutputTokens == 0 {
		if output := est.estimateOutput(); output > 0 {
			reported.OutputTokens, reported.OutputEstimated = output, true
		}
	}
	return reported
}

// providerAdapter carries the per-provider knowledge: request-format paths,
// credential header shape, usage extraction (buffered and streamed), and
// whether a streaming request needs the usage option. The served types are
// anthropic, openai, openai-compatible, and google-vertex.
type providerAdapter interface {
	// formatName identifies the request format for logs and metrics.
	formatName() string
	// injectCredential sets the provider's auth header.
	injectCredential(h http.Header, credential string)
	// usageReader returns a reader for the usage of a JSON response body,
	// fed the body's bytes as the relay copies them.
	usageReader() *usageReader
	// accumulateStreamUsage inspects one SSE data payload and folds any usage
	// it carries into u. It reports whether the payload is the stream's
	// final event, after which the stream's usage is complete.
	accumulateStreamUsage(data []byte, u *Usage) (final bool)
	// streamEstimator returns a new estimator for one stream, given the body
	// the provider received, or nil when the adapter estimates nothing.
	streamEstimator(request []byte) streamEstimator
	// streamUsageOption reports whether a streaming request in this format
	// needs stream_options.include_usage set for the stream to carry usage.
	streamUsageOption() bool
	// upstreamPath rewrites the inbound request path for the upstream. Most
	// adapters pass it through; Vertex embeds the model in the path and
	// injects ?alt=sse.
	upstreamPath(inboundPath, modelID string) string
	// modelFromPath returns the qualified model a format carries in the
	// URL, or "" when the model travels in the body.
	modelFromPath(path string) string
}

// adapterForPath maps a request path to the adapter of its format: the
// three body-model paths, or the Vertex path shape. Unrecognized paths on
// the cluster listener are rejected with 400.
func adapterForPath(urlPath string) (providerAdapter, bool) {
	path, _, _ := strings.Cut(urlPath, "?") // strip any query string for matching
	switch path {
	case "/v1/messages":
		return anthropicAdapter{}, true
	case "/v1/chat/completions", "/v1/completions":
		return openaiAdapter{}, true
	}
	// The Vertex parse reads the whole path, the same string modelFromPath
	// and upstreamPath read, so a '?' the mux decoded from %3F inside the
	// method segment fails all three alike instead of only here.
	if _, ok := parseVertexPath(urlPath); ok {
		return vertexAdapter{}, true
	}
	// The mux routes the Vertex prefixes here whole, so a path under them
	// that is not a Vertex call lands here.
	return nil, false
}

// Provider type enum values (ModelProvider.spec.type).
const (
	providerTypeAnthropic        = "anthropic"
	providerTypeOpenAI           = "openai"
	providerTypeOpenAICompatible = "openai-compatible"
	providerTypeVertex           = "google-vertex"
)

// adapterForProviderType returns the adapter that speaks a ModelProvider's
// wire protocol, used for credential injection.
func adapterForProviderType(providerType string) (providerAdapter, bool) {
	switch providerType {
	case providerTypeAnthropic:
		return anthropicAdapter{}, true
	case providerTypeOpenAI, providerTypeOpenAICompatible:
		return openaiAdapter{}, true
	case providerTypeVertex:
		return vertexAdapter{}, true
	}
	return nil, false
}

// ---- Anthropic ----

type anthropicAdapter struct{}

func (anthropicAdapter) formatName() string { return providerTypeAnthropic }

func (anthropicAdapter) injectCredential(h http.Header, credential string) {
	h.Set("x-api-key", credential)
}

func (anthropicAdapter) usageReader() *usageReader { return newAnthropicUsageReader() }

// serverToolCounts decodes an Anthropic usage.server_tool_use object
// ({"web_search_requests": 1}) into normalized per-tool counts
// ("web_search" -> 1). Decoded separately from the token fields so an
// unexpected value shape loses only the tool counts, never the tokens.
func serverToolCounts(raw json.RawMessage) map[string]int64 {
	var counts map[string]int64
	if len(raw) == 0 || json.Unmarshal(raw, &counts) != nil {
		return nil
	}
	var tools map[string]int64
	for key, n := range counts {
		if n <= 0 {
			continue
		}
		if tools == nil {
			tools = make(map[string]int64, len(counts))
		}
		tools[strings.TrimSuffix(key, "_requests")] = n
	}
	return tools
}

// accumulateStreamUsage: input_tokens arrive on message_start, cumulative
// output_tokens (and cumulative server_tool_use counts) on message_delta.
// message_stop carries no usage and is the final event.
func (anthropicAdapter) accumulateStreamUsage(data []byte, u *Usage) bool {
	if !mayBeAnthropicStreamEvent(data) {
		return false
	}
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
		return false
	}
	switch evt.Type {
	case "message_start":
		u.InputTokens = evt.Message.Usage.InputTokens
	case "message_delta":
		if evt.Usage.OutputTokens > 0 {
			u.OutputTokens = evt.Usage.OutputTokens
		}
		// Counts are cumulative, so the latest snapshot replaces the last.
		if tools := serverToolCounts(evt.Usage.ServerToolUse); tools != nil {
			u.ServerTools = tools
		}
	case "message_stop":
		return true
	}
	return false
}

// mayBeAnthropicStreamEvent reports whether an Anthropic stream payload can
// be a message_start, message_delta, or message_stop event, the only events
// accumulateStreamUsage reads, so the other events (the text deltas) skip
// the decode. The decoder compares the type value exactly, so it must hold
// one of the three names as written, or a \u escape that could spell one.
func mayBeAnthropicStreamEvent(data []byte) bool {
	return bytes.Contains(data, []byte("message_start")) ||
		bytes.Contains(data, []byte("message_delta")) ||
		bytes.Contains(data, []byte("message_stop")) ||
		bytes.Contains(data, []byte(`\u`))
}

// streamEstimator estimates from the generated content of the deltas: text
// (text_delta), tool input JSON (input_json_delta), and thinking
// (thinking_delta). message_start, content_block_start with empty text,
// message_delta, and message_stop hold no such member with content, so
// they add nothing. The input estimate leaves out the base64 source data of
// image and document blocks.
func (anthropicAdapter) streamEstimator(request []byte) streamEstimator {
	return newContentEstimator(request, anthropicContentMembers, nil, anthropicMediaMembers)
}

var (
	anthropicContentMembers = [][]byte{[]byte("text"), []byte("partial_json"), []byte("thinking")}
	anthropicMediaMembers   = []mediaMember{{name: []byte("data")}}
)

func (anthropicAdapter) streamUsageOption() bool { return false }

func (anthropicAdapter) upstreamPath(inboundPath, _ string) string { return inboundPath }

func (anthropicAdapter) modelFromPath(string) string { return "" }

// ---- OpenAI and OpenAI-compatible ----

type openaiAdapter struct{}

func (openaiAdapter) formatName() string { return "openai" }

func (openaiAdapter) injectCredential(h http.Header, credential string) {
	h.Set("Authorization", "Bearer "+credential)
}

func (openaiAdapter) usageReader() *usageReader { return newOpenAIUsageReader() }

// accumulateStreamUsage: a usage object appears in the final chunk preceding
// [DONE], present only when stream_options.include_usage was set (which
// the proxy sets on every streaming request). [DONE] is the final event.
func (openaiAdapter) accumulateStreamUsage(data []byte, u *Usage) bool {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return true
	}
	if !mayCarryOpenAIUsage(data) {
		return false
	}
	var chunk struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil || chunk.Usage == nil {
		return false
	}
	u.InputTokens = chunk.Usage.PromptTokens
	u.OutputTokens = chunk.Usage.CompletionTokens
	return false
}

// mayCarryOpenAIUsage reports whether an OpenAI stream payload can hold a
// usage member, so the content chunks skip the decode. encoding/json matches
// a member name to a field case-insensitively, with Unicode folding (the
// long s, U+017F, matches s), and after unescaping, so the payload must
// hold a folded spelling of "usage" or a \u escape that could spell one.
func mayCarryOpenAIUsage(data []byte) bool {
	if bytes.Contains(data, []byte(`\u`)) {
		return true
	}
	for i := 0; i+5 <= len(data); i++ {
		if data[i]|0x20 != 'u' {
			continue
		}
		j := i + 1
		switch {
		case data[j]|0x20 == 's':
			j++
		case data[j] == 0xc5 && j+1 < len(data) && data[j+1] == 0xbf: // U+017F
			j += 2
		default:
			continue
		}
		if j+3 <= len(data) && data[j]|0x20 == 'a' && data[j+1]|0x20 == 'g' && data[j+2]|0x20 == 'e' {
			return true
		}
	}
	return false
}

// streamEstimator estimates from the generated content of the chunks:
// content and refusal, the reasoning deltas some compatible providers send
// (reasoning_content, reasoning), tool-call arguments, and the text of a
// legacy completions chunk. A provider that sends both text and content in
// one chat chunk is counted twice, a known overcount used only when usage
// is missing. Log probabilities are skipped, since they repeat the content
// with token metadata. The role chunk (empty content), the finish chunk,
// the usage chunk, and [DONE] hold no such member with content, so they add
// nothing. The input estimate leaves out base64 media: data URLs (image_url
// and its url, file_data) and input_audio data.
func (openaiAdapter) streamEstimator(request []byte) streamEstimator {
	return newContentEstimator(request, openaiContentMembers, openaiSkippedMembers, openaiMediaMembers)
}

var (
	openaiContentMembers = [][]byte{
		[]byte("content"), []byte("refusal"), []byte("reasoning_content"), []byte("reasoning"), []byte("arguments"),
		[]byte("text"),
	}
	openaiSkippedMembers = [][]byte{[]byte("logprobs")}
	openaiMediaMembers   = []mediaMember{
		{name: []byte("url"), prefix: []byte("data:")},
		{name: []byte("image_url"), prefix: []byte("data:")},
		{name: []byte("file_data")},
		{name: []byte("data")},
	}
)

// streamUsageOption: without stream_options.include_usage, OpenAI-format
// streams emit no usage at all, so every streaming request gets it (see
// needsStreamOptions). The extra terminal usage chunk is backward-compatible.
func (openaiAdapter) streamUsageOption() bool { return true }

func (openaiAdapter) upstreamPath(inboundPath, _ string) string { return inboundPath }

func (openaiAdapter) modelFromPath(string) string { return "" }
