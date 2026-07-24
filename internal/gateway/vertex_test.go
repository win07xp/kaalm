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
	"testing"
)

func TestVertexPathDetection(t *testing.T) {
	base := "/v1/projects/p/locations/us/publishers/google/models/gw-vertex%2Fgemini-2:generateContent"
	if _, ok := adapterForPath(base); !ok {
		t.Fatal("generateContent path must resolve to the Vertex adapter")
	}
	stream := "/v1/projects/p/locations/us/publishers/google/models/gw-vertex%2Fgemini-2:streamGenerateContent"
	if _, ok := adapterForPath(stream); !ok {
		t.Fatal("streamGenerateContent path must resolve to the Vertex adapter")
	}
	if _, ok := adapterForPath("/v1/nonsense"); ok {
		t.Error("an unrecognized path must not resolve")
	}
}

func TestVertexUpstreamPath_ModelRewriteAndAltSSE(t *testing.T) {
	v := vertexAdapter{}
	// The qualified {providerRef}/{modelId} in the URL is rewritten to the raw
	// model ID (URL-encoded).
	in := "/v1/projects/p/locations/us/publishers/google/models/prov%2Fgemini-2:generateContent"
	got := v.upstreamPath(in, "gemini-2")
	if got != "/v1/projects/p/locations/us/publishers/google/models/gemini-2:generateContent" {
		t.Errorf("model not rewritten: %q", got)
	}
	// Streaming gets ?alt=sse appended when absent.
	streamIn := "/v1/projects/p/locations/us/publishers/google/models/prov%2Fgemini-2:streamGenerateContent"
	gotStream := v.upstreamPath(streamIn, "gemini-2")
	if gotStream != "/v1/projects/p/locations/us/publishers/google/models/gemini-2:streamGenerateContent?alt=sse" {
		t.Errorf("alt=sse not injected: %q", gotStream)
	}
	// An existing alt= is preserved (not doubled).
	withAlt := streamIn + "?alt=sse"
	if got := v.upstreamPath(withAlt, "gemini-2"); got != gotStream {
		t.Errorf("alt=sse should not be doubled: %q", got)
	}
}

func TestVertexUsageExtraction(t *testing.T) {
	v := vertexAdapter{}
	u, ok := v.extractUsage([]byte(`{"usageMetadata":{"promptTokenCount":88,"candidatesTokenCount":12}}`))
	if !ok || u.InputTokens != 88 || u.OutputTokens != 12 {
		t.Errorf("buffered usage wrong: %+v ok=%v", u, ok)
	}
	var acc Usage
	v.accumulateStreamUsage([]byte(`{"candidates":[{"content":{}}]}`), &acc)
	v.accumulateStreamUsage([]byte(`{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}`), &acc)
	if acc.InputTokens != 5 || acc.OutputTokens != 3 {
		t.Errorf("stream usage wrong: %+v", acc)
	}
}

func TestVertexCredentialHeader(t *testing.T) {
	h := http.Header{}
	vertexAdapter{}.injectCredential(h, "ya29.minted-token")
	if h.Get("Authorization") != "Bearer ya29.minted-token" {
		t.Errorf("vertex credential must be a bearer token, got %q", h.Get("Authorization"))
	}
}
