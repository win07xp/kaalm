/*
Copyright 2026.

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
)

// Usage is the token spend extracted from a provider response.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

// providerAdapter carries the per-provider knowledge: request-format paths,
// credential header shape, usage extraction (buffered and streamed), and
// streaming request fixups. Anthropic and OpenAI/OpenAI-compatible ship in
// Phase 5; Vertex lands in the hardening phase.
type providerAdapter interface {
	// formatName identifies the request format for logs and metrics.
	formatName() string
	// injectCredential sets the provider's auth header.
	injectCredential(h http.Header, credential string)
	// extractUsage reads token counts from a buffered JSON response body.
	extractUsage(body []byte) (Usage, bool)
	// accumulateStreamUsage inspects one SSE data payload and folds any usage
	// it carries into u.
	accumulateStreamUsage(data []byte, u *Usage)
	// fixupRequestBody may rewrite the (already model-rewritten) request body
	// map before forwarding, for example injecting stream_options.
	fixupRequestBody(body map[string]any)
}

// adapterForPath maps a request path to the adapter that registered it.
// Unrecognized paths on the LLM listener are rejected with 400.
func adapterForPath(urlPath string) (providerAdapter, bool) {
	switch urlPath {
	case "/v1/messages":
		return anthropicAdapter{}, true
	case "/v1/chat/completions", "/v1/completions":
		return openaiAdapter{}, true
	}
	return nil, false
}

// Provider type enum values (ModelProvider.spec.type).
const (
	providerTypeAnthropic        = "anthropic"
	providerTypeOpenAI           = "openai"
	providerTypeOpenAICompatible = "openai-compatible"
)

// adapterForProviderType returns the adapter that speaks a ModelProvider's
// wire protocol, used for credential injection.
func adapterForProviderType(providerType string) (providerAdapter, bool) {
	switch providerType {
	case providerTypeAnthropic:
		return anthropicAdapter{}, true
	case providerTypeOpenAI, providerTypeOpenAICompatible:
		return openaiAdapter{}, true
	}
	return nil, false
}

// ---- Anthropic ----

type anthropicAdapter struct{}

func (anthropicAdapter) formatName() string { return providerTypeAnthropic }

func (anthropicAdapter) injectCredential(h http.Header, credential string) {
	h.Set("x-api-key", credential)
}

func (anthropicAdapter) extractUsage(body []byte) (Usage, bool) {
	var resp struct {
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Usage{}, false
	}
	if resp.Usage.InputTokens == 0 && resp.Usage.OutputTokens == 0 {
		return Usage{}, false
	}
	return Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}, true
}

// accumulateStreamUsage: input_tokens arrive on message_start, cumulative
// output_tokens on the final message_delta. message_stop carries no usage.
func (anthropicAdapter) accumulateStreamUsage(data []byte, u *Usage) {
	var evt struct {
		Type    string `json:"type"`
		Message struct {
			Usage struct {
				InputTokens int64 `json:"input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage struct {
			OutputTokens int64 `json:"output_tokens"`
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
	}
}

func (anthropicAdapter) fixupRequestBody(map[string]any) {}

// ---- OpenAI and OpenAI-compatible ----

type openaiAdapter struct{}

func (openaiAdapter) formatName() string { return "openai" }

func (openaiAdapter) injectCredential(h http.Header, credential string) {
	h.Set("Authorization", "Bearer "+credential)
}

func (openaiAdapter) extractUsage(body []byte) (Usage, bool) {
	var resp struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Usage{}, false
	}
	if resp.Usage.PromptTokens == 0 && resp.Usage.CompletionTokens == 0 {
		return Usage{}, false
	}
	return Usage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens}, true
}

// accumulateStreamUsage: a usage object appears in the final chunk preceding
// [DONE], present only when stream_options.include_usage was set (which
// fixupRequestBody guarantees).
func (openaiAdapter) accumulateStreamUsage(data []byte, u *Usage) {
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

// fixupRequestBody injects stream_options: {include_usage: true} into
// streaming requests when absent; without it OpenAI-format streams emit no
// usage at all. The extra terminal usage chunk is backward-compatible.
func (openaiAdapter) fixupRequestBody(body map[string]any) {
	stream, _ := body["stream"].(bool)
	if !stream {
		return
	}
	if _, present := body["stream_options"]; !present {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
}
