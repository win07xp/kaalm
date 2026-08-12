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
		Candidates []vertexCandidate `json:"candidates"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Usage{}, false
	}
	u := Usage{
		InputTokens:  resp.UsageMetadata.PromptTokenCount,
		OutputTokens: resp.UsageMetadata.CandidatesTokenCount,
		ServerTools:  vertexServerTools(resp.Candidates),
	}
	if u.isZero() {
		return Usage{}, false
	}
	return u, true
}

// vertexCandidate is the slice of a Vertex candidate the usage extraction
// reads: grounding with Google Search reports its executed queries in
// groundingMetadata.webSearchQueries.
type vertexCandidate struct {
	GroundingMetadata struct {
		WebSearchQueries []string `json:"webSearchQueries"`
	} `json:"groundingMetadata"`
}

// vertexServerTools counts executed grounding queries across candidates as
// the "google_search" server tool. Nil when no grounding ran.
func vertexServerTools(candidates []vertexCandidate) map[string]int64 {
	var queries int64
	for _, c := range candidates {
		queries += int64(len(c.GroundingMetadata.WebSearchQueries))
	}
	if queries == 0 {
		return nil
	}
	return map[string]int64{"google_search": queries}
}

// accumulateStreamUsage: usageMetadata arrives on the final streamed chunk;
// groundingMetadata arrives once, on the chunk closing its candidate, so a
// non-empty count replaces the last snapshot rather than accumulating.
func (vertexAdapter) accumulateStreamUsage(data []byte, u *Usage) {
	var chunk struct {
		UsageMetadata *struct {
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
		Candidates []vertexCandidate `json:"candidates"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return
	}
	if tools := vertexServerTools(chunk.Candidates); tools != nil {
		u.ServerTools = tools
	}
	if chunk.UsageMetadata == nil {
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
