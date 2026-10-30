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

// The adapter for the google-vertex type, in Google's native Gemini format
// (generateContent and streamGenerateContent). The model travels in the URL
// path, not the body: the caller writes the qualified name as the publisher
// and model segments, and upstreamPath points the publisher back at google
// with the raw model id. The bearer credential is an access token the
// gateway mints from the provider's service-account key (vertex_token.go).
// The platform's product name is now the Gemini Enterprise Agent Platform;
// the wire API and the enum value are unchanged.

type vertexAdapter struct{}

func (vertexAdapter) formatName() string { return providerTypeVertex }

// injectCredential attaches the bearer token forwardOnce minted from the
// provider's service-account key.
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

// streamUsageOption is false: a Vertex request needs no body change, since
// the streaming toggle is a query parameter (see upstreamPath).
func (vertexAdapter) streamUsageOption() bool { return false }

// vertexPath is a parsed Vertex call path:
// /{version}/projects/{project}/locations/{location}/publishers/{providerRef}/models/{model}:{method}.
type vertexPath struct {
	version, project, location, providerRef, model, method string
}

// parseVertexPath parses a decoded request path in the Vertex shape. Only
// the v1 and v1beta1 versions (the google-genai SDK defaults to v1beta1)
// and the generateContent and streamGenerateContent methods are served;
// every segment must be present and non-empty, so a %2F the mux decoded
// into an extra segment does not parse.
func parseVertexPath(path string) (vertexPath, bool) {
	seg := strings.Split(path, "/")
	// "", version, projects, p, locations, l, publishers, ref, models, model:method
	if len(seg) != 10 || seg[0] != "" ||
		seg[2] != "projects" || seg[4] != "locations" || seg[6] != "publishers" || seg[8] != "models" {
		return vertexPath{}, false
	}
	if seg[1] != "v1" && seg[1] != "v1beta1" {
		return vertexPath{}, false
	}
	model, method, ok := strings.Cut(seg[9], ":")
	if !ok || (method != "generateContent" && method != "streamGenerateContent") {
		return vertexPath{}, false
	}
	p := vertexPath{
		version: seg[1], project: seg[3], location: seg[5],
		providerRef: seg[7], model: model, method: method,
	}
	if p.project == "" || p.location == "" || p.providerRef == "" || p.model == "" {
		return vertexPath{}, false
	}
	return p, true
}

// qualified is the {providerRef}/{modelId} name the path carries.
func (p vertexPath) qualified() string { return p.providerRef + "/" + p.model }

// modelFromPath returns the qualified model of a Vertex path.
func (vertexAdapter) modelFromPath(path string) string {
	if p, ok := parseVertexPath(path); ok {
		return p.qualified()
	}
	return ""
}

// upstreamPath rebuilds a Vertex path for the upstream: the publisher is
// google (the type serves Google's Gemini models) and the model the raw id
// the request resolved to (degraded or mapped by a fallback edge). Project,
// location, and version pass through. streamGenerateContent gets ?alt=sse
// so the answer is an SSE stream the relay can meter. A path that is not a
// Vertex path passes through unchanged.
func (vertexAdapter) upstreamPath(inboundPath, modelID string) string {
	p, ok := parseVertexPath(inboundPath)
	if !ok {
		return inboundPath
	}
	out := "/" + p.version + "/projects/" + url.PathEscape(p.project) +
		"/locations/" + url.PathEscape(p.location) +
		"/publishers/google/models/" + url.PathEscape(modelID) + ":" + p.method
	if p.method == "streamGenerateContent" {
		out += "?alt=sse"
	}
	return out
}
