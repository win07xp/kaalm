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
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// Google Vertex (Gemini) adapter. Vertex is the one format that names the
// model in the URL path rather than the request body, does not accept static
// API keys (the credential is a service-account JSON key the ModelProvider
// reconciler and gateway mint OAuth2 tokens from), and returns a JSON-array
// stream unless ?alt=sse is present. See
// docs/src/gateways/llm/provider-routing.md and request-handling.md.

// isVertexPath matches the :generateContent / :streamGenerateContent method
// suffix, since Vertex paths embed project and location segments.
func isVertexPath(p string) bool {
	return strings.HasSuffix(p, ":generateContent") || strings.HasSuffix(p, ":streamGenerateContent")
}

type vertexAdapter struct{}

func (vertexAdapter) formatName() string { return providerTypeVertex }

// injectCredential attaches the OAuth2 access token. The credential passed in
// is already the minted bearer token (the Store resolves the SA key to a token
// via the VertexTokenSource seam).
func (vertexAdapter) injectCredential(h http.Header, credential string) {
	h.Set("Authorization", "Bearer "+credential)
}

func (vertexAdapter) extractUsage(body []byte) (Usage, bool) {
	var resp struct {
		UsageMetadata struct {
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Usage{}, false
	}
	u := resp.UsageMetadata
	if u.PromptTokenCount == 0 && u.CandidatesTokenCount == 0 {
		return Usage{}, false
	}
	return Usage{InputTokens: u.PromptTokenCount, OutputTokens: u.CandidatesTokenCount}, true
}

// accumulateStreamUsage: usageMetadata arrives on the final streamed chunk.
func (vertexAdapter) accumulateStreamUsage(data []byte, u *Usage) {
	var chunk struct {
		UsageMetadata *struct {
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil || chunk.UsageMetadata == nil {
		return
	}
	u.InputTokens = chunk.UsageMetadata.PromptTokenCount
	u.OutputTokens = chunk.UsageMetadata.CandidatesTokenCount
}

// fixupRequestBody is a no-op: Vertex carries the model in the URL, not the
// body, and the streaming toggle is a query parameter (see upstreamPath).
func (vertexAdapter) fixupRequestBody(map[string]any) {}

// upstreamPath rewrites the {model} URL segment from the qualified name to the
// raw model ID and appends ?alt=sse to streaming requests when absent, so the
// SSE relay engages.
func (vertexAdapter) upstreamPath(inboundPath, modelID string) string {
	path, query, hasQuery := strings.Cut(inboundPath, "?")
	// Rewrite the {model}:method segment: everything after the last '/' up to
	// the ':' is the (URL-encoded) qualified model name.
	if slash := strings.LastIndex(path, "/models/"); slash >= 0 {
		rest := path[slash+len("/models/"):]
		if colon := strings.Index(rest, ":"); colon >= 0 {
			method := rest[colon:]
			path = path[:slash+len("/models/")] + url.PathEscape(modelID) + method
		}
	}
	if strings.HasSuffix(path, ":streamGenerateContent") {
		if !hasQuery {
			return path + "?alt=sse"
		}
		if !strings.Contains(query, "alt=") {
			return path + "?" + query + "&alt=sse"
		}
	}
	if hasQuery {
		return path + "?" + query
	}
	return path
}
