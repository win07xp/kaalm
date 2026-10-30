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

// The Vertex path carries the qualified model as its publisher and model
// segments; only generateContent and streamGenerateContent under v1 and
// v1beta1 resolve.
func TestVertexPathsServed(t *testing.T) {
	served := []string{
		"/v1/projects/p/locations/us-central1/publishers/prov/models/gemini-2.5-flash:generateContent",
		"/v1/projects/p/locations/us-central1/publishers/prov/models/gemini-2.5-flash:streamGenerateContent",
		"/v1beta1/projects/p/locations/us-central1/publishers/prov/models/gemini-2.5-flash:generateContent",
		"/v1beta1/projects/p/locations/global/publishers/prov/models/gemini-2.5-flash:streamGenerateContent",
	}
	for _, path := range served {
		a, ok := adapterForPath(path)
		if !ok || a.formatName() != providerTypeVertex {
			t.Errorf("%s: adapter %v, %v; want the Vertex adapter", path, a, ok)
			continue
		}
		if got := a.modelFromPath(path); got != "prov/gemini-2.5-flash" {
			t.Errorf("%s: modelFromPath = %q, want prov/gemini-2.5-flash", path, got)
		}
	}
	notServed := []string{
		// %2F decoded by the mux: one segment too many.
		"/v1/projects/p/locations/l/publishers/google/models/prov/gemini:generateContent",
		"/v1/projects/p/locations/l/publishers/prov/models/gemini:countTokens",
		"/v1/projects/p/locations/l/publishers/prov/models/gemini:rawPredict",
		"/v1/projects/p/publishers/prov/models/gemini:generateContent",
		"/v1/projects/p/locations/l/publishers//models/gemini:generateContent",
		"/v1/projects/p/locations/l/publishers/prov/models/:generateContent",
		"/v1/projects/p/locations/l/publishers/prov/models/gemini",
		"/v2/projects/p/locations/l/publishers/prov/models/gemini:generateContent",
		"/v1/projects/p/locations/l/publishers/prov/models/gemini:generateContent/extra",
		"/v1/projects/nonsense",
		"/v1/nonsense",
		// %3F decoded by the mux: a '?' inside the method segment.
		"/v1/projects/p/locations/l/publishers/prov/models/gemini:streamGenerateContent?alt=json",
	}
	for _, path := range notServed {
		if a, ok := adapterForPath(path); ok {
			t.Errorf("%s resolved to %v, want no adapter", path, a)
		}
	}
	for _, a := range []providerAdapter{anthropicAdapter{}, openaiAdapter{}} {
		if got := a.modelFromPath(served[0]); got != "" {
			t.Errorf("%s modelFromPath = %q, want empty: the model travels in the body", a.formatName(), got)
		}
	}
}

// upstreamPath points the publisher at google, writes the raw model id, and
// sets alt=sse on streamGenerateContent only.
func TestVertexUpstreamPath(t *testing.T) {
	v := vertexAdapter{}
	cases := []struct{ in, model, want string }{
		{"/v1/projects/p/locations/us-central1/publishers/prov/models/gemini-2.5-flash:generateContent", "gemini-2.5-flash",
			"/v1/projects/p/locations/us-central1/publishers/google/models/gemini-2.5-flash:generateContent"},
		// A fallback edge's mapped model replaces the inbound one.
		{"/v1/projects/p/locations/us-central1/publishers/prov/models/gemini-2.5-flash:generateContent", "gemini-2.0-flash",
			"/v1/projects/p/locations/us-central1/publishers/google/models/gemini-2.0-flash:generateContent"},
		{"/v1beta1/projects/p/locations/global/publishers/prov/models/gemini-2.5-flash:streamGenerateContent", "gemini-2.5-flash",
			"/v1beta1/projects/p/locations/global/publishers/google/models/gemini-2.5-flash:streamGenerateContent?alt=sse"},
		// Project and location survive, escaped for the URL.
		{"/v1/projects/my proj/locations/l?x/publishers/prov/models/m:generateContent", "m",
			"/v1/projects/my%20proj/locations/l%3Fx/publishers/google/models/m:generateContent"},
		// A path that is not a Vertex path passes through.
		{"/v1/chat/completions", "m", "/v1/chat/completions"},
	}
	for _, c := range cases {
		if got := v.upstreamPath(c.in, c.model); got != c.want {
			t.Errorf("upstreamPath(%q, %q) = %q, want %q", c.in, c.model, got, c.want)
		}
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

// Gemini reports thinking tokens in thoughtsTokenCount, outside
// candidatesTokenCount, and bills them as output; both count as output.
func TestVertexUsageExtraction_Thoughts(t *testing.T) {
	v := vertexAdapter{}
	u, ok := v.extractUsage([]byte(`{"usageMetadata":{"promptTokenCount":88,"candidatesTokenCount":12,"thoughtsTokenCount":40}}`))
	if !ok || u.InputTokens != 88 || u.OutputTokens != 52 {
		t.Errorf("buffered usage = %+v ok=%v, want input 88, output 52", u, ok)
	}
	var acc Usage
	v.accumulateStreamUsage([]byte(`{"candidates":[{"content":{}}]}`), &acc)
	v.accumulateStreamUsage([]byte(`{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"thoughtsTokenCount":7}}`), &acc)
	if acc.InputTokens != 5 || acc.OutputTokens != 10 {
		t.Errorf("stream usage = %+v, want input 5, output 10", acc)
	}
}

// Grounding with Google Search reports executed queries in
// groundingMetadata.webSearchQueries; the adapter counts them as the
// "google_search" server tool.
func TestVertexUsageExtraction_Grounding(t *testing.T) {
	v := vertexAdapter{}
	u, ok := v.extractUsage([]byte(`{"usageMetadata":{"promptTokenCount":88,"candidatesTokenCount":12},` +
		`"candidates":[{"groundingMetadata":{"webSearchQueries":["euro 2024 winner","who won euro 2024"]}}]}`))
	if !ok || u.ServerTools["google_search"] != 2 {
		t.Errorf("grounded usage = %+v ok=%v, want google_search:2", u, ok)
	}

	// Streaming: groundingMetadata arrives on its own chunk before the final
	// usageMetadata chunk; both must survive.
	var acc Usage
	v.accumulateStreamUsage([]byte(`{"candidates":[{"groundingMetadata":{"webSearchQueries":["q1"]}}]}`), &acc)
	v.accumulateStreamUsage([]byte(`{"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}`), &acc)
	if acc.InputTokens != 5 || acc.OutputTokens != 3 || acc.ServerTools["google_search"] != 1 {
		t.Errorf("grounded stream usage = %+v", acc)
	}

	// Ungrounded responses report no server tools.
	u, _ = v.extractUsage([]byte(`{"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1},"candidates":[{}]}`))
	if u.ServerTools != nil {
		t.Errorf("ungrounded response must yield nil ServerTools: %v", u.ServerTools)
	}
}

func TestVertexCredentialHeader(t *testing.T) {
	h := http.Header{}
	vertexAdapter{}.injectCredential(h, "ya29.minted-token")
	if h.Get("Authorization") != "Bearer ya29.minted-token" {
		t.Errorf("vertex credential must be a bearer token, got %q", h.Get("Authorization"))
	}
}
